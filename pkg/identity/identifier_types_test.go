package identity

import (
	"encoding/json"
	"testing"

	"github.com/ciram-co/looprig/pkg/uuid"
)

func TestAgencyZeroValueIsMachine(t *testing.T) {
	t.Parallel()
	var a Agency
	if a != AgencyMachine {
		t.Errorf("zero Agency = %v, want AgencyMachine", a)
	}
}

// TestAgentNameZeroValueIsUnset documents the contract: the zero AgentName is the
// empty string, meaning "unset" (a plain loop, or a pre-AgentName persisted record).
func TestAgentNameZeroValueIsUnset(t *testing.T) {
	t.Parallel()
	var n AgentName
	if n != "" {
		t.Errorf("zero AgentName = %q, want empty (unset)", n)
	}
}

// TestAgentNameRoundTrip covers AgentName's string round-trip across the JSON
// boundary it rides on event.Header: a set name survives encode/decode, and the
// unset (empty) zero value round-trips to empty.
func TestAgentNameRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   AgentName
	}{
		{name: "unset (empty) name", in: ""},
		{name: "operator", in: "operator"},
		{name: "multi-word role name", in: "code reviewer"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("json.Marshal err = %v", err)
			}
			var got AgentName
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("json.Unmarshal err = %v", err)
			}
			if got != tt.in {
				t.Errorf("round-trip = %q, want %q", got, tt.in)
			}
		})
	}
}

func TestAgencyString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a    Agency
		want string
	}{
		{name: "machine", a: AgencyMachine, want: "machine"},
		{name: "user", a: AgencyUser, want: "user"},
		{name: "unknown", a: Agency(9), want: "Agency(9)"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.a.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// fixedUUID builds a deterministic non-zero uuid from a single seed byte.
func fixedUUID(seed byte) uuid.UUID {
	var u uuid.UUID
	for i := range u {
		u[i] = seed
	}
	return u
}

func TestCauseJSONOmitzero(t *testing.T) {
	t.Parallel()
	cmd := fixedUUID(0x01)
	tests := []struct {
		name string
		in   Cause
		want string
	}{
		{name: "zero cause marshals empty", in: Cause{}, want: `{}`},
		{
			name: "command id and user agency only",
			in:   Cause{CommandID: cmd, Agency: AgencyUser},
			want: `{"command_id":"01010101-0101-0101-0101-010101010101","agency":1}`,
		},
		{
			name: "machine agency is omitted",
			in:   Cause{CommandID: cmd, Agency: AgencyMachine},
			want: `{"command_id":"01010101-0101-0101-0101-010101010101"}`,
		},
		{
			name: "coordinates promote into the object",
			in:   Cause{Coordinates: Coordinates{LoopID: fixedUUID(0x02)}},
			want: `{"loop_id":"02020202-0202-0202-0202-020202020202"}`,
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
			var got Cause
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("json.Unmarshal err = %v", err)
			}
			if got != tt.in {
				t.Errorf("round-trip = %+v, want %+v", got, tt.in)
			}
		})
	}
}

func TestCoordinatesJSONOmitzero(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   Coordinates
		want string
	}{
		{name: "zero", in: Coordinates{}, want: `{}`},
		{
			name: "session only",
			in:   Coordinates{SessionID: fixedUUID(0x03)},
			want: `{"session_id":"03030303-0303-0303-0303-030303030303"}`,
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
		})
	}
}
