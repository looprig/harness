package catalogreader_test

// These are in-process tests over a REAL memstore-backed sessionstore.Store +
// Catalog. Because memstore is an in-process reference backend (no process boundary,
// no filesystem, no network), they are ordinary unit tests — NOT integration-tagged.
// CLAUDE.md's integration-tag rule targets code that crosses a process boundary; an
// in-memory store does not, so the fast default `go test` covers this adapter.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/serve"
	"github.com/looprig/harness/pkg/serve/catalogreader"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/storage/memstore"
)

// mutClock is a mutable CatalogClock backing so a test can stamp a distinct
// LastActiveAt per session (the field the list read sorts on).
type mutClock struct{ t time.Time }

func (m *mutClock) now() time.Time { return m.t }

// fixedUUID builds a deterministic non-zero uuid from a seed byte; ids sort by seed.
func fixedUUID(seed byte) uuid.UUID {
	var u uuid.UUID
	for i := range u {
		u[i] = seed
	}
	return u
}

func aiMsg(text string) *content.AIMessage {
	return &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

func userMsg(text string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{
		Role:   content.RoleUser,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

// sessionStarted builds a session-scoped SessionStarted for sid.
func sessionStarted(sid uuid.UUID) event.SessionStarted {
	return event.SessionStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid},
		EventID:     fixedUUID(0xA0),
	}}
}

// turnStarted builds a loop-scoped TurnStarted for sid/loop/turn.
func turnStarted(sid, loop, turn uuid.UUID) event.TurnStarted {
	return event.TurnStarted{
		Header:    event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: turn}, EventID: fixedUUID(0xA1)},
		TurnIndex: 1,
		Message:   userMsg("hello"),
	}
}

// turnDone builds a valid loop-scoped TurnDone (reconstructed as LastTurn).
func turnDone(sid, loop, turn uuid.UUID) event.TurnDone {
	return event.TurnDone{
		Header:    event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: turn}, EventID: fixedUUID(0xA2)},
		TurnIndex: 1,
		Message:   aiMsg("done"),
	}
}

// stepDone builds a valid step-scoped StepDone (reconstructed as LastStep).
func stepDone(sid, loop, turn, step uuid.UUID) event.StepDone {
	return event.StepDone{
		Header:   event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: turn, StepID: step}, EventID: fixedUUID(0xA3)},
		Messages: content.AgenticMessages{aiMsg("step")},
	}
}

// gateOpened builds a loop-scoped GateOpened carrying gate gid.
func gateOpened(sid, loop uuid.UUID, gid gate.ID) event.GateOpened {
	return event.GateOpened{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop}, EventID: fixedUUID(0xA4)},
		Gate:   gate.Gate{ID: gid, Kind: gate.KindPermission, Resolver: gate.ResolverLoop},
	}
}

func sessionStopped(sid uuid.UUID) event.SessionStopped {
	return event.SessionStopped{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid},
		EventID:     fixedUUID(0xA5),
	}}
}

// newCatalog opens a memstore Store + Catalog with a mutable clock.
func newCatalog(t *testing.T) (*sessionstore.Store, *sessionstore.Catalog, *mutClock) {
	t.Helper()
	st, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}
	clk := &mutClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	cat := st.OpenCatalog(sessionstore.WithCatalogClock(clk.now))
	return st, cat, clk
}

// update folds ev into the catalog at seq, failing the test on error.
func update(t *testing.T, cat *sessionstore.Catalog, ev event.Event, seq uint64) {
	t.Helper()
	if err := cat.UpdateOnEvent(context.Background(), ev, seq); err != nil {
		t.Fatalf("UpdateOnEvent(%T) err = %v", ev, err)
	}
}

func TestReaderListSessions(t *testing.T) {
	t.Parallel()

	// Three sessions with strictly increasing LastActiveAt; expected sort is
	// most-recent-first (C, B, A), tie-broken by session id ascending.
	base := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	idA, idB, idC := fixedUUID(0x11), fixedUUID(0x22), fixedUUID(0x33)

	seed := func(t *testing.T, cat *sessionstore.Catalog, clk *mutClock, sid uuid.UUID, active time.Time, seq uint64) {
		update(t, cat, sessionStarted(sid), seq)
		clk.t = active
		update(t, cat, turnStarted(sid, fixedUUID(0x01), fixedUUID(0x02)), seq+1)
	}

	tests := []struct {
		name     string
		empty    bool
		page     serve.Page
		wantIDs  []uuid.UUID
		wantSkip int
		wantNext int
		wantDone bool
	}{
		{name: "empty catalog", empty: true, page: serve.Page{Skip: 0, Limit: 100}, wantIDs: nil, wantSkip: 0, wantNext: 0, wantDone: true},
		{name: "default window sorted desc", page: serve.Page{Skip: 0, Limit: 100}, wantIDs: []uuid.UUID{idC, idB, idA}, wantSkip: 0, wantNext: 0, wantDone: true},
		{name: "limit smaller than total not done", page: serve.Page{Skip: 0, Limit: 2}, wantIDs: []uuid.UUID{idC, idB}, wantSkip: 0, wantNext: 2, wantDone: false},
		{name: "skip into middle", page: serve.Page{Skip: 1, Limit: 100}, wantIDs: []uuid.UUID{idB, idA}, wantSkip: 1, wantNext: 0, wantDone: true},
		{name: "skip past end is empty done", page: serve.Page{Skip: 10, Limit: 100}, wantIDs: nil, wantSkip: 10, wantNext: 0, wantDone: true},
		{name: "exact page boundary not done", page: serve.Page{Skip: 0, Limit: 3}, wantIDs: []uuid.UUID{idC, idB, idA}, wantSkip: 0, wantNext: 3, wantDone: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, cat, clk := newCatalog(t)
			if !tt.empty {
				seed(t, cat, clk, idA, base.Add(1*time.Hour), 10)
				seed(t, cat, clk, idB, base.Add(2*time.Hour), 20)
				seed(t, cat, clk, idC, base.Add(3*time.Hour), 30)
			}
			r := catalogreader.New(cat, st)

			got, err := r.ListSessions(context.Background(), tt.page)
			if err != nil {
				t.Fatalf("ListSessions() err = %v", err)
			}
			if len(got.Sessions) != len(tt.wantIDs) {
				t.Fatalf("returned %d sessions, want %d (%+v)", len(got.Sessions), len(tt.wantIDs), got.Sessions)
			}
			for i, want := range tt.wantIDs {
				if got.Sessions[i].SessionID != want {
					t.Errorf("session[%d] id = %v, want %v", i, got.Sessions[i].SessionID, want)
				}
			}
			if got.Skip != tt.wantSkip || got.NextSkip != tt.wantNext || got.Done != tt.wantDone {
				t.Errorf("paging = {skip:%d next:%d done:%v}, want {skip:%d next:%d done:%v}",
					got.Skip, got.NextSkip, got.Done, tt.wantSkip, tt.wantNext, tt.wantDone)
			}
			if got.Limit != tt.page.Limit {
				t.Errorf("limit = %d, want %d", got.Limit, tt.page.Limit)
			}
		})
	}
}

func TestReaderReadStatus(t *testing.T) {
	t.Parallel()

	sid := fixedUUID(0x44)
	loop, turn, step := fixedUUID(0x51), fixedUUID(0x52), fixedUUID(0x53)
	gid := gate.ID(fixedUUID(0x54))

	tests := []struct {
		name         string
		absent       bool
		build        func(t *testing.T, cat *sessionstore.Catalog)
		wantState    string
		wantActive   uuid.UUID
		wantWaiting  uuid.UUID
		wantLastTurn bool
		wantLastStep bool
		wantNotFound bool
	}{
		{
			name: "running (turn active)",
			build: func(t *testing.T, cat *sessionstore.Catalog) {
				update(t, cat, sessionStarted(sid), 1)
				update(t, cat, turnStarted(sid, loop, turn), 2)
			},
			wantState: "running", wantActive: turn,
		},
		{
			name: "waiting on gate",
			build: func(t *testing.T, cat *sessionstore.Catalog) {
				update(t, cat, sessionStarted(sid), 1)
				update(t, cat, turnStarted(sid, loop, turn), 2)
				update(t, cat, gateOpened(sid, loop, gid), 3)
			},
			wantState: "waiting_on_gate", wantActive: turn, wantWaiting: uuid.UUID(gid),
		},
		{
			name: "idle after turn done (last_turn set)",
			build: func(t *testing.T, cat *sessionstore.Catalog) {
				update(t, cat, sessionStarted(sid), 1)
				update(t, cat, turnStarted(sid, loop, turn), 2)
				update(t, cat, stepDone(sid, loop, turn, step), 3)
				update(t, cat, turnDone(sid, loop, turn), 4)
			},
			wantState: "idle", wantLastTurn: true, wantLastStep: true,
		},
		{
			name: "stopped",
			build: func(t *testing.T, cat *sessionstore.Catalog) {
				update(t, cat, sessionStarted(sid), 1)
				update(t, cat, sessionStopped(sid), 2)
			},
			wantState: "stopped",
		},
		{name: "absent session is not found", absent: true, wantNotFound: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, cat, _ := newCatalog(t)
			if tt.build != nil {
				tt.build(t, cat)
			}
			r := catalogreader.New(cat, st)

			status, err := r.ReadStatus(context.Background(), sid)
			if tt.wantNotFound {
				var nf serve.SessionNotFoundError
				if err == nil || !errors.As(err, &nf) {
					t.Fatalf("ReadStatus() err = %v, want SessionNotFoundError", err)
				}
				if nf.SessionID != sid {
					t.Errorf("not-found session id = %v, want %v", nf.SessionID, sid)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadStatus() err = %v", err)
			}
			if status.SessionID != sid {
				t.Errorf("session id = %v, want %v", status.SessionID, sid)
			}
			if status.State != tt.wantState {
				t.Errorf("state = %q, want %q", status.State, tt.wantState)
			}
			if status.ActiveTurnID != tt.wantActive {
				t.Errorf("active_turn_id = %v, want %v", status.ActiveTurnID, tt.wantActive)
			}
			if status.WaitingGateID != tt.wantWaiting {
				t.Errorf("waiting_gate_id = %v, want %v", status.WaitingGateID, tt.wantWaiting)
			}
			if tt.wantLastTurn {
				if status.LastTurn == nil {
					t.Fatal("last_turn = nil, want reconstructed TurnDone")
				}
				if _, ok := status.LastTurn.Event.(event.TurnDone); !ok {
					t.Errorf("last_turn event = %T, want event.TurnDone", status.LastTurn.Event)
				}
				if status.LastTurn.JournalSeq != 4 {
					t.Errorf("last_turn seq = %d, want 4", status.LastTurn.JournalSeq)
				}
			}
			if tt.wantLastStep {
				if status.LastStep == nil {
					t.Fatal("last_step = nil, want reconstructed StepDone")
				}
				if _, ok := status.LastStep.Event.(event.StepDone); !ok {
					t.Errorf("last_step event = %T, want event.StepDone", status.LastStep.Event)
				}
				if status.LastStep.JournalSeq != 3 {
					t.Errorf("last_step seq = %d, want 3", status.LastStep.JournalSeq)
				}
			}
		})
	}
}

// openJournal acquires a lease and opens the session journal for appends.
func openJournal(t *testing.T, st *sessionstore.Store, id uuid.UUID) journal.SessionJournal {
	t.Helper()
	lease, err := st.AcquireLease(context.Background(), id)
	if err != nil {
		t.Fatalf("AcquireLease() err = %v", err)
	}
	j, err := st.OpenJournal(context.Background(), id, lease)
	if err != nil {
		t.Fatalf("OpenJournal() err = %v", err)
	}
	return j
}

func TestReaderReadJournal(t *testing.T) {
	t.Parallel()

	sid := fixedUUID(0x66)
	loop := fixedUUID(0x67)

	// Build a ledger holding: SessionStarted, a private GatePreparedRecord (must be
	// filtered), then two TurnDones. The event replayer yields ONLY the three events.
	buildLedger := func(t *testing.T) *sessionstore.Store {
		st, err := sessionstore.Open(memstore.New())
		if err != nil {
			t.Fatalf("Open() err = %v", err)
		}
		j := openJournal(t, st, sid)

		stepH := event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: fixedUUID(0x70), StepID: fixedUUID(0x71)}, EventID: fixedUUID(0x72)}
		g := gate.Gate{ID: gate.ID(fixedUUID(0x73)), Kind: gate.KindPermission, Resolver: gate.ResolverLoop}
		prepared := event.GatePrepared{Header: stepH, Gate: g}
		openPayload := gate.OpenPayload{GateID: g.ID, Payload: gate.PermissionPayload{Request: tool.BashRequest{Command: "echo ok"}}}

		recs := []journal.JournalRecord{
			journal.NewEventRecord(sessionStarted(sid)),                  // seq 2 (after opening fence at 1)
			journal.NewGatePreparedRecord(prepared, openPayload),         // seq 3 (filtered)
			journal.NewEventRecord(turnDone(sid, loop, fixedUUID(0x74))), // seq 4
			journal.NewEventRecord(turnDone(sid, loop, fixedUUID(0x75))), // seq 5
		}
		for _, rec := range recs {
			if _, err := j.Append(context.Background(), rec); err != nil {
				t.Fatalf("Append(%T) err = %v", rec, err)
			}
		}
		return st
	}

	tests := []struct {
		name         string
		absent       bool
		page         serve.JournalPage
		wantCount    int
		wantFirstSeq uint64
		wantNext     uint64
		wantDone     bool
		wantNoGate   bool
	}{
		{name: "absent session yields empty done", absent: true, page: serve.JournalPage{From: 0, Limit: 100}, wantCount: 0, wantNext: 0, wantDone: true},
		{name: "from beginning yields all events done", page: serve.JournalPage{From: 0, Limit: 100}, wantCount: 3, wantFirstSeq: 2, wantNext: 0, wantDone: true, wantNoGate: true},
		{name: "limit under total not done", page: serve.JournalPage{From: 0, Limit: 2}, wantCount: 2, wantFirstSeq: 2, wantNext: 5, wantDone: false},
		{name: "from interior sequence", page: serve.JournalPage{From: 4, Limit: 100}, wantCount: 2, wantFirstSeq: 4, wantNext: 0, wantDone: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var st *sessionstore.Store
			if tt.absent {
				var err error
				st, err = sessionstore.Open(memstore.New())
				if err != nil {
					t.Fatalf("Open() err = %v", err)
				}
			} else {
				st = buildLedger(t)
			}
			cat := st.OpenCatalog()
			r := catalogreader.New(cat, st)

			page, err := r.ReadJournal(context.Background(), sid, tt.page)
			if err != nil {
				t.Fatalf("ReadJournal() err = %v", err)
			}
			if len(page.Events) != tt.wantCount {
				t.Fatalf("returned %d events, want %d (%+v)", len(page.Events), tt.wantCount, page.Events)
			}
			if tt.wantCount > 0 && page.Events[0].JournalSeq != tt.wantFirstSeq {
				t.Errorf("first event seq = %d, want %d", page.Events[0].JournalSeq, tt.wantFirstSeq)
			}
			if page.NextJournalSeq != tt.wantNext || page.Done != tt.wantDone {
				t.Errorf("cursor = {next:%d done:%v}, want {next:%d done:%v}", page.NextJournalSeq, page.Done, tt.wantNext, tt.wantDone)
			}
			if tt.wantNoGate {
				for _, se := range page.Events {
					if _, ok := se.Event.(event.GatePrepared); ok {
						t.Errorf("GatePrepared leaked into journal page at seq %d", se.JournalSeq)
					}
				}
			}
		})
	}
}
