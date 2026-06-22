package tui

import "testing"

// TestNextTip covers the rotating-hint picker: every result is a member of the tip set,
// and (with more than one tip) it never repeats the current one, so the hint visibly
// changes turn to turn. It loops to exercise the randomness without pinning a pick.
func TestNextTip(t *testing.T) {
	t.Parallel()

	inSet := func(s string) bool {
		for _, tip := range tips {
			if tip == s {
				return true
			}
		}
		return false
	}

	tests := []struct {
		name    string
		current string
	}{
		{name: "from empty seed", current: ""},
		{name: "from a known tip", current: tips[0]},
		{name: "from a non-member", current: "not a real tip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for i := 0; i < 300; i++ {
				got := nextTip(tt.current)
				if !inSet(got) {
					t.Fatalf("nextTip(%q) = %q, not in the tip set", tt.current, got)
				}
				if len(tips) > 1 && got == tt.current {
					t.Fatalf("nextTip(%q) returned the same tip; it must rotate", tt.current)
				}
			}
		})
	}
}
