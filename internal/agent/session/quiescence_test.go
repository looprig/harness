package session

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
)

// drainSub reads from sub until the predicate is satisfied (returns true) or a
// timeout elapses. It returns the ordered slice of kinds it observed for the events
// it saw, plus whether the predicate fired.
func drainSub(t *testing.T, sub interface{ Events() <-chan event.Event }, want func(ev event.Event) bool) ([]event.Event, bool) {
	t.Helper()
	var seen []event.Event
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return seen, false
			}
			seen = append(seen, ev)
			if want(ev) {
				return seen, true
			}
		case <-deadline:
			return seen, false
		}
	}
}

// TestEndToEndQuiescence proves the live hub wiring closes the loop: after Invoke
// completes, WaitIdle returns (the loop emitted LoopIdle -> active empties ->
// SessionIdle), and a subscriber sees the ordered live stream
// SessionActive -> TurnStarted -> ... -> LoopIdle -> SessionIdle. For the single
// primary (synchronous) loop, quiescence is exactly the primary loop going idle.
func TestEndToEndQuiescence(t *testing.T) {
	t.Parallel()
	s, err := NewAgent(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("hi")}}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	// Collect the live stream concurrently so a slow Invoke drain never blocks the
	// loop's publish (the egress is bounded but we drain it promptly).
	var (
		mu   sync.Mutex
		seen []event.Event
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case ev, ok := <-sub.Events():
				if !ok {
					return
				}
				mu.Lock()
				seen = append(seen, ev)
				stop := isSessionIdle(ev)
				mu.Unlock()
				if stop {
					return
				}
			case <-time.After(2 * time.Second):
				return
			}
		}
	}()

	term, err := s.Invoke(context.Background(), textBlocks("go"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if _, ok := term.(event.TurnDone); !ok {
		t.Fatalf("Invoke terminal = %T, want event.TurnDone", term)
	}

	// After Invoke returns, the loop goes idle (LoopIdle) and the session reaches
	// SessionIdle; WaitIdle must return nil.
	waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.WaitIdle(waitCtx); err != nil {
		t.Fatalf("WaitIdle after Invoke = %v, want nil", err)
	}

	<-done
	mu.Lock()
	got := append([]event.Event(nil), seen...)
	mu.Unlock()

	// Ordering: the derived session edge follows its triggering loop event (the hub
	// delivers ev, then the derived post event). So TurnStarted precedes its derived
	// SessionActive, and LoopIdle precedes its derived SessionIdle; the terminal
	// (TurnDone) precedes LoopIdle.
	idx := func(match func(event.Event) bool) int {
		for i, ev := range got {
			if match(ev) {
				return i
			}
		}
		return -1
	}
	active := idx(func(ev event.Event) bool { _, ok := ev.(event.SessionActive); return ok })
	started := idx(func(ev event.Event) bool { _, ok := ev.(event.TurnStarted); return ok })
	turnDone := idx(func(ev event.Event) bool { _, ok := ev.(event.TurnDone); return ok })
	loopIdle := idx(func(ev event.Event) bool { _, ok := ev.(event.LoopIdle); return ok })
	sessionIdle := idx(func(ev event.Event) bool { return isSessionIdle(ev) })

	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"SessionActive present", active >= 0},
		{"TurnStarted present", started >= 0},
		{"TurnDone present", turnDone >= 0},
		{"LoopIdle present", loopIdle >= 0},
		{"SessionIdle present", sessionIdle >= 0},
		{"TurnStarted before its derived SessionActive", started >= 0 && active >= 0 && started < active},
		{"TurnDone before LoopIdle", turnDone >= 0 && loopIdle >= 0 && turnDone < loopIdle},
		{"LoopIdle before its derived SessionIdle", loopIdle >= 0 && sessionIdle >= 0 && loopIdle < sessionIdle},
	} {
		if !c.ok {
			t.Errorf("ordering check failed: %s (order: %s)", c.name, kindsOf(got))
		}
	}
}

func isSessionIdle(ev event.Event) bool { _, ok := ev.(event.SessionIdle); return ok }

// kindsOf renders the event kinds in order for a readable failure.
func kindsOf(evs []event.Event) string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		switch ev.(type) {
		case event.SessionActive:
			out = append(out, "SessionActive")
		case event.SessionIdle:
			out = append(out, "SessionIdle")
		case event.TurnStarted:
			out = append(out, "TurnStarted")
		case event.TurnDone:
			out = append(out, "TurnDone")
		case event.LoopIdle:
			out = append(out, "LoopIdle")
		case event.TokenDelta:
			out = append(out, "TokenDelta")
		case event.StepDone:
			out = append(out, "StepDone")
		default:
			out = append(out, "?")
		}
	}
	return joinKinds(out)
}

func joinKinds(ss []string) string {
	s := ""
	for i, k := range ss {
		if i > 0 {
			s += ","
		}
		s += k
	}
	return s
}

func textBlocks(s string) []content.Block {
	return []content.Block{&content.TextBlock{Text: s}}
}

// chainStubLLM streams one text chunk per Stream call (one no-tool step per turn ->
// TurnDone), and runs an optional per-call hook at the START of each Stream so a test
// can queue input while a turn is running. It is the session-test analogue of the
// loop package's scriptedLLM onStreamN hook.
type chainStubLLM struct {
	mu     sync.Mutex
	calls  int
	onCall map[int]func()
	text   string
}

func (c *chainStubLLM) Invoke(context.Context, llm.Request) (*llm.Response, error) {
	return nil, io.EOF
}

func (c *chainStubLLM) Stream(ctx context.Context, _ llm.Request) (*llm.StreamReader[content.Chunk], error) {
	c.mu.Lock()
	n := c.calls
	c.calls++
	hook := c.onCall[n]
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	i := 0
	next := func() (content.Chunk, error) {
		if i == 0 {
			i++
			return &content.TextChunk{Text: c.text}, nil
		}
		return nil, io.EOF
	}
	return llm.NewStreamReader(next, nil), nil
}

// TestChainedTurnsEmitNoLoopIdleBetween proves the running->running transition: when
// turn N completes normally with queued input, the actor chains directly into turn
// N+1 WITHOUT emitting LoopIdle between them. Across the whole chain the subscriber
// sees exactly ONE SessionActive (the first TurnStarted out of idle), two
// TurnStarteds, two TurnDones, exactly ONE LoopIdle (only when the loop finally
// parks), and exactly ONE SessionIdle. No LoopIdle appears between the first TurnDone
// and the second TurnStarted.
func TestChainedTurnsEmitNoLoopIdleBetween(t *testing.T) {
	t.Parallel()
	client := &chainStubLLM{text: "ok", onCall: map[int]func(){}}
	s, err := NewAgent(context.Background(), cfg(client))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	queuedID := mustUUID()
	// At the START of turn 1's only step, queue a second AllowFold UserInput. The first
	// turn is running, so it is accepted into the inbox; on the normal terminal the
	// actor chains directly into turn 2 from it (running->running, no LoopIdle).
	client.onCall[0] = func() {
		l, _ := s.loopFor(s.primaryLoopID)
		// The unbuffered send completes only once the actor has RECEIVED the command,
		// so when this returns the queued input is guaranteed in the actor's hands
		// (appended to the inbox) before turn 1's terminal — no ack needed now that the
		// outcome (InputQueued) is published to the fan-in rather than replied.
		select {
		case l.Commands <- command.UserInput{Header: command.Header{ID: queuedID}, Mode: command.AllowFold, Blocks: textBlocks("turn2")}:
		case <-l.Done:
		}
	}

	// Invoke turn 1 (StartOnly). It completes, then the actor chains into turn 2.
	if _, err := s.Invoke(context.Background(), textBlocks("turn1")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// Collect until SessionIdle (the whole chain is at rest).
	got, ok := drainSub(t, sub, isSessionIdle)
	if !ok {
		t.Fatalf("never reached SessionIdle; saw: %s", kindsOf(got))
	}

	var sessionActive, turnStarted, turnDone, loopIdle, sessionIdle int
	// firstTurnDoneIdx / secondTurnStartedIdx bracket the chain handoff; no LoopIdle
	// may appear strictly between them.
	firstTurnDoneIdx, secondTurnStartedIdx := -1, -1
	for i, ev := range got {
		switch ev.(type) {
		case event.SessionActive:
			sessionActive++
		case event.TurnStarted:
			turnStarted++
			if turnStarted == 2 {
				secondTurnStartedIdx = i
			}
		case event.TurnDone:
			turnDone++
			if turnDone == 1 {
				firstTurnDoneIdx = i
			}
		case event.LoopIdle:
			loopIdle++
		case event.SessionIdle:
			sessionIdle++
		}
	}

	if turnStarted != 2 {
		t.Errorf("TurnStarted count = %d, want 2 (chained turns); order: %s", turnStarted, kindsOf(got))
	}
	if turnDone != 2 {
		t.Errorf("TurnDone count = %d, want 2; order: %s", turnDone, kindsOf(got))
	}
	if sessionActive != 1 {
		t.Errorf("SessionActive count = %d, want 1 (one Idle->Active edge across the chain); order: %s", sessionActive, kindsOf(got))
	}
	if loopIdle != 1 {
		t.Errorf("LoopIdle count = %d, want 1 (only when the loop finally parks); order: %s", loopIdle, kindsOf(got))
	}
	if sessionIdle != 1 {
		t.Errorf("SessionIdle count = %d, want 1 (one Active->Idle edge); order: %s", sessionIdle, kindsOf(got))
	}
	// No LoopIdle between the first TurnDone and the second TurnStarted.
	if firstTurnDoneIdx >= 0 && secondTurnStartedIdx > firstTurnDoneIdx {
		for i := firstTurnDoneIdx + 1; i < secondTurnStartedIdx; i++ {
			if _, ok := got[i].(event.LoopIdle); ok {
				t.Errorf("LoopIdle emitted between chained turns (idx %d); order: %s", i, kindsOf(got))
			}
		}
	}
}
