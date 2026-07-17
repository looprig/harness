package loopruntime

import (
	"context"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

// newNamedLoop builds a loop whose resolved config carries agent — the same
// bound.Name() the session stamps onto the loop's LoopStarted. newLoop (loop_test.go)
// hardcodes an unnamed config, so the attribution assertions below need their own
// constructor rather than a parameter no other test wants.
func newNamedLoop(t *testing.T, agent identity.AgentName) (*Loop, *recordingPublisher, uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rec := &recordingPublisher{}
	loopID := mustID(t)
	l, err := newWithConfig(ctx, mustID(t), loopID, Provenance{}, rec, runtimeConfig{
		Client:       &fakeLLM{chunks: []content.Chunk{textChunk("hi")}},
		Model:        testModel(),
		AgentName:    agent,
		DrainTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newWithConfig: %v", err)
	}
	return l, rec, loopID
}

// awaitLoopIdle drives one turn to its terminal and returns the LoopIdle the
// running->idle transition publishes.
func awaitLoopIdle(t *testing.T, l *Loop, rec *recordingPublisher) event.LoopIdle {
	t.Helper()
	cmdID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	l.Commands <- command.UserInput{Header: command.Header{CommandID: cmdID}}
	if _, ok := awaitReply(t, rec, cmdID).(event.TurnStarted); !ok {
		t.Fatal("submit did not start a turn")
	}
	if _, ok := drainToTerminal(t, rec).(event.TurnDone); !ok {
		t.Fatal("terminal != TurnDone")
	}
	blockUntilEvents(t, rec, func(events []event.Event) bool {
		for _, ev := range events {
			if _, ok := ev.(event.LoopIdle); ok {
				return true
			}
		}
		return false
	})
	for _, ev := range rec.events() {
		if idle, ok := ev.(event.LoopIdle); ok {
			return idle
		}
	}
	t.Fatal("no LoopIdle published")
	return event.LoopIdle{}
}

// TestLoopIdleStampsAgentName pins the loop's running->idle announcement to the
// loop's immutable attribution name. A consumer that keys off the idle boundary
// selects loops BY AGENT — an external tool catalog adopting its next generation
// only for a named agent, say — and Header.AgentName is the only thing on the
// event that can answer "which agent parked?". Without it such a selector silently
// matches nothing: LoopIdle carries no error, it just never names the loop.
//
// The empty case is the other half of the contract: a plain (unnamed) loop must
// leave AgentName zero, exactly as its LoopStarted does, so the omitzero field
// stays absent from the journal rather than serializing an invented name.
func TestLoopIdleStampsAgentName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		agent identity.AgentName
		want  identity.AgentName
	}{
		{name: "named loop carries its agent", agent: identity.AgentName("operator"), want: identity.AgentName("operator")},
		{name: "plain loop leaves it zero", agent: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l, rec, _ := newNamedLoop(t, tt.agent)
			idle := awaitLoopIdle(t, l, rec)
			if got := idle.EventHeader().AgentName; got != tt.want {
				t.Errorf("LoopIdle.AgentName = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLoopIdleAgentNameMatchesLoopStarted holds the two loop-scoped attribution
// stamps to the same source. The session stamps LoopStarted from bound.Name(); the
// actor stamps LoopIdle from its own resolved config. They are different code paths
// and could drift, and a consumer that joins the two (start to learn the name, idle
// to learn the boundary) breaks silently if they ever disagree.
func TestLoopIdleAgentNameMatchesLoopStarted(t *testing.T) {
	t.Parallel()
	const agent = identity.AgentName("researcher")
	l, rec, loopID := newNamedLoop(t, agent)
	idle := awaitLoopIdle(t, l, rec)

	// The session — not the actor — publishes LoopStarted, so this reconstructs the
	// name it would stamp from the same definition and asserts the actor agrees.
	if got := idle.EventHeader().AgentName; got != agent {
		t.Fatalf("LoopIdle.AgentName = %q, want %q (the name LoopStarted is stamped with)", got, agent)
	}
	if got := idle.EventHeader().LoopID; got != loopID {
		t.Errorf("LoopIdle.LoopID = %v, want %v", got, loopID)
	}
	if !idle.EventHeader().TurnID.IsZero() {
		t.Errorf("LoopIdle.TurnID = %v, want zero (LoopIdle is loop-scoped)", idle.EventHeader().TurnID)
	}
}
