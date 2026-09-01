package sessionwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
)

type applicationPrefix struct {
	CommandID        string
	RuntimeCommandID uuid.UUID
	LeaseEpoch       uint64
}

type credentialPayload struct{ Token string }

func testUUID(seed byte) uuid.UUID {
	var id uuid.UUID
	for index := range id {
		id[index] = seed
	}
	return id
}

func TestEveryHarnessEventHasExplicitProjectionPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value any
		want  EventClass
	}{
		{event.SessionStarted{}, PublicEnduring},
		{event.SessionActive{}, PublicEnduring},
		{event.SessionIdle{}, PublicEnduring},
		{event.SessionStopped{}, PublicEnduring},
		{event.RestoreStarted{}, PublicEnduring},
		{event.RestoreDone{}, PublicEnduring},
		{event.RestoreErrored{}, PublicEnduring},
		{event.ConfigurationAdopted{}, PublicEnduring},
		{event.WorkspaceCheckpointed{}, PublicEnduring},
		{event.WorkspaceRestored{}, PublicEnduring},
		{event.ActiveLoopChanged{}, PublicEnduring},
		{event.DelegateDeliveryStateChanged{}, PublicEnduring},
		{event.WorkflowActivity{}, PublicEnduring},
		{event.LoopRestoreTombstoned{}, PublicEnduring},
		{event.IntegrationStatus{}, PublicEphemeral},
		{event.HustleStarted{}, PrivateRejected},
		{event.HustleCompleted{}, PrivateRejected},
		{event.HustleFailed{}, PrivateRejected},
		{event.PermissionReviewStarted{}, PrivateRejected},
		{event.PermissionReviewCompleted{}, PrivateRejected},
		{event.ProcessStarted{}, PublicEnduring},
		{event.ProcessBackgrounded{}, PublicEnduring},
		{event.ProcessCompleted{}, PublicEnduring},
		{event.ProcessStopRequested{}, PublicEnduring},
		{event.ProcessLost{}, PublicEnduring},
		{event.LoopIdle{}, PublicEnduring},
		{event.LoopStarted{}, PublicEnduring},
		{event.DelegateRequestAccepted{}, PublicEnduring},
		{event.LoopInferenceChanged{}, PublicEnduring},
		{event.LoopModeChanged{}, PublicEnduring},
		{event.LoopExternalToolsetChanged{}, PublicEnduring},
		{event.ContextMeasured{}, PublicEnduring},
		{event.ContextPressure{}, PublicEphemeral},
		{event.CompactionStarted{}, PublicEphemeral},
		{event.CompactionCommitted{}, PublicEnduring},
		{event.CompactionRejected{}, PublicEnduring},
		{event.CompactWaiterResolved{}, PublicEnduring},
		{event.CompactWaiterRejected{}, PublicEnduring},
		{event.ForeignSessionBound{}, PublicEnduring},
		{event.LoopAgentSessionBound{}, PublicEnduring},
		{event.TokenDelta{}, PublicEphemeral},
		{event.TurnStarted{}, PublicEnduring},
		{event.StepDone{}, PublicEnduring},
		{event.TurnFoldedInto{}, PublicEnduring},
		{event.InputCancelled{}, PublicEnduring},
		{event.InputQueued{}, PublicEphemeral},
		{event.TurnRejected{}, PublicEnduring},
		{event.TurnDone{}, PublicEnduring},
		{event.TurnFailed{}, PublicEnduring},
		{event.TurnInterrupted{}, PublicEnduring},
		{event.PermissionRequested{}, PublicEnduring},
		{event.PermissionDecided{}, PublicEnduring},
		{event.UserInputRequested{}, PublicEnduring},
		{event.ToolCallStarted{}, PublicEphemeral},
		{event.ToolCallCompleted{}, PublicEphemeral},
		{event.GatePrepared{}, PrivateRejected},
		{event.GateOpened{}, PublicEnduring},
		{event.GateResolved{}, PublicEnduring},

		// These are deliberately outside the event union. They are private inputs
		// or persistence controls and must never acquire a generic JSON fallback.
		{gate.Answer{Values: map[string]string{"answer": "private"}}, PrivateRejected},
		{tool.PreparedCall{Grants: []string{"grant-token"}}, PrivateRejected},
		{credentialPayload{Token: "credential-token"}, PrivateRejected},
		{applicationPrefix{CommandID: "command-1", RuntimeCommandID: testUUID(90), LeaseEpoch: 4}, PrivateRejected},
	}

	seen := make(map[reflect.Type]struct{}, len(tests))
	for _, test := range tests {
		typeOf := reflect.TypeOf(test.value)
		if _, duplicate := seen[typeOf]; duplicate {
			t.Fatalf("duplicate policy row for %v", typeOf)
		}
		seen[typeOf] = struct{}{}
		if got := classify(test.value); got != test.want {
			t.Errorf("classify(%T) = %q, want %q", test.value, got, test.want)
		}
		ev, isEvent := test.value.(event.Event)
		if !isEvent || test.want == PrivateRejected {
			continue
		}
		wantLifecycle := event.Enduring
		if test.want == PublicEphemeral {
			wantLifecycle = event.Ephemeral
		}
		if got := ev.Class(); got != wantLifecycle {
			t.Errorf("classify(%T) = %q but Event.Class() = %v, want %v", test.value, test.want, got, wantLifecycle)
		}
	}
	if len(tests) < 62 {
		t.Fatalf("policy table has %d rows, want at least 62", len(tests))
	}
}

func TestAlwaysInternalEventTypesAreRejectedByPolicy(t *testing.T) {
	t.Parallel()
	values := []event.Event{
		event.HustleStarted{},
		event.HustleCompleted{},
		event.HustleFailed{},
		event.PermissionReviewStarted{},
		event.PermissionReviewCompleted{},
	}
	for _, value := range values {
		if got := classify(value); got != PrivateRejected {
			t.Errorf("classify(%T) = %q, want %q", value, got, PrivateRejected)
		}
	}
}

func TestProjectPreservesCallerScopeEventAndToolCorrelations(t *testing.T) {
	t.Parallel()
	header := event.Header{
		Coordinates: identity.Coordinates{
			SessionID: testUUID(1), LoopID: testUUID(2), TurnID: testUUID(3), StepID: testUUID(4),
		},
		EventID: testUUID(5),
	}
	ev := event.ToolCallStarted{Header: header, ToolExecutionID: testUUID(6), ToolName: "Bash", Summary: "run tests"}

	got, err := Project(coresessionwire.TenantID("tenant-a"), coresessionwire.SessionID("public-session"), ev)
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	if got.Class != PublicEphemeral || got.TenantID != "tenant-a" || got.SessionID != "public-session" {
		t.Fatalf("projection metadata = %#v", got)
	}
	if got.EventID != "" {
		t.Fatalf("ephemeral EventID = %q, want empty envelope identity", got.EventID)
	}
	var body map[string]any
	if err := json.Unmarshal(got.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal(body): %v", err)
	}
	for field, want := range map[string]string{
		"event_id":          testUUID(5).String(),
		"loop_id":           testUUID(2).String(),
		"turn_id":           testUUID(3).String(),
		"step_id":           testUUID(4).String(),
		"tool_execution_id": testUUID(6).String(),
	} {
		if gotValue, _ := body[field].(string); gotValue != want {
			t.Errorf("body[%q] = %q, want %q; body=%s", field, gotValue, want, got.Body)
		}
	}
	publication := coresessionwire.EphemeralPublication{TenantID: got.TenantID, SessionID: got.SessionID, Body: got.Body}
	if err := publication.Validate(); err != nil {
		t.Fatalf("Core EphemeralPublication.Validate() = %v; body=%s", err, got.Body)
	}
}

func TestProjectEnduringDoesNotManufactureJournalSequence(t *testing.T) {
	t.Parallel()
	header := event.Header{Coordinates: identity.Coordinates{SessionID: testUUID(1)}, EventID: testUUID(5)}
	got, err := Project("tenant-a", "public-session", event.SessionActive{Header: header})
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	if got.Class != PublicEnduring || got.EventID != coresessionwire.EventID(testUUID(5).String()) {
		t.Fatalf("projection = %#v", got)
	}
	if bytes.Contains(got.Body, []byte("journal_seq")) || bytes.Contains(got.Body, []byte("covered_through")) {
		t.Fatalf("pre-append body manufactured journal metadata: %s", got.Body)
	}
	canonical := coresessionwire.JournalEvent{EventID: got.EventID, JournalSeq: 1, Body: got.Body}
	if err := canonical.Validate(); err != nil {
		t.Fatalf("Core JournalEvent.Validate() = %v; body=%s", err, got.Body)
	}
}

func TestProjectRelayBytesAreStable(t *testing.T) {
	t.Parallel()
	header := event.Header{Coordinates: identity.Coordinates{SessionID: testUUID(1)}, EventID: testUUID(5)}
	first, err := Project("tenant-a", "public-session", event.SessionIdle{Header: header})
	if err != nil {
		t.Fatalf("first Project() error = %v", err)
	}
	second, err := Project("tenant-a", "public-session", event.SessionIdle{Header: header})
	if err != nil {
		t.Fatalf("second Project() error = %v", err)
	}
	if !bytes.Equal(first.Body, second.Body) {
		t.Fatalf("projection bytes differ:\nfirst  %s\nsecond %s", first.Body, second.Body)
	}
	wantBody := `{"event_id":"` + testUUID(5).String() + `","session_id":"` + testUUID(1).String() + `","type":"SessionIdle","v":1}`
	if string(first.Body) != wantBody {
		t.Fatalf("body = %s, want stable canonical bytes %s", first.Body, wantBody)
	}
	publication := coresessionwire.EnduringPublication{
		TenantID: first.TenantID, SessionID: first.SessionID, EventID: first.EventID,
		JournalSeq: 7, CoveredThrough: 7, Body: first.Body,
	}
	wire, err := json.Marshal(publication)
	if err != nil {
		t.Fatalf("json.Marshal(publication): %v", err)
	}
	var relayed coresessionwire.EnduringPublication
	if err := json.Unmarshal(wire, &relayed); err != nil {
		t.Fatalf("json.Unmarshal(publication): %v", err)
	}
	if !bytes.Equal(relayed.Body, first.Body) {
		t.Fatalf("relay changed body:\nprojected %s\nrelayed   %s", first.Body, relayed.Body)
	}
}

func TestProjectRejectsGatePreparedBeforeValidation(t *testing.T) {
	t.Parallel()
	_, err := Project("tenant-a", "public-session", event.GatePrepared{})
	var projectionErr *ProjectionError
	if !errors.As(err, &projectionErr) {
		t.Fatalf("error = %T %v, want *ProjectionError", err, err)
	}
	if projectionErr.Reason != ProjectionRejected {
		t.Fatalf("reason = %q, want %q", projectionErr.Reason, ProjectionRejected)
	}
}

func TestProjectRejectsPermissionReviewEventsBeforeValidation(t *testing.T) {
	t.Parallel()
	internalHeader := event.Header{
		Coordinates: identity.Coordinates{
			SessionID: testUUID(1), LoopID: testUUID(2), TurnID: testUUID(3), StepID: testUUID(4),
		},
		EventID: testUUID(5), EventVisibility: event.Internal,
	}
	validStarted := event.PermissionReviewStarted{
		Header: internalHeader, GateID: gate.ID(testUUID(7)), ToolExecutionID: testUUID(8),
		Classifier: hustle.Name("command-safety"), ClassifierRevision: "classifier-v1",
	}
	validCompleted := event.PermissionReviewCompleted{
		Header: internalHeader, GateID: gate.ID(testUUID(7)), ToolExecutionID: testUUID(8),
		Classifier: hustle.Name("command-safety"), ClassifierRevision: "classifier-v1",
		Status: gate.ReviewStatusAllowed, Risk: gate.ReviewRiskLow,
		Authorization: gate.ReviewAuthorizationUnknown,
		Categories:    []gate.ReviewRiskCategory{gate.ReviewCategoryMutableNetwork},
		AutoApproved:  true,
	}
	for name, value := range map[string]event.Event{
		"valid internal started":   validStarted,
		"valid internal completed": validCompleted,
	} {
		if err := event.ValidateEvent(value); err != nil {
			t.Fatalf("%s fixture is not valid-shaped: %v", name, err)
		}
	}

	tests := []struct {
		name  string
		value any
	}{
		{name: "valid internal started", value: validStarted},
		{name: "valid internal completed", value: validCompleted},
		{name: "malformed public started", value: event.PermissionReviewStarted{}},
		{name: "malformed public completed", value: event.PermissionReviewCompleted{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Project("tenant-a", "public-session", test.value)
			var projectionErr *ProjectionError
			if !errors.As(err, &projectionErr) {
				t.Fatalf("error = %T %v, want *ProjectionError", err, err)
			}
			if projectionErr.Reason != ProjectionRejected {
				t.Fatalf("reason = %q, want rejection before event validation", projectionErr.Reason)
			}
		})
	}
}

func TestProjectRejectsMalformedAndPrivateValues(t *testing.T) {
	t.Parallel()
	validHeader := event.Header{Coordinates: identity.Coordinates{SessionID: testUUID(1)}, EventID: testUUID(5)}
	tests := []struct {
		name  string
		value any
	}{
		{name: "nil"},
		{name: "malformed public event", value: event.SessionActive{}},
		{name: "private visibility", value: event.SessionActive{Header: event.Header{EventVisibility: event.Internal}}},
		{name: "GatePrepared rejected regardless of public visibility", value: event.GatePrepared{Header: validHeader}},
		{name: "raw gate answer", value: gate.Answer{Values: map[string]string{"secret": "answer"}}},
		{name: "grant-bearing invocation", value: tool.PreparedCall{Grants: []string{"grant-token"}}},
		{name: "credential payload", value: credentialPayload{Token: "credential-token"}},
		{name: "application prefix", value: applicationPrefix{CommandID: "command-1", RuntimeCommandID: testUUID(9), LeaseEpoch: 1}},
		{name: "pointer event has no implicit admission", value: &event.SessionActive{Header: validHeader}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Project("tenant-a", "public-session", test.value)
			if err == nil {
				t.Fatal("Project() error = nil")
			}
			var projectionErr *ProjectionError
			if !errors.As(err, &projectionErr) {
				t.Fatalf("error = %T %v, want *ProjectionError", err, err)
			}
			for _, secret := range []string{"secret", "answer", "grant-token", "credential-token"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaked %q: %v", secret, err)
				}
			}
		})
	}
}

func TestProjectGateResolvedOmitsRawAnswerAudit(t *testing.T) {
	t.Parallel()
	header := event.Header{
		Coordinates: identity.Coordinates{SessionID: testUUID(1), LoopID: testUUID(2), TurnID: testUUID(3), StepID: testUUID(4)},
		EventID:     testUUID(5),
	}
	ev := event.GateResolved{
		Header: header, GateID: gate.ID(testUUID(7)), Resolver: gate.ResolverLoop,
		Reason: gate.CloseAnswered, Action: gate.FormActionAccept,
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
		Audit:  gate.FormAudit{Values: map[string]string{"password": "raw-answer-marker"}},
	}
	got, err := Project("tenant-a", "public-session", ev)
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	if bytes.Contains(got.Body, []byte("raw-answer-marker")) || bytes.Contains(got.Body, []byte(`"audit"`)) {
		t.Fatalf("public gate resolution leaked raw audit: %s", got.Body)
	}
	for _, marker := range []string{`"type":"GateResolved"`, `"gate_id":"` + testUUID(7).String() + `"`, `"action":"accept"`} {
		if !bytes.Contains(got.Body, []byte(marker)) {
			t.Errorf("body missing %s: %s", marker, got.Body)
		}
	}
}

func TestProjectTokenDeltaOmitsRawToolArguments(t *testing.T) {
	t.Parallel()
	header := event.Header{
		Coordinates: identity.Coordinates{SessionID: testUUID(1), LoopID: testUUID(2), TurnID: testUUID(3)},
		EventID:     testUUID(5),
	}
	ev := event.TokenDelta{
		Header: header,
		Chunk:  &content.ToolUseChunk{Index: 2, ID: "call-1", Name: "shell", InputJSON: `{"token":"raw-tool-argument"}`},
	}
	got, err := Project("tenant-a", "public-session", ev)
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	if bytes.Contains(got.Body, []byte("raw-tool-argument")) || bytes.Contains(got.Body, []byte("input_json")) {
		t.Fatalf("public token delta leaked raw tool input: %s", got.Body)
	}
	for _, marker := range []string{`"chunk_type":"tool_use"`, `"id":"call-1"`, `"name":"shell"`} {
		if !bytes.Contains(got.Body, []byte(marker)) {
			t.Errorf("body missing %s: %s", marker, got.Body)
		}
	}
}

func TestProjectAcceptsProductionShapedZeroEventIDEphemerals(t *testing.T) {
	t.Parallel()
	turnHeader := event.Header{Coordinates: identity.Coordinates{
		SessionID: testUUID(1), LoopID: testUUID(2), TurnID: testUUID(3),
	}}
	toolHeader := turnHeader
	toolHeader.StepID = testUUID(4)
	loopHeader := event.Header{Coordinates: identity.Coordinates{SessionID: testUUID(1), LoopID: testUUID(2)}}
	measurement := validProjectionContextMeasurement()
	tests := []struct {
		name  string
		value event.Event
	}{
		{name: "TokenDelta", value: event.TokenDelta{Header: turnHeader, TurnIndex: 2, Chunk: &content.TextChunk{Text: "partial"}}},
		{name: "ToolCallStarted", value: event.ToolCallStarted{Header: toolHeader, ToolExecutionID: testUUID(5), ToolName: "shell", Summary: "run tests"}},
		{name: "ToolCallCompleted", value: event.ToolCallCompleted{Header: toolHeader, ToolExecutionID: testUUID(5), ResultPreview: "done"}},
		{name: "InputQueued", value: event.InputQueued{Header: event.Header{Coordinates: loopHeader.Coordinates, Cause: identity.Cause{CommandID: testUUID(6)}}}},
		{name: "ContextPressure", value: event.ContextPressure{Header: loopHeader, Measurement: measurement, Occupancy: 8_000, Previous: event.PressureNormal, Current: event.PressureCompact}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projection, err := Project("tenant-a", "public-session", test.value)
			if err != nil {
				var invalid *event.InvalidEventError
				if errors.As(err, &invalid) && invalid.Field == event.FieldEventID {
					t.Fatalf("production-shaped zero-EventID ephemeral was rejected by enduring identity validation: %v", err)
				}
				t.Fatalf("Project() error = %T %v", err, err)
			}
			if projection.Class != PublicEphemeral || projection.EventID != "" {
				t.Fatalf("projection = %#v, want public ephemeral without envelope EventID", projection)
			}
			if bytes.Contains(projection.Body, []byte(`"event_id"`)) {
				t.Fatalf("body manufactured EventID: %s", projection.Body)
			}
		})
	}
}

func TestProjectStillValidatesZeroEventIDEphemerals(t *testing.T) {
	t.Parallel()
	turnHeader := event.Header{Coordinates: identity.Coordinates{
		SessionID: testUUID(1), LoopID: testUUID(2), TurnID: testUUID(3),
	}}
	toolHeader := turnHeader
	toolHeader.StepID = testUUID(4)
	loopHeader := event.Header{Coordinates: identity.Coordinates{SessionID: testUUID(1), LoopID: testUUID(2)}}
	tests := []struct {
		name      string
		value     event.Event
		wantField event.FieldName
	}{
		{name: "TokenDelta missing session", value: event.TokenDelta{Header: event.Header{Coordinates: identity.Coordinates{LoopID: testUUID(2), TurnID: testUUID(3)}}, Chunk: &content.TextChunk{Text: "partial"}}, wantField: event.FieldSessionID},
		{name: "ToolCallStarted missing tool correlation", value: event.ToolCallStarted{Header: toolHeader, ToolName: "shell"}, wantField: event.FieldToolExecutionID},
		{name: "ToolCallCompleted missing step", value: event.ToolCallCompleted{Header: turnHeader, ToolExecutionID: testUUID(5)}, wantField: event.FieldStepID},
		{name: "InputQueued carries forbidden turn", value: event.InputQueued{Header: event.Header{Coordinates: identity.Coordinates{SessionID: testUUID(1), LoopID: testUUID(2), TurnID: testUUID(3)}, Cause: identity.Cause{CommandID: testUUID(6)}}}, wantField: event.FieldTurnID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Project("tenant-a", "public-session", test.value)
			var invalid *event.InvalidEventError
			if !errors.As(err, &invalid) {
				t.Fatalf("error = %T %v, want *event.InvalidEventError", err, err)
			}
			if invalid.Field != test.wantField {
				t.Fatalf("field = %q, want %q", invalid.Field, test.wantField)
			}
		})
	}

	invalidPressure := event.ContextPressure{
		Header: loopHeader, Measurement: validProjectionContextMeasurement(), Occupancy: 8_000,
		Previous: event.PressureCompact, Current: event.PressureCompact,
	}
	_, err := Project("tenant-a", "public-session", invalidPressure)
	var contextErr *event.ContextValidationError
	if !errors.As(err, &contextErr) || contextErr.Field != event.ContextField("Current") {
		t.Fatalf("invalid ContextPressure error = %T %v, want Current ContextValidationError", err, err)
	}
}

func TestProjectRejectsEveryReplyWithoutCommandCorrelation(t *testing.T) {
	t.Parallel()
	for _, test := range replyProjectionCases(uuid.UUID{}) {
		t.Run(test.name, func(t *testing.T) {
			_, err := Project("tenant-a", "public-session", test.value)
			var invalid *event.InvalidEventError
			if !errors.As(err, &invalid) {
				t.Fatalf("Project() error = %T %v, want *event.InvalidEventError", err, err)
			}
			if invalid.Event != event.EventName(test.name) || invalid.Field != event.FieldCommandID || invalid.Rule != event.RuleRequired {
				t.Fatalf("invalid event = %#v, want %s CommandID required", invalid, test.name)
			}
		})
	}
}

func TestProjectPreservesReplyCommandCorrelation(t *testing.T) {
	t.Parallel()
	commandID := testUUID(11)
	for _, test := range replyProjectionCases(commandID) {
		t.Run(test.name, func(t *testing.T) {
			projection, err := Project("tenant-a", "public-session", test.value)
			if err != nil {
				t.Fatalf("Project() error = %T %v", err, err)
			}
			if !bytes.Contains(projection.Body, []byte(commandID.String())) {
				t.Fatalf("projected body lost ReplyTo correlation %q: %s", commandID, projection.Body)
			}
		})
	}
}

func replyProjectionCases(commandID uuid.UUID) []struct {
	name  string
	value event.Event
} {
	loopHeader := event.Header{
		Coordinates: identity.Coordinates{SessionID: testUUID(1), LoopID: testUUID(2)},
		EventID:     testUUID(7),
		Cause:       identity.Cause{CommandID: commandID},
	}
	turnHeader := loopHeader
	turnHeader.TurnID = testUUID(3)
	attemptID := event.CompactAttemptID(testUUID(8))
	resolvedHeader := loopHeader
	resolvedHeader.EventID = event.CompactWaiterReplyID(attemptID, commandID, true)
	rejectedHeader := loopHeader
	rejectedHeader.EventID = event.CompactWaiterReplyID(attemptID, commandID, false)
	queuedHeader := loopHeader
	queuedHeader.EventID = uuid.UUID{}
	return []struct {
		name  string
		value event.Event
	}{
		{name: "TurnStarted", value: event.TurnStarted{Header: turnHeader, TurnIndex: 1}},
		{name: "InputQueued", value: event.InputQueued{Header: queuedHeader}},
		{name: "TurnRejected", value: event.TurnRejected{Header: loopHeader, Reason: event.RejectQueueFull}},
		{name: "TurnFoldedInto", value: event.TurnFoldedInto{Header: turnHeader, TurnIndex: 1}},
		{name: "InputCancelled", value: event.InputCancelled{Header: loopHeader, Reason: event.CancelClientRetracted}},
		{name: "CompactWaiterResolved", value: event.CompactWaiterResolved{Header: resolvedHeader, AttemptID: attemptID, CommittedEventID: testUUID(9)}},
		{name: "CompactWaiterRejected", value: event.CompactWaiterRejected{Header: rejectedHeader, AttemptID: attemptID, Reason: event.CompactRejectControlLaneFull}},
	}
}

func TestContextPressureProjectionMatchesAuthoritativeValidation(t *testing.T) {
	t.Parallel()
	valid := validProjectionContextMeasurement()
	tests := []struct {
		name   string
		mutate func(*event.ContextPressure)
	}{
		{name: "valid unknown to normal", mutate: func(value *event.ContextPressure) {
			value.Previous = event.PressureUnknown
			value.Current = event.PressureNormal
		}},
		{name: "valid full scale compact to hard limit", mutate: func(value *event.ContextPressure) {
			value.Occupancy = event.FullScaleBasisPoints
			value.Previous = event.PressureCompact
			value.Current = event.PressureHardLimit
		}},
		{name: "invalid measurement revision", mutate: func(value *event.ContextPressure) { value.Measurement.Basis.Revision = 0 }},
		{name: "invalid measurement event identity", mutate: func(value *event.ContextPressure) { value.Measurement.Basis.ThroughEventID = uuid.UUID{} }},
		{name: "invalid measurement model", mutate: func(value *event.ContextPressure) { value.Measurement.Model = model.ModelKey{} }},
		{name: "invalid measurement fingerprint", mutate: func(value *event.ContextPressure) { value.Measurement.RequestFingerprint = [32]byte{} }},
		{name: "invalid measurement input limit", mutate: func(value *event.ContextPressure) { value.Measurement.InputLimit = 0 }},
		{name: "invalid measurement quality", mutate: func(value *event.ContextPressure) { value.Measurement.Quality = contextcount.CountQuality(255) }},
		{name: "invalid occupancy above full scale", mutate: func(value *event.ContextPressure) { value.Occupancy = event.FullScaleBasisPoints + 1 }},
		{name: "invalid previous level", mutate: func(value *event.ContextPressure) { value.Previous = event.PressureHardLimit + 1 }},
		{name: "invalid current unknown", mutate: func(value *event.ContextPressure) { value.Current = event.PressureUnknown }},
		{name: "invalid current above hard limit", mutate: func(value *event.ContextPressure) { value.Current = event.PressureHardLimit + 1 }},
		{name: "invalid unchanged transition", mutate: func(value *event.ContextPressure) {
			value.Previous = event.PressureCompact
			value.Current = event.PressureCompact
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := event.ContextPressure{
				Header:      event.Header{Coordinates: identity.Coordinates{SessionID: testUUID(1), LoopID: testUUID(2)}},
				Measurement: valid, Occupancy: 8_000, Previous: event.PressureNormal, Current: event.PressureCompact,
			}
			test.mutate(&value)
			authoritative := value
			authoritative.EventID = testUUID(7)
			wantErr := event.ValidateEvent(authoritative)
			projection, gotErr := Project("tenant-a", "public-session", value)
			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("Project() error = %T %v, authoritative validation error = %T %v", gotErr, gotErr, wantErr, wantErr)
			}
			if wantErr == nil {
				if projection.Class != PublicEphemeral || projection.EventID != "" {
					t.Fatalf("projection = %#v, want zero-ID public ephemeral", projection)
				}
				return
			}
			var wantContext, gotContext *event.ContextValidationError
			if !errors.As(wantErr, &wantContext) || !errors.As(gotErr, &gotContext) || gotContext.Field != wantContext.Field {
				t.Fatalf("Project() error = %T %v, authoritative validation error = %T %v", gotErr, gotErr, wantErr, wantErr)
			}
		})
	}
}

func validProjectionContextMeasurement() event.ContextMeasurement {
	return event.ContextMeasurement{
		Basis:              event.ContextBasis{Revision: 1, ThroughEventID: testUUID(9)},
		Model:              model.ModelKey{Provider: "provider", Model: "model"},
		RequestFingerprint: [32]byte{1},
		InputTokens:        80,
		InputLimit:         100,
		Quality:            contextcount.CountQualityExactLocal,
	}
}

// Compile-time use keeps the private Hustle types explicit even if their
// constructors evolve; their projection policy is rejection, never validation.
var _ = []event.Event{event.HustleStarted{Run: event.HustleRunDescriptor{RunID: hustle.RunID{}}}}
