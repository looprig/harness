package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/command"
	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/hub"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// sessionWithHubAndFakeLoop builds a Session with a REAL hub (so quiescence
// transitions run) and a fake parent loop whose Commands channel the test reads and
// whose Done channel the test controls. The fake loop lets a test drive
// deliverSubagentResult (now fire-and-forget) without a full loop: the test reads the
// routed command to confirm it landed, and drives any quiescence transitions by
// publishing events directly through the session.
func sessionWithHubAndFakeLoop() (s *Session, cmds chan command.Command, done chan struct{}) {
	cmds = make(chan command.Command)
	done = make(chan struct{})
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	id := mustUUID()
	primaryLoopID := mustUUID()
	s = &Session{
		SessionID:     id,
		hub:           hub.New(id),
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops: map[uuid.UUID]*loopHandle{
			primaryLoopID: {backend: &loop.Loop{Commands: cmds, Done: done}},
		},
		primaryLoopID: primaryLoopID,
		newID:         uuid.New,
	}
	return s, cmds, done
}

// TestSubagentHandBackWakeReleaseViaTurnStarted drives the synchronous-spawn
// quiescence accounting through the session + hub directly: a {wake} token taken at
// (would-be) spawn keeps the session Active even while the parent loop is idle; the
// simulated hand-back TurnStarted carrying Cause.LoopID releases the token, and
// because the same event adds the parent's {loop} key, active never dips to empty
// across the handoff — SessionIdle fires only after the parent itself goes idle.
func TestSubagentHandBackWakeReleaseViaTurnStarted(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	subagentLoopID := mustUUID()
	parentLoopID := s.primaryLoopID

	// Spawn-time guard: take the {wake} token. The session was idle, so this crosses
	// the Idle->Active edge and derives SessionActive.
	s.expectTurn(context.Background(), subagentLoopID)
	if !drainFor[event.SessionActive](t, sub) {
		t.Fatal("expectTurn did not derive SessionActive")
	}

	// While the {wake} token is held, WaitIdle must block even though no loop is busy.
	blockCtx, blockCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer blockCancel()
	if err := s.WaitIdle(blockCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitIdle with a {wake} token held = %v, want DeadlineExceeded", err)
	}

	// Simulate the hand-back landing in the parent and starting a turn: publish a
	// TurnStarted carrying Cause.LoopID == subagentLoopID. The hub removes
	// {wake, subagentLoopID} and adds {loop, parentLoopID} in the same step, so active
	// stays non-empty (no false SessionIdle).
	tid := mustUUID()
	if err := s.PublishEvent(context.Background(), event.TurnStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{
				SessionID: s.SessionID,
				LoopID:    parentLoopID,
				TurnID:    tid,
			},
			Cause: identity.Cause{
				CommandID:   mustUUID(),
				Coordinates: identity.Coordinates{LoopID: subagentLoopID},
			},
		},
	}); err != nil {
		t.Fatalf("PublishEvent(TurnStarted): %v", err)
	}

	// Still Active (the parent is now the busy loop): WaitIdle still blocks.
	blockCtx2, blockCancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer blockCancel2()
	if err := s.WaitIdle(blockCtx2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitIdle after hand-back TurnStarted = %v, want DeadlineExceeded (parent busy)", err)
	}

	// The parent goes idle: now active empties and SessionIdle fires.
	if err := s.PublishEvent(context.Background(), event.LoopIdle{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: s.SessionID, LoopID: parentLoopID}},
	}); err != nil {
		t.Fatalf("PublishEvent(LoopIdle): %v", err)
	}
	if !drainFor[event.SessionIdle](t, sub) {
		t.Fatal("parent LoopIdle after wake-release did not derive SessionIdle")
	}
	if err := s.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle after parent idle = %v, want nil", err)
	}
}

// TestSubagentResultNeverRejectedReleasesWakeViaInputCancelled replaces the old
// reject-release test: a SubagentResult can no longer be rejected, so its {wake} token
// can no longer leak via an off-publish reconciliation. This drives the END-TO-END
// event path on a REAL loop: a SubagentResult delivered while the loop is BUSY queues
// (never rejected, even with a full inbox); interrupting the running turn ends it and
// makes returnQueuedInbox emit event.InputCancelled carrying Cause.LoopID ==
// fromLoopID, which releases the {wake} token ON THE PUBLISH PATH (NOT cancelExpectTurn).
// Quiescence then reaches SessionIdle and WaitIdle returns.
func TestSubagentResultNeverRejectedReleasesWakeViaInputCancelled(t *testing.T) {
	t.Parallel()
	// blockUntilCancel keeps the turn running so the SubagentResult queues behind it.
	s, err := New(context.Background(), cfg(&stubLLM{blockUntilCancel: true}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	subagentLoopID := mustUUID()

	// Occupy the loop with a running turn (Submit is fire-and-forget; the provider
	// blocks so the turn stays running).
	if _, err := s.Submit(context.Background(), textBlocks("occupy")); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Wait until the loop is actually running (TurnStarted observed on the fan-in).
	if !drainFor[event.TurnStarted](t, sub) {
		t.Fatal("occupying turn never started")
	}

	// Spawn-time guard: take the {wake, subagentLoopID} token. The session is already
	// Active (a turn runs), so this just adds the token; WaitIdle must block on it.
	s.expectTurn(context.Background(), subagentLoopID)

	// Deliver the SubagentResult: the loop is busy, so it QUEUES (never rejected). Its
	// {wake} token is NOT released yet — it rides the eventual resolution event.
	if err := s.deliverSubagentResult(context.Background(), s.primaryLoopID, subagentLoopID, textBlocks("subagent output")); err != nil {
		t.Fatalf("deliverSubagentResult: %v", err)
	}

	// Interrupt the running turn: it ends TurnInterrupted, and returnQueuedInbox emits
	// InputCancelled for the queued SubagentResult, carrying Cause.LoopID ==
	// subagentLoopID — the publish-path release of the {wake} token.
	if _, err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	// Observe the InputCancelled that releases the token (the wake release rides THIS
	// event, not cancelExpectTurn), then SessionIdle once active empties.
	sawRelease := false
	deadline := time.After(2 * time.Second)
	for !sawRelease {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before the InputCancelled wake-release")
			}
			if ic, ok := ev.(event.InputCancelled); ok && ic.Cause.LoopID == subagentLoopID {
				sawRelease = true
			}
		case <-deadline:
			t.Fatal("no InputCancelled carrying the subagent loop id (wake token leaked off the event path)")
		}
	}

	// With the {wake} token released by the event and the loop idle, quiescence reaches
	// SessionIdle and WaitIdle returns.
	if err := s.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle after event-path wake-release = %v, want nil", err)
	}
}

// TestSubagentResultQueuedDoesNotReleaseOffPublishPath proves the {wake} token is NOT
// released off the publish path: after a SubagentResult queues (fire-and-forget, no
// disposition), the token is still held — WaitIdle blocks — until the resulting event
// (here a simulated TurnStarted carrying Cause.LoopID) releases it on the publish
// path. This is the inverse guarantee to the InputCancelled release above.
func TestSubagentResultQueuedDoesNotReleaseOffPublishPath(t *testing.T) {
	t.Parallel()
	s, cmds, done := sessionWithHubAndFakeLoop()
	defer close(done)

	subagentLoopID := mustUUID()
	s.expectTurn(context.Background(), subagentLoopID)

	// Fake parent loop: just receive the routed SubagentResult (fire-and-forget; no
	// reply). Confirm it carried the CHILD via Cause.LoopID == subagentLoopID and the
	// PARENT delivery target via the embedded Coordinates.LoopID == primaryLoopID.
	routed := make(chan command.SubagentResult, 1)
	go func() {
		if sr, ok := (<-cmds).(command.SubagentResult); ok {
			routed <- sr
		}
	}()

	if err := s.deliverSubagentResult(context.Background(), s.primaryLoopID, subagentLoopID, textBlocks("output")); err != nil {
		t.Fatalf("deliverSubagentResult: %v", err)
	}
	select {
	case sr := <-routed:
		if sr.Cause.LoopID != subagentLoopID {
			t.Errorf("routed SubagentResult.Cause.LoopID = %v, want %v (the CHILD)", sr.Cause.LoopID, subagentLoopID)
		}
		if sr.LoopID != s.primaryLoopID {
			t.Errorf("routed SubagentResult.LoopID = %v, want %v (the PARENT delivery target)", sr.LoopID, s.primaryLoopID)
		}
	case <-time.After(time.Second):
		t.Fatal("SubagentResult was not routed to the parent loop")
	}

	// The hand-back released nothing off the publish path: the token is still held, so
	// WaitIdle blocks. (In production the resulting TurnStarted/TurnFoldedInto/
	// InputCancelled carrying Cause.LoopID releases it on the publish path.)
	blockCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.WaitIdle(blockCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitIdle after queued hand-back = %v, want DeadlineExceeded (token still held)", err)
	}

	// Releasing it on the publish path (the hand-back's TurnStarted) then idles it.
	if err := s.PublishEvent(context.Background(), event.TurnStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{
				SessionID: s.SessionID,
				LoopID:    s.primaryLoopID,
				TurnID:    mustUUID(),
			},
			Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: subagentLoopID}},
		},
	}); err != nil {
		t.Fatalf("PublishEvent(TurnStarted): %v", err)
	}
	if err := s.PublishEvent(context.Background(), event.LoopIdle{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: s.SessionID, LoopID: s.primaryLoopID}},
	}); err != nil {
		t.Fatalf("PublishEvent(LoopIdle): %v", err)
	}
	if err := s.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle after publish-path release = %v, want nil", err)
	}
}
