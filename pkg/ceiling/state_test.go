package ceiling

import (
	"sync"
	"testing"
)

// TestStateCurrentDefault: a fresh State starts at the most-restrictive ordinal (0) —
// the fail-secure default before any SetSecurityCeiling command is applied.
func TestStateCurrentDefault(t *testing.T) {
	t.Parallel()
	if got := New().Current(); got != 0 {
		t.Fatalf("New().Current() = %d, want 0 (fail-secure most-restrictive default)", got)
	}
	if got := NewClamped(3).Current(); got != 0 {
		t.Fatalf("NewClamped(3).Current() = %d, want 0", got)
	}
}

// TestStateSet is the table for Set: single sets, last-write-wins (the replay
// determinism at the unit level), and clamp-to-max (a journaled command can never raise
// the ordinal above the operator's configured cap). The last Set's return value is the
// EFFECTIVE ordinal the session emits on the event, so it is asserted too.
func TestStateSet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		clamped bool
		max     uint8
		seq     []uint8
		want    uint8
	}{
		{name: "single set", seq: []uint8{2}, want: 2},
		{name: "set to zero", seq: []uint8{0}, want: 0},
		{name: "last write wins lower", seq: []uint8{1, 2, 0}, want: 0},
		{name: "last write wins raise", seq: []uint8{0, 3, 1}, want: 1},
		{name: "no clamp stores as-is", seq: []uint8{200}, want: 200},
		{name: "clamp above max", clamped: true, max: 2, seq: []uint8{5}, want: 2},
		{name: "clamp keeps below max", clamped: true, max: 2, seq: []uint8{1}, want: 1},
		{name: "clamp exact max", clamped: true, max: 2, seq: []uint8{2}, want: 2},
		{name: "clamp then last wins", clamped: true, max: 3, seq: []uint8{5, 1, 9}, want: 3},
		{name: "clamp max zero pins restrictive", clamped: true, max: 0, seq: []uint8{9}, want: 0},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var s *State
			if tt.clamped {
				s = NewClamped(tt.max)
			} else {
				s = New()
			}
			var lastReturned uint8
			for _, lv := range tt.seq {
				lastReturned = s.Set(lv)
			}
			if got := s.Current(); got != tt.want {
				t.Errorf("Current() = %d, want %d", got, tt.want)
			}
			if lastReturned != tt.want {
				t.Errorf("last Set() return = %d, want %d (effective ordinal for the emitted event)", lastReturned, tt.want)
			}
		})
	}
}

// TestStateConcurrent proves Current and Set are safe under the race detector — a
// checker reads Current on many goroutines while the applier Sets.
func TestStateConcurrent(t *testing.T) {
	t.Parallel()
	s := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(v uint8) {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				s.Set(v)
				_ = s.Current()
			}
		}(uint8(i))
	}
	wg.Wait()
	if got := s.Current(); got > 7 {
		t.Fatalf("Current() = %d, want one of the set values (0..7)", got)
	}
}

// *State satisfies the read-side Source contract.
var _ Source = (*State)(nil)
