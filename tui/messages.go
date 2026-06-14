package tui

import (
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
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

// systemReadyMsg triggers the initial RoleSystem "ready" row at startup.
type systemReadyMsg struct{}

// queuedInput is a submission made while a turn was Running. DisplayIndex is the
// index in messages of the RoleUser row shown for it, so the renderer can mark
// exactly that row "(queued)" and an interrupt can remove exactly those rows.
type queuedInput struct {
	Blocks       []content.Block
	DisplayIndex int
}
