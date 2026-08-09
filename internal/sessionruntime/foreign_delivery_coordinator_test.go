package sessionruntime

import (
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

// TestForeignDeliveryCoordinatorNotifiesAfterPrivatePendingBeforeTimeout
// reproduces the observer-expiry -> explicit-background-timeout sequence. The
// first pending mark is private bookkeeping; the later timeout must still wake
// the initial caller with accepted-pending exactly once.
func TestForeignDeliveryCoordinatorNotifiesAfterPrivatePendingBeforeTimeout(t *testing.T) {
	coordinator := &foreignDeliveryCoordinator{
		background: true,
		tracked:    &requestTracker{lifecycle: requestActive},
		updates:    make(chan foreignDeliveryCoordinatorUpdate, 2),
	}
	coordinator.markPending(false) // private responsiveness expiry
	if got := coordinator.tracked.deliveryStatus(); got != tool.DelegateDeliveryAcceptedPending {
		t.Fatalf("private pending delivery = %q, want accepted_pending", got)
	}
	coordinator.markPending(true) // explicit response timeout
	select {
	case update := <-coordinator.updates:
		if update.status != tool.DelegateDeliveryAcceptedPending {
			t.Fatalf("timeout update = %q, want accepted_pending", update.status)
		}
	default:
		t.Fatal("explicit timeout did not notify initial caller after private pending")
	}
	coordinator.markPending(true)
	select {
	case update := <-coordinator.updates:
		t.Fatalf("duplicate timeout update = %q, want one notification", update.status)
	default:
	}
}
