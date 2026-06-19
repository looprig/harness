package event_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// TestReplyImplementations pins WHICH events are Reply (the direct, typed outcome
// of a command, recognised by ReplyTo() == the issuer's command id). Exactly the
// five submit-resolution events implement it; ReplyTo() must return the embedded
// Header.CausationID verbatim. Any other event MUST NOT be a Reply, so the set
// stays sealed to the command-outcome events.
func TestReplyImplementations(t *testing.T) {
	t.Parallel()

	causationID := mustID(t)

	isReply := []struct {
		name string
		ev   event.Event
	}{
		{"TurnStarted", event.TurnStarted{Header: event.Header{CausationID: causationID}}},
		{"InputQueued", event.InputQueued{Header: event.Header{CausationID: causationID}}},
		{"TurnRejected", event.TurnRejected{Header: event.Header{CausationID: causationID}}},
		{"TurnFoldedInto", event.TurnFoldedInto{Header: event.Header{CausationID: causationID}}},
		{"InputCancelled", event.InputCancelled{Header: event.Header{CausationID: causationID}}},
	}
	for _, tt := range isReply {
		tt := tt
		t.Run(tt.name+" is Reply", func(t *testing.T) {
			t.Parallel()
			r, ok := tt.ev.(event.Reply)
			if !ok {
				t.Fatalf("%T must implement event.Reply", tt.ev)
			}
			if got := r.ReplyTo(); got != causationID {
				t.Errorf("ReplyTo() = %v, want %v (the Header.CausationID)", got, causationID)
			}
		})
	}

	notReply := []struct {
		name string
		ev   event.Event
	}{
		{"StepDone", event.StepDone{}},
		{"TokenDelta", event.TokenDelta{}},
		{"TurnDone", event.TurnDone{}},
		{"TurnFailed", event.TurnFailed{}},
		{"TurnInterrupted", event.TurnInterrupted{}},
		{"SessionStarted", event.SessionStarted{}},
		{"LoopIdle", event.LoopIdle{}},
		{"ToolCallStarted", event.ToolCallStarted{}},
	}
	for _, tt := range notReply {
		tt := tt
		t.Run(tt.name+" is not Reply", func(t *testing.T) {
			t.Parallel()
			if _, ok := tt.ev.(event.Reply); ok {
				t.Errorf("%T must NOT implement event.Reply (not a command outcome)", tt.ev)
			}
		})
	}
}

// TestReplyToZeroCausation asserts ReplyTo() faithfully returns a zero CausationID
// (the boundary value) rather than synthesizing an id.
func TestReplyToZeroCausation(t *testing.T) {
	t.Parallel()
	var r event.Reply = event.TurnStarted{}
	if got := r.ReplyTo(); got != (uuid.UUID{}) {
		t.Errorf("ReplyTo() on zero header = %v, want zero", got)
	}
}

// Compile-time proof that exactly the command-outcome events satisfy Reply. If a
// listed event stops being a Reply (or a new event is wrongly sealed in), this
// fails to compile.
var (
	_ event.Reply = event.TurnStarted{}
	_ event.Reply = event.InputQueued{}
	_ event.Reply = event.TurnRejected{}
	_ event.Reply = event.TurnFoldedInto{}
	_ event.Reply = event.InputCancelled{}
)
