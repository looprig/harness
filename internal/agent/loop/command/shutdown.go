package command

// Shutdown cancels the running turn (if any), delivers its terminal event, and
// exits the actor. Ack receives nil after clean exit, or *LoopTerminatedError
// if the loop's root context was cancelled before cleanup completed.
// Ack is required and must be non-nil.
type Shutdown struct {
	Ack chan<- error
}

func (Shutdown) isCommand() {}

// LoopTerminatedError is sent on Shutdown.Ack when the loop's root context was
// cancelled before the actor finished cleanup.
type LoopTerminatedError struct{ Cause error }

func (e *LoopTerminatedError) Error() string {
	return "loop: terminated by context: " + e.Cause.Error()
}
func (e *LoopTerminatedError) Unwrap() error { return e.Cause }
