package tui

import (
	"fmt"
	"math"
	"strings"

	"charm.land/lipgloss/v2"
)

// Status-label gradient endpoints — the two brand colors the animated status text sweeps
// between: gradLime is the assistant lime, gradBlue the soft brand blue. They are held as
// RGB triples (not lipgloss.Color, which is a string) so gradientLabel can interpolate
// between them per character without re-parsing hex on every render.
var (
	gradLime = rgb{0xD5, 0xF8, 0x4D} // #D5F84D
	gradBlue = rgb{0xA2, 0xD2, 0xFF} // #A2D2FF
)

// gradSpatialFreq is the gradient's angular step PER CHARACTER (radians): how quickly the
// color sweeps along the label. ~0.45 rad/char spreads a little over half a cosine cycle
// across a typical 8-glyph label, so both endpoint colors show without the band repeating
// within one short word.
//
// gradStepPerFrame is the angular SHIFT PER ANIMATION FRAME (radians): how far the band
// slides each blink tick (~450ms). Matching it to gradSpatialFreq advances the gradient by
// ~one character per tick — a calm left-to-right shimmer in step with the dot pulse.
const (
	gradSpatialFreq  = 0.45
	gradStepPerFrame = 0.45
)

// rgb is an 8-bit-per-channel color used only for gradient interpolation (lipgloss.Color
// is a string and cannot be lerped directly). Channels are float64 so a lerp result keeps
// sub-integer precision until it is rounded to hex. It never escapes this file.
type rgb struct{ r, g, b float64 }

// lerp returns the channel-wise linear interpolation from c to other at t, clamped to
// [0,1] so an out-of-range t can never produce an out-of-gamut channel.
func (c rgb) lerp(other rgb, t float64) rgb {
	switch {
	case t < 0:
		t = 0
	case t > 1:
		t = 1
	}
	return rgb{
		r: c.r + (other.r-c.r)*t,
		g: c.g + (other.g-c.g)*t,
		b: c.b + (other.b-c.b)*t,
	}
}

// hex renders the color as a "#RRGGBB" string for lipgloss.Color, rounding each channel to
// the nearest integer.
func (c rgb) hex() string {
	return fmt.Sprintf("#%02X%02X%02X", int(c.r+0.5), int(c.g+0.5), int(c.b+0.5))
}

// gradientLabel renders s as a horizontal lime↔blue gradient that flows with phase: glyph
// i is colored by a cosine wave ((1-cos θ)/2 ∈ [0,1]) sampled at θ = i·gradSpatialFreq −
// phase·gradStepPerFrame, so the band slides one tick's worth each frame and reverses
// smoothly at the endpoints (a cosine has no seam, so the flow never jumps). Spaces are
// emitted uncolored — an SGR run around a blank cell is invisible and wasteful. phase is
// the live animation frame; at rest it is 0, yielding a static (but still gradient) label.
// It is pure and width-preserving: the per-glyph SGR styling adds no display columns.
func gradientLabel(s string, phase uint) string {
	var b strings.Builder
	for i, r := range []rune(s) {
		if r == ' ' {
			b.WriteRune(r)
			continue
		}
		theta := float64(i)*gradSpatialFreq - float64(phase)*gradStepPerFrame
		t := (1 - math.Cos(theta)) / 2
		col := lipgloss.Color(gradLime.lerp(gradBlue, t).hex())
		b.WriteString(lipgloss.NewStyle().Foreground(col).Render(string(r)))
	}
	return b.String()
}
