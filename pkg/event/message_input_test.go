package event

import (
	"bytes"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
)

func TestPreFeatureEventsAreByteIdentical(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"turn_started.json", "turn_folded_into.json", "input_cancelled.json", "turn_interrupted.json", "gate_resolved.json"} {
		raw, err := os.ReadFile("../../internal/compat/testdata/pre_v0410/" + name)
		if err != nil {
			t.Fatal(err)
		}
		raw = bytes.TrimSpace(raw)
		ev, err := UnmarshalEvent(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out, err := MarshalEvent(ev)
		if err != nil || !bytes.Equal(out, raw) {
			t.Fatalf("%s drifted from v0.40.2:\n%s\n%s (%v)", name, raw, out, err)
		}
	}
}

func attributedTurnStarted() TurnStarted {
	return TurnStarted{
		Header: fullHeaderTurn(), TurnIndex: 1,
		Message: &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{
			&content.TextBlock{Text: "[from: A]"}, &content.TextBlock{Text: "Add milk"},
		}}},
		Input: &MessageInput{
			Principal: &sessionwire.Principal{Tenant: "acme", Subject: "u", Kind: sessionwire.PrincipalKindActor},
			Metadata:  sessionwire.MessageMetadata{"space": "family"}, Prefix: 1,
		},
	}
}

func TestMessageInputRoundTripAndValidation(t *testing.T) {
	t.Parallel()
	ev := attributedTurnStarted()
	body, err := MarshalEvent(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(body), `"v":1`) {
		t.Fatalf("event version moved: %s", body)
	}
	decoded, err := UnmarshalEvent(body)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	got := decoded.(TurnStarted)
	if got.Input == nil || got.Input.Prefix != 1 || got.Input.Principal.Subject != "u" {
		t.Fatalf("Input = %#v", got.Input)
	}
	if user := UserBlocks(got.Message, got.Input); len(user) != 1 || user[0].(*content.TextBlock).Text != "Add milk" {
		t.Fatalf("UserBlocks = %#v", user)
	}
	bad := []struct {
		name  string
		input *MessageInput
	}{
		{name: "empty input", input: &MessageInput{}},
		{name: "frame longer than message", input: &MessageInput{Prefix: 2, Suffix: 1}},
		{name: "overflowed frame counts", input: &MessageInput{Prefix: math.MaxInt, Suffix: math.MaxInt}},
		{name: "invalid principal", input: &MessageInput{Principal: &sessionwire.Principal{}}},
		{name: "invalid metadata", input: &MessageInput{Metadata: sessionwire.MessageMetadata{"Bad Key": "v"}}},
	}
	for _, tc := range bad {
		invalid := ev
		invalid.Input = tc.input
		if _, err := MarshalEvent(invalid); err == nil {
			t.Errorf("%s: MarshalEvent accepted it", tc.name)
		}
	}
	machine := ev
	machine.Cause.Agency = identity.AgencyMachine
	if _, err := MarshalEvent(machine); err == nil {
		t.Error("machine-agency message carried input attribution")
	}
}

func TestMessageInputDecodeIsStrict(t *testing.T) {
	t.Parallel()
	body, err := MarshalEvent(attributedTurnStarted())
	if err != nil {
		t.Fatal(err)
	}
	forged := bytes.Replace(body, []byte(`"input":{`), []byte(`"input":{"future":1,`), 1)
	if bytes.Equal(forged, body) {
		t.Fatalf("test failed to inject input member: %s", body)
	}
	if _, err := UnmarshalEvent(forged); err == nil {
		t.Fatal("unknown member inside input must fail closed")
	}
}

func TestGateResolvedAndTurnInterruptedCarryPrincipal(t *testing.T) {
	t.Parallel()
	principal := &sessionwire.Principal{Tenant: "acme", Subject: "u", Kind: sessionwire.PrincipalKindActor}
	interrupt := TurnInterrupted{Header: fullHeaderTurn(), TurnIndex: 1, Principal: principal}
	gateAnswer := GateResolved{
		Header: fullHeaderTurn(), GateID: gate.ID(seededUUID(0x71)), Resolver: gate.ResolverSession,
		Reason: gate.CloseAnswered, Action: "approve", Principal: principal,
	}
	for _, ev := range []Event{interrupt, gateAnswer} {
		body, err := MarshalEvent(ev)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := UnmarshalEvent(body)
		if err != nil {
			t.Fatal(err)
		}
		switch value := decoded.(type) {
		case TurnInterrupted:
			if value.Principal == nil || value.Principal.Subject != "u" {
				t.Fatalf("interrupt principal lost: %s", body)
			}
		case GateResolved:
			if value.Principal == nil || value.Principal.Subject != "u" {
				t.Fatalf("gate principal lost: %s", body)
			}
		}
	}
}
