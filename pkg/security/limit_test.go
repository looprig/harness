package security

import "testing"

var _ LimitSource = (*Limit)(nil)

func TestBoundedLimitClampsRequestedLevel(t *testing.T) {
	t.Parallel()

	limit := NewBounded(Level(2))
	if got := limit.Set(Level(3)); got != Level(2) {
		t.Fatalf("Set(3) = %d, want bounded level 2", got)
	}
	if got := limit.Current(); got != Level(2) {
		t.Fatalf("Current() = %d, want 2", got)
	}
}
