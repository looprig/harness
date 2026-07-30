package gate_test

import (
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/gate"
)

func TestObservationRequirementValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		req  gate.ObservationRequirement
		want bool
	}{
		{name: "valid", req: gate.ObservationRequirement{Target: "/workspace/file", Token: "abc123"}, want: true},
		{name: "zero value", req: gate.ObservationRequirement{}, want: false},
		{name: "empty target", req: gate.ObservationRequirement{Target: "", Token: "abc123"}, want: false},
		{name: "empty token", req: gate.ObservationRequirement{Target: "/workspace/file", Token: ""}, want: false},
		{name: "invalid utf8 target", req: gate.ObservationRequirement{Target: "\xff\xfe", Token: "abc123"}, want: false},
		{name: "invalid utf8 token", req: gate.ObservationRequirement{Target: "/workspace/file", Token: "\xff\xfe"}, want: false},
		{name: "nul byte in target", req: gate.ObservationRequirement{Target: "/workspace/\x00file", Token: "abc123"}, want: false},
		{name: "nul byte in token", req: gate.ObservationRequirement{Target: "/workspace/file", Token: "abc\x00123"}, want: false},
		{
			name: "target at max bytes",
			req: gate.ObservationRequirement{
				Target: strings.Repeat("a", gate.MaxObservationRequirementTargetBytes),
				Token:  "abc123",
			},
			want: true,
		},
		{
			name: "target one over max bytes",
			req: gate.ObservationRequirement{
				Target: strings.Repeat("a", gate.MaxObservationRequirementTargetBytes+1),
				Token:  "abc123",
			},
			want: false,
		},
		{
			name: "token at max bytes",
			req: gate.ObservationRequirement{
				Target: "/workspace/file",
				Token:  strings.Repeat("t", gate.MaxObservationRequirementTokenBytes),
			},
			want: true,
		},
		{
			name: "token one over max bytes",
			req: gate.ObservationRequirement{
				Target: "/workspace/file",
				Token:  strings.Repeat("t", gate.MaxObservationRequirementTokenBytes+1),
			},
			want: false,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.req.Valid(); got != tt.want {
				t.Errorf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestObservationRequirementComparable proves the type is an ordinary
// comparable Go value (design intent: callers compare/dedupe/use it as a map
// key without a bespoke Equal method), and that equality is exactly the pair
// of field values — no hidden state.
func TestObservationRequirementComparable(t *testing.T) {
	t.Parallel()
	a := gate.ObservationRequirement{Target: "/workspace/file", Token: "tok"}
	b := gate.ObservationRequirement{Target: "/workspace/file", Token: "tok"}
	c := gate.ObservationRequirement{Target: "/workspace/other", Token: "tok"}
	if a != b {
		t.Fatalf("a != b, want equal requirements with identical fields to compare equal")
	}
	if a == c {
		t.Fatalf("a == c, want requirements with different targets to compare unequal")
	}
	set := map[gate.ObservationRequirement]struct{}{a: {}}
	if _, ok := set[b]; !ok {
		t.Fatalf("b not found in map keyed by a, want ObservationRequirement usable as a map key")
	}
}
