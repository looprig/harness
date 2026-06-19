package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/session/hub"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// sessionWithHubAndFakeLoop builds an AgentSession with a REAL hub (so quiescence
// transitions run) and a fake parent loop whose Commands channel the test reads and
// whose Done channel the test controls. The fake loop lets a test drive
// deliverSubagentResult without a full loop's reject machinery: the test reads the
// routed command and replies whatever Disposition it wants on the command's Ack.
func sessionWithHubAndFakeLoop() (s *AgentSession, cmds chan command.Command, done chan struct{}) {
	cmds = make(chan command.Command)
	done = make(chan struct{})
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	id := mustUUID()
	primaryLoopID := mustUUID()
	s = &AgentSession{
		SessionID:     id,
		hub:           hub.New(id),
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops: map[uuid.UUID]*loopHandle{
			primaryLoopID: {loop: &loop.Loop{Commands: cmds, Done: done}},
		},
		primaryLoopID: primaryLoopID,
		newID:         uuid.New,
	}
	return s, cmds, done
}

// TestSubagentHandBackWakeReleaseViaTurnStarted drives the synchronous-spawn
// quiescence accounting through the session + hub directly: a {wake} token taken at
// (would-be) spawn keeps the session Active even while the parent loop is idle; the
// simulated hand-back TurnStarted carrying TriggeredByLoopID releases the token, and
// because the same event adds the parent's {loop} key, active never dips to empty
// across the handoff — SessionIdle fires only after the parent itself goes idle.
func TestSubagentHandBackWakeReleaseViaTurnStarted(t *testing.T) {
	t.Parallel()
	s, err := NewAgent(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
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
	// TurnStarted carrying TriggeredByLoopID == subagentLoopID. The hub removes
	// {wake, subagentLoopID} and adds {loop, parentLoopID} in the same step, so active
	// stays non-empty (no false SessionIdle).
	tid := mustUUID()
	if err := s.PublishEvent(context.Background(), event.TurnStarted{
		Header: event.Header{
			SessionID:         s.SessionID,
			LoopID:            parentLoopID,
			TurnID:            tid,
			CausationID:       mustUUID(),
			TriggeredByLoopID: subagentLoopID,
		},
		InputID: mustUUID(),
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
		Header: event.Header{SessionID: s.SessionID, LoopID: parentLoopID},
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

// TestSubagentResultRejectReleasesWakeToken drives the off-publish-path release: a
// SubagentResult that the parent loop rejects produces no event, so its {wake} token
// would leak — deliverSubagentResult must release it via cancelExpectTurn after
// reading the TurnRejected disposition. With the token the only outstanding work,
// that release empties active and derives SessionIdle.
func TestSubagentResultRejectReleasesWakeToken(t *testing.T) {
	t.Parallel()
	s, cmds, done := sessionWithHubAndFakeLoop()
	defer close(done)

	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	subagentLoopID := mustUUID()

	// Spawn-time guard: SessionActive on the Idle->Active edge.
	s.expectTurn(context.Background(), subagentLoopID)
	if !drainFor[event.SessionActive](t, sub) {
		t.Fatal("expectTurn did not derive SessionActive")
	}

	// Parent loop replies TurnRejected to the routed SubagentResult. Run the fake
	// loop's reply in a goroutine so deliverSubagentResult's send + ack-wait proceed.
	routed := make(chan command.SubagentResult, 1)
	go func() {
		c := <-cmds
		sr, ok := c.(command.SubagentResult)
		if !ok {
			return
		}
		routed <- sr
		sr.Ack <- command.TurnRejected{Reason: command.RejectShuttingDown}
	}()

	d, err := s.deliverSubagentResult(context.Background(), s.primaryLoopID, subagentLoopID, textBlocks("subagent output"))
	if err != nil {
		t.Fatalf("deliverSubagentResult: %v", err)
	}
	if _, ok := d.(command.TurnRejected); !ok {
		t.Fatalf("disposition = %T, want command.TurnRejected", d)
	}

	// The routed command carried FromLoopID == subagentLoopID (so the loop would have
	// stamped TriggeredByLoopID on any event it produced).
	select {
	case sr := <-routed:
		if sr.FromLoopID != subagentLoopID {
			t.Errorf("routed SubagentResult.FromLoopID = %v, want %v", sr.FromLoopID, subagentLoopID)
		}
	case <-time.After(time.Second):
		t.Fatal("SubagentResult was not routed to the parent loop")
	}

	// The rejected hand-back produced no event; deliverSubagentResult released the
	// {wake} token via cancelExpectTurn, emptying active -> SessionIdle.
	if !drainFor[event.SessionIdle](t, sub) {
		t.Fatal("TurnRejected hand-back did not release the wake token (no SessionIdle)")
	}
	if err := s.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle after reject-release = %v, want nil", err)
	}
}

// TestSubagentResultStartedQueuedDoesNotReleaseOffPublishPath proves the inverse: a
// non-rejected disposition (Started/InputQueued) does NOT release the {wake} token off
// the publish path — that release rides the resulting event (TurnStarted/
// TurnFoldedInto/InputCancelled) instead. So after a Started disposition the token is
// still held and WaitIdle still blocks.
func TestSubagentResultStartedQueuedDoesNotReleaseOffPublishPath(t *testing.T) {
	t.Parallel()
	s, cmds, done := sessionWithHubAndFakeLoop()
	defer close(done)

	subagentLoopID := mustUUID()
	s.expectTurn(context.Background(), subagentLoopID)

	// Parent loop replies Started (a turn was created).
	go func() {
		c := <-cmds
		sr, ok := c.(command.SubagentResult)
		if !ok {
			return
		}
		sr.Ack <- command.Started{TurnID: mustUUID(), InputID: sr.CommandHeader().ID}
	}()

	d, err := s.deliverSubagentResult(context.Background(), s.primaryLoopID, subagentLoopID, textBlocks("output"))
	if err != nil {
		t.Fatalf("deliverSubagentResult: %v", err)
	}
	if _, ok := d.(command.Started); !ok {
		t.Fatalf("disposition = %T, want command.Started", d)
	}

	// A Started disposition must NOT release the token off the publish path: the token
	// is still held, so WaitIdle blocks. (In production the resulting TurnStarted
	// carrying TriggeredByLoopID releases it on the publish path.)
	blockCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.WaitIdle(blockCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitIdle after Started hand-back = %v, want DeadlineExceeded (token still held)", err)
	}

	// Releasing it on the publish path (the hand-back's TurnStarted) then idles it.
	if err := s.PublishEvent(context.Background(), event.TurnStarted{
		Header: event.Header{
			SessionID:         s.SessionID,
			LoopID:            s.primaryLoopID,
			TurnID:            mustUUID(),
			TriggeredByLoopID: subagentLoopID,
		},
	}); err != nil {
		t.Fatalf("PublishEvent(TurnStarted): %v", err)
	}
	if err := s.PublishEvent(context.Background(), event.LoopIdle{
		Header: event.Header{SessionID: s.SessionID, LoopID: s.primaryLoopID},
	}); err != nil {
		t.Fatalf("PublishEvent(LoopIdle): %v", err)
	}
	if err := s.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle after publish-path release = %v, want nil", err)
	}
}
