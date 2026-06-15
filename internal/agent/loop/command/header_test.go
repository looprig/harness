package command

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/uuid"
)

// TestHeaderPromotedOnCommands asserts that every concrete command exposes the
// embedded Header via the promoted CommandHeader() method, and that both Header
// fields (ID and CausationID) round-trip unchanged through each command type.
func TestHeaderPromotedOnCommands(t *testing.T) {
	t.Parallel()

	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	causation, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}

	headers := []struct {
		name   string
		header Header
	}{
		{name: "id and causation set", header: Header{ID: id, CausationID: causation}},
		{name: "zero header is boundary", header: Header{}},
	}

	for _, hc := range headers {
		hc := hc
		commands := []struct {
			name string
			cmd  Command
		}{
			{name: "StartTurn", cmd: StartTurn{Header: hc.header}},
			{name: "Interrupt", cmd: Interrupt{Header: hc.header}},
			{name: "Shutdown", cmd: Shutdown{Header: hc.header}},
		}
		for _, cc := range commands {
			cc := cc
			t.Run(hc.name+"/"+cc.name, func(t *testing.T) {
				t.Parallel()
				got := cc.cmd.CommandHeader()
				if got.ID != hc.header.ID {
					t.Errorf("%T: CommandHeader().ID = %v, want %v", cc.cmd, got.ID, hc.header.ID)
				}
				if got.CausationID != hc.header.CausationID {
					t.Errorf("%T: CommandHeader().CausationID = %v, want %v", cc.cmd, got.CausationID, hc.header.CausationID)
				}
			})
		}
	}
}
