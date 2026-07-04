package loop

import (
	"context"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
)

// Backend is the narrow turn-engine contract Session drives. Both *loop.Loop and
// *foreignloop.Loop satisfy it. It is deliberately the minimal subset Session uses
// (command submission, completion signalling, committed-state snapshot) — it does
// NOT expose the native loop's internal gate/commit/drain seams, which a foreign
// loop has no analogue for. Snapshot's signature is exactly *Loop.Snapshot's.
type Backend interface {
	CommandSink() chan<- command.Command
	DoneChan() <-chan struct{}
	Snapshot(ctx context.Context) (content.AgenticMessages, event.TurnIndex, error)
}

// CommandSink exposes the command send-side so callers can depend on Backend
// rather than the concrete *Loop. It returns the existing Commands field.
func (l *Loop) CommandSink() chan<- command.Command { return l.Commands }

// DoneChan exposes the completion channel for the Backend contract.
func (l *Loop) DoneChan() <-chan struct{} { return l.Done }
