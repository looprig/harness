package serve

import (
	"context"
	"encoding/json"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// Page is the offset/limit window a session-list read requests. Skip is the number
// of entries to drop from the front of the stable-sorted list; Limit is the maximum
// number to return. Both are validated (non-negative, Limit hard-capped) at the HTTP
// boundary before a Page is built, so a Reader implementation may trust them.
type Page struct {
	Skip  int
	Limit int
}

// JournalPage is the cursor/limit window a journal read requests. From is the
// inclusive journal sequence to begin at (0 = from the beginning); Limit is the
// maximum number of Enduring events to return. Both are validated at the HTTP
// boundary before a JournalPage is built.
type JournalPage struct {
	From  uint64
	Limit int
}

// Reader is the narrow, stateless read plane over persisted session history. It is
// the seam serve's read handlers depend on (Dependency Inversion): serve declares
// the interface and the DTOs, and a concrete adapter (pkg/serve/catalogreader) wires
// it over the session store WITHOUT serve importing any store package. Every method
// is a pure read — it consults durable projection/history only, never a live
// in-process session — so any pod can serve a read and no live session need exist.
//
//   - ListSessions returns a stable-sorted, offset-paged slice of session summaries.
//   - ReadStatus returns one session's public projected status (no replay). It
//     returns a SessionNotFoundError when the session has no catalog entry.
//   - ReadJournal returns a cursor-paged slice of the session's public Enduring
//     events. Serve validates both event-bearing results again before writing.
type Reader interface {
	ListSessions(ctx context.Context, page Page) (SessionList, error)
	ReadStatus(ctx context.Context, id uuid.UUID) (SessionStatus, error)
	ReadJournal(ctx context.Context, id uuid.UUID, page JournalPage) (EventJournalPage, error)
}

// SessionSummary is one row of a session list: the small, picker-facing projection
// of a session's catalog entry. It is deliberately narrow (Interface Segregation) —
// a list caller needs an identity, a lifecycle state, and recency, not the full
// status projection (LastTurn/LastStep replay-safe summaries live on SessionStatus).
type SessionSummary struct {
	SessionID    uuid.UUID `json:"session_id"`
	State        string    `json:"state,omitempty"`
	Title        string    `json:"title,omitempty"`
	CreatedAt    time.Time `json:"created_at,omitzero"`
	LastActiveAt time.Time `json:"last_active_at,omitzero"`
}

// SessionList is the GET /v1/sessions response: a page of summaries plus the paging
// cursor a client resumes from. Skip/Limit echo the request window; NextSkip is the
// Skip to pass for the next page (set only when more may remain); Done reports the
// end of the list was reached (the page returned fewer than Limit entries).
type SessionList struct {
	Sessions []SessionSummary `json:"sessions"`
	Skip     int              `json:"skip"`
	Limit    int              `json:"limit"`
	NextSkip int              `json:"next_skip"`
	Done     bool             `json:"done"`
}

// StatusEvent pairs a durable journal sequence with the concrete event recorded at
// that sequence. Event is the event.Event INTERFACE, which encoding/json cannot
// serialize directly (an interface has no stable wire shape), so StatusEvent defines
// a custom MarshalJSON that emits the codec-safe {journal_seq, event} shape with the
// event serialized by event.MarshalEvent (the single durable-envelope authority). It
// is a write-only DTO — the read plane serializes it outward and never decodes it.
type StatusEvent struct {
	JournalSeq uint64
	Event      event.Event
}

// statusEventWire is StatusEvent's on-the-wire form: the journal sequence plus the
// event pre-encoded to its durable envelope as opaque raw JSON. A nil Event yields an
// omitted "event" key (rather than a null) — the summary is present but carries no
// event, which the omitempty on a nil RawMessage expresses.
type statusEventWire struct {
	JournalSeq uint64          `json:"journal_seq"`
	Event      json.RawMessage `json:"event,omitempty"`
}

// MarshalJSON emits the codec-safe {journal_seq, event} shape: the event is encoded
// via event.MarshalEvent so the nested "event" value is the durable wire envelope
// (type-tagged, versioned) a decoder can round-trip, NOT a Go-struct dump of the
// interface. A nil Event is omitted (see statusEventWire). A MarshalEvent failure
// (an Ephemeral or unknown event handed to a status projection) surfaces as a marshal
// error rather than emitting a lossy record.
func (s StatusEvent) MarshalJSON() ([]byte, error) {
	w := statusEventWire{JournalSeq: s.JournalSeq}
	if s.Event != nil {
		if err := validateStatusEvent(s); err != nil {
			return nil, err
		}
		raw, err := event.MarshalEvent(s.Event)
		if err != nil {
			return nil, err
		}
		w.Event = raw
	}
	return json.Marshal(w)
}

// SessionStatus is the GET /v1/sessions/{sid}/status response: one session's
// projected lifecycle status, read from the catalog projection with NO journal
// replay. State is the lifecycle fold (running/waiting_on_gate/idle/failed/
// interrupted/stopped); ActiveTurnID and WaitingGateID are zero (omitted) unless a
// turn is running or a gate is open; LastTurn/LastStep are the codec-safe summaries
// of the most recent terminal turn and completed step; UpdatedAt is the projection's
// most-recent-activity instant.
type SessionStatus struct {
	SessionID      uuid.UUID    `json:"session_id"`
	State          string       `json:"state,omitempty"`
	LastJournalSeq uint64       `json:"last_journal_seq"`
	ActiveTurnID   uuid.UUID    `json:"active_turn_id,omitzero"`
	WaitingGateID  uuid.UUID    `json:"waiting_gate_id,omitzero"`
	LastTurn       *StatusEvent `json:"last_turn,omitempty"`
	LastStep       *StatusEvent `json:"last_step,omitempty"`
	UpdatedAt      time.Time    `json:"updated_at,omitzero"`
}

// EventJournalPage is the GET /v1/sessions/{sid}/journal response: a page of a
// session's Enduring events in journal-sequence order plus the resume cursor.
// NextJournalSeq is the sequence to pass as from_journal_seq to fetch the next page
// (set only when more may remain); Done reports the journal was exhausted (fewer than
// Limit events remained). GatePrepared never appears — the event replayer filters it.
type EventJournalPage struct {
	Events         []StatusEvent `json:"events"`
	NextJournalSeq uint64        `json:"next_journal_seq"`
	Done           bool          `json:"done"`
}
