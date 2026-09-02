package event_test

import (
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

// TestReplyImplementations pins WHICH events are Reply (the direct, typed outcome
// of a command, recognised by ReplyTo() == the issuer's command id). Exactly the
// seven command-resolution events implement it; ReplyTo() must return the embedded
// Header.Cause.CommandID verbatim. Any other event MUST NOT be a Reply, so the set
// stays sealed to the command-outcome events.
func TestReplyImplementations(t *testing.T) {
	t.Parallel()

	causationID := mustID(t)

	isReply := []struct {
		name string
		ev   event.Event
	}{
		{"TurnStarted", event.TurnStarted{Header: event.Header{Cause: identity.Cause{CommandID: causationID}}}},
		{"InputQueued", event.InputQueued{Header: event.Header{Cause: identity.Cause{CommandID: causationID}}}},
		{"TurnRejected", event.TurnRejected{Header: event.Header{Cause: identity.Cause{CommandID: causationID}}}},
		{"TurnFoldedInto", event.TurnFoldedInto{Header: event.Header{Cause: identity.Cause{CommandID: causationID}}}},
		{"InputCancelled", event.InputCancelled{Header: event.Header{Cause: identity.Cause{CommandID: causationID}}}},
		{"CompactWaiterResolved", event.CompactWaiterResolved{Header: event.Header{Cause: identity.Cause{CommandID: causationID}}}},
		{"CompactWaiterRejected", event.CompactWaiterRejected{Header: event.Header{Cause: identity.Cause{CommandID: causationID}}}},
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
				t.Errorf("ReplyTo() = %v, want %v (the Header.Cause.CommandID)", got, causationID)
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
		{"CompactionStarted", event.CompactionStarted{}},
		{"CompactionCommitted", event.CompactionCommitted{}},
		{"CompactionRejected", event.CompactionRejected{}},
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

// TestDeliveryZeroValue asserts a Delivery carries its JournalSeq verbatim: the
// zero value is seq 0, an ephemeral-style delivery (never persisted) is 0, and an
// enduring-style delivery carries the append sequence unchanged. The Event rides
// alongside untouched.
func TestDeliveryZeroValue(t *testing.T) {
	t.Parallel()
	ev := event.StepDone{}
	tests := []struct {
		name    string
		d       event.Delivery
		wantSeq uint64
	}{
		{name: "zero value has seq 0", d: event.Delivery{}, wantSeq: 0},
		{name: "ephemeral-style delivery has seq 0", d: event.Delivery{Event: ev, JournalSeq: 0}, wantSeq: 0},
		{name: "enduring-style delivery carries append seq", d: event.Delivery{Event: ev, JournalSeq: 42}, wantSeq: 42},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.d.JournalSeq != tt.wantSeq {
				t.Errorf("JournalSeq = %d, want %d", tt.d.JournalSeq, tt.wantSeq)
			}
		})
	}
}

// TestReplyToZeroCausation asserts ReplyTo() faithfully returns a zero Cause.CommandID
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
	_ event.Reply = event.CompactWaiterResolved{}
	_ event.Reply = event.CompactWaiterRejected{}
)

// TestDeliveryWidensAdditivelyForExistingConsumers drives the LEGAL case as hard as
// the illegal ones: a consumer written before the committed-publication fields
// existed — keyed literal with only Event and JournalSeq, reading only those two —
// must still compile and still mean exactly what it meant. The new fields read as
// their zero value, and Committed() reports false rather than "unknown".
func TestDeliveryWidensAdditivelyForExistingConsumers(t *testing.T) {
	t.Parallel()
	legacy := func(d event.Delivery) (event.Event, uint64) { return d.Event, d.JournalSeq }

	ev := event.SessionStarted{Header: event.Header{EventID: mustID(t)}}
	d := event.Delivery{Event: ev, JournalSeq: 7}
	gotEvent, gotSeq := legacy(d)
	if _, isStarted := gotEvent.(event.SessionStarted); !isStarted || gotSeq != 7 {
		t.Fatalf("legacy consumer read (%T, %d), want (event.SessionStarted, 7)", gotEvent, gotSeq)
	}
	if d.EventID != "" || d.PublicBody != nil || d.CoveredThrough != 0 {
		t.Errorf("widened Delivery = %+v, want zero committed fields for a legacy literal", d)
	}
	if d.Committed() {
		t.Error("Committed() = true for a delivery with no committed bytes, want false")
	}
}

// TestDeliveryCommittedRequiresBothIdentityAndBytes proves Committed() is not a
// partial-credit predicate: an id without bytes, or bytes without an id, is not a
// committed publication and must not be read as one.
func TestDeliveryCommittedRequiresBothIdentityAndBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		delivery event.Delivery
		want     bool
	}{
		{name: "id and bytes", delivery: event.Delivery{EventID: "e1", PublicBody: []byte(`{}`)}, want: true},
		{name: "id only", delivery: event.Delivery{EventID: "e1"}},
		{name: "bytes only", delivery: event.Delivery{PublicBody: []byte(`{}`)}},
		{name: "empty bytes", delivery: event.Delivery{EventID: "e1", PublicBody: []byte{}}},
		{name: "neither"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.delivery.Committed(); got != tt.want {
				t.Errorf("Committed() = %t, want %t", got, tt.want)
			}
		})
	}
}

// TestAppendCommitPublishesPublicBodyRequiresItsOwnCommit proves a commit that did
// not append — a deduplicated retry — never advertises bytes to publish, even if a
// backend were to hand some back. Only the append that actually committed may
// publish, because only it earned the coverage its delivery would carry.
func TestAppendCommitPublishesPublicBodyRequiresItsOwnCommit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		commit event.AppendCommit
		want   bool
	}{
		{
			name:   "committed public",
			commit: event.AppendCommit{Sequence: 3, Appended: true, EventID: "e1", PublicBody: []byte(`{}`), CoveredThrough: 3},
			want:   true,
		},
		{
			name:   "deduplicated retry",
			commit: event.AppendCommit{Sequence: 3, EventID: "e1", PublicBody: []byte(`{}`)},
		},
		{name: "committed private", commit: event.AppendCommit{Sequence: 3, Appended: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.commit.PublishesPublicBody(); got != tt.want {
				t.Errorf("PublishesPublicBody() = %t, want %t", got, tt.want)
			}
		})
	}
}
