package command

// Interrupt cancels the running turn. Ack receives true if a turn was cancelled,
// false if idle or the session is already shutting down.
// Ack is required and must be non-nil.
type Interrupt struct {
	Ack chan<- bool
}

func (Interrupt) isCommand() {}
