package runtimecontract

import "testing"

func TestManagedInputQueueCapacityContract(t *testing.T) {
	t.Parallel()
	if ManagedInputQueueCapacity != 64 {
		t.Fatalf("managed input queue capacity = %d, want compatibility value 64", ManagedInputQueueCapacity)
	}
}
