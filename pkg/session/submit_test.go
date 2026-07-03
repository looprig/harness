package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/uuid"
)

// TestSubmitFireAndForget asserts Submit's fire-and-forget contract end-to-end on
// the command channel: a successful send returns a non-zero InputID (the submit
// command's Header.ID) and a nil error, and the loop receives a command.UserInput
// stamped with exactly that id, carrying the input blocks — the outcome is observed
// on the session fan-in, never returned. A send to a loop whose Done channel is
// already closed must fail secure with *SessionError{SessionLoopExited} and the
// returned id must be zero (no usable correlation when nothing was sent).
//
// The fake-loop seam (sessionWithFakeLoop) captures the exact command the session
// sent on the unbuffered Commands channel — the only observable effect of a
// fire-and-forget submit (it reads no reply). Submit runs in a goroutine so the
// test can be the channel's reader; the success cases assert id + command shape,
// the loop-gone case asserts the typed error without ever reading the (never-sent)
// command.
func TestSubmitFireAndForget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		blocks        []content.Block
		loopGone      bool // close the fake loop's Done before Submit, forcing the exited path
		wantErr       bool
		wantKind      SessionErrorKind
		wantNonZeroID bool
	}{
		{
			name:          "idle session queues a UserInput",
			blocks:        []content.Block{&content.TextBlock{Text: "hello"}},
			wantNonZeroID: true,
		},
		{
			name:          "nil blocks still send fire-and-forget",
			blocks:        nil,
			wantNonZeroID: true,
		},
		{
			name:     "loop gone returns SessionLoopExited",
			blocks:   []content.Block{&content.TextBlock{Text: "x"}},
			loopGone: true,
			wantErr:  true,
			wantKind: SessionLoopExited,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, cmds, done := sessionWithFakeLoop()

			if tt.loopGone {
				// A closed Done makes the send select fall through to the exited path
				// without the test ever reading Commands.
				close(done)
				id, err := s.Submit(context.Background(), tt.blocks)
				var se *SessionError
				if !errors.As(err, &se) || se.Kind != tt.wantKind {
					t.Fatalf("Submit err = %v, want *SessionError{%s}", err, tt.wantKind)
				}
				if !id.IsZero() {
					t.Errorf("Submit id = %v on loop-gone path, want zero (nothing sent)", id)
				}
				// No command may have been sent on the exited path.
				select {
				case cmd := <-cmds:
					t.Fatalf("Submit sent %T on loop-gone path, want no command", cmd)
				default:
				}
				return
			}

			type result struct {
				id  uuid.UUID
				err error
			}
			resCh := make(chan result, 1)
			go func() {
				id, err := s.Submit(context.Background(), tt.blocks)
				resCh <- result{id: id, err: err}
			}()

			var cmd command.Command
			select {
			case cmd = <-cmds:
			case <-time.After(2 * time.Second):
				t.Fatal("Submit never sent a command")
			}

			var res result
			select {
			case res = <-resCh:
			case <-time.After(2 * time.Second):
				t.Fatal("Submit never returned after send")
			}
			if res.err != nil {
				t.Fatalf("Submit returned err = %v, want nil", res.err)
			}
			if tt.wantNonZeroID && res.id.IsZero() {
				t.Fatal("Submit returned a zero InputID, want a fresh non-zero id")
			}

			ui, ok := cmd.(command.UserInput)
			if !ok {
				t.Fatalf("Submit sent %T, want command.UserInput", cmd)
			}
			// The returned InputID is the command's Header.ID — the Cause.CommandID the
			// Reply events will carry. They must be identical.
			if ui.Header.CommandID != res.id {
				t.Errorf("UserInput.Header.ID = %v, want returned InputID %v", ui.Header.CommandID, res.id)
			}
			if len(ui.Blocks) != len(tt.blocks) {
				t.Errorf("UserInput.Blocks len = %d, want %d", len(ui.Blocks), len(tt.blocks))
			}
		})
	}
}

// TestSubmitLoopNotFound asserts Submit fails secure with
// *SessionError{SessionLoopNotFound} when the primary loop id resolves to no
// registry entry, and sends no command. The miss is forced by deleting the primary
// entry while leaving primaryLoopID set — the exact state the loopFor guard covers.
func TestSubmitLoopNotFound(t *testing.T) {
	t.Parallel()
	s, cmds, _ := sessionWithFakeLoop() // Commands never read: a send would block forever

	s.loopsMu.Lock()
	delete(s.loops, s.primaryLoopID)
	s.loopsMu.Unlock()

	id, err := s.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "x"}})
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionLoopNotFound {
		t.Fatalf("Submit err = %v, want *SessionError{SessionLoopNotFound}", err)
	}
	if !id.IsZero() {
		t.Errorf("Submit id = %v on loop-not-found path, want zero", id)
	}
	select {
	case cmd := <-cmds:
		t.Fatalf("Submit sent %T on a missing-loop path, want no command", cmd)
	default:
	}
}

// TestSubmitCtxCancelled asserts Submit returns *SessionError{SessionContextDone}
// when ctx is already cancelled and the loop's Commands channel is not being read
// (the send cannot proceed), and that no command escapes.
func TestSubmitCtxCancelled(t *testing.T) {
	t.Parallel()
	s, cmds, _ := sessionWithFakeLoop() // unbuffered Commands, never read

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	id, err := s.Submit(ctx, []content.Block{&content.TextBlock{Text: "x"}})
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionContextDone {
		t.Fatalf("Submit err = %v, want *SessionError{SessionContextDone}", err)
	}
	if !id.IsZero() {
		t.Errorf("Submit id = %v on ctx-done path, want zero", id)
	}
	select {
	case cmd := <-cmds:
		t.Fatalf("Submit sent %T on a ctx-done path, want no command", cmd)
	default:
	}
}

// TestSubmitFreshIDPerCall asserts Submit mints a distinct InputID on every call
// (fresh per command, never reused), so each submit correlates its own Reply
// events. Each call's command is drained from the fake loop so the next can send.
func TestSubmitFreshIDPerCall(t *testing.T) {
	t.Parallel()
	s, cmds, _ := sessionWithFakeLoop()

	ids := make([]uuid.UUID, 0, 2)
	for i := 0; i < 2; i++ {
		idCh := make(chan uuid.UUID, 1)
		go func() {
			id, err := s.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "x"}})
			if err != nil {
				idCh <- uuid.UUID{}
				return
			}
			idCh <- id
		}()
		select {
		case <-cmds: // drain so the next Submit can send
		case <-time.After(2 * time.Second):
			t.Fatal("Submit never sent a command")
		}
		select {
		case id := <-idCh:
			if id.IsZero() {
				t.Fatal("Submit returned a zero id")
			}
			ids = append(ids, id)
		case <-time.After(2 * time.Second):
			t.Fatal("Submit never returned")
		}
	}
	if ids[0] == ids[1] {
		t.Errorf("Submit reused id %v across calls, want distinct ids", ids[0])
	}
}

// waitTurnStartedOn polls the recorded events until a TurnStarted on loopID has
// been drained, returning it. It bridges the async subscription drain the same way
// waitTurnStartedAgency / waitTurnCausationID do — the recordingSub goroutine may
// not have appended the event by the time submitToLoop returns.
func waitTurnStartedOn(r *recordingSub, loopID uuid.UUID, d time.Duration) (event.TurnStarted, bool) {
	deadline := time.Now().Add(d)
	for {
		r.mu.Lock()
		for _, ev := range r.events {
			if ts, ok := ev.(event.TurnStarted); ok && ts.LoopID == loopID {
				r.mu.Unlock()
				return ts, true
			}
		}
		r.mu.Unlock()
		if time.Now().After(deadline) {
			return event.TurnStarted{}, false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSubmitToLoopTargetsSubLoop is the end-to-end proof that the loop-targeted
// submitToLoop drives a SPECIFIC (non-primary) sub-loop with a caller-chosen Agency:
// a NewLoop'd second loop receives the submit (not the primary), and the resulting
// event.TurnStarted on THAT loop carries Cause.CommandID == the returned id and
// Cause.Agency == the agency the caller passed (AgencyMachine, the subagent-task
// case). This is the loop-targeted, agency-parameterized core that the public Submit
// (primary loop, AgencyUser) and the later subagent path both build on.
func TestSubmitToLoopTargetsSubLoop(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("hi")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// A second loop in the same session — the submit target. Its parent provenance is
	// the primary loop (a stand-in for the real subagent spawn).
	subLoopID, err := s.NewLoop(loop.Provenance{LoopID: s.primaryLoopID}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("sub")}}))
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}

	// Subscribe to the sub-loop's Enduring events BEFORE submitting, so the resulting
	// TurnStarted (Enduring, loop-scoped) cannot be missed (the hub has no replay).
	sub, err := s.SubscribeEvents(event.EventFilter{
		Enduring: event.LoopScope{Loops: map[uuid.UUID]struct{}{subLoopID: {}}},
	})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	rec := &recordingSub{}
	go func() {
		for ev := range sub.Events() {
			rec.record(ev)
		}
	}()

	id, err := s.submitToLoop(context.Background(), subLoopID, nil, identity.AgencyMachine)
	if err != nil {
		t.Fatalf("submitToLoop: %v", err)
	}
	if id.IsZero() {
		t.Fatal("submitToLoop returned a zero id, want a fresh non-zero command id")
	}

	ts, ok := waitTurnStartedOn(rec, subLoopID, 2*time.Second)
	if !ok {
		t.Fatal("no TurnStarted observed on the sub-loop via the subscription")
	}
	if ts.LoopID != subLoopID {
		t.Errorf("TurnStarted LoopID = %v, want sub-loop %v", ts.LoopID, subLoopID)
	}
	if ts.Cause.CommandID != id {
		t.Errorf("TurnStarted Cause.CommandID = %v, want returned id %v", ts.Cause.CommandID, id)
	}
	if ts.Cause.Agency != identity.AgencyMachine {
		t.Errorf("TurnStarted Cause.Agency = %v, want AgencyMachine", ts.Cause.Agency)
	}
}

// TestSubmitToLoopUnknownLoop asserts submitToLoop fails secure with
// *SessionError{SessionLoopNotFound} (and returns a zero id, nothing sent) when the
// target loop id resolves to no registry entry.
func TestSubmitToLoopUnknownLoop(t *testing.T) {
	t.Parallel()
	s, cmds, _ := sessionWithFakeLoop() // Commands never read: a send would block forever

	id, err := s.submitToLoop(context.Background(), mustUUID(), []content.Block{&content.TextBlock{Text: "x"}}, identity.AgencyMachine)
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionLoopNotFound {
		t.Fatalf("submitToLoop err = %v, want *SessionError{SessionLoopNotFound}", err)
	}
	if !id.IsZero() {
		t.Errorf("submitToLoop id = %v on unknown-loop path, want zero", id)
	}
	select {
	case cmd := <-cmds:
		t.Fatalf("submitToLoop sent %T on an unknown-loop path, want no command", cmd)
	default:
	}
}
