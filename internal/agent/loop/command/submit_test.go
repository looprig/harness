package command_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/uuid"
)

func newID(t *testing.T) uuid.UUID {
	t.Helper()
	u, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return u
}

// TestSubmitCommandsSatisfyCommand asserts UserInput and SubagentResult are sealed
// Commands that round-trip their embedded Header. They carry NO Ctx field; the
// loop derives the turn context from its loopCtx, so the table exercises the
// fields that DO exist (Mode, FromLoopID, optional stream).
func TestSubmitCommandsSatisfyCommand(t *testing.T) {
	t.Parallel()

	headerID := newID(t)
	fromLoop := newID(t)
	ev := make(chan event.Event, 1)
	ack := make(chan command.Disposition, 1)
	ab := make(chan struct{})
	blocks := []content.Block{&content.TextBlock{Text: "hi"}}

	tests := []struct {
		name       string
		cmd        command.Command
		wantHeader uuid.UUID
	}{
		{
			name:       "UserInput AllowFold fan-in only (nil stream)",
			cmd:        command.UserInput{Header: command.Header{ID: headerID}, Blocks: blocks, Mode: command.AllowFold, Ack: ack},
			wantHeader: headerID,
		},
		{
			name:       "UserInput StartOnly with per-turn stream",
			cmd:        command.UserInput{Header: command.Header{ID: headerID}, Blocks: blocks, Mode: command.StartOnly, Events: ev, Abandoned: ab, Ack: ack},
			wantHeader: headerID,
		},
		{
			name:       "UserInput zero header is boundary",
			cmd:        command.UserInput{Ack: ack},
			wantHeader: uuid.UUID{},
		},
		{
			name:       "SubagentResult carries FromLoopID",
			cmd:        command.SubagentResult{Header: command.Header{ID: headerID}, FromLoopID: fromLoop, Blocks: blocks, Ack: ack},
			wantHeader: headerID,
		},
		{
			name:       "SubagentResult zero header is boundary",
			cmd:        command.SubagentResult{Ack: ack},
			wantHeader: uuid.UUID{},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.cmd.CommandHeader().ID; got != tt.wantHeader {
				t.Errorf("CommandHeader().ID = %v, want %v", got, tt.wantHeader)
			}
		})
	}
}

// TestInputModeValues pins the InputMode enum: AllowFold is the zero value
// (default interactive mode) and StartOnly is distinct.
func TestInputModeValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		mode command.InputMode
		want command.InputMode
	}{
		{name: "AllowFold is zero", mode: command.AllowFold, want: 0},
		{name: "StartOnly is one", mode: command.StartOnly, want: 1},
		{name: "default mode is AllowFold", mode: command.UserInput{}.Mode, want: command.AllowFold},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.mode != tt.want {
				t.Errorf("mode = %d, want %d", tt.mode, tt.want)
			}
		})
	}
}
