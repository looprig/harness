package command

import "github.com/inventivepotter/urvi/internal/uuid"

// Route addresses a command to a specific point in the session topology. It
// selects the loop (and, for finer-grained routing, the turn/step/tool call) a
// command targets. The submit/cancel commands carry it so a multi-loop session
// can route a CancelQueuedInput to the loop that holds the queued input; the
// gate commands (Approve/Deny/ProvideUserInput) carry it alongside their legacy
// CallID so routing can migrate from CallID-keyed to Route-keyed without breaking
// existing callers.
//
// A zero field means "unspecified at this granularity": e.g. a Route with only
// LoopID set addresses a loop without naming a turn. The loop resolves whatever
// finer key it needs from its own state (CancelQueuedInput resolves by InputID,
// the gates by CallID), so Route is the addressing envelope, not the match key.
type Route struct {
	SessionID  uuid.UUID
	LoopID     uuid.UUID
	TurnID     uuid.UUID
	StepID     uuid.UUID
	ToolCallID uuid.UUID
}
