package rig_test

// This file is the external-consumer proof for session.GateHost. It lives in
// package rig_test and imports harness exactly the way an integration module
// would: rig.Define, rig.NewSession, and the published contract. Nothing here
// reaches into internal/sessionruntime, so if the surface were not genuinely
// usable from outside the module, this file would not compile.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

type gateHostStubLLM struct{}

func (*gateHostStubLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, nil
}

func (*gateHostStubLLM) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, nil
}

// gateHostSession brings up a live session through the public rig API and
// returns both views of it: the controller a client answers gates on, and the
// GateHost an integration raises them on.
func gateHostSession(t *testing.T) (session.SessionController, session.GateHost) {
	t.Helper()
	backend := memstore.New()
	composite, err := storage.NewComposite(backend.Ledger, backend.Leaser, backend.KV, backend.Blobs)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sessionstore.Open(composite)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := loop.Define(
		loop.WithName("agent"),
		loop.WithInference(&gateHostStubLLM{}, model.Model{
			Provider: "test", APIFormat: model.APIFormatOpenAI, BaseURL: "http://localhost", Name: "model",
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	r, err := rig.Define(rig.WithLoops(definition), rig.WithPrimers("agent"), rig.WithSessionStore(store))
	if err != nil {
		t.Fatal(err)
	}
	controller, err := r.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Shutdown(context.Background()) })

	// The published capability is reached by asserting on the controller rig
	// returns. This assertion IS part of the proof: it is the step an integration
	// host performs, and it fails if the surface was never published.
	host, ok := controller.(session.GateHost)
	if !ok {
		t.Fatal("rig session controller does not implement session.GateHost")
	}
	return controller, host
}

// formGate builds a host-owned form envelope and its authoritative payload.
func formGate(t *testing.T) (gate.Gate, gate.FormPayload) {
	t.Helper()
	turnID, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	stepID, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	schema := gate.PromptSchema{Fields: []gate.Field{
		{Name: "username", Label: "Username", Kind: gate.FieldText, Required: true},
		{Name: "region", Label: "Region", Kind: gate.FieldSelect, Options: []gate.Option{
			{Value: "eu", Label: "Europe"}, {Value: "us", Label: "United States"},
		}},
	}}
	envelope := gate.Gate{
		Kind:     gate.KindForm,
		Resolver: gate.ResolverSession,
		Blocks:   gate.BlocksToolCall,
		Effect:   gate.EffectResume,
		Subject:  gate.Subject{TurnID: turnID, StepID: stepID},
		Prompt: gate.Prompt{
			Title:  "Sign in",
			Schema: schema,
			Controls: []gate.Control{
				{Action: gate.FormActionAccept, Label: "Submit"},
				{Action: gate.FormActionDecline, Label: "Decline"},
			},
		},
	}
	return envelope, gate.FormPayload{Title: "Sign in", Schema: schema}
}

func mustGateError(t *testing.T, err error, want session.GateErrorKind) {
	t.Helper()
	var gateErr *session.GateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("error = %T %v, want *session.GateError", err, err)
	}
	if gateErr.Kind != want {
		t.Fatalf("gate error kind = %q, want %q", gateErr.Kind, want)
	}
}

// TestGateHostRoundTripsAFormAnswer is the headline proof: an external package
// opens a form gate, a client answers it with RespondGate, and the opener
// receives the values — including the free text that is deliberately absent from
// every durable record.
func TestGateHostRoundTripsAFormAnswer(t *testing.T) {
	t.Parallel()
	controller, host := gateHostSession(t)
	envelope, payload := formGate(t)

	gateID, err := host.OpenHostGate(context.Background(), controller.ActiveLoop().ID(), envelope, payload)
	if err != nil {
		t.Fatalf("OpenHostGate: %v", err)
	}

	answers := make(chan gate.Answer, 1)
	errs := make(chan error, 1)
	go func() {
		answer, err := host.AwaitGateAnswer(context.Background(), gateID)
		if err != nil {
			errs <- err
			return
		}
		answers <- answer
	}()

	// The answer slot is installed at open time, so this cannot race the awaiter
	// above: a response landing before AwaitGateAnswer runs is buffered, not lost.
	respond := func() error {
		return controller.RespondGate(context.Background(), gate.GateResponse{
			GateID: gateID,
			Action: gate.FormActionAccept,
			Values: map[string]json.RawMessage{
				"username": json.RawMessage(`"ada lovelace"`),
				"region":   json.RawMessage(`"eu"`),
			},
			Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
		})
	}
	if err := respond(); err != nil {
		t.Fatalf("RespondGate: %v", err)
	}

	select {
	case err := <-errs:
		t.Fatalf("AwaitGateAnswer: %v", err)
	case answer := <-answers:
		if answer.GateID != gateID {
			t.Errorf("answer gate id = %v, want %v", answer.GateID, gateID)
		}
		if answer.Action != gate.FormActionAccept {
			t.Errorf("answer action = %q, want %q", answer.Action, gate.FormActionAccept)
		}
		if got := answer.Values["username"]; got != "ada lovelace" {
			t.Errorf("username = %q, want %q", got, "ada lovelace")
		}
		if got := answer.Values["region"]; got != "eu" {
			t.Errorf("region = %q, want %q", got, "eu")
		}
		if answer.Source.Kind != gate.ResponseFromUser {
			t.Errorf("answer source = %q, want %q", answer.Source.Kind, gate.ResponseFromUser)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the answer")
	}

	// An answer is delivered exactly once: the gate is gone and a second response
	// finds nothing to answer.
	mustGateError(t, respond(), session.GateNotFound)
}

// TestGateHostRefusesLoopOwnedGates is the fail-closed boundary. Each case is a
// gate a host must not be able to raise, and every one of them is refused BEFORE
// the gate exists — a permission gate opened here would be an integration
// minting an approval prompt against a loop that is not its own.
func TestGateHostRefusesLoopOwnedGates(t *testing.T) {
	t.Parallel()
	turnID, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	stepID, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	subject := gate.Subject{TurnID: turnID, StepID: stepID}
	schema := gate.PromptSchema{Fields: []gate.Field{{Name: "answer", Kind: gate.FieldText}}}

	tests := []struct {
		name    string
		gate    gate.Gate
		payload gate.Payload
	}{
		{
			name:    "permission gate is not host-owned",
			gate:    gate.Gate{Kind: gate.KindPermission, Resolver: gate.ResolverLoop, Subject: subject},
			payload: gate.FormPayload{Schema: schema},
		},
		{
			name:    "permission gate cannot borrow the session resolver",
			gate:    gate.Gate{Kind: gate.KindPermission, Resolver: gate.ResolverSession, Subject: subject},
			payload: gate.FormPayload{Schema: schema},
		},
		{
			name:    "ask-user gate is not host-owned",
			gate:    gate.Gate{Kind: gate.KindAskUser, Resolver: gate.ResolverSession, Subject: subject},
			payload: gate.AskUserPayload{Question: "who?"},
		},
		{
			// Refused at OPEN time, not at answer time. The same gate would be
			// rejected by RespondGate, but only after a human had already typed an
			// answer that could then go nowhere.
			name:    "form gate with a loop resolver",
			gate:    gate.Gate{Kind: gate.KindForm, Resolver: gate.ResolverLoop, Subject: subject},
			payload: gate.FormPayload{Schema: schema},
		},
		{
			name:    "open-url gate with a loop resolver",
			gate:    gate.Gate{Kind: gate.KindOpenURL, Resolver: gate.ResolverLoop, Subject: subject},
			payload: gate.OpenURLPayload{DisplayOrigin: "https://example.com", URL: "https://example.com/auth?state=s"},
		},
		{
			name:    "unknown kind fails closed",
			gate:    gate.Gate{Kind: gate.Kind("harness.invented"), Resolver: gate.ResolverSession, Subject: subject},
			payload: gate.FormPayload{Schema: schema},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			controller, host := gateHostSession(t)
			_, err := host.OpenHostGate(context.Background(), controller.ActiveLoop().ID(), tt.gate, tt.payload)
			mustGateError(t, err, session.GateKindMismatch)
			if gates := listGates(t, controller); len(gates) != 0 {
				t.Fatalf("refused open left %d gate(s) in the directory", len(gates))
			}
		})
	}
}

// TestGateHostRefusesIncoherentPayloads covers the payload half of the open-time
// contract: a gate whose payload could never produce an answer is refused before
// a human sees it.
func TestGateHostRefusesIncoherentPayloads(t *testing.T) {
	t.Parallel()
	turnID, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	stepID, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	subject := gate.Subject{TurnID: turnID, StepID: stepID}

	tests := []struct {
		name    string
		gate    gate.Gate
		payload gate.Payload
	}{
		{
			name:    "form gate with an open-url payload",
			gate:    gate.Gate{Kind: gate.KindForm, Resolver: gate.ResolverSession, Subject: subject},
			payload: gate.OpenURLPayload{DisplayOrigin: "https://example.com", URL: "https://example.com/x"},
		},
		{
			name:    "form gate with an empty schema",
			gate:    gate.Gate{Kind: gate.KindForm, Resolver: gate.ResolverSession, Subject: subject},
			payload: gate.FormPayload{},
		},
		{
			// A form answer is one string per field; a multi-select cannot be
			// represented, so ParseFormAnswers would refuse the answer. Refuse the
			// prompt instead.
			name: "form gate asking for a multi-select",
			gate: gate.Gate{Kind: gate.KindForm, Resolver: gate.ResolverSession, Subject: subject},
			payload: gate.FormPayload{Schema: gate.PromptSchema{Fields: []gate.Field{
				{Name: "tags", Kind: gate.FieldMultiSelect, Options: []gate.Option{{Value: "a"}}},
			}}},
		},
		{
			name:    "open-url gate with a form payload",
			gate:    gate.Gate{Kind: gate.KindOpenURL, Resolver: gate.ResolverSession, Subject: subject},
			payload: gate.FormPayload{Schema: gate.PromptSchema{Fields: []gate.Field{{Name: "a", Kind: gate.FieldText}}}},
		},
		{
			// The whole point of DisplayOrigin is that it is coarse enough to be
			// journaled and shown. A full action URL passed as the origin would
			// defeat that, so it is not an origin.
			name:    "open-url gate whose origin is really an action URL",
			gate:    gate.Gate{Kind: gate.KindOpenURL, Resolver: gate.ResolverSession, Subject: subject},
			payload: gate.OpenURLPayload{DisplayOrigin: "https://idp.example/authorize?state=SECRET", URL: "https://idp.example/authorize?state=SECRET"},
		},
		{
			// An origin with no target renders an "open" action that opens nothing.
			name:    "open-url gate with no action target",
			gate:    gate.Gate{Kind: gate.KindOpenURL, Resolver: gate.ResolverSession, Subject: subject},
			payload: gate.OpenURLPayload{DisplayOrigin: "https://example.com"},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			controller, host := gateHostSession(t)
			_, err := host.OpenHostGate(context.Background(), controller.ActiveLoop().ID(), tt.gate, tt.payload)
			mustGateError(t, err, session.GateKindMismatch)
			if gates := listGates(t, controller); len(gates) != 0 {
				t.Fatalf("refused open left %d gate(s) in the directory", len(gates))
			}
		})
	}
}

// TestGateHostAwaitCancellationFreesTheSlot proves the cancel path an OpenGate
// implementation depends on: a caller that gives up abandons its wait, does not
// leak the slot, and — because the gate is durable state and the context is only
// the caller's — leaves the gate itself open until CloseGate withdraws it.
func TestGateHostAwaitCancellationFreesTheSlot(t *testing.T) {
	t.Parallel()
	controller, host := gateHostSession(t)
	envelope, payload := formGate(t)

	gateID, err := host.OpenHostGate(context.Background(), controller.ActiveLoop().ID(), envelope, payload)
	if err != nil {
		t.Fatalf("OpenHostGate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := host.AwaitGateAnswer(ctx, gateID); !errors.Is(err, context.Canceled) {
		t.Fatalf("AwaitGateAnswer error = %v, want context.Canceled", err)
	}

	// The gate survived the abandoned wait.
	if gates := listGates(t, controller); len(gates) != 1 {
		t.Fatalf("open gates = %d, want 1 (cancelling a wait must not close the gate)", len(gates))
	}
	// The slot is gone, so a second await has nothing to wait on rather than
	// blocking forever on a channel nobody will write to.
	mustGateError(t, awaitErr(t, host, gateID), session.GateNotFound)

	// CloseGate is the opener's obligation after giving up, and it withdraws the
	// gate for real.
	if err := host.CloseGate(context.Background(), gateID, gate.CloseAbandoned); err != nil {
		t.Fatalf("CloseGate: %v", err)
	}
	if gates := listGates(t, controller); len(gates) != 0 {
		t.Fatalf("open gates after close = %d, want 0", len(gates))
	}
}

// TestGateHostCloseWakesTheAwaiter proves a blocked opener is woken rather than
// stranded when the gate is withdrawn — the timeout path of an OpenGate
// implementation.
func TestGateHostCloseWakesTheAwaiter(t *testing.T) {
	t.Parallel()
	controller, host := gateHostSession(t)
	envelope, payload := formGate(t)

	gateID, err := host.OpenHostGate(context.Background(), controller.ActiveLoop().ID(), envelope, payload)
	if err != nil {
		t.Fatalf("OpenHostGate: %v", err)
	}

	waiting := make(chan error, 1)
	go func() {
		_, err := host.AwaitGateAnswer(context.Background(), gateID)
		waiting <- err
	}()

	if err := host.CloseGate(context.Background(), gateID, gate.CloseAbandoned); err != nil {
		t.Fatalf("CloseGate: %v", err)
	}

	select {
	case err := <-waiting:
		// GateNotFound, not a zero Answer an opener could mistake for a real one.
		mustGateError(t, err, session.GateNotFound)
	case <-time.After(5 * time.Second):
		t.Fatal("CloseGate did not wake the awaiter")
	}
}

func awaitErr(t *testing.T, host session.GateHost, id gate.ID) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := host.AwaitGateAnswer(ctx, id)
	return err
}

// listGates reads the live gate directory. It is the one place this file needs a
// capability the public contracts do not carry, and it is a test-only assertion
// helper rather than something an integration host needs.
func listGates(t *testing.T, controller session.SessionController) []gate.Gate {
	t.Helper()
	lister, ok := controller.(interface {
		ListGates(context.Context) []gate.Gate
	})
	if !ok {
		t.Fatal("session controller cannot list gates")
	}
	return lister.ListGates(context.Background())
}
