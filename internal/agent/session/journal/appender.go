package journal

import (
	"context"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
)

// NilJournalError reports that a JournalEventAppender was constructed over a nil
// SessionJournal — a composition-wiring bug. The checked constructor fails loud with
// this typed error rather than letting the nil surface as a panic at the first append.
type NilJournalError struct{}

func (*NilJournalError) Error() string {
	return "journal: JournalEventAppender requires a non-nil SessionJournal"
}

// JournalEventAppender adapts a SessionJournal (the write side) to the narrow
// "append one Enduring event" seam the session hub depends on. The hub holds an
// unexported eventAppender interface (AppendEvent(ctx, event.Event) error); this type
// satisfies it structurally, so the composition root (Phase 10) wires it in via
// hub.WithAppender without the hub ever importing the journal package (Dependency
// Inversion). It carries no state beyond the journal — one method, one responsibility:
// wrap the event in an EventRecord (which self-derives its subject from the event's
// scope+coordinates and its idempotency id from the EventID) and append it.
type JournalEventAppender struct {
	journal SessionJournal
}

// NewJournalEventAppender wraps journal as an event appender. It does NOT guard
// against a nil journal — use NewJournalEventAppenderChecked at the composition root
// where a wiring bug must fail loud. This unchecked form exists for call sites that
// have already validated the journal (and for the structural-satisfaction assertion).
func NewJournalEventAppender(journal SessionJournal) *JournalEventAppender {
	return &JournalEventAppender{journal: journal}
}

// NewJournalEventAppenderChecked is the fail-loud constructor for the composition
// root: it returns a typed *NilJournalError if journal is nil rather than deferring
// the failure to a nil-deref at the first append.
func NewJournalEventAppenderChecked(journal SessionJournal) (*JournalEventAppender, error) {
	if journal == nil {
		return nil, &NilJournalError{}
	}
	return &JournalEventAppender{journal: journal}, nil
}

// AppendEvent durably appends one Enduring event: it wraps ev in an EventRecord and
// calls the journal's Append, returning the underlying typed error unchanged (the hub
// maps it onto a SessionPersistenceFault — never swallowed). The EventRecord routes a
// session-scoped event to the session subject and a loop-scoped event to its loop
// event subject, and uses the event's EventID as the Nats-Msg-Id (idempotency). An
// Ephemeral event is never appended by the hub; if one were passed, the serializer's
// event.MarshalEvent fails closed inside Append, so this path stays fail-secure. The
// returned sequence is discarded — the hub needs only the success/failure signal.
func (a *JournalEventAppender) AppendEvent(ctx context.Context, ev event.Event) error {
	_, err := a.journal.Append(ctx, NewEventRecord(ev))
	return err
}
