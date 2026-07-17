package sessionruntime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/tool"
)

// formSchema is the representative form request used across this file: one field
// of every answerable kind, with "note" required so the missing-required path is
// reachable.
func formSchema() gate.PromptSchema {
	return gate.PromptSchema{Fields: []gate.Field{
		{Name: "note", Kind: gate.FieldText, Required: true},
		{Name: "env", Kind: gate.FieldSelect, Options: []gate.Option{{Value: "prod"}, {Value: "dev"}}},
		{Name: "sure", Kind: gate.FieldConfirm},
	}}
}

// formGate builds a host-owned form gate envelope. Resolver is ResolverSession:
// that is what makes it host-owned, and the answer route depends on it.
func formGate() gate.Gate {
	return gate.Gate{
		Kind:     gate.KindForm,
		Resolver: gate.ResolverSession,
		Blocks:   gate.BlocksSession,
		Effect:   gate.EffectResume,
		Subject:  gate.Subject{TurnID: gate.ID(mustUUID()), StepID: gate.ID(mustUUID())},
		Prompt: gate.Prompt{
			Title: "Deploy details",
			Controls: []gate.Control{
				{Action: gate.FormActionAccept, Label: "Submit"},
				{Action: gate.FormActionDecline, Label: "Decline"},
				{Action: gate.FormActionCancel, Label: "Cancel"},
			},
		},
	}
}

func openURLGate() gate.Gate {
	return gate.Gate{
		Kind:     gate.KindOpenURL,
		Resolver: gate.ResolverSession,
		Blocks:   gate.BlocksSession,
		Effect:   gate.EffectResume,
		Subject:  gate.Subject{TurnID: gate.ID(mustUUID()), StepID: gate.ID(mustUUID())},
		Prompt: gate.Prompt{
			Title: "Authorize",
			Controls: []gate.Control{
				{Action: gate.FormActionAccept, Label: "I have completed it"},
				{Action: gate.FormActionDecline, Label: "Decline"},
				{Action: gate.FormActionCancel, Label: "Cancel"},
			},
		},
	}
}

// openHostGate prepares and activates a host-owned gate, returning its id.
func openHostGate(t *testing.T, s *Session, g gate.Gate, payload gate.Payload) gate.ID {
	t.Helper()
	id, err := s.PrepareGateOpen(context.Background(), mustUUID(), g, payload)
	if err != nil {
		t.Fatalf("PrepareGateOpen() error = %v", err)
	}
	if err := s.ActivateGate(context.Background(), id, gate.Route{GateID: id}); err != nil {
		t.Fatalf("ActivateGate() error = %v", err)
	}
	return id
}

func rawValues(t *testing.T, in map[string]any) map[string]json.RawMessage {
	t.Helper()
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal(%v): %v", v, err)
		}
		out[k] = data
	}
	return out
}

// awaitAnswer runs AwaitGateAnswer in the background and returns a channel that
// yields its result, so a test can start waiting before it answers.
type awaitResult struct {
	answer gate.Answer
	err    error
}

func awaitAnswer(s *Session, ctx context.Context, id gate.ID) <-chan awaitResult {
	out := make(chan awaitResult, 1)
	go func() {
		answer, err := s.AwaitGateAnswer(ctx, id)
		out <- awaitResult{answer: answer, err: err}
	}()
	return out
}

func waitAnswer(t *testing.T, ch <-chan awaitResult) awaitResult {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("AwaitGateAnswer did not return")
		return awaitResult{}
	}
}

// TestRespondFormGateAcceptDeliversValuesToTheOpener is the headline: a form gate
// can now be ANSWERED, and the values reach the host blocked on it.
func TestRespondFormGateAcceptDeliversValuesToTheOpener(t *testing.T) {
	t.Parallel()
	s, app, _, cmds := gateSession(t)
	id := openHostGate(t, s, formGate(), gate.FormPayload{Title: "Deploy", Schema: formSchema()})

	waiter := awaitAnswer(s, context.Background(), id)

	err := s.RespondGate(context.Background(), gate.GateResponse{
		GateID: id,
		Action: gate.FormActionAccept,
		Values: rawValues(t, map[string]any{"note": "shipping the fix", "env": "prod", "sure": true}),
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	})
	if err != nil {
		t.Fatalf("RespondGate() error = %v", err)
	}

	got := waitAnswer(t, waiter)
	if got.err != nil {
		t.Fatalf("AwaitGateAnswer() error = %v", got.err)
	}
	if got.answer.Action != gate.FormActionAccept {
		t.Errorf("answer action = %q, want %q", got.answer.Action, gate.FormActionAccept)
	}
	// The confirm value is normalized by ParseFormAnswers.
	want := map[string]string{"note": "shipping the fix", "env": "prod", "sure": "true"}
	if !reflect.DeepEqual(got.answer.Values, want) {
		t.Errorf("answer values = %v, want %v", got.answer.Values, want)
	}
	if got.answer.Source.Kind != gate.ResponseFromUser {
		t.Errorf("answer source = %+v, want the responder's source", got.answer.Source)
	}

	// A host-owned gate mints no loop command: there is no loop to command.
	select {
	case cmd := <-cmds:
		t.Fatalf("a host-owned gate dispatched a loop command: %#v", cmd)
	default:
	}

	// The gate is durably resolved, and the audit is the redacted form audit.
	resolved := app.snapshotResolved()
	if len(resolved) != 1 {
		t.Fatalf("appender recorded %d resolved events, want 1", len(resolved))
	}
	audit, ok := resolved[0].Audit.(gate.FormAudit)
	if !ok {
		t.Fatalf("resolved audit = %#v, want gate.FormAudit", resolved[0].Audit)
	}
	if !reflect.DeepEqual(audit.Choices, map[string]string{"env": "prod", "sure": "true"}) {
		t.Errorf("audit choices = %v", audit.Choices)
	}
	if _, leaked := audit.Choices["note"]; leaked {
		t.Error("the free-text answer reached the durable audit")
	}
	// The gate is gone, so it cannot be answered twice.
	if len(s.ListGates(context.Background())) != 0 {
		t.Error("the gate is still listed after being answered")
	}
}

// TestRespondFormGateFreeTextNeverReachesADurableRecord probes every durable
// artifact a form answer touches with a canary, proving the redaction is not
// merely a property of one struct field.
func TestRespondFormGateFreeTextNeverReachesADurableRecord(t *testing.T) {
	t.Parallel()
	s, app, _, _ := gateSession(t)
	id := openHostGate(t, s, formGate(), gate.FormPayload{Schema: formSchema()})

	const canary = "CANARY-typed-by-a-human"
	if err := s.RespondGate(context.Background(), gate.GateResponse{
		GateID: id,
		Action: gate.FormActionAccept,
		Values: rawValues(t, map[string]any{"note": canary, "env": "dev"}),
	}); err != nil {
		t.Fatalf("RespondGate() error = %v", err)
	}

	resolved := app.snapshotResolved()
	if len(resolved) != 1 {
		t.Fatalf("appender recorded %d resolved events, want 1", len(resolved))
	}

	// The audit as it would be journaled.
	auditJSON, err := gate.MarshalResponseAudit(resolved[0].Audit)
	if err != nil {
		t.Fatalf("MarshalResponseAudit() error = %v", err)
	}
	if strings.Contains(string(auditJSON), canary) {
		t.Errorf("the audit record carries the typed text: %s", auditJSON)
	}

	// The resolved event as a whole.
	eventJSON, err := json.Marshal(resolved[0])
	if err != nil {
		t.Fatalf("json.Marshal(GateResolved): %v", err)
	}
	if strings.Contains(string(eventJSON), canary) {
		t.Errorf("the GateResolved event carries the typed text: %s", eventJSON)
	}
}

// TestRespondFormGateInvalidAnswersFailClosed proves the response is validated
// against the PAYLOAD's schema and rejected with a typed error, leaving the gate
// answerable rather than durably resolving it.
func TestRespondFormGateInvalidAnswersFailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		values map[string]any
	}{
		{name: "missing required field", values: map[string]any{"env": "prod"}},
		{name: "unknown field", values: map[string]any{"note": "x", "nope": "y"}},
		{name: "oversized value", values: map[string]any{"note": strings.Repeat("A", 4097)}},
		{name: "option not allowed", values: map[string]any{"note": "x", "env": "staging"}},
		{name: "wrong type for confirm", values: map[string]any{"note": "x", "sure": "yes"}},
		{name: "wrong type for text", values: map[string]any{"note": 42}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, app, _, _ := gateSession(t)
			id := openHostGate(t, s, formGate(), gate.FormPayload{Schema: formSchema()})

			err := s.RespondGate(context.Background(), gate.GateResponse{
				GateID: id,
				Action: gate.FormActionAccept,
				Values: rawValues(t, tt.values),
			})
			if err == nil {
				t.Fatal("RespondGate() accepted an answer the schema forbids")
			}
			var gateErr *GateError
			if !errors.As(err, &gateErr) {
				t.Fatalf("RespondGate() error = %v, want *GateError", err)
			}
			if gateErr.Kind != GateActionInvalid {
				t.Errorf("error kind = %v, want %v", gateErr.Kind, GateActionInvalid)
			}
			// A rejected answer is not a resolution: nothing durable, still open.
			if got := app.snapshotResolved(); len(got) != 0 {
				t.Errorf("a rejected answer appended %d resolved events, want 0", len(got))
			}
			if len(s.ListGates(context.Background())) != 1 {
				t.Error("a rejected answer closed the gate; it must stay answerable")
			}
		})
	}
}

// TestRespondFormGateDeclineAndCancelCarryNoValues proves the two explicit
// non-answers resolve the gate, reach the opener, and record no audit.
func TestRespondFormGateDeclineAndCancelCarryNoValues(t *testing.T) {
	t.Parallel()

	for _, action := range []string{gate.FormActionDecline, gate.FormActionCancel} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			s, app, _, _ := gateSession(t)
			id := openHostGate(t, s, formGate(), gate.FormPayload{Schema: formSchema()})

			waiter := awaitAnswer(s, context.Background(), id)
			if err := s.RespondGate(context.Background(), gate.GateResponse{GateID: id, Action: action}); err != nil {
				t.Fatalf("RespondGate(%s) error = %v", action, err)
			}

			got := waitAnswer(t, waiter)
			if got.err != nil {
				t.Fatalf("AwaitGateAnswer() error = %v", got.err)
			}
			if got.answer.Action != action {
				t.Errorf("answer action = %q, want %q", got.answer.Action, action)
			}
			if got.answer.Values != nil {
				t.Errorf("answer values = %v, want nil for a non-answer", got.answer.Values)
			}

			resolved := app.snapshotResolved()
			if len(resolved) != 1 {
				t.Fatalf("appender recorded %d resolved events, want 1", len(resolved))
			}
			if resolved[0].Reason != gate.CloseAnswered {
				t.Errorf("reason = %q, want %q", resolved[0].Reason, gate.CloseAnswered)
			}
			if resolved[0].Audit != nil {
				t.Errorf("audit = %#v, want nil for a non-answer", resolved[0].Audit)
			}
		})
	}
}

// TestRespondOpenURLGateCompletionReachesTheOpener proves the open-url answer
// route, and that the ACTION URL never reaches any durable record.
func TestRespondOpenURLGateCompletionReachesTheOpener(t *testing.T) {
	t.Parallel()
	s, app, _, _ := gateSession(t)

	const canaryURL = "https://idp.example/authorize?state=CANARYSECRET&code_challenge=CANARYPKCE"
	payload := gate.OpenURLPayload{
		DisplayOrigin:      "https://idp.example",
		URL:                canaryURL,
		RequiresCompletion: true,
	}
	id := openHostGate(t, s, openURLGate(), payload)

	waiter := awaitAnswer(s, context.Background(), id)
	if err := s.RespondGate(context.Background(), gate.GateResponse{
		GateID: id,
		Action: gate.FormActionAccept,
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	}); err != nil {
		t.Fatalf("RespondGate() error = %v", err)
	}

	got := waitAnswer(t, waiter)
	if got.err != nil {
		t.Fatalf("AwaitGateAnswer() error = %v", got.err)
	}
	if got.answer.Action != gate.FormActionAccept {
		t.Errorf("answer action = %q, want completion", got.answer.Action)
	}

	// The canary must appear in NO durable artifact: not the prepared record's
	// payload, not the opened event, not the resolved event, not the audit.
	prepared := app.snapshotPrepared()
	if len(prepared) != 1 {
		t.Fatalf("appender recorded %d prepared records, want 1", len(prepared))
	}
	payloadJSON, err := gate.MarshalPayload(prepared[0].Payload())
	if err != nil {
		t.Fatalf("MarshalPayload() error = %v", err)
	}
	resolved := app.snapshotResolved()
	if len(resolved) != 1 {
		t.Fatalf("appender recorded %d resolved events, want 1", len(resolved))
	}
	openedJSON, err := json.Marshal(app.snapshotOpened())
	if err != nil {
		t.Fatalf("json.Marshal(GateOpened): %v", err)
	}
	resolvedJSON, err := json.Marshal(resolved[0])
	if err != nil {
		t.Fatalf("json.Marshal(GateResolved): %v", err)
	}
	for _, probe := range []struct {
		name string
		data string
	}{
		{"prepared payload", string(payloadJSON)},
		{"opened event", string(openedJSON)},
		{"resolved event", string(resolvedJSON)},
	} {
		if strings.Contains(probe.data, "CANARY") {
			t.Errorf("the action URL reached the %s: %s", probe.name, probe.data)
		}
	}
	// An open-url answer records no audit: there is nothing to redact and nothing
	// to add that Action does not already say.
	if resolved[0].Audit != nil {
		t.Errorf("audit = %#v, want nil", resolved[0].Audit)
	}
}

// TestRespondHostOwnedGateRejectsAnUnofferedAction proves the existing control
// check still governs the new kinds: an action the prompt never offered is
// refused before any schema work.
func TestRespondHostOwnedGateRejectsAnUnofferedAction(t *testing.T) {
	t.Parallel()
	s, _, _, _ := gateSession(t)
	g := formGate()
	g.Prompt.Controls = []gate.Control{{Action: gate.FormActionDecline}}
	id := openHostGate(t, s, g, gate.FormPayload{Schema: formSchema()})

	err := s.RespondGate(context.Background(), gate.GateResponse{
		GateID: id,
		Action: gate.FormActionAccept,
		Values: rawValues(t, map[string]any{"note": "x"}),
	})
	var gateErr *GateError
	if !errors.As(err, &gateErr) || gateErr.Kind != GateActionInvalid {
		t.Fatalf("RespondGate() error = %v, want *GateError{GateActionInvalid}", err)
	}
}

// TestRespondFormGateWithLoopResolverFailsClosed proves the resolver half of
// hostOwnedGate is load-bearing. A form gate claiming ResolverLoop has no
// loop-side resolver to route to, so it is refused rather than answered into a
// void.
func TestRespondFormGateWithLoopResolverFailsClosed(t *testing.T) {
	t.Parallel()
	s, app, _, _ := gateSession(t)
	g := formGate()
	g.Resolver = gate.ResolverLoop
	id := openHostGate(t, s, g, gate.FormPayload{Schema: formSchema()})

	err := s.RespondGate(context.Background(), gate.GateResponse{
		GateID: id,
		Action: gate.FormActionAccept,
		Values: rawValues(t, map[string]any{"note": "x"}),
	})
	var gateErr *GateError
	if !errors.As(err, &gateErr) || gateErr.Kind != GateKindMismatch {
		t.Fatalf("RespondGate() error = %v, want *GateError{GateKindMismatch}", err)
	}
	if got := app.snapshotResolved(); len(got) != 0 {
		t.Errorf("a refused response appended %d resolved events, want 0", len(got))
	}
}

// TestRespondFormGateWithMismatchedPayloadFailsClosed proves the answer is
// validated against a real FormPayload and cannot be smuggled past a gate whose
// payload is something else entirely.
func TestRespondFormGateWithMismatchedPayloadFailsClosed(t *testing.T) {
	t.Parallel()
	s, _, _, _ := gateSession(t)
	id := openHostGate(t, s, formGate(), gate.AskUserPayload{Question: "not a form"})

	err := s.RespondGate(context.Background(), gate.GateResponse{
		GateID: id,
		Action: gate.FormActionAccept,
		Values: rawValues(t, map[string]any{"note": "x"}),
	})
	var gateErr *GateError
	if !errors.As(err, &gateErr) || gateErr.Kind != GateKindMismatch {
		t.Fatalf("RespondGate() error = %v, want *GateError{GateKindMismatch}", err)
	}
}

// TestAwaitGateAnswerIsRaceFreeAgainstAnEarlyResponse proves the slot is
// installed at PREPARE time: an answer that lands before anyone waits is still
// delivered, not dropped.
func TestAwaitGateAnswerIsRaceFreeAgainstAnEarlyResponse(t *testing.T) {
	t.Parallel()
	s, _, _, _ := gateSession(t)
	id := openHostGate(t, s, formGate(), gate.FormPayload{Schema: formSchema()})

	// Answer FIRST, then wait.
	if err := s.RespondGate(context.Background(), gate.GateResponse{
		GateID: id,
		Action: gate.FormActionAccept,
		Values: rawValues(t, map[string]any{"note": "early"}),
	}); err != nil {
		t.Fatalf("RespondGate() error = %v", err)
	}

	answer, err := s.AwaitGateAnswer(context.Background(), id)
	if err != nil {
		t.Fatalf("AwaitGateAnswer() error = %v", err)
	}
	if answer.Values["note"] != "early" {
		t.Errorf("answer values = %v, want the answer that landed before the wait", answer.Values)
	}
}

// TestAwaitGateAnswerDeliversExactlyOnce proves a second await cannot re-read an
// answer that was already taken.
func TestAwaitGateAnswerDeliversExactlyOnce(t *testing.T) {
	t.Parallel()
	s, _, _, _ := gateSession(t)
	id := openHostGate(t, s, formGate(), gate.FormPayload{Schema: formSchema()})

	if err := s.RespondGate(context.Background(), gate.GateResponse{
		GateID: id,
		Action: gate.FormActionAccept,
		Values: rawValues(t, map[string]any{"note": "once"}),
	}); err != nil {
		t.Fatalf("RespondGate() error = %v", err)
	}
	if _, err := s.AwaitGateAnswer(context.Background(), id); err != nil {
		t.Fatalf("first AwaitGateAnswer() error = %v", err)
	}

	_, err := s.AwaitGateAnswer(context.Background(), id)
	var gateErr *GateError
	if !errors.As(err, &gateErr) || gateErr.Kind != GateNotFound {
		t.Fatalf("second AwaitGateAnswer() error = %v, want *GateError{GateNotFound}", err)
	}
}

// TestAwaitGateAnswerOnALoopOwnedGateFailsClosed proves no slot is installed for
// a gate whose answer becomes a loop command, so a host cannot wait on one
// forever.
func TestAwaitGateAnswerOnALoopOwnedGateFailsClosed(t *testing.T) {
	t.Parallel()
	s, _, loopID, _ := gateSession(t)
	id, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(),
		gate.PermissionPayload{Request: tool.BashRequest{Command: "echo ok"}})
	if err != nil {
		t.Fatalf("PrepareGateOpen() error = %v", err)
	}

	_, err = s.AwaitGateAnswer(context.Background(), id)
	var gateErr *GateError
	if !errors.As(err, &gateErr) || gateErr.Kind != GateNotFound {
		t.Fatalf("AwaitGateAnswer() error = %v, want *GateError{GateNotFound}", err)
	}
}

// TestCloseGateClosesTheOpenersSlot proves CloseGate CLOSES the delivery slot
// rather than merely dropping it from the directory.
//
// The distinction is the whole point: an opener already blocked in
// AwaitGateAnswer holds the channel itself, so deleting the map entry would not
// wake it — it would hang until its context expired. This test captures the slot
// BEFORE closing the gate, so it deterministically exercises that path; the
// goroutine-based variant below cannot, because a scheduler is free to run
// CloseGate before the opener ever blocks.
func TestCloseGateClosesTheOpenersSlot(t *testing.T) {
	t.Parallel()
	s, _, _, _ := gateSession(t)
	id := openHostGate(t, s, formGate(), gate.FormPayload{Schema: formSchema()})

	s.gatesMu.Lock()
	slot := s.gateAnswers[id]
	s.gatesMu.Unlock()
	if slot == nil {
		t.Fatal("a host-owned gate has no delivery slot")
	}

	if err := s.CloseGate(context.Background(), id, gate.CloseAbandoned); err != nil {
		t.Fatalf("CloseGate() error = %v", err)
	}

	select {
	case _, ok := <-slot:
		if ok {
			t.Fatal("the slot delivered a value for a gate that was never answered")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CloseGate left the slot open: an opener blocked in AwaitGateAnswer would hang forever")
	}
}

// TestAwaitGateAnswerUnblocksWhenTheGateIsClosed proves an opener is woken with a
// typed error when its gate is withdrawn. Whether it blocks first or finds the
// slot already gone, the answer must be the same: the gate is not coming back.
func TestAwaitGateAnswerUnblocksWhenTheGateIsClosed(t *testing.T) {
	t.Parallel()
	s, _, _, _ := gateSession(t)
	id := openHostGate(t, s, formGate(), gate.FormPayload{Schema: formSchema()})

	waiter := awaitAnswer(s, context.Background(), id)
	if err := s.CloseGate(context.Background(), id, gate.CloseAbandoned); err != nil {
		t.Fatalf("CloseGate() error = %v", err)
	}

	got := waitAnswer(t, waiter)
	var gateErr *GateError
	if !errors.As(got.err, &gateErr) || gateErr.Kind != GateNotFound {
		t.Fatalf("AwaitGateAnswer() error = %v, want *GateError{GateNotFound}", got.err)
	}
	if got.answer.Action != "" {
		t.Errorf("answer = %+v, want the zero answer for a withdrawn gate", got.answer)
	}
}

// TestAwaitGateAnswerHonorsContextCancellation proves an opener that gives up is
// released and does not leak its slot.
func TestAwaitGateAnswerHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	s, _, _, _ := gateSession(t)
	id := openHostGate(t, s, formGate(), gate.FormPayload{Schema: formSchema()})

	ctx, cancel := context.WithCancel(context.Background())
	waiter := awaitAnswer(s, ctx, id)
	cancel()

	got := waitAnswer(t, waiter)
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("AwaitGateAnswer() error = %v, want context.Canceled", got.err)
	}

	s.gatesMu.Lock()
	_, leaked := s.gateAnswers[id]
	s.gatesMu.Unlock()
	if leaked {
		t.Error("an abandoned wait left its delivery slot behind")
	}
}

// TestRespondFormGateWithNoOpenerDoesNotBlock proves a host that never awaits
// costs a discarded value, not a stuck session.
func TestRespondFormGateWithNoOpenerDoesNotBlock(t *testing.T) {
	t.Parallel()
	s, _, _, _ := gateSession(t)
	id := openHostGate(t, s, formGate(), gate.FormPayload{Schema: formSchema()})

	done := make(chan error, 1)
	go func() {
		done <- s.RespondGate(context.Background(), gate.GateResponse{
			GateID: id,
			Action: gate.FormActionAccept,
			Values: rawValues(t, map[string]any{"note": "nobody is listening"}),
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RespondGate() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RespondGate blocked on an absent opener")
	}
}

// TestFormGatePolicyRespondDeliversTheTemplateAnswer proves a form gate is
// answerable through the ordinary ResponsePolicy machinery, which is the
// fail-secure default an unattended integration configures (see gate.FormAction*).
func TestFormGatePolicyRespondDeliversTheTemplateAnswer(t *testing.T) {
	t.Parallel()
	s, _, _, _ := gateSession(t)
	g := formGate()
	g.ResponsePolicy = gate.ResponsePolicy{
		Timeout:   10 * time.Millisecond,
		OnTimeout: gate.PolicyRespond,
		Response:  gate.ResponseTemplate{Action: gate.FormActionDecline},
	}
	id := openHostGate(t, s, g, gate.FormPayload{Schema: formSchema()})

	got := waitAnswer(t, awaitAnswer(s, context.Background(), id))
	if got.err != nil {
		t.Fatalf("AwaitGateAnswer() error = %v", got.err)
	}
	if got.answer.Action != gate.FormActionDecline {
		t.Errorf("answer action = %q, want the policy template's decline", got.answer.Action)
	}
	if got.answer.Source.Kind != gate.ResponseFromPolicy {
		t.Errorf("answer source = %+v, want policy", got.answer.Source)
	}
}

// TestRestoreLeavesHostOwnedGatesUnavailable proves the restore contract still
// holds for the new kinds after the answer route exists.
//
// Neither kind is restorable, and that is not an oversight: the thing waiting on
// a host-owned gate is an opener's blocked AwaitGateAnswer call, which does not
// survive the process that held it. Reinstalling the gate would present a human
// with a prompt whose answer has nowhere to go (and, for open-url, no action
// target at all — it was never journaled). Restore must resolve it unavailable
// so a live integration can mint a fresh request.
func TestRestoreLeavesHostOwnedGatesUnavailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		gate       gate.Gate
		payload    gate.Payload
		restorable bool
	}{
		{name: "form", gate: formGate(), payload: gate.FormPayload{Schema: formSchema()}},
		{
			// A form gate is not rejected by ValidateGate for being restorable, so
			// this is the case that proves the restore hook — not the envelope
			// validator — is what keeps it out of a restored directory.
			name: "form marked restorable", gate: formGate(),
			payload: gate.FormPayload{Schema: formSchema()}, restorable: true,
		},
		{name: "open url", gate: openURLGate(), payload: gate.OpenURLPayload{DisplayOrigin: "https://idp.example"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			g := tt.gate
			g.ID = gate.ID(mustUUID())
			g.Restorable = tt.restorable

			plan := foldRestoredGates([]journal.JournalRecord{
				journal.NewGatePreparedRecord(
					event.GatePrepared{Gate: g},
					gate.OpenPayload{GateID: g.ID, Payload: tt.payload},
				),
				journal.NewEventRecord(event.GateOpened{Gate: g}),
			})

			if len(plan.open) != 0 {
				t.Errorf("restore reinstalled %d host-owned gates, want 0", len(plan.open))
			}
			if len(plan.unavailable) != 1 {
				t.Fatalf("restore marked %d gates unavailable, want 1", len(plan.unavailable))
			}
			if plan.unavailable[0].Gate.ID != g.ID {
				t.Errorf("unavailable gate = %v, want %v", plan.unavailable[0].Gate.ID, g.ID)
			}
		})
	}
}
