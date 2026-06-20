package command_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
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
// fields that DO exist (Mode, optional stream, the two SubagentResult loop ids).
func TestSubmitCommandsSatisfyCommand(t *testing.T) {
	t.Parallel()

	headerID := newID(t)
	parentLoop := newID(t)
	childLoop := newID(t)
	ev := make(chan event.Event, 1)
	ab := make(chan struct{})
	blocks := []content.Block{&content.TextBlock{Text: "hi"}}

	tests := []struct {
		name       string
		cmd        command.Command
		wantHeader uuid.UUID
	}{
		{
			name:       "UserInput AllowFold fan-in only (nil stream)",
			cmd:        command.UserInput{Header: command.Header{CommandID: headerID}, Blocks: blocks, Mode: command.AllowFold},
			wantHeader: headerID,
		},
		{
			name:       "UserInput StartOnly with per-turn stream",
			cmd:        command.UserInput{Header: command.Header{CommandID: headerID}, Blocks: blocks, Mode: command.StartOnly, Events: ev, Abandoned: ab},
			wantHeader: headerID,
		},
		{
			name:       "UserInput zero header is boundary",
			cmd:        command.UserInput{},
			wantHeader: uuid.UUID{},
		},
		{
			name: "SubagentResult carries parent Coordinates and child Cause",
			cmd: command.SubagentResult{
				Coordinates: identity.Coordinates{LoopID: parentLoop},
				Header:      command.Header{CommandID: headerID, Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: childLoop}}},
				Blocks:      blocks,
			},
			wantHeader: headerID,
		},
		{
			name:       "SubagentResult zero header is boundary",
			cmd:        command.SubagentResult{},
			wantHeader: uuid.UUID{},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.cmd.CommandHeader().CommandID; got != tt.wantHeader {
				t.Errorf("CommandHeader().CommandID = %v, want %v", got, tt.wantHeader)
			}
		})
	}
}

// TestSubagentResultTwoLoopIDs pins the crux of the normalized shape: a
// SubagentResult addresses the PARENT loop via its embedded Coordinates.LoopID
// (the delivery target) and names the CHILD loop that produced the result via
// Header.Cause.LoopID (the quiescence wake token). The two ids are distinct and
// the embedded-Coordinates selector (sr.LoopID) promotes the PARENT, never the
// child — proving the embedding of BOTH Header and identity.Coordinates is
// unambiguous.
func TestSubagentResultTwoLoopIDs(t *testing.T) {
	t.Parallel()

	parentLoop := newID(t)
	childLoop := newID(t)
	cmdID := newID(t)

	sr := command.SubagentResult{
		Coordinates: identity.Coordinates{LoopID: parentLoop},
		Header: command.Header{
			CommandID: cmdID,
			Cause:     identity.Cause{Coordinates: identity.Coordinates{LoopID: childLoop}},
		},
		Blocks: []content.Block{&content.TextBlock{Text: "hi"}},
	}

	tests := []struct {
		name string
		got  uuid.UUID
		want uuid.UUID
	}{
		{name: "embedded Coordinates.LoopID promotes the PARENT (delivery target)", got: sr.LoopID, want: parentLoop},
		{name: "Header.Cause.LoopID is the CHILD (wake token)", got: sr.Cause.LoopID, want: childLoop},
		{name: "CommandHeader().CommandID round-trips", got: sr.CommandHeader().CommandID, want: cmdID},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Errorf("got %v, want %v", tt.got, tt.want)
			}
		})
	}

	// The parent and child ids must be distinct in this fixture, else the test
	// proves nothing about the two-id separation.
	if parentLoop == childLoop {
		t.Fatal("fixture parent and child loop ids collided")
	}
	// A hand-back is machine-originated: Agency stays the zero default AgencyMachine.
	if sr.CommandHeader().Agency != identity.AgencyMachine {
		t.Errorf("SubagentResult Agency = %v, want AgencyMachine (default)", sr.CommandHeader().Agency)
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
