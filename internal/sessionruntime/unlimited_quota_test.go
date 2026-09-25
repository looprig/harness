package sessionruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/loop"
)

func TestUnlimitedQuotaPreservedByDefaults(t *testing.T) {
	t.Parallel()
	if got := (Limits{Depth: 2, Quota: loop.Unlimited}).withDefaults(); got != (Limits{Depth: 2, Quota: loop.Unlimited}) {
		t.Fatalf("withDefaults() = %+v, want Depth 2, Quota Unlimited", got)
	}
	if got := (Limits{Depth: loop.Unlimited, Quota: loop.Unlimited}).withDefaults(); got.Depth != defaultDepth {
		t.Fatalf("Depth = %d, want default %d", got.Depth, defaultDepth)
	}
	if got := (Limits{Quota: -2}).withDefaults(); got.Quota != defaultQuota {
		t.Fatalf("Quota = %d, want default %d", got.Quota, defaultQuota)
	}
}

func TestNewLoopUnlimitedQuotaExceedsFormerLifetimeQuota(t *testing.T) {
	t.Parallel()
	s, err := newTestSession(context.Background(),
		cfg(&stubLLM{chunks: []content.Chunk{textChunk("primary")}}),
		WithLimits(Limits{Depth: 2, Quota: loop.Unlimited}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	var child loop.Provenance
	for i := 0; i < defaultQuota+1; i++ {
		id, err := s.NewLoop(loop.Provenance{LoopID: s.ActiveLoopID()}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("ok")}}))
		if err != nil {
			t.Fatalf("NewLoop #%d: %v", i+1, err)
		}
		child = loop.Provenance{LoopID: id}
	}
	if got := readSpawned(t, s); got != defaultQuota+1 {
		t.Fatalf("spawned = %d, want %d", got, defaultQuota+1)
	}
	_, err = s.NewLoop(child, cfg(&stubLLM{chunks: []content.Chunk{textChunk("deep")}}))
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionLoopDepthExceeded {
		t.Fatalf("grandchild NewLoop = %v, want depth exceeded", err)
	}
}
