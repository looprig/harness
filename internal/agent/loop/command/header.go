package command

import "github.com/inventivepotter/urvi/internal/uuid"

// Header is the correlation/idempotency metadata embedded in every command.
type Header struct {
	ID          uuid.UUID // fresh per command instance (uuid.New at construction)
	CausationID uuid.UUID // message-ID of the cause; zero = root (user-initiated)
}

// CommandHeader is promoted onto every command that embeds Header.
func (h Header) CommandHeader() Header { return h }
