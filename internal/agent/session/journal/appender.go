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

// JournalCommandAppender adapts a SessionJournal to the narrow "append one command"
// seam the session depends on for the intent log. The session holds an unexported
// commandAppender interface (AppendCommand(ctx, CommandRecord) error); this type
// satisfies it structurally, so the composition root (Phase 10) wires it in without
// the session importing journal internals beyond the CommandRecord constructor
// (Dependency Inversion). It carries no state beyond the journal — one method, one
// responsibility: append a CommandRecord (which the SESSION built with the dispatch
// target loopID, since a command — Interrupt/Shutdown especially — does not carry its
// own routing) and return the underlying typed error.
//
// Unlike the event appender, the SESSION treats this seam as AUDIT-ONLY: a non-nil
// error is logged and the dispatch proceeds (losing a command record must never block
// the user's action). This façade itself never swallows — it returns the journal's
// error unchanged so the session owns the log-and-proceed decision.
type JournalCommandAppender struct {
	journal SessionJournal
}

// NewJournalCommandAppender wraps journal as a command appender. Like the event
// appender's unchecked form, it does NOT guard against a nil journal — use
// NewJournalCommandAppenderChecked at the composition root where a wiring bug must
// fail loud.
func NewJournalCommandAppender(journal SessionJournal) *JournalCommandAppender {
	return &JournalCommandAppender{journal: journal}
}

// NewJournalCommandAppenderChecked is the fail-loud constructor for the composition
// root: it returns a typed *NilJournalError if journal is nil rather than deferring the
// failure to a nil-deref at the first append.
func NewJournalCommandAppenderChecked(journal SessionJournal) (*JournalCommandAppender, error) {
	if journal == nil {
		return nil, &NilJournalError{}
	}
	return &JournalCommandAppender{journal: journal}, nil
}

// AppendCommand appends one intent-log command record: it calls the journal's Append
// with the session-built CommandRecord and returns the underlying typed error
// unchanged (the session logs+proceeds — audit-only — never faulting the session on a
// command-append failure). The CommandRecord routes to the target loop's command
// (intent-log) subject and uses the command's CommandID as the Nats-Msg-Id
// (idempotency). The returned sequence is discarded — the session needs only the
// success/failure signal.
func (a *JournalCommandAppender) AppendCommand(ctx context.Context, rec CommandRecord) error {
	_, err := a.journal.Append(ctx, rec)
	return err
}
