package tui

import (
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
