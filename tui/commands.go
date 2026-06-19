package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// blinkInterval is the cadence of the live-surface animation tick: the streaming
// assistant dot blinks and the running tool spinner steps once per interval. ~450ms
// reads as a calm "working" pulse — fast enough to feel live, slow enough not to
// strobe or churn the render loop.
const blinkInterval = 450 * time.Millisecond

// blinkTick schedules ONE live-surface animation tick after blinkInterval, delivering
// a blinkMsg. It is a single-shot tick (tea.Tick semantics); the blinkMsg handler
// reschedules it ONLY while the turn is still Running, so the loop self-terminates at
// Idle with no orphaned timer. It never touches scrollback.
func blinkTick() tea.Cmd {
	return tea.Tick(blinkInterval, func(t time.Time) tea.Msg { return blinkMsg(t) })
}

// interruptTimeout bounds an Interrupt ack so Update never waits on a wedged session.
const interruptTimeout = 2 * time.Second

// reopenTimeout bounds a /clear reopen so a slow agent construction cannot hang.
const reopenTimeout = 5 * time.Second

// closeTimeout bounds a best-effort close so a hung session cannot wedge quit.
const closeTimeout = 5 * time.Second

// promptDispatchTimeout bounds an approve/deny/answer send so Update never blocks
// on a wedged session resolving a permission or AskUser gate. It mirrors
// interruptTimeout's shape: the dispatch is fire-and-route, so a lost or late send
// is self-healing (the next terminal event clears the prompt queue regardless).
const promptDispatchTimeout = 2 * time.Second

// subscribeCmd attaches the session-lifetime event subscription with the
// single-loop DefaultEventFilter and reports the outcome. It is the ONE event
// source for the whole session — established once at startup (batched into Init)
// and re-established after a /clear swaps the agent. Unlike the old per-turn
// reader, the returned stream spans every turn and loop; it closes only on Close,
// hub-forced loss, or hub teardown, never per turn.
func subscribeCmd(agent Agent) tea.Cmd {
	return func() tea.Msg {
		sub, err := agent.Subscribe(DefaultEventFilter(agent.PrimaryLoopID()))
		return subscribedMsg{sub: sub, err: err}
	}
}

// subNext receives exactly one event from the session subscription and maps it to
// a tea.Msg: a closed channel → subClosedMsg carrying the typed termination cause
// (nil for an intentional Close, a *hub.SubscriptionLossError for a hub-forced
// loss), otherwise eventMsg. Re-dispatch it after each event to drive the
// continuous reader forward without a drain goroutine. It NEVER EOFs per turn —
// the subscription is whole-session.
func subNext(sub EventStream) tea.Cmd {
	return func() tea.Msg {
		// Defensive: during the /clear re-subscribe window m.sub is briefly nil
		// (closed, awaiting the new subscribedMsg). A re-arm constructed from that
		// transient nil must be a no-op, not a nil-deref panic — the fresh
		// subscription's reader is started by handleSubscribed.
		if sub == nil {
			return nil
		}
		ev, ok := <-sub.Events()
		if !ok {
			return subClosedMsg{err: sub.Err()}
		}
		return eventMsg{ev: ev}
	}
}

// submitCmd sends blocks fire-and-forget via Submit under the app context and
// reports the outcome, capturing the loop-assigned InputID and echoing back the
// submitted blocks. The loop owns queueing, so there is no per-turn reader to
// install and no status branching here: Submit returns immediately once the input
// is enqueued, and the loop publishes the turn-lifecycle + content events back on
// the subscription. On success the (InputID, blocks) let handleSubmitResult record
// the submit so the queued affordance can show once the loop's InputQueued event
// arrives; the authoritative user row is committed later from the
// TurnStarted/TurnFoldedInto Message, never optimistically at submit. A non-nil err
// lets Update surface a faint, non-fatal send failure.
func submitCmd(ctx context.Context, agent Agent, blocks []content.Block) tea.Cmd {
	return func() tea.Msg {
		id, err := agent.Submit(ctx, blocks)
		return submitResultMsg{inputID: id, blocks: blocks, err: err}
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

// promptResultMsg reports the outcome of a bounded prompt dispatch (approve,
// deny, or provide-answer). Only the error matters at the UI: the optimistic-pop
// design needs no ack, so a nil err is a silent success and a non-nil err lets
// Update surface a faint failure line. It is a tea.Msg.
type promptResultMsg struct{ err error }

// approveCmd issues a bounded Approve for a pending permission gate and reports the
// result, so Update never blocks on the session resolving the gate. callID
// identifies the gate; scope is the chosen persistence breadth.
func approveCmd(ctx context.Context, agent Agent, callID uuid.UUID, scope tool.ApprovalScope) tea.Cmd {
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, promptDispatchTimeout)
		defer cancel()
		return promptResultMsg{err: agent.Approve(c, callID, scope)}
	}
}

// denyCmd issues a bounded Deny (fail-secure) for a pending permission gate and
// reports the result, so Update never blocks on the session failing it closed.
func denyCmd(ctx context.Context, agent Agent, callID uuid.UUID) tea.Cmd {
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, promptDispatchTimeout)
		defer cancel()
		return promptResultMsg{err: agent.Deny(c, callID)}
	}
}

// provideAnswerCmd issues a bounded ProvideAnswer for a pending AskUser request and
// reports the result, so Update never blocks on the session consuming the answer.
func provideAnswerCmd(ctx context.Context, agent Agent, callID uuid.UUID, answer string) tea.Cmd {
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, promptDispatchTimeout)
		defer cancel()
		return promptResultMsg{err: agent.ProvideAnswer(c, callID, answer)}
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
