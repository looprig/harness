package foreignloop

import (
	"context"

	"github.com/ciram-co/looprig/pkg/command"
)

// runTurn drives one foreign turn from a UserInput submit. D1 stub: the real
// turn machinery (TurnStarted-before-spawn, stream drain, transcript commit,
// terminals, interrupt) lands in D2–D4. It returns whether the actor must EXIT
// (a Shutdown that arrived mid-turn); the stub never exits.
func (l *Loop) runTurn(_ context.Context, _ command.UserInput) (exit bool) {
	return false
}
