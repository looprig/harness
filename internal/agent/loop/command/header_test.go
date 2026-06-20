package command

import (
	"encoding/json"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// fixedUUID builds a deterministic non-zero uuid from a single seed byte.
func fixedUUID(seed byte) uuid.UUID {
	var u uuid.UUID
	for i := range u {
		u[i] = seed
	}
	return u
}

// TestHeaderPromotedOnCommands asserts that every concrete command exposes the
// embedded Header via the promoted CommandHeader() method, and that all Header
// fields (CommandID, Cause, Agency) round-trip unchanged through each command type.
func TestHeaderPromotedOnCommands(t *testing.T) {
	t.Parallel()

	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	causeCmd, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}

	headers := []struct {
		name   string
		header Header
	}{
		{name: "command id, cause and user agency set", header: Header{
			CommandID: id,
			Cause:     identity.Cause{CommandID: causeCmd},
			Agency:    identity.AgencyUser,
		}},
		{name: "zero header is boundary", header: Header{}},
	}

	for _, hc := range headers {
		hc := hc
		commands := []struct {
			name string
			cmd  Command
		}{
			{name: "UserInput", cmd: UserInput{Header: hc.header}},
			{name: "SubagentResult", cmd: SubagentResult{Header: hc.header}},
			{name: "CancelQueuedInput", cmd: CancelQueuedInput{Header: hc.header}},
			{name: "Interrupt", cmd: Interrupt{Header: hc.header}},
			{name: "Shutdown", cmd: Shutdown{Header: hc.header}},
		}
		for _, cc := range commands {
			cc := cc
			t.Run(hc.name+"/"+cc.name, func(t *testing.T) {
				t.Parallel()
				got := cc.cmd.CommandHeader()
				if got.CommandID != hc.header.CommandID {
					t.Errorf("%T: CommandHeader().CommandID = %v, want %v", cc.cmd, got.CommandID, hc.header.CommandID)
				}
				if got.Cause != hc.header.Cause {
					t.Errorf("%T: CommandHeader().Cause = %v, want %v", cc.cmd, got.Cause, hc.header.Cause)
				}
				if got.Agency != hc.header.Agency {
					t.Errorf("%T: CommandHeader().Agency = %v, want %v", cc.cmd, got.Agency, hc.header.Agency)
				}
			})
		}
	}
}

// TestCommandHeaderJSONOmitzero asserts the journal-only encoding drops the
// machine default (Agency) and any zero id, so a root machine command is "{}".
func TestCommandHeaderJSONOmitzero(t *testing.T) {
	t.Parallel()
	cmd := fixedUUID(0x01)
	tests := []struct {
		name string
		in   Header
		want string
	}{
		{name: "zero header marshals empty", in: Header{}, want: `{}`},
		{
			name: "command id and user agency",
			in:   Header{CommandID: cmd, Agency: identity.AgencyUser},
			want: `{"command_id":"01010101-0101-0101-0101-010101010101","agency":1}`,
		},
		{
			name: "machine agency omitted",
			in:   Header{CommandID: cmd, Agency: identity.AgencyMachine},
			want: `{"command_id":"01010101-0101-0101-0101-010101010101"}`,
		},
		{
			name: "cause nests its own id",
			in:   Header{Cause: identity.Cause{CommandID: cmd}},
			want: `{"cause":{"command_id":"01010101-0101-0101-0101-010101010101"}}`,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("json.Marshal err = %v", err)
			}
			if string(data) != tt.want {
				t.Errorf("json.Marshal = %s, want %s", data, tt.want)
			}
			var got Header
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("json.Unmarshal err = %v", err)
			}
			if got != tt.in {
				t.Errorf("round-trip = %+v, want %+v", got, tt.in)
			}
		})
	}
}
