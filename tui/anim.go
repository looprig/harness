package tui

import "github.com/inventivepotter/urvi/tui/styles"

// animState is the live-surface animation state advanced once per blink tick while
// a turn is Running. It is meaningful ONLY for the live tail rendered in View(); the
// committed scrollback path never consults it (already-printed entries are frozen).
// blink toggles the streaming assistant dot between its lit and dimmed glyph; frame
// indexes the running tool-card spinner; ticking guards against double-starting the
// tick loop (a second TurnStarted while a tick is already in flight must not spawn a
// parallel loop). The zero value is the idle, not-yet-ticking state.
type animState struct {
	blink   bool
	frame   uint
	ticking bool
}

// advance moves the animation one step: it flips blink and increments frame
// (wrapping is handled at index time by spinnerGlyph's modulo). It does NOT touch
// ticking — start/stop of the loop is the caller's (Screen's) concern.
func (a animState) advance() animState {
	a.blink = !a.blink
	a.frame++
	return a
}

// reset returns the idle animation state: blink off, frame zeroed, not ticking. It
// is applied when a turn ends so the next live render carries no lingering animation
// and a fresh turn starts a clean tick loop.
func (a animState) reset() animState { return animState{} }

// spinnerFrames are the running-tool spinner cells, cycled one per blink tick. The
// braille dots rotate a single filled segment around the cell, reading as a smooth
// "working" spinner at the ~450ms blink cadence.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerGlyph returns the running-tool spinner cell for frame, wrapping modulo the
// frame count so any frame value is in range (the counter grows unbounded over a long
// turn). It is used ONLY for a LIVE running tool card; a resolved card renders its
// static ✓/✗ glyph via toolGlyph.
func spinnerGlyph(frame uint) string {
	return spinnerFrames[frame%uint(len(spinnerFrames))]
}

// liveDotLit / liveDotDim are the two glyphs the live (streaming) assistant bullet
// alternates between on each blink tick: the normal lit "● " and a dimmed hollow
// "◦ ". Both keep the dotWidth (2 columns) so narration alignment is unchanged.
const (
	liveDotLit = styles.Dot // "● "
	liveDotDim = "◦ "
)

// liveDot returns the live assistant bullet for the current blink phase: the dimmed
// hollow dot when blink is true, the lit dot otherwise. Only the LIVE streaming dot
// blinks; a committed assistant "●" renders the static styles.Dot via renderMD.
func liveDot(blink bool) string {
	if blink {
		return liveDotDim
	}
	return liveDotLit
}
