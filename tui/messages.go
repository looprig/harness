package tui

import (
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
)

// eventMsg carries one event pulled from the session-lifetime subscription.
type eventMsg struct{ ev event.Event }

// subscribedMsg carries the session-lifetime EventStream established at startup
// (and re-established on /clear). On a non-nil err the TUI cannot observe the
// session at all, so Update commits a fatal error entry; on success it stores the
// stream and starts the continuous reader.
type subscribedMsg struct {
	sub EventStream
	err error
}

// subClosedMsg signals the subscription's Events channel closed. err is the typed
// termination cause: nil for an intentional Close (e.g. a /clear swap or quit), or
// a *hub.SubscriptionLossError for a hub-forced drop (egress overflow). It is the
// continuous reader's only terminal — there is no per-turn EOF anymore.
type subClosedMsg struct{ err error }

// submitResultMsg reports the outcome of a fire-and-forget Submit. Only the error
// matters at the UI: the optimistic CommitUser row already landed in scrollback, so
// a nil err is a silent success and a non-nil err lets Update note that the send
// failed without removing the record. It is a tea.Msg.
type submitResultMsg struct{ err error }

// interruptResultMsg carries the outcome of an Interrupt call.
type interruptResultMsg struct {
	cancelled bool
	err       error
}

// reopenResultMsg carries the freshly opened replacement agent (nil on err)
// from a /clear reopen.
type reopenResultMsg struct {
	agent Agent
	err   error
}

// systemReadyMsg triggers the initial system "session ready" entry at startup.
type systemReadyMsg struct{}

// blinkMsg is the periodic live-surface animation tick, emitted by blinkTick while a
// turn is Running. Handling it ONLY advances the animation state and (while still
// Running) reschedules the next tick — it is a pure active-surface re-render and must
// NEVER flush/print to scrollback. It carries the tick time (unused at the UI) so it
// satisfies tea.Tick's func(time.Time) tea.Msg shape.
type blinkMsg time.Time
