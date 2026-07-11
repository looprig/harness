package sessionruntime

import (
	"context"
	"testing"

	"github.com/looprig/core/content"
)

// TestLimitsWithDefaults proves the zero-value defaulting rule: a zero Depth/Quota
// adopts the package defaults (Depth 3 / Quota 64), an explicit positive value is
// preserved, and a negative value (a wiring slip) also falls back to the default so a
// caller can never disable a cap with a bad value.
func TestLimitsWithDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   Limits
		want Limits
	}{
		{name: "zero adopts both defaults", in: Limits{}, want: Limits{Depth: defaultDepth, Quota: defaultQuota}},
		{name: "explicit positive preserved", in: Limits{Depth: 5, Quota: 10}, want: Limits{Depth: 5, Quota: 10}},
		{name: "zero depth only", in: Limits{Depth: 0, Quota: 10}, want: Limits{Depth: defaultDepth, Quota: 10}},
		{name: "zero quota only", in: Limits{Depth: 5, Quota: 0}, want: Limits{Depth: 5, Quota: defaultQuota}},
		{name: "min valid depth 1", in: Limits{Depth: 1, Quota: 1}, want: Limits{Depth: 1, Quota: 1}},
		{name: "negative falls back to default", in: Limits{Depth: -2, Quota: -9}, want: Limits{Depth: defaultDepth, Quota: defaultQuota}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.in.withDefaults(); got != tt.want {
				t.Errorf("withDefaults() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestNewAppliesLimits proves newSession installs the default Limits when no
// WithLimits option is given, and adopts an explicit one (after defaulting) when it
// is. The limits field is read under loopsMu (the same lock that guards spawned).
func TestNewAppliesLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts []Option
		want Limits
	}{
		{name: "no option installs defaults", opts: nil, want: Limits{Depth: defaultDepth, Quota: defaultQuota}},
		{name: "explicit limits preserved", opts: []Option{WithLimits(Limits{Depth: 2, Quota: 7})}, want: Limits{Depth: 2, Quota: 7}},
		{name: "explicit zero quota defaults", opts: []Option{WithLimits(Limits{Depth: 2, Quota: 0})}, want: Limits{Depth: 2, Quota: defaultQuota}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}), tt.opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			s.loopsMu.RLock()
			got := s.limits
			s.loopsMu.RUnlock()
			if got != tt.want {
				t.Errorf("session limits = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestNewInitialSpawned proves a fresh session starts with spawned == 0: the primary
// loop is built by New (not via the quota-counted NewLoop spawn path), so it does not
// count toward the quota.
func TestNewInitialSpawned(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	s.loopsMu.RLock()
	got := s.spawned
	s.loopsMu.RUnlock()
	if got != 0 {
		t.Errorf("fresh session spawned = %d, want 0 (primary does not count)", got)
	}
}
