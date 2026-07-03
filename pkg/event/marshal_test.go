package event

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// seededUUID builds a deterministic non-zero uuid from a single seed byte so the
// marshalled output is stable across runs.
func seededUUID(seed byte) uuid.UUID {
	var u uuid.UUID
	for i := range u {
		u[i] = seed
	}
	return u
}

// topLevelKeys parses a JSON object into the set of its top-level key names. It is
// the shape assertion used by the journal-stability tests: we care that the
// produced keys are the stable snake_case names, not the (interface-laden,
// non-round-trippable) values behind them.
func topLevelKeys(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal to key map: %v", err)
	}
	return m
}

func hasKey(m map[string]json.RawMessage, key string) bool {
	_, ok := m[key]
	return ok
}

// TestEventBodyJSONKeysAreStableSnakeCase marshals fully-populated representative
// events and asserts the journal output carries the stable snake_case keys from
// the design's Naming Rules. It asserts the marshalled SHAPE (top-level key names)
// rather than a full round-trip: content.Block is a sealed interface with no
// generic UnmarshalJSON, so a whole-event round-trip through encoding/json is
// infeasible for events that carry blocks/messages. The keys are what the journal
// reader depends on, so the shape is the contract under test.
func TestEventBodyJSONKeysAreStableSnakeCase(t *testing.T) {
	t.Parallel()

	hdr := Header{
		Coordinates: identity.Coordinates{
			SessionID: seededUUID(0x11),
			LoopID:    seededUUID(0x22),
			TurnID:    seededUUID(0x33),
			StepID:    seededUUID(0x44),
		},
		EventID: seededUUID(0x55),
		Cause: identity.Cause{
			CommandID: seededUUID(0x66),
			Agency:    identity.AgencyUser,
		},
	}

	tests := []struct {
		name     string
		event    Event
		wantKeys []string
		// absentKeys must NOT appear (e.g. machine default agency is omitzero).
		absentKeys []string
	}{
		{
			name: "ToolCallStarted carries tool_execution_id, tool_name, summary",
			event: ToolCallStarted{
				Header:          hdr,
				ToolExecutionID: seededUUID(0x77),
				ToolName:        "Bash",
				Summary:         "ls -la",
			},
			wantKeys: []string{
				"session_id", "loop_id", "turn_id", "step_id",
				"event_id", "cause", "tool_execution_id", "tool_name", "summary",
			},
		},
		{
			name: "TurnStarted carries turn_index and message",
			event: TurnStarted{
				Header:    hdr,
				TurnIndex: 7,
				Message: &content.UserMessage{Message: content.Message{
					Role:   content.RoleUser,
					Blocks: []content.Block{&content.TextBlock{Text: "hi"}},
				}},
			},
			wantKeys: []string{"session_id", "loop_id", "turn_id", "event_id", "cause", "turn_index", "message"},
		},
		{
			name: "ToolCallCompleted carries is_error and result_preview",
			event: ToolCallCompleted{
				Header:          hdr,
				ToolExecutionID: seededUUID(0x77),
				IsError:         true,
				ResultPreview:   "boom",
			},
			wantKeys: []string{"tool_execution_id", "is_error", "result_preview"},
		},
		{
			// Request is a no-codec sealed interface tagged json:"-"; even a NON-nil
			// request must never reach the journal (it would marshal to lossy,
			// un-keyed PascalCase). Only the addressable tool_execution_id survives.
			name: "PermissionRequested journals tool_execution_id but never request",
			event: PermissionRequested{
				Header:          hdr,
				ToolExecutionID: seededUUID(0x77),
				Request:         tool.BashRequest{Command: "rm -rf /"},
			},
			wantKeys:   []string{"tool_execution_id"},
			absentKeys: []string{"request", "Request"},
		},
		{
			name: "UserInputRequested carries question and choices",
			event: UserInputRequested{
				Header:          hdr,
				ToolExecutionID: seededUUID(0x77),
				Question:        "pick one",
				Choices:         []string{"a", "b"},
			},
			wantKeys: []string{"tool_execution_id", "question", "choices"},
		},
		{
			name: "TurnRejected carries reason",
			event: TurnRejected{
				Header: hdr,
				Reason: RejectQueueFull,
			},
			wantKeys: []string{"event_id", "cause", "reason"},
		},
		{
			// A non-zero reason is used so the omitzero scalar tag emits the key; a
			// zero reason (CancelClientRetracted) is intentionally dropped by omitzero,
			// matching the design's "omitzero for scalars" rule.
			name: "InputCancelled carries reason and message",
			event: InputCancelled{
				Header:    hdr,
				TurnIndex: 1,
				Reason:    CancelTurnInterrupted,
				Message: &content.UserMessage{Message: content.Message{
					Role:   content.RoleUser,
					Blocks: []content.Block{&content.TextBlock{Text: "retract"}},
				}},
			},
			wantKeys: []string{"turn_index", "reason", "message"},
		},
		{
			name: "TurnInterrupted carries turn_index, no machine-default agency in cause",
			event: TurnInterrupted{
				Header:    Header{EventID: seededUUID(0x55)},
				TurnIndex: 2,
			},
			wantKeys:   []string{"event_id", "turn_index"},
			absentKeys: []string{"cause"}, // zero Cause is omitzero
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(tt.event)
			if err != nil {
				t.Fatalf("json.Marshal(%T) error = %v", tt.event, err)
			}
			keys := topLevelKeys(t, data)
			for _, want := range tt.wantKeys {
				if !hasKey(keys, want) {
					t.Errorf("%T journal output missing key %q; got keys %v\nraw: %s", tt.event, want, keysOf(keys), data)
				}
			}
			for _, absent := range tt.absentKeys {
				if hasKey(keys, absent) {
					t.Errorf("%T journal output unexpectedly has key %q\nraw: %s", tt.event, absent, data)
				}
			}
		})
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestTurnFailedErrNotMarshalled proves the un-marshalable TurnFailed.Err is tagged
// json:"-": the typed error cause cannot round-trip through encoding/json, so it
// must never appear in the journal as garbage. The turn_index still serializes.
func TestTurnFailedErrNotMarshalled(t *testing.T) {
	t.Parallel()
	ev := TurnFailed{
		Header:    Header{EventID: seededUUID(0x55)},
		TurnIndex: 3,
		Err:       &content.BlockDecodeError{Cause: errSentinel{}},
	}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("json.Marshal(TurnFailed) error = %v", err)
	}
	keys := topLevelKeys(t, data)
	if hasKey(keys, "err") || hasKey(keys, "Err") {
		t.Errorf("TurnFailed journal output must not carry the error field; got %s", data)
	}
	if !hasKey(keys, "turn_index") {
		t.Errorf("TurnFailed journal output missing turn_index; got %s", data)
	}
}

// TestRestoreErroredErrNotMarshalled proves RestoreErrored.Err is tagged json:"-"
// (mirroring TurnFailed.Err): the typed restore-failure cause cannot round-trip
// through encoding/json, so it must never appear in the journal as garbage. The
// event still marshals (here, only the embedded Header fields).
func TestRestoreErroredErrNotMarshalled(t *testing.T) {
	t.Parallel()
	ev := RestoreErrored{
		Header: Header{EventID: seededUUID(0x66)},
		Err:    errSentinel{},
	}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("json.Marshal(RestoreErrored) error = %v", err)
	}
	keys := topLevelKeys(t, data)
	if hasKey(keys, "err") || hasKey(keys, "Err") {
		t.Errorf("RestoreErrored journal output must not carry the error field; got %s", data)
	}
	if !hasKey(keys, "event_id") {
		t.Errorf("RestoreErrored journal output missing event_id; got %s", data)
	}
}

// errSentinel is a tiny error used only to populate TurnFailed.Err.
type errSentinel struct{}

func (errSentinel) Error() string { return "sentinel" }

// fullHeader is a representative, fully-populated Header used by the round-trip
// table. CreatedAt is a fixed UTC instant with whole-nanosecond precision so it
// survives the RFC3339Nano text codec without monotonic-clock or sub-nanosecond
// drift (which would defeat reflect.DeepEqual).
func fullHeader() Header {
	return Header{
		Coordinates: identity.Coordinates{
			SessionID: seededUUID(0x11),
			LoopID:    seededUUID(0x22),
			TurnID:    seededUUID(0x33),
			StepID:    seededUUID(0x44),
		},
		EventID:   seededUUID(0x55),
		CreatedAt: time.Date(2026, time.June, 21, 12, 34, 56, 123456789, time.UTC),
		Cause: identity.Cause{
			CommandID: seededUUID(0x66),
			Agency:    identity.AgencyUser,
		},
	}
}

// userMsg / aiMsg / sampleMessages build representative content so the message
// and block delegation paths are exercised by the round-trip.
func userMsg(text string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{
		Role:   content.RoleUser,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

func aiMsg(text string) *content.AIMessage {
	return &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

// sampleMessages is a representative committed step group: one AIMessage followed
// by a ToolResultMessage, covering both the role discriminator arms and the
// ToolResultMessage-specific fields (tool_use_id, is_error).
func sampleMessages() content.AgenticMessages {
	return content.AgenticMessages{
		&content.AIMessage{Message: content.Message{
			Role: content.RoleAssistant,
			Blocks: []content.Block{
				&content.TextBlock{Text: "calling a tool"},
				&content.ToolUseBlock{ID: "tu-1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)},
			},
		}},
		&content.ToolResultMessage{
			Message: content.Message{
				Role:   content.RoleTool,
				Blocks: []content.Block{&content.TextBlock{Text: "file.txt"}},
			},
			ToolUseID: "tu-1",
			IsError:   false,
		},
	}
}

// TestMarshalEventRoundTripEnduring is the exhaustive fidelity table: one instance
// of every Enduring event type round-trips through MarshalEvent/UnmarshalEvent
// deep-equal to the original. TurnFailed.Err and RestoreErrored.Err are compared
// separately as *RestoredError (an error value has no general codec — it projects
// to a {kind,message} pair); PermissionRequested.Request is asserted via its
// accessor contract because the interface value reconstructs to a value (not
// pointer) form.
func TestMarshalEventRoundTripEnduring(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ev   Event
	}{
		{"SessionStarted", SessionStarted{
			Header: fullHeaderSession(),
			Config: ConfigFingerprint{AgentKind: "primary", ModelID: "m-1", SystemPromptRev: "abc", ToolPolicyRev: "def", AgentAdapter: "claude", PermissionPosture: "default"},
		}},
		{"SessionActive", SessionActive{Header: fullHeaderSession()}},
		{"SessionIdle", SessionIdle{Header: fullHeaderSession()}},
		{"SessionStopped", SessionStopped{Header: fullHeaderSession()}},
		{"RestoreStarted", RestoreStarted{Header: fullHeaderSession()}},
		{"RestoreDone", RestoreDone{Header: fullHeaderSession()}},
		{"WorkspaceCheckpointed", WorkspaceCheckpointed{
			Header: fullHeaderSession(),
			Ref:    "v1:sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899",
		}},
		// Empty Ref is a legal string at the event layer (codec fidelity, not Ref
		// grammar validity): it must round-trip back to "".
		{"WorkspaceCheckpointed empty ref", WorkspaceCheckpointed{Header: fullHeaderSession()}},
		// RestoreErrored.Err handled in the dedicated err-projection test below.
		{"LoopIdle", LoopIdle{Header: fullHeaderLoop()}},
		{"LoopStarted", LoopStarted{Header: fullHeaderLoop()}},
		{"LoopStarted with AgentName", LoopStarted{Header: loopHeaderWithAgent("operator")}},
		{"LoopStarted with ParentToolUseID", LoopStarted{Header: fullHeaderLoop(), ParentToolUseID: "toolu_abc123"}},
		{"LoopStarted with ForeignSID", LoopStarted{Header: fullHeaderLoop(), ForeignSID: "11111111-1111-1111-1111-111111111111"}},
		{"TurnStarted", TurnStarted{Header: fullHeaderTurn(), TurnIndex: 7, Message: userMsg("hi")}},
		{"StepDone", StepDone{Header: fullHeader(), Messages: sampleMessages()}},
		{"TurnFoldedInto", TurnFoldedInto{Header: fullHeaderTurn(), TurnIndex: 2, Message: userMsg("fold")}},
		{"InputCancelled", InputCancelled{Header: fullHeaderLoop(), TurnIndex: 1, Reason: CancelTurnInterrupted, Message: userMsg("retract")}},
		{"TurnRejected", TurnRejected{Header: fullHeaderLoop(), Reason: RejectQueueFull}},
		{"UserInputRequested", UserInputRequested{Header: fullHeader(), ToolExecutionID: seededUUID(0x77), Question: "pick one", Choices: []string{"a", "b"}}},
		{"TurnDone", TurnDone{Header: fullHeaderTurn(), TurnIndex: 9, Message: aiMsg("done")}},
		{"TurnInterrupted", TurnInterrupted{Header: fullHeaderTurn(), TurnIndex: 4}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := MarshalEvent(tt.ev)
			if err != nil {
				t.Fatalf("MarshalEvent(%s) error = %v", tt.name, err)
			}
			got, err := UnmarshalEvent(data)
			if err != nil {
				t.Fatalf("UnmarshalEvent(%s) error = %v\nwire: %s", tt.name, err, data)
			}
			if !reflect.DeepEqual(got, tt.ev) {
				t.Errorf("round-trip(%s) mismatch:\n got = %#v\nwant = %#v\nwire: %s", tt.name, got, tt.ev, data)
			}
		})
	}
}

// TestMarshalWorkspaceCheckpointedWire proves the WorkspaceCheckpointed envelope
// carries the stable "type" discriminator (== its classify name) and the "ref"
// payload key with the opaque ref value verbatim — the resume token's pointer to
// the workspace store must reach the journal intact.
func TestMarshalWorkspaceCheckpointedWire(t *testing.T) {
	t.Parallel()

	const ref = "v1:sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	data, err := MarshalEvent(WorkspaceCheckpointed{Header: fullHeaderSession(), Ref: ref})
	if err != nil {
		t.Fatalf("MarshalEvent(WorkspaceCheckpointed) error = %v", err)
	}
	keys := topLevelKeys(t, data)

	var gotType string
	if err := json.Unmarshal(keys["type"], &gotType); err != nil {
		t.Fatalf("unmarshal type tag: %v", err)
	}
	if gotType != "WorkspaceCheckpointed" {
		t.Errorf("wire type tag = %q, want %q\nraw: %s", gotType, "WorkspaceCheckpointed", data)
	}

	var gotRef string
	if err := json.Unmarshal(keys["ref"], &gotRef); err != nil {
		t.Fatalf("unmarshal ref key: %v", err)
	}
	if gotRef != ref {
		t.Errorf("wire ref = %q, want %q\nraw: %s", gotRef, ref, data)
	}
}

// TestMarshalEventTurnFailedErrProjection proves TurnFailed.Err and
// RestoreErrored.Err project to a *RestoredError on restore: an event-package
// cause maps to its stable ErrKind; a foreign (provider-origin) cause projects to
// KindUnknown with its Error() text preserved verbatim.
func TestMarshalEventTurnFailedErrProjection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		wantKind string
		wantMsg  string
	}{
		{"in-package empty response", &EmptyResponseError{}, KindEmptyResponse, (&EmptyResponseError{}).Error()},
		{"in-package tool limit", &ToolLimitError{Iterations: 1, MaxIterations: 2, Calls: 3, MaxCalls: 4}, KindToolLimit,
			(&ToolLimitError{Iterations: 1, MaxIterations: 2, Calls: 3, MaxCalls: 4}).Error()},
		{"in-package turn panic", &TurnPanicError{Detail: "boom"}, KindTurnPanic, (&TurnPanicError{Detail: "boom"}).Error()},
		{"provider-origin foreign error", errSentinel{}, KindUnknown, "sentinel"},
		{"nil error projects to unknown", nil, KindUnknown, ""},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// TurnFailed.
			tf := TurnFailed{Header: fullHeaderTurn(), TurnIndex: 5, Err: tt.err}
			assertErrRoundTrip(t, tf, func(ev Event) error { return ev.(TurnFailed).Err }, tt.wantKind, tt.wantMsg)

			// RestoreErrored (session-scoped sibling that carries the same kind of Err).
			re := RestoreErrored{Header: fullHeaderSession(), Err: tt.err}
			assertErrRoundTrip(t, re, func(ev Event) error { return ev.(RestoreErrored).Err }, tt.wantKind, tt.wantMsg)
		})
	}
}

// assertErrRoundTrip marshals ev, unmarshals it, and asserts the restored event's
// error field (extracted by getErr) is a *RestoredError with the wanted kind and
// message. A nil original projects to KindUnknown with an empty message.
func assertErrRoundTrip(t *testing.T, ev Event, getErr func(Event) error, wantKind, wantMsg string) {
	t.Helper()
	data, err := MarshalEvent(ev)
	if err != nil {
		t.Fatalf("MarshalEvent(%T) error = %v", ev, err)
	}
	got, err := UnmarshalEvent(data)
	if err != nil {
		t.Fatalf("UnmarshalEvent(%T) error = %v\nwire: %s", ev, err, data)
	}
	restoredErr := getErr(got)
	var re *RestoredError
	if !errors.As(restoredErr, &re) {
		t.Fatalf("%T restored Err = %T (%v), want *RestoredError\nwire: %s", ev, restoredErr, restoredErr, data)
	}
	if re.Kind != wantKind {
		t.Errorf("%T restored Err.Kind = %q, want %q", ev, re.Kind, wantKind)
	}
	if re.Message != wantMsg {
		t.Errorf("%T restored Err.Message = %q, want %q", ev, re.Message, wantMsg)
	}
}

// TestMarshalEventErrReMarshalFixedPoint proves a decoded error-bearing event is a
// FIXED POINT under re-marshal: marshaling a TurnFailed/RestoreErrored whose Err is
// already a *RestoredError (the decode form) must reproduce the SAME {kind,message}
// pair, not accrete a "<kind>: " prefix onto Message on every cycle. The journal
// compaction / checkpoint re-persist paths (Phase 4/5) re-marshal decoded events, so
// without idempotent projection the Message would grow unboundedly across cycles.
// The double round-trip e -> e1 -> e2 asserts e1.Err == e2.Err as equal
// *RestoredError values (same Kind AND same Message).
func TestMarshalEventErrReMarshalFixedPoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{"in-package tool limit", &ToolLimitError{Iterations: 1, MaxIterations: 2, Calls: 3, MaxCalls: 4}},
		{"provider-origin foreign error", errSentinel{}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// TurnFailed.
			tf := TurnFailed{Header: fullHeaderTurn(), TurnIndex: 5, Err: tt.err}
			assertErrFixedPoint(t, tf, func(ev Event) error { return ev.(TurnFailed).Err })

			// RestoreErrored (session-scoped sibling carrying the same kind of Err).
			re := RestoreErrored{Header: fullHeaderSession(), Err: tt.err}
			assertErrFixedPoint(t, re, func(ev Event) error { return ev.(RestoreErrored).Err })
		})
	}
}

// assertErrFixedPoint round-trips ev twice (e -> e1 -> e2) and asserts the error
// field (extracted by getErr) of e1 and e2 are equal *RestoredError values — same
// Kind AND same Message, with no prefix accretion across the second cycle.
func assertErrFixedPoint(t *testing.T, ev Event, getErr func(Event) error) {
	t.Helper()

	data1, err := MarshalEvent(ev)
	if err != nil {
		t.Fatalf("MarshalEvent(%T) #1 error = %v", ev, err)
	}
	e1, err := UnmarshalEvent(data1)
	if err != nil {
		t.Fatalf("UnmarshalEvent(%T) #1 error = %v\nwire: %s", ev, err, data1)
	}

	data2, err := MarshalEvent(e1)
	if err != nil {
		t.Fatalf("MarshalEvent(%T) #2 error = %v", e1, err)
	}
	e2, err := UnmarshalEvent(data2)
	if err != nil {
		t.Fatalf("UnmarshalEvent(%T) #2 error = %v\nwire: %s", e1, err, data2)
	}

	var re1, re2 *RestoredError
	if !errors.As(getErr(e1), &re1) {
		t.Fatalf("%T e1 Err = %T, want *RestoredError", ev, getErr(e1))
	}
	if !errors.As(getErr(e2), &re2) {
		t.Fatalf("%T e2 Err = %T, want *RestoredError", ev, getErr(e2))
	}
	if !reflect.DeepEqual(re1, re2) {
		t.Errorf("%T re-marshal not a fixed point:\n e1 = %#v\n e2 = %#v\n(Message must not accrete a %q prefix across cycles)", ev, re1, re2, "<kind>: ")
	}
}

// TestMarshalEventPermissionRequestedFullRequest proves PermissionRequested
// round-trips with its FULL Request (header-only would panic on TUI replay): the
// reconstructed event's tool_execution_id matches and the Request's accessor
// contract (ToolName/Description/AllowedScopes) survives.
func TestMarshalEventPermissionRequestedFullRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  tool.PermissionRequest
	}{
		{"bash", tool.BashRequest{Command: "rm -rf /tmp/x"}},
		{"file write", tool.FileWriteRequest{Path: "/etc/passwd"}},
		{"fetch", tool.FetchRequest{Method: "GET", URL: "https://example.com"}},
		{"web search", tool.WebSearchRequest{Query: "how to escape a sandbox"}},
		{"unknown", tool.UnknownRequest{Tool: "Mystery", Summary: "redacted"}},
		{"skill load", tool.SkillLoadRequest{
			RelPath: ".skills/lint/SKILL.md",
			Agent:   identity.AgentName("explorer"),
			Size:    2048,
			SHA256:  "feedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface0",
		}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev := PermissionRequested{Header: fullHeader(), ToolExecutionID: seededUUID(0x77), Request: tt.req}
			data, err := MarshalEvent(ev)
			if err != nil {
				t.Fatalf("MarshalEvent error = %v", err)
			}
			got, err := UnmarshalEvent(data)
			if err != nil {
				t.Fatalf("UnmarshalEvent error = %v\nwire: %s", err, data)
			}
			pr, ok := got.(PermissionRequested)
			if !ok {
				t.Fatalf("UnmarshalEvent returned %T, want PermissionRequested", got)
			}
			if pr.ToolExecutionID != ev.ToolExecutionID {
				t.Errorf("ToolExecutionID = %v, want %v", pr.ToolExecutionID, ev.ToolExecutionID)
			}
			if pr.Request == nil {
				t.Fatalf("restored Request is nil; the full request must survive")
			}
			if pr.Request.ToolName() != tt.req.ToolName() {
				t.Errorf("ToolName() = %q, want %q", pr.Request.ToolName(), tt.req.ToolName())
			}
			if pr.Request.Description() != tt.req.Description() {
				t.Errorf("Description() = %q, want %q", pr.Request.Description(), tt.req.Description())
			}
			if !reflect.DeepEqual(pr.Request.AllowedScopes(), tt.req.AllowedScopes()) {
				t.Errorf("AllowedScopes() = %v, want %v", pr.Request.AllowedScopes(), tt.req.AllowedScopes())
			}
			// The non-Request header fields must also survive intact.
			if !reflect.DeepEqual(pr.EventHeader(), ev.EventHeader()) {
				t.Errorf("header mismatch: got %#v, want %#v", pr.EventHeader(), ev.EventHeader())
			}
		})
	}
}

// wantEnduringTypes is the count of Enduring event types the codec MUST cover. It
// is the drift-guard tripwire: adding a new Enduring type to the sealed union
// without codec coverage changes the live count derived from classify+Class() and
// fails TestMarshalEventCoversEveryEnduringType. A missed Enduring type is an
// unpersistable event = silent restore data loss, which this guard forbids.
const wantEnduringTypes = 20

// unionInstances is one instance of EVERY type in the sealed union (Enduring and
// Ephemeral alike), mirroring TestClassifyExhaustive. The drift guard partitions
// it by Class() and asserts the codec handles each partition correctly, so a new
// union member is forced through the guard.
func unionInstances() []Event {
	return []Event{
		SessionStarted{}, SessionActive{}, SessionIdle{}, SessionStopped{},
		RestoreStarted{}, RestoreDone{}, RestoreErrored{}, WorkspaceCheckpointed{},
		LoopIdle{}, LoopStarted{},
		TokenDelta{}, TurnStarted{}, StepDone{}, TurnFoldedInto{}, InputCancelled{},
		InputQueued{}, TurnRejected{}, TurnDone{}, TurnFailed{}, TurnInterrupted{},
		PermissionRequested{}, UserInputRequested{}, ToolCallStarted{}, ToolCallCompleted{},
	}
}

// TestMarshalEventCoversEveryEnduringType is the drift guard. It derives the
// Enduring set from classify+Class() (the single sealed-union enumeration) and
// asserts: (a) every Enduring type marshals without an unknown-type error (the
// codec has a case for it), and (b) the Enduring count equals wantEnduringTypes.
// Adding a new Enduring type without a codec arm makes MarshalEvent return a
// *UnknownEventTypeError here, failing the build's tests.
func TestMarshalEventCoversEveryEnduringType(t *testing.T) {
	t.Parallel()

	var enduring int
	for _, ev := range unionInstances() {
		name, _, ok := classify(ev)
		if !ok {
			t.Fatalf("classify(%T) ok = false; the drift guard's union list is stale", ev)
		}
		if ev.Class() != Enduring {
			continue
		}
		enduring++
		// A zero-valued instance may fail post-decode validation, but it must NOT
		// fail with an unknown-type error: that would mean the codec has no arm.
		_, err := MarshalEvent(ev)
		var unknown *UnknownEventTypeError
		if errors.As(err, &unknown) {
			t.Errorf("MarshalEvent(%s) returned UnknownEventTypeError; the codec is missing an arm for this Enduring type", name)
		}
	}
	if enduring != wantEnduringTypes {
		t.Errorf("Enduring type count = %d, want %d; update wantEnduringTypes AND add codec coverage for the new Enduring type", enduring, wantEnduringTypes)
	}
}

// TestMarshalEventRejectsEphemeral asserts every Ephemeral type is refused with a
// typed *EphemeralNotPersistableError: the Ephemeral set is never persisted (no
// durable codec for TokenDelta.Chunk, and they self-heal from a later
// authoritative event), so the marshaler fails closed rather than emit a lossy
// record. The Ephemeral set is derived from Class(), not hardcoded.
func TestMarshalEventRejectsEphemeral(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ev   Event
	}{
		{"TokenDelta", TokenDelta{Header: fullHeader()}},
		{"ToolCallStarted", ToolCallStarted{Header: fullHeader(), ToolExecutionID: seededUUID(0x77), ToolName: "Bash", Summary: "ls"}},
		{"ToolCallCompleted", ToolCallCompleted{Header: fullHeader(), ToolExecutionID: seededUUID(0x77), IsError: true, ResultPreview: "boom"}},
		{"InputQueued", InputQueued{Header: fullHeaderLoop()}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.ev.Class() != Ephemeral {
				t.Fatalf("%s is not Ephemeral; test table is stale", tt.name)
			}
			data, err := MarshalEvent(tt.ev)
			if data != nil {
				t.Errorf("MarshalEvent(%s) returned non-nil bytes on an Ephemeral event: %s", tt.name, data)
			}
			var e *EphemeralNotPersistableError
			if !errors.As(err, &e) {
				t.Fatalf("MarshalEvent(%s) error = %T (%v), want *EphemeralNotPersistableError", tt.name, err, err)
			}
			if e.Type != tt.name {
				t.Errorf("EphemeralNotPersistableError.Type = %q, want %q (classify name)", e.Type, tt.name)
			}
		})
	}
}

// TestUnmarshalEventRejectsMalformed proves the untrusted-restore boundary fails
// closed on bad input with typed errors: nil/empty/garbage → typed decode error;
// a valid envelope with an unknown "type" → *UnknownEventTypeError; a missing
// "type" → *UnknownEventTypeError (empty tag has no concrete type).
func TestUnmarshalEventRejectsMalformed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		data        []byte
		wantUnknown bool // expect *UnknownEventTypeError specifically
	}{
		{name: "nil bytes", data: nil},
		{name: "empty bytes", data: []byte{}},
		{name: "garbage", data: []byte("not json at all")},
		{name: "json array not object", data: []byte("[1,2,3]")},
		{name: "json null", data: []byte("null")},
		{name: "missing type tag", data: []byte(`{"v":1,"event_id":"x"}`), wantUnknown: true},
		{name: "unknown type tag", data: []byte(`{"type":"NotARealEvent","v":1}`), wantUnknown: true},
		{name: "known type malformed payload", data: []byte(`{"type":"TurnStarted","turn_index":"not a number"}`)},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := UnmarshalEvent(tt.data)
			if err == nil {
				t.Fatalf("UnmarshalEvent(%q) error = nil, want a typed error; got event %#v", tt.data, got)
			}
			if got != nil {
				t.Errorf("UnmarshalEvent(%q) returned non-nil event on error: %#v", tt.data, got)
			}
			if tt.wantUnknown {
				var unknown *UnknownEventTypeError
				if !errors.As(err, &unknown) {
					t.Errorf("UnmarshalEvent(%q) error = %T (%v), want *UnknownEventTypeError", tt.data, err, err)
				}
			}
		})
	}
}

// FuzzDecodeEvent drives UnmarshalEvent over the untrusted-restore boundary: it
// must never panic and must always return either a valid event or a typed error
// (never an untyped error or a non-nil event with a non-nil error). Seeds include
// valid encodings of several event types plus assorted garbage.
func FuzzDecodeEvent(f *testing.F) {
	seedEvents := []Event{
		SessionStarted{Header: fullHeaderSession(), Config: ConfigFingerprint{ModelID: "m"}},
		LoopStarted{Header: fullHeaderLoop()},
		TurnStarted{Header: fullHeaderTurn(), TurnIndex: 1, Message: userMsg("hi")},
		StepDone{Header: fullHeader(), Messages: sampleMessages()},
		TurnDone{Header: fullHeaderTurn(), Message: aiMsg("done")},
		TurnFailed{Header: fullHeaderTurn(), Err: &ToolLimitError{}},
		PermissionRequested{Header: fullHeader(), ToolExecutionID: seededUUID(0x77), Request: tool.BashRequest{Command: "ls"}},
		PermissionRequested{Header: fullHeader(), ToolExecutionID: seededUUID(0x78), Request: tool.SkillLoadRequest{RelPath: ".skills/x/SKILL.md", Agent: identity.AgentName("explorer"), Size: 10, SHA256: "abc"}},
		TurnInterrupted{Header: fullHeaderTurn()},
	}
	for _, ev := range seedEvents {
		if data, err := MarshalEvent(ev); err == nil {
			f.Add(data)
		}
	}
	f.Add([]byte(""))
	f.Add([]byte("null"))
	f.Add([]byte("{}"))
	f.Add([]byte(`{"type":"TurnStarted"}`))
	f.Add([]byte(`{"type":"NotReal","v":1}`))
	f.Add([]byte(`{"type":"StepDone","messages":[{"role":"tool"}]}`))
	f.Add([]byte("not json"))

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := UnmarshalEvent(data)
		if err != nil {
			if got != nil {
				t.Errorf("UnmarshalEvent returned both an event (%#v) and an error (%v)", got, err)
			}
			// Errors must be typed (one of the codec's concrete error types).
			if !isTypedDecodeError(err) {
				t.Errorf("UnmarshalEvent returned an untyped error %T: %v", err, err)
			}
			return
		}
		if got == nil {
			t.Errorf("UnmarshalEvent returned nil event with nil error for input %q", data)
		}
	})
}

// isTypedDecodeError reports whether err is one of the codec's concrete error
// types (or a delegated content/tool/uuid codec error), proving the parser never
// leaks an untyped error across the boundary.
func isTypedDecodeError(err error) bool {
	var (
		unknownEvent *UnknownEventTypeError
		decode       *EventDecodeError
		limit        *EventLimitError
		invalid      *InvalidEventError
		blockDecode  *content.BlockDecodeError
		blockLimit   *content.BlockLimitError
		unknownBlock *content.UnknownBlockTypeError
		reqDecode    *tool.PermissionRequestDecodeError
		reqLimit     *tool.PermissionRequestLimitError
		reqUnknown   *tool.UnknownPermissionRequestError
	)
	return errors.As(err, &unknownEvent) ||
		errors.As(err, &decode) ||
		errors.As(err, &limit) ||
		errors.As(err, &invalid) ||
		errors.As(err, &blockDecode) ||
		errors.As(err, &blockLimit) ||
		errors.As(err, &unknownBlock) ||
		errors.As(err, &reqDecode) ||
		errors.As(err, &reqLimit) ||
		errors.As(err, &reqUnknown)
}

// fullHeaderSession / fullHeaderLoop / fullHeaderTurn project fullHeader onto each
// scope's valid coordinate shape so a round-tripped event also passes
// ValidateEvent (session: only SessionID; loop: +LoopID; turn: +TurnID).
func fullHeaderSession() Header {
	h := fullHeader()
	h.LoopID, h.TurnID, h.StepID = uuid.UUID{}, uuid.UUID{}, uuid.UUID{}
	return h
}

func fullHeaderLoop() Header {
	h := fullHeader()
	h.TurnID, h.StepID = uuid.UUID{}, uuid.UUID{}
	return h
}

func fullHeaderTurn() Header {
	h := fullHeader()
	h.StepID = uuid.UUID{}
	return h
}

// loopHeaderWithAgent is a loop-scoped header carrying an AgentName, so a round-trip
// proves the attribution name survives MarshalEvent/UnmarshalEvent additively.
func loopHeaderWithAgent(name identity.AgentName) Header {
	h := fullHeaderLoop()
	h.AgentName = name
	return h
}
