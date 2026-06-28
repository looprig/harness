package tui

import "testing"

// TestRGBLerp covers the channel-wise interpolation and its [0,1] clamp: the two
// endpoints, the midpoint, an equal-color no-op, and out-of-range t values that must
// clamp rather than extrapolate out of gamut.
func TestRGBLerp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		from, to rgb
		t        float64
		want     string // expected #RRGGBB of the lerped color
	}{
		{name: "t=0 yields from", from: gradLime, to: gradBlue, t: 0, want: "#D5F84D"},
		{name: "t=1 yields to", from: gradLime, to: gradBlue, t: 1, want: "#A2D2FF"},
		{name: "t=0.5 yields midpoint", from: gradLime, to: gradBlue, t: 0.5, want: "#BCE5A6"},
		{name: "equal colors is a no-op", from: gradLime, to: gradLime, t: 0.5, want: "#D5F84D"},
		{name: "negative t clamps to from", from: gradLime, to: gradBlue, t: -1, want: "#D5F84D"},
		{name: "t above one clamps to to", from: gradLime, to: gradBlue, t: 2, want: "#A2D2FF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.from.lerp(tt.to, tt.t).hex(); got != tt.want {
				t.Errorf("lerp(%v,%v,%v).hex() = %q, want %q", tt.from, tt.to, tt.t, got, tt.want)
			}
		})
	}
}

// TestGradientLabel covers the per-glyph gradient renderer: empty input, space
// preservation, content/width preservation (the SGR runs add no display columns, so
// stripping ANSI must recover the input verbatim), a multi-byte rune (the ellipsis the
// live labels carry), and determinism for a fixed phase.
func TestGradientLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		s     string
		phase uint
	}{
		{name: "empty", s: "", phase: 0},
		{name: "single glyph", s: "x", phase: 0},
		{name: "word", s: "idle", phase: 0},
		{name: "label with ellipsis", s: "thinking…", phase: 7},
		{name: "spaces preserved", s: "awaiting approval", phase: 3},
		{name: "animated phase", s: "streaming…", phase: 42},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := gradientLabel(tt.s, tt.phase)

			// Content + width are preserved: stripping the per-glyph SGR runs recovers
			// the exact input (no added or dropped columns), which also proves spaces
			// survive verbatim.
			if plain := stripANSI(got); plain != tt.s {
				t.Errorf("gradientLabel(%q,%d) stripped = %q, want %q", tt.s, tt.phase, plain, tt.s)
			}
			// Determinism: same (s, phase) renders identically.
			if again := gradientLabel(tt.s, tt.phase); again != got {
				t.Errorf("gradientLabel(%q,%d) not deterministic", tt.s, tt.phase)
			}
		})
	}
}
