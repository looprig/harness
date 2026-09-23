package event

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/gate"
)

func resumeGate() gate.Gate {
	return gate.Gate{
		ID:       gate.ID(gateWireHeader().EventID),
		Kind:     gate.KindAskUser,
		Resolver: gate.ResolverLoop,
		Blocks:   gate.BlocksToolCall,
		Effect:   gate.EffectResume,
		Subject: gate.Subject{
			ToolExecutionID: gate.ID(gateWireHeader().EventID),
			TurnID:          gate.ID(gateWireHeader().TurnID),
			StepID:          gate.ID(gateWireHeader().StepID),
		},
		Restorable: true,
		Prompt:     gate.Prompt{Title: "User input requested", Body: "Which color?"},
	}
}

func resumeMessage() *content.AIMessage {
	return &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{
		&content.ThinkingBlock{Thinking: "", Signature: "sig", SignatureFormat: "anthropic", ProviderState: json.RawMessage(`{"redacted":"x"}`), ProviderStateFormat: "anthropic"},
		&content.TextBlock{Text: "let me check"},
		&content.ToolUseBlock{ID: "call-0", Name: "Echo", Input: json.RawMessage(`{"a":1}`)},
		&content.ToolUseBlock{ID: "call-1", Name: "Ask", Input: json.RawMessage(`{"question":"Which color?"}`), ProviderState: json.RawMessage(`"opaque"`), ProviderStateFormat: "vendor"},
	}}}
}

// TestGatePreparedWithoutResumeIsByteCompatible: a gate with no resume snapshot
// encodes exactly as before the field existed, so nothing an older runtime wrote
// or reads changes shape.
func TestGatePreparedWithoutResumeIsByteCompatible(t *testing.T) {
	t.Parallel()
	data, err := MarshalEvent(GatePrepared{Header: gateWireHeader(), Gate: resumeGate()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"resume":`) {
		t.Fatalf("a nil resume is encoded: %s", data)
	}
}

// TestGatePreparedResumeRoundTripsProviderState: the snapshot is the committed
// assistant message, so it must survive the durable round trip whole — including
// reasoning signatures and provider-opaque state, without which the provider would
// refuse the resumed tool loop.
func TestGatePreparedResumeRoundTripsProviderState(t *testing.T) {
	t.Parallel()
	in := GatePrepared{Header: gateWireHeader(), Gate: resumeGate(), Resume: &ToolStepResume{
		StepIndex: 2, Message: resumeMessage(), ToolUseID: "call-1",
	}}
	data, err := MarshalEvent(in)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalEvent(data)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v", err)
	}
	out, ok := decoded.(GatePrepared)
	if !ok {
		t.Fatalf("decoded %T, want GatePrepared", decoded)
	}
	if out.Resume == nil || out.Resume.StepIndex != 2 || out.Resume.ToolUseID != "call-1" {
		t.Fatalf("resume = %+v, want the snapshot back", out.Resume)
	}
	if !reflect.DeepEqual(out.Resume.Message, in.Resume.Message) {
		t.Fatalf("resume message did not round-trip:\n got %#v\nwant %#v", out.Resume.Message, in.Resume.Message)
	}
}

// TestGatePreparedResumeIsIgnoredByAnOlderShape: an older runtime decodes the
// plain GatePrepared shape without the field; the snapshot must be additive (an
// ignored member), never a reason to refuse the record.
func TestGatePreparedResumeIsIgnoredByAnOlderShape(t *testing.T) {
	t.Parallel()
	data, err := MarshalEvent(GatePrepared{Header: gateWireHeader(), Gate: resumeGate(), Resume: &ToolStepResume{
		StepIndex: 0, Message: resumeMessage(), ToolUseID: "call-1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	// The v0.38 GatePrepared: the same struct minus Resume, decoded the way
	// decodePlain decodes it.
	var older struct {
		enduring
		loopScoped
		Header
		Gate gate.Gate `json:"gate,omitzero"`
	}
	if err := json.Unmarshal(data, &older); err != nil {
		t.Fatalf("an older decoder refuses the record: %v", err)
	}
	if older.Gate.ID != resumeGate().ID {
		t.Fatalf("older decode lost the gate: %+v", older.Gate)
	}
}

func TestToolStepResumeValid(t *testing.T) {
	t.Parallel()
	duplicate := resumeMessage()
	duplicate.Blocks = append(duplicate.Blocks, &content.ToolUseBlock{ID: "call-1", Name: "Ask"})
	tests := []struct {
		name   string
		resume *ToolStepResume
		want   bool
	}{
		{name: "names one of its calls", resume: &ToolStepResume{Message: resumeMessage(), ToolUseID: "call-1"}, want: true},
		{name: "nil", resume: nil, want: false},
		{name: "no message", resume: &ToolStepResume{ToolUseID: "call-1"}, want: false},
		{name: "empty call id", resume: &ToolStepResume{Message: resumeMessage()}, want: false},
		{name: "call not in the message", resume: &ToolStepResume{Message: resumeMessage(), ToolUseID: "call-9"}, want: false},
		{name: "ambiguous call id", resume: &ToolStepResume{Message: duplicate, ToolUseID: "call-1"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.resume.Valid(); got != tt.want {
				t.Fatalf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}
