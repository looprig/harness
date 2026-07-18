package loop

import "testing"

func TestManagedInputQueueCapacityCompatibility(t *testing.T) {
	if ManagedInputQueueCapacity != 64 {
		t.Fatalf("ManagedInputQueueCapacity = %d, want 64", ManagedInputQueueCapacity)
	}
}
