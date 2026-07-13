package sessionruntime

import (
	"context"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/hustle"
)

func testHustleActivityUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}

func TestHubHustleActivityAdapterCompatibility(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "adapter satisfies narrow return contract"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var tracker hustleActivityTracker = newHubHustleActivityTracker(hub.New(testHustleActivityUUID(t)))
			lease, err := tracker.AcquireHustleActivity(context.Background(), hustle.RunID(testHustleActivityUUID(t)))
			if err != nil || lease == nil {
				t.Fatalf("AcquireHustleActivity() = (%v,%v), want nonnil,nil", lease, err)
			}
			if err := lease.Release(context.Background()); err != nil {
				t.Fatalf("Release() error = %v", err)
			}
		})
	}
}
