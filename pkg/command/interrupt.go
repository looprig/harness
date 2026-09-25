package command

import sessionwire "github.com/looprig/core/sessionwire/v1"

const (
	CommandInterrupt CommandName  = "Interrupt"
	InterruptAck     CommandField = "Ack"
)

// Interrupt cancels the running turn. Ack receives true if a turn was cancelled,
// false if idle or the session is already shutting down.
// Ack is required and must be non-nil.
type Interrupt struct {
	Header
	// Principal is who stopped the session, when a Host admitted one.
	Principal *sessionwire.Principal `json:"principal,omitzero"`
	Ack       chan<- bool            `json:"-"` // live reply channel; no JSON representation
}

func (Interrupt) isCommand() {}

// Validate checks that all required fields are non-nil.
func (c Interrupt) Validate() error {
	if c.Ack == nil {
		return &InvalidCommandError{Command: CommandInterrupt, Field: InterruptAck}
	}
	return nil
}
