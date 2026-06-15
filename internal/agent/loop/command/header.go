package command

import "github.com/inventivepotter/urvi/internal/uuid"

// Header is the correlation/idempotency metadata embedded in every command.
// The sender stamps the fields before sending the command (the session does this
// via newCommandID); zero-valued fields mean the sender supplied none.
type Header struct {
	ID          uuid.UUID // fresh per command instance, stamped by the sender before send
	CausationID uuid.UUID // message-ID of the cause; zero = root (user-initiated)
}

// CommandHeader is promoted onto every command that embeds Header.
func (h Header) CommandHeader() Header { return h }
