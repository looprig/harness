package security

import (
	"sync"
	"testing"
)

// TestLimitCurrentDefault: a fresh Limit starts at the most-restrictive ordinal (0) —
// the fail-secure default before any SetSecurityLimit command is applied.
func TestLimitCurrentDefault(t *testing.T) {
	t.Parallel()
	if got := New().Current(); got != 0 {
		t.Fatalf("New().Current() = %d, want 0 (fail-secure most-restrictive default)", got)
	}
	if got := NewBounded(3).Current(); got != 0 {
		t.Fatalf("NewBounded(3).Current() = %d, want 0", got)
	}
}

func TestLimitCurrentFailsSecureOnOutOfDomainAtomicValue(t *testing.T) {
	t.Parallel()

	s := New()
	// The public Set path can only store a uint8-backed Level. Exercise the
	// defensive read boundary directly so a corrupted high byte cannot truncate
	// into a more-permissive in-domain ordinal.
	s.current.Store(1<<8 | 7)
	if got := s.Current(); got != 0 {
		t.Fatalf("Current() from out-of-domain atomic value = %d, want fail-secure 0", got)
	}
}

// TestLimitSet is the table for Set: single sets, last-write-wins (the replay
// determinism at the unit level), and clamp-to-max (a journaled command can never raise
// the ordinal above the operator's configured cap). The last Set's return value is the
// EFFECTIVE ordinal the session emits on the event, so it is asserted too.
func TestLimitSet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		clamped bool
		max     Level
		seq     []Level
		want    Level
	}{
		{name: "single set", seq: []Level{2}, want: 2},
		{name: "set to zero", seq: []Level{0}, want: 0},
		{name: "last write wins lower", seq: []Level{1, 2, 0}, want: 0},
		{name: "last write wins raise", seq: []Level{0, 3, 1}, want: 1},
		{name: "no clamp stores as-is", seq: []Level{200}, want: 200},
		{name: "clamp above max", clamped: true, max: 2, seq: []Level{5}, want: 2},
		{name: "clamp keeps below max", clamped: true, max: 2, seq: []Level{1}, want: 1},
		{name: "clamp exact max", clamped: true, max: 2, seq: []Level{2}, want: 2},
		{name: "clamp then last wins", clamped: true, max: 3, seq: []Level{5, 1, 9}, want: 3},
		{name: "clamp max zero pins restrictive", clamped: true, max: 0, seq: []Level{9}, want: 0},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var s *Limit
			if tt.clamped {
				s = NewBounded(tt.max)
			} else {
				s = New()
			}
			var lastReturned Level
			for _, lv := range tt.seq {
				lastReturned = s.Set(lv)
			}
			if got := Level(s.Current()); got != tt.want {
				t.Errorf("Current() = %d, want %d", got, tt.want)
			}
			if lastReturned != tt.want {
				t.Errorf("last Set() return = %d, want %d (effective ordinal for the emitted event)", lastReturned, tt.want)
			}
		})
	}
}

// TestLimitClamp proves Clamp is a PURE projection (the same reduction Set applies) that
// never mutates Current — the applier relies on this to learn the effective ordinal and
// pick the apply/emit direction before committing.
func TestLimitClamp(t *testing.T) {
	t.Parallel()
	// No cap: Clamp is the identity.
	if got := New().Clamp(200); got != 200 {
		t.Errorf("New().Clamp(200) = %d, want 200 (no cap)", got)
	}
	// Capped: reduce above the cap, keep at/below.
	capped := NewBounded(2)
	for _, tc := range []struct{ in, want Level }{{5, 2}, {2, 2}, {1, 1}, {0, 0}} {
		if got := capped.Clamp(tc.in); got != tc.want {
			t.Errorf("NewBounded(2).Clamp(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
	// Clamp must not mutate Current.
	if got := capped.Current(); got != 0 {
		t.Errorf("Current() after Clamp calls = %d, want 0 (Clamp is pure, never stores)", got)
	}
}

// TestLimitConcurrent proves Current and Set are safe under the race detector — a
// checker reads Current on many goroutines while the applier Sets.
func TestLimitConcurrent(t *testing.T) {
	t.Parallel()
	s := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(v Level) {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				s.Set(v)
				_ = s.Current()
			}
		}(Level(i))
	}
	wg.Wait()
	if got := s.Current(); got > 7 {
		t.Fatalf("Current() = %d, want one of the set values (0..7)", got)
	}
}

// *Limit satisfies the read-side LimitSource contract.
var _ LimitSource = (*Limit)(nil)
