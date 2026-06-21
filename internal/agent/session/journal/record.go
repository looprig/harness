package journal

import (
	"strconv"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// JournalRecord is the sealed sum the serializer writes to a session's stream:
// an Enduring event, a command (the intent log), or an internal LeaseFence. It is
// a marker plus the two facts the writer needs to publish without re-inspecting
// the payload — the JetStream subject the record lands on and its idempotency id
// (the Nats-Msg-Id that de-duplicates a redelivered publish). The concrete
// payload codec is the existing event/command marshaler; a record only carries the
// typed payload and exposes how to route it.
//
// The set is sealed by the unexported isJournalRecord marker: only the wrapper
// types in this package implement it, so the serializer's switch over the sum is
// exhaustive and a foreign type can never masquerade as a record.
type JournalRecord interface {
	isJournalRecord()
	// Subject is the fully-resolved JetStream subject this record lands on. Each
	// wrapper captures its routing ids at construction (an event from its own
	// header; a command/fence from the writer), so Subject needs no arguments.
	Subject() string
	// IdempotencyID is the stable per-record id used as the Nats-Msg-Id so a
	// redelivered publish de-duplicates: an event's EventID, a command's CommandID,
	// or a fence's epoch.
	IdempotencyID() string
}

// EventRecord wraps an Enduring event.Event as a JournalRecord. The event already
// carries its producer Coordinates and Scope, so the record self-derives its
// subject — session events land on the session subject, loop events on the loop
// event subject — and its id is the event's EventID. The serializer encodes the
// wrapped event via event.MarshalEvent (which fails closed on an Ephemeral event);
// the record never re-encodes.
type EventRecord struct {
	ev event.Event
}

// NewEventRecord wraps ev for the journal. ev must be an Enduring event; the
// Ephemeral check is the serializer's (event.MarshalEvent), not this wrapper's.
func NewEventRecord(ev event.Event) EventRecord { return EventRecord{ev: ev} }

// Event returns the wrapped event for the serializer to marshal.
func (r EventRecord) Event() event.Event { return r.ev }

func (EventRecord) isJournalRecord() {}

// Subject derives the event's subject from its scope and coordinates: a
// session-scoped event lands on the session subject; a loop-scoped event lands on
// its loop event subject.
func (r EventRecord) Subject() string {
	h := r.ev.EventHeader()
	if r.ev.Scope() == event.ScopeSession {
		return SessionEventSubject(h.SessionID)
	}
	return LoopEventSubject(h.SessionID, h.LoopID)
}

// IdempotencyID is the event's EventID rendered canonically.
func (r EventRecord) IdempotencyID() string { return r.ev.EventHeader().EventID.String() }

// CommandRecord wraps a command.Command targeting a specific loop. Unlike an
// event, a command does not uniformly carry its routing coordinates (Interrupt and
// Shutdown carry only a Header; the session dispatches them by other means), so the
// writer supplies the target sessionID/loopID at construction. The command lands on
// that loop's command (intent-log) subject; its id is the command's CommandID. The
// serializer encodes the wrapped command via command.MarshalCommand.
type CommandRecord struct {
	sessionID uuid.UUID
	loopID    uuid.UUID
	cmd       command.Command
}

// NewCommandRecord wraps cmd as the intent-log record targeting loop loopID in
// session sessionID. The caller is the writer, which knows the dispatch target;
// the command itself may not carry it.
func NewCommandRecord(sessionID, loopID uuid.UUID, cmd command.Command) CommandRecord {
	return CommandRecord{sessionID: sessionID, loopID: loopID, cmd: cmd}
}

// Command returns the wrapped command for the serializer to marshal.
func (r CommandRecord) Command() command.Command { return r.cmd }

func (CommandRecord) isJournalRecord() {}

// Subject is the target loop's command (intent-log) subject.
func (r CommandRecord) Subject() string { return LoopCommandSubject(r.sessionID, r.loopID) }

// IdempotencyID is the command's CommandID rendered canonically.
func (r CommandRecord) IdempotencyID() string { return r.cmd.CommandHeader().CommandID.String() }

// LeaseFence is an internal journal record marking a lease-handover boundary: a
// monotonically increasing Epoch fenced into the stream when ownership of a
// session's writer lease changes. It is journal-private (not an event or a
// command) and lands on the session's fence subject; the EventReplayer never
// decodes it. Codec: MarshalLeaseFence/UnmarshalLeaseFence in record_json.go.
type LeaseFence struct {
	Epoch uint64 `json:"epoch"`
}

// FenceRecord wraps a LeaseFence as a JournalRecord. The fence carries no session
// id of its own, so the writer supplies the target sessionID at construction; the
// record lands on that session's fence subject and its idempotency id is the epoch.
type FenceRecord struct {
	sessionID uuid.UUID
	fence     LeaseFence
}

// NewFenceRecord wraps fence as the fence record for session sessionID.
func NewFenceRecord(sessionID uuid.UUID, fence LeaseFence) FenceRecord {
	return FenceRecord{sessionID: sessionID, fence: fence}
}

// Fence returns the wrapped LeaseFence for the serializer to marshal.
func (r FenceRecord) Fence() LeaseFence { return r.fence }

func (FenceRecord) isJournalRecord() {}

// Subject is the session's fence subject.
func (r FenceRecord) Subject() string { return FenceSubject(r.sessionID) }

// IdempotencyID is the epoch rendered as a decimal string.
func (r FenceRecord) IdempotencyID() string { return strconv.FormatUint(r.fence.Epoch, 10) }
