package tui

import (
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
)

// eventMsg carries one event pulled from the active turn's stream.
type eventMsg struct{ ev event.Event }

// streamEOFMsg signals the active turn's stream is exhausted (io.EOF).
type streamEOFMsg struct{}

// streamErrMsg carries a non-EOF stream read error.
type streamErrMsg struct{ err error }

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
