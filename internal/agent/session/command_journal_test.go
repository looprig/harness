package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/agent/session/hub"
	"github.com/inventivepotter/urvi/internal/agent/session/journal"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// fakeCommandAppender is a commandAppender double that records every CommandRecord the
// session appends and optionally fails. It is concurrency-safe because Interrupt and
// Shutdown fan out across loops on the calling goroutine but the test reads the loops'
// Commands channels from goroutines, so the appended records may be observed across
// goroutines.
type fakeCommandAppender struct {
	mu      sync.Mutex
	records []journal.CommandRecord
	err     error
}

func (f *fakeCommandAppender) AppendCommand(_ context.Context, rec journal.CommandRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, rec)
	return f.err
}

func (f *fakeCommandAppender) snapshot() []journal.CommandRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]journal.CommandRecord, len(f.records))
	copy(out, f.records)
	return out
}

// pinnedClock returns a Clock that always reports ts, so a test can assert the
// session stamped CreatedAt from the injected clock (not the wall clock).
func pinnedClock(ts time.Time) func() time.Time { return func() time.Time { return ts } }

// fakeAppenderSession builds a struct-literal Session wired to N fake loops, a fake
// commandAppender, and a pinned clock, returning the per-loop Commands channels so a
// test can both read the dispatched command and inspect the appended record. It is the
// command-journal counterpart to sessionWithFakeLoop/sessionWithTwoFakeLoops.
func fakeAppenderSession(app *fakeCommandAppender, ts time.Time, loopIDs ...uuid.UUID) (s *Session, cmds map[uuid.UUID]chan command.Command) {
	cmds = make(map[uuid.UUID]chan command.Command)
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	id := mustUUID()
	loops := make(map[uuid.UUID]*loopHandle, len(loopIDs))
	for _, lid := range loopIDs {
		ch := make(chan command.Command)
		cmds[lid] = ch
		loops[lid] = &loopHandle{loop: &loop.Loop{Commands: ch, Done: make(chan struct{})}}
	}
	s = &Session{
		SessionID:     id,
		hub:           hub.New(id),
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops:         loops,
		primaryLoopID: loopIDs[0],
		newID:         uuid.New,
		now:           pinnedClock(ts),
		cmdAppender:   app,
	}
	return s, cmds
}

// recvCommand reads one command off ch within a short deadline, failing the test if
// none arrives (a dispatch that never reached the loop).
func recvCommand(t *testing.T, ch chan command.Command) command.Command {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(time.Second):
		t.Fatal("no command dispatched to the loop within the deadline")
		return nil
	}
}

// assertRecord checks the appended record's target subject (session+loop), idempotency
// id (CommandID), and the dispatched command's CommandID/Agency/non-zero CreatedAt.
func assertRecord(t *testing.T, rec journal.CommandRecord, sid, loopID uuid.UUID, dispatched command.Command, wantAgency identity.Agency, wantTS time.Time) {
	t.Helper()
	wantSubject := journal.LoopCommandSubject(sid, loopID)
	if rec.Subject() != wantSubject {
		t.Errorf("record subject = %q, want %q", rec.Subject(), wantSubject)
	}
	dh := dispatched.CommandHeader()
	if rec.IdempotencyID() != dh.CommandID.String() {
		t.Errorf("record idempotency id = %q, want %q (dispatched CommandID)", rec.IdempotencyID(), dh.CommandID.String())
	}
	rh := rec.Command().CommandHeader()
	if rh.CommandID != dh.CommandID {
		t.Errorf("record CommandID = %v, want %v (matches dispatched)", rh.CommandID, dh.CommandID)
	}
	if rh.Agency != wantAgency {
		t.Errorf("record Agency = %v, want %v", rh.Agency, wantAgency)
	}
	if dh.Agency != wantAgency {
		t.Errorf("dispatched Agency = %v, want %v", dh.Agency, wantAgency)
	}
	if rh.CreatedAt.IsZero() {
		t.Error("record CreatedAt is zero, want a non-zero stamp from the injected clock")
	}
	if !rh.CreatedAt.Equal(wantTS) {
		t.Errorf("record CreatedAt = %v, want %v (the injected clock)", rh.CreatedAt, wantTS)
	}
}

// TestSubmitAppendsCommandRecord covers the human Submit path (UserInput, AgencyUser)
// and the machine submitToLoop path (UserInput, AgencyMachine): each appends a
// CommandRecord targeting the right loop, with the dispatched command's CommandID,
// Agency, and a non-zero CreatedAt from the injected clock.
func TestSubmitAppendsCommandRecord(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		call       func(s *Session, loopID uuid.UUID) (uuid.UUID, error)
		wantAgency identity.Agency
	}{
		{
			name:       "human Submit stamps AgencyUser",
			call:       func(s *Session, loopID uuid.UUID) (uuid.UUID, error) { return s.Submit(context.Background(), nil) },
			wantAgency: identity.AgencyUser,
		},
		{
			name: "machine submitToLoop stamps AgencyMachine",
			call: func(s *Session, loopID uuid.UUID) (uuid.UUID, error) {
				return s.submitToLoop(context.Background(), loopID, nil, identity.AgencyMachine)
			},
			wantAgency: identity.AgencyMachine,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app := &fakeCommandAppender{}
			lid := mustUUID()
			s, cmds := fakeAppenderSession(app, ts, lid)

			var (
				dispatched command.Command
				wg         sync.WaitGroup
			)
			wg.Add(1)
			go func() { defer wg.Done(); dispatched = recvCommand(t, cmds[lid]) }()

			id, err := tt.call(s, lid)
			if err != nil {
				t.Fatalf("submit = %v, want nil", err)
			}
			wg.Wait()

			if _, ok := dispatched.(command.UserInput); !ok {
				t.Fatalf("dispatched %T, want command.UserInput", dispatched)
			}
			if dispatched.CommandHeader().CommandID != id {
				t.Errorf("dispatched CommandID = %v, want returned id %v", dispatched.CommandHeader().CommandID, id)
			}
			recs := app.snapshot()
			if len(recs) != 1 {
				t.Fatalf("appended %d records, want 1", len(recs))
			}
			assertRecord(t, recs[0], s.SessionID, lid, dispatched, tt.wantAgency, ts)
		})
	}
}

// TestSubagentResultAppendsCommandRecord covers the spawner hand-back
// (deliverSubagentResult → command.SubagentResult): the record targets the PARENT loop
// and carries machine agency.
func TestSubagentResultAppendsCommandRecord(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 21, 10, 1, 0, 0, time.UTC)
	app := &fakeCommandAppender{}
	parentLoopID := mustUUID()
	fromLoopID := mustUUID()
	s, cmds := fakeAppenderSession(app, ts, parentLoopID)

	var (
		dispatched command.Command
		wg         sync.WaitGroup
	)
	wg.Add(1)
	go func() { defer wg.Done(); dispatched = recvCommand(t, cmds[parentLoopID]) }()

	if err := s.deliverSubagentResult(context.Background(), parentLoopID, fromLoopID, []content.Block{&content.TextBlock{Text: "done"}}); err != nil {
		t.Fatalf("deliverSubagentResult = %v, want nil", err)
	}
	wg.Wait()

	sr, ok := dispatched.(command.SubagentResult)
	if !ok {
		t.Fatalf("dispatched %T, want command.SubagentResult", dispatched)
	}
	if sr.LoopID != parentLoopID {
		t.Errorf("SubagentResult target LoopID = %v, want parent %v", sr.LoopID, parentLoopID)
	}
	recs := app.snapshot()
	if len(recs) != 1 {
		t.Fatalf("appended %d records, want 1", len(recs))
	}
	assertRecord(t, recs[0], s.SessionID, parentLoopID, dispatched, identity.AgencyMachine, ts)
}

// TestGateRepliesAppendCommandRecord covers Approve/Deny/ProvideUserInput (the
// routeGate fire-and-route sites): each appends a record for the gate command to the
// resolved loop, with AgencyUser.
func TestGateRepliesAppendCommandRecord(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 21, 10, 2, 0, 0, time.UTC)
	callID := mustUUID()
	tests := []struct {
		name    string
		call    func(s *Session, loopID uuid.UUID) error
		wantCmd func(command.Command) bool
	}{
		{
			name: "Approve",
			call: func(s *Session, loopID uuid.UUID) error {
				return s.Approve(context.Background(), loopID, callID, tool.ScopeSession)
			},
			wantCmd: func(c command.Command) bool { _, ok := c.(command.ApproveToolCall); return ok },
		},
		{
			name:    "Deny",
			call:    func(s *Session, loopID uuid.UUID) error { return s.Deny(context.Background(), loopID, callID) },
			wantCmd: func(c command.Command) bool { _, ok := c.(command.DenyToolCall); return ok },
		},
		{
			name: "ProvideUserInput",
			call: func(s *Session, loopID uuid.UUID) error {
				return s.ProvideUserInput(context.Background(), loopID, callID, "answer")
			},
			wantCmd: func(c command.Command) bool { _, ok := c.(command.ProvideUserInput); return ok },
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app := &fakeCommandAppender{}
			lid := mustUUID()
			s, cmds := fakeAppenderSession(app, ts, lid)

			var (
				dispatched command.Command
				wg         sync.WaitGroup
			)
			wg.Add(1)
			go func() { defer wg.Done(); dispatched = recvCommand(t, cmds[lid]) }()

			if err := tt.call(s, lid); err != nil {
				t.Fatalf("gate reply = %v, want nil", err)
			}
			wg.Wait()

			if !tt.wantCmd(dispatched) {
				t.Fatalf("dispatched %T, not the expected gate command", dispatched)
			}
			recs := app.snapshot()
			if len(recs) != 1 {
				t.Fatalf("appended %d records, want 1", len(recs))
			}
			assertRecord(t, recs[0], s.SessionID, lid, dispatched, identity.AgencyUser, ts)
		})
	}
}

// TestInterruptAppendsOneRecordPerLoop proves Interrupt fans out one CommandRecord per
// loop (N loops → N records), each targeting its own loop, stamped AgencyUser.
func TestInterruptAppendsOneRecordPerLoop(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 21, 10, 3, 0, 0, time.UTC)
	app := &fakeCommandAppender{}
	loopA := mustUUID()
	loopB := mustUUID()
	s, cmds := fakeAppenderSession(app, ts, loopA, loopB)

	// Drain both loops' Commands and ack (false: nothing was running) so Interrupt
	// returns.
	for _, lid := range []uuid.UUID{loopA, loopB} {
		ch := cmds[lid]
		go func() {
			c := recvCommand(t, ch)
			if ic, ok := c.(command.Interrupt); ok && ic.Ack != nil {
				ic.Ack <- false
			}
		}()
	}

	if _, err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt = %v, want nil", err)
	}

	recs := app.snapshot()
	if len(recs) != 2 {
		t.Fatalf("appended %d records, want 2 (one per loop)", len(recs))
	}
	seen := map[string]bool{}
	for _, rec := range recs {
		seen[rec.Subject()] = true
		if _, ok := rec.Command().(command.Interrupt); !ok {
			t.Errorf("record command = %T, want command.Interrupt", rec.Command())
		}
		if rec.Command().CommandHeader().Agency != identity.AgencyUser {
			t.Errorf("record Agency = %v, want AgencyUser", rec.Command().CommandHeader().Agency)
		}
		if rec.Command().CommandHeader().CreatedAt.IsZero() {
			t.Error("record CreatedAt is zero, want the injected clock stamp")
		}
	}
	if !seen[journal.LoopCommandSubject(s.SessionID, loopA)] || !seen[journal.LoopCommandSubject(s.SessionID, loopB)] {
		t.Errorf("records did not target both loop subjects: %v", seen)
	}
}

// TestShutdownAppendsOneRecordPerLoop proves Shutdown fans out one CommandRecord per
// loop (N loops → N records).
func TestShutdownAppendsOneRecordPerLoop(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 21, 10, 4, 0, 0, time.UTC)
	app := &fakeCommandAppender{}
	loopA := mustUUID()
	loopB := mustUUID()
	s, cmds := fakeAppenderSession(app, ts, loopA, loopB)

	for _, lid := range []uuid.UUID{loopA, loopB} {
		ch := cmds[lid]
		go func() {
			c := recvCommand(t, ch)
			if sc, ok := c.(command.Shutdown); ok && sc.Ack != nil {
				sc.Ack <- nil
			}
		}()
	}

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}

	recs := app.snapshot()
	if len(recs) != 2 {
		t.Fatalf("appended %d records, want 2 (one per loop)", len(recs))
	}
	for _, rec := range recs {
		if _, ok := rec.Command().(command.Shutdown); !ok {
			t.Errorf("record command = %T, want command.Shutdown", rec.Command())
		}
		if rec.Command().CommandHeader().CreatedAt.IsZero() {
			t.Error("record CreatedAt is zero, want the injected clock stamp")
		}
	}
}

// TestCommandAppendIsAuditOnly proves a failing commandAppender does NOT block the
// dispatch: the command still reaches the loop and the method returns success. The
// failure is logged (not asserted) and swallowed by design (the ONE deliberate
// non-fatal path).
func TestCommandAppendIsAuditOnly(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 21, 10, 5, 0, 0, time.UTC)
	tests := []struct {
		name     string
		dispatch func(s *Session, loopID uuid.UUID) error
		wantType func(command.Command) bool
	}{
		{
			name:     "Submit proceeds when append fails",
			dispatch: func(s *Session, loopID uuid.UUID) error { _, err := s.Submit(context.Background(), nil); return err },
			wantType: func(c command.Command) bool { _, ok := c.(command.UserInput); return ok },
		},
		{
			name: "Approve proceeds when append fails",
			dispatch: func(s *Session, loopID uuid.UUID) error {
				return s.Approve(context.Background(), loopID, mustUUID(), tool.ScopeOnce)
			},
			wantType: func(c command.Command) bool { _, ok := c.(command.ApproveToolCall); return ok },
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app := &fakeCommandAppender{err: errors.New("journal unavailable")}
			lid := mustUUID()
			s, cmds := fakeAppenderSession(app, ts, lid)

			var (
				dispatched command.Command
				wg         sync.WaitGroup
			)
			wg.Add(1)
			go func() { defer wg.Done(); dispatched = recvCommand(t, cmds[lid]) }()

			if err := tt.dispatch(s, lid); err != nil {
				t.Fatalf("dispatch returned %v despite append failure, want nil (audit-only)", err)
			}
			wg.Wait()

			if dispatched == nil || !tt.wantType(dispatched) {
				t.Fatalf("command did not reach the loop despite append failure: %T", dispatched)
			}
			// The record was still attempted (logged-and-proceeded), proving the failure
			// path runs the append before dispatch.
			if len(app.snapshot()) != 1 {
				t.Errorf("append attempts = %d, want 1 (attempted before dispatch)", len(app.snapshot()))
			}
		})
	}
}

// TestNopCommandAppenderDefault proves a session built without an injected appender
// (the headless/no-persistence default) dispatches commands normally and never
// nil-derefs on the append path.
func TestNopCommandAppenderDefault(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("hi")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	if _, err := s.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "hello"}}); err != nil {
		t.Fatalf("Submit with the nop appender = %v, want nil", err)
	}
}
