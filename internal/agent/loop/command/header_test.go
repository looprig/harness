package command

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/uuid"
)

func TestHeaderPromotedOnCommands(t *testing.T) {
	t.Parallel()
	id, _ := uuid.New()
	// Every concrete command must expose CommandHeader() via the embedded Header.
	var cmds = []Command{
		StartTurn{Header: Header{ID: id}},
		Interrupt{Header: Header{ID: id}},
		Shutdown{Header: Header{ID: id}},
	}
	for _, c := range cmds {
		if c.CommandHeader().ID != id {
			t.Errorf("%T: CommandHeader().ID = %v, want %v", c, c.CommandHeader().ID, id)
		}
	}
}
