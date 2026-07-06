package tool

import (
	"context"
	"slices"
	"testing"
)

// TestGrantsContextRoundTrip verifies WithGrants/GrantsFromContext are a symmetric
// carrier: tokens placed on a ctx read back verbatim, and a ctx that never carried
// grants (or the background ctx) reads back nil. Grant tokens are OPAQUE strings —
// the carrier stores and returns them unmodified, never inspecting their content.
func TestGrantsContextRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		set  bool     // whether to call WithGrants at all
		put  []string // grants to place when set
		want []string
	}{
		{name: "background context has no grants", set: false, want: nil},
		{name: "single grant round-trips", set: true, put: []string{"tok-a"}, want: []string{"tok-a"}},
		{name: "multiple grants round-trip in order", set: true, put: []string{"tok-a", "tok-b"}, want: []string{"tok-a", "tok-b"}},
		{name: "nil slice round-trips as nil", set: true, put: nil, want: nil},
		{name: "empty slice round-trips as empty", set: true, put: []string{}, want: []string{}},
		{name: "opaque token stored verbatim", set: true, put: []string{"  weird\ttoken  "}, want: []string{"  weird\ttoken  "}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if tt.set {
				ctx = WithGrants(ctx, tt.put)
			}
			got := GrantsFromContext(ctx)
			if !slices.Equal(got, tt.want) {
				t.Errorf("GrantsFromContext = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestGrantsFromContextNilOnBackground pins the exact contract the runner relies
// on: no grants on the ctx yields nil (not an empty non-nil slice), so a caller
// can treat "no grants" as len==0 without a nil check surprise.
func TestGrantsFromContextNilOnBackground(t *testing.T) {
	t.Parallel()
	if got := GrantsFromContext(context.Background()); got != nil {
		t.Errorf("GrantsFromContext(Background) = %#v, want nil", got)
	}
}
