package gate

import (
	"math"
	"testing"
)

func TestCheckedReviewContextAddBoundaries(t *testing.T) {
	t.Parallel()

	if got, ok := checkedReviewContextAdd(math.MaxInt-1, 1); !ok || got != math.MaxInt {
		t.Fatalf("checkedReviewContextAdd(exact) = (%d, %t), want (%d, true)", got, ok, math.MaxInt)
	}
	if got, ok := checkedReviewContextAdd(math.MaxInt, 1); ok || got != 0 {
		t.Fatalf("checkedReviewContextAdd(overflow) = (%d, %t), want (0, false)", got, ok)
	}
	if got, ok := checkedReviewContextAdd(-1, 0); ok || got != 0 {
		t.Fatalf("checkedReviewContextAdd(negative) = (%d, %t), want (0, false)", got, ok)
	}
}
