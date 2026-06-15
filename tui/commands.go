package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/llm"
)

// interruptTimeout bounds an Interrupt ack so Update never waits on a wedged session.
const interruptTimeout = 2 * time.Second

// reopenTimeout bounds a /clear reopen so a slow agent construction cannot hang.
const reopenTimeout = 5 * time.Second

// closeTimeout bounds a best-effort close so a hung session cannot wedge quit.
const closeTimeout = 5 * time.Second

// readNext pulls exactly one event from r and maps it to a tea.Msg: io.EOF →
// streamEOFMsg, any other error → streamErrMsg, otherwise eventMsg. Re-dispatch
// it after each event to drive the stream forward without a drain goroutine.
func readNext(r *llm.StreamReader[event.Event]) tea.Cmd {
	return func() tea.Msg {
		ev, err := r.Next()
		switch {
		case errors.Is(err, io.EOF):
			return streamEOFMsg{}
		case err != nil:
			return streamErrMsg{err: err}
		default:
			return eventMsg{ev: ev}
		}
	}
}

// interruptTurn issues a bounded Interrupt and reports the result, so Update
// never blocks on the session's interrupt ack.
func interruptTurn(ctx context.Context, agent Agent) tea.Cmd {
	return func() tea.Msg {
		ictx, cancel := context.WithTimeout(ctx, interruptTimeout)
		defer cancel()
		cancelled, err := agent.Interrupt(ictx)
		return interruptResultMsg{cancelled: cancelled, err: err}
	}
}

// reopenAgent builds a fresh agent for /clear under a bounded context. It only
// constructs the agent; the swap and the old agent's shutdown happen on the
// Update loop in reopenResultMsg, so no two goroutines ever touch m.agent.
func reopenAgent(ctx context.Context, open OpenAgent) tea.Cmd {
	return func() tea.Msg {
		rctx, cancel := context.WithTimeout(ctx, reopenTimeout)
		defer cancel()
		a, err := open(rctx)
		return reopenResultMsg{agent: a, err: err}
	}
}

// closeAgent closes agent best-effort under a bounded Background context (not
// the app context, which may already be cancelled on quit), so a hung session
// cannot wedge the exit.
func closeAgent(agent Agent) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()
		_ = agent.Close(ctx) // best-effort; Close is idempotent, nothing actionable at the UI
		return nil
	}
}

// printPayload flattens every action's Lines, in order, into a single string
// joined by "\n". Each action's trailing "" line therefore yields the blank-line
// separation between entries in scrollback. It is pure: the input actions and
// their Lines are read-only, and a fresh slice is built (never appended into a
// caller's backing array). No actions yields "".
func printPayload(actions []printAction) string {
	var all []string
	for _, a := range actions {
		all = append(all, a.Lines...)
	}
	return strings.Join(all, "\n")
}

// printToScrollback emits the assembled payload to the native terminal scrollback
// via tea.Println. It returns nil (a no-op command) when there is nothing to print,
// so the caller can dispatch it unconditionally.
func printToScrollback(actions []printAction) tea.Cmd {
	if len(actions) == 0 {
		return nil
	}
	return tea.Println(printPayload(actions))
}
