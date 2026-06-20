package command

import (
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// Header is the correlation/idempotency metadata embedded in every command.
// The sender stamps the fields before sending the command (the session does this
// via newCommandID); zero-valued fields mean the sender supplied none.
type Header struct {
	CommandID uuid.UUID       `json:"command_id,omitzero"` // fresh per command instance, stamped by the sender before send
	Cause     identity.Cause  `json:"cause,omitzero"`      // the direct cause of this command; zero = root (user-initiated)
	Agency    identity.Agency `json:"agency,omitzero"`     // who issued this command; AgencyMachine (zero) by default
}

// CommandHeader is promoted onto every command that embeds Header.
func (h Header) CommandHeader() Header { return h }
