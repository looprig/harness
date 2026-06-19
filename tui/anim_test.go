package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/tui/styles"
)

// TestAnimStateAdvance covers the per-tick animation step: blink toggles each call
// and frame increments. ticking is intentionally untouched (start/stop is Screen's
// concern, not the animState's).
func TestAnimStateAdvance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        animState
		wantBlink bool
		wantFrame uint
	}{
		{name: "zero advances to blink on, frame 1", in: animState{}, wantBlink: true, wantFrame: 1},
		{name: "blink on advances to off, frame increments", in: animState{blink: true, frame: 1}, wantBlink: false, wantFrame: 2},
		{name: "ticking preserved through advance", in: animState{ticking: true, frame: 7}, wantBlink: true, wantFrame: 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.in.advance()
			if got.blink != tt.wantBlink {
				t.Errorf("advance().blink = %v, want %v", got.blink, tt.wantBlink)
			}
			if got.frame != tt.wantFrame {
				t.Errorf("advance().frame = %d, want %d", got.frame, tt.wantFrame)
			}
			if got.ticking != tt.in.ticking {
				t.Errorf("advance() changed ticking to %v, want %v (unchanged)", got.ticking, tt.in.ticking)
			}
		})
	}
}

// TestAnimStateReset covers the idle reset: every field returns to its zero value so
// no animation lingers and a fresh turn starts a clean tick loop.
func TestAnimStateReset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   animState
	}{
		{name: "zero stays zero", in: animState{}},
		{name: "blinking ticking state resets", in: animState{blink: true, frame: 42, ticking: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.in.reset(); got != (animState{}) {
				t.Errorf("reset() = %+v, want zero animState", got)
			}
		})
	}
}

// TestSpinnerGlyph covers the running-tool spinner cell selection: it returns the
// frame-indexed cell and wraps modulo the frame count so any (unbounded) frame value
// is in range and never panics.
func TestSpinnerGlyph(t *testing.T) {
	t.Parallel()

	n := uint(len(spinnerFrames))
	tests := []struct {
		name  string
		frame uint
		want  string
	}{
		{name: "frame 0", frame: 0, want: spinnerFrames[0]},
		{name: "frame 1", frame: 1, want: spinnerFrames[1]},
		{name: "last frame", frame: n - 1, want: spinnerFrames[n-1]},
		{name: "wraps at count", frame: n, want: spinnerFrames[0]},
		{name: "wraps far past count", frame: n*3 + 2, want: spinnerFrames[2]},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := spinnerGlyph(tt.frame); got != tt.want {
				t.Errorf("spinnerGlyph(%d) = %q, want %q", tt.frame, got, tt.want)
			}
		})
	}
}

// TestWorkingWord covers the live working-word selection for an empty-text tool step
// (design §3 rule 4): the list is non-empty, frame 0 returns the first word, and the
// counter wraps modulo the word count so any (unbounded) live frame is in range and
// never panics. It mirrors TestSpinnerGlyph — the word is a purely live affordance.
func TestWorkingWord(t *testing.T) {
	t.Parallel()

	if len(workingWords) == 0 {
		t.Fatal("workingWords is empty; an empty-text tool step needs at least one live word")
	}

	n := uint(len(workingWords))
	tests := []struct {
		name  string
		frame uint
		want  string
	}{
		{name: "frame 0", frame: 0, want: workingWords[0]},
		{name: "last frame", frame: n - 1, want: workingWords[n-1]},
		{name: "wraps at count", frame: n, want: workingWords[0]},
		{name: "wraps far past count", frame: n*4 + 1, want: workingWords[1]},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := workingWord(tt.frame); got != tt.want {
				t.Errorf("workingWord(%d) = %q, want %q", tt.frame, got, tt.want)
			}
		})
	}
}

// TestLiveDot covers the blink phase of the live assistant bullet: lit when blink is
// off, dimmed when on, and both 2 columns wide so narration alignment is unchanged.
func TestLiveDot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		blink bool
		want  string
	}{
		{name: "blink off renders lit dot", blink: false, want: liveDotLit},
		{name: "blink on renders dimmed dot", blink: true, want: liveDotDim},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := liveDot(tt.blink)
			if got != tt.want {
				t.Errorf("liveDot(%v) = %q, want %q", tt.blink, got, tt.want)
			}
			if len([]rune(got)) != dotWidth {
				t.Errorf("liveDot(%v) width = %d runes, want %d (alignment)", tt.blink, len([]rune(got)), dotWidth)
			}
		})
	}
}

// TestLiveDotPhasesDiffer pins that the two blink phases are visually distinct — the
// whole point of the blink is that the live dot changes between frames.
func TestLiveDotPhasesDiffer(t *testing.T) {
	t.Parallel()

	if liveDot(true) == liveDot(false) {
		t.Errorf("liveDot blink phases identical (%q); they must differ to animate", liveDot(true))
	}
}

// TestRenderLiveAssistantBlink covers the live-only animation threading: the live
// assistant bullet differs between blink phases, while the committed renderAssistant
// is UNCHANGED by any animation state (it never takes an anim — it always renders the
// static lit dot).
func TestRenderLiveAssistantBlink(t *testing.T) {
	t.Parallel()

	const text = "working on it"

	lit := renderLiveAssistant("", text, nil, false, 80, animState{blink: false})
	dim := renderLiveAssistant("", text, nil, false, 80, animState{blink: true})

	if lit == dim {
		t.Fatalf("live assistant identical across blink phases (%q); the live dot must blink", stripANSI(lit))
	}
	// The lit live phase matches the static committed render (same lit dot).
	committed := renderAssistant("", text, nil, false, false, 80)
	if stripANSI(lit) != stripANSI(committed) {
		t.Errorf("lit live render %q != committed render %q; lit phase must equal the static dot",
			stripANSI(lit), stripANSI(committed))
	}
	// The dimmed phase carries the dimmed bullet, not the lit "●".
	if !strings.Contains(stripANSI(dim), strings.TrimRight(liveDotDim, " ")) {
		t.Errorf("dimmed live render %q missing the dimmed bullet %q", stripANSI(dim), liveDotDim)
	}
}

// TestRenderLiveAssistantSpinner covers the running tool card showing the spinner
// frame for the current animState.frame, while a RESOLVED card keeps its static
// glyph regardless of frame.
func TestRenderLiveAssistantSpinner(t *testing.T) {
	t.Parallel()

	t.Run("running card shows spinner frame", func(t *testing.T) {
		t.Parallel()

		calls := []ToolCallView{{ToolName: "Bash", Summary: "ls", Status: ToolRunning}}
		for _, frame := range []uint{0, 1, 5} {
			got := stripANSI(renderLiveAssistant("", "checking", calls, false, 80, animState{frame: frame}))
			if !strings.Contains(got, spinnerGlyph(frame)) {
				t.Errorf("frame %d: live render %q missing spinner glyph %q", frame, got, spinnerGlyph(frame))
			}
			if strings.Contains(got, glyphRunning) {
				t.Errorf("frame %d: live render %q still shows the static running glyph %q, want the spinner",
					frame, got, glyphRunning)
			}
		}
	})

	t.Run("resolved card keeps static glyph", func(t *testing.T) {
		t.Parallel()

		calls := []ToolCallView{{ToolName: "Bash", Summary: "ls", Status: ToolOK, Result: []string{"a.go"}}}
		got := stripANSI(renderLiveAssistant("", "done", calls, false, 80, animState{frame: 3}))
		if !strings.Contains(got, glyphOK) {
			t.Errorf("resolved card live render %q missing static OK glyph %q", got, glyphOK)
		}
		if strings.Contains(got, spinnerGlyph(3)) {
			t.Errorf("resolved card live render %q animated the static OK glyph", got)
		}
	})

	t.Run("card-only segment blinks bare bullet", func(t *testing.T) {
		t.Parallel()

		calls := []ToolCallView{{ToolName: "Bash", Status: ToolRunning}}
		lit := stripANSI(renderLiveAssistant("", "", calls, false, 80, animState{blink: false}))
		dim := stripANSI(renderLiveAssistant("", "", calls, false, 80, animState{blink: true, frame: 1}))
		if lit == dim {
			t.Errorf("card-only live segment identical across blink phases (%q); the bare bullet must blink", lit)
		}
	})
}

// TestRenderEntryStaticUnderAnimation pins the committed path's immutability: the
// committed renderEntry has NO animation parameter, so a committed assistant entry and
// a committed (resolved) tool entry render identically no matter what — there is no
// way for animation state to reach them. Asserting the rendered lines are stable
// proves the frozen-scrollback invariant at the render boundary.
func TestRenderEntryStaticUnderAnimation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		e    entry
	}{
		{
			name: "committed assistant entry",
			e: entry{Kind: kindAssistant, Blocks: []content.Block{
				&content.TextBlock{Text: "final answer"},
			}},
		},
		{
			name: "committed resolved tool entry",
			e: entry{Kind: kindTool, Calls: []ToolCallView{
				{ToolName: "Bash", Summary: "ls", Status: ToolOK, Result: []string{"a.go"}},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// renderEntry takes only (entry, expand, width) — no anim. Two calls with
			// identical inputs must be byte-identical: nothing can animate a committed row.
			a := renderEntry(tt.e, false, 80)
			b := renderEntry(tt.e, false, 80)
			if strings.Join(a, "\n") != strings.Join(b, "\n") {
				t.Errorf("renderEntry is not deterministic across calls:\n a=%q\n b=%q", a, b)
			}
			// A committed tool entry must NEVER carry a spinner glyph (frozen → static ⋯/✓/✗).
			joined := stripANSI(strings.Join(a, "\n"))
			for _, sf := range spinnerFrames {
				if strings.Contains(joined, sf) {
					t.Errorf("committed renderEntry %q contains spinner frame %q; committed rows must be static", joined, sf)
				}
			}
		})
	}
}

// TestCommittedAssistantNeverDimmed pins that the committed assistant bullet is the
// lit "●" — the dimmed live bullet must never leak into scrollback.
func TestCommittedAssistantNeverDimmed(t *testing.T) {
	t.Parallel()

	e := entry{Kind: kindAssistant, Blocks: []content.Block{&content.TextBlock{Text: "hello"}}}
	got := stripANSI(strings.Join(renderEntry(e, false, 80), "\n"))
	if strings.Contains(got, strings.TrimRight(liveDotDim, " ")) {
		t.Errorf("committed assistant render %q contains the dimmed live bullet; committed dot must be lit", got)
	}
	if !strings.Contains(got, strings.TrimRight(styles.Dot, " ")) {
		t.Errorf("committed assistant render %q missing the lit dot %q", got, styles.Dot)
	}
}

// TestHandleBlinkWhileRunning covers the tick handler in the Running state: it
// advances the animation AND reschedules the tick (non-nil cmd).
func TestHandleBlinkWhileRunning(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := runningScreen(t, agent)
	m.anim = animState{ticking: true} // a tick loop is in flight

	got, cmd := updateScreen(t, m, blinkMsg{})

	if cmd == nil {
		t.Fatal("blinkMsg while Running returned nil cmd, want a rescheduled tick")
	}
	if !got.anim.blink {
		t.Error("blinkMsg while Running did not advance blink (still false)")
	}
	if got.anim.frame != 1 {
		t.Errorf("blinkMsg while Running frame = %d, want 1 (advanced)", got.anim.frame)
	}
	if !got.anim.ticking {
		t.Error("blinkMsg while Running cleared ticking; the loop must keep running")
	}
}

// TestHandleBlinkWhileIdle covers the tick handler at Idle (and other non-Running
// states): it does NOT reschedule (nil cmd) and resets the animation state, so the
// loop self-terminates with no orphan tick and no lingering blink.
func TestHandleBlinkWhileIdle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status Status
	}{
		{name: "idle", status: StatusIdle},
		{name: "interrupting", status: StatusInterrupting},
		{name: "resetting", status: StatusResetting},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
			m.status = tt.status
			m.anim = animState{blink: true, frame: 9, ticking: true}

			got, cmd := updateScreen(t, m, blinkMsg{})

			if cmd != nil {
				t.Errorf("blinkMsg at %s returned non-nil cmd, want nil (loop must stop)", tt.name)
			}
			if got.anim != (animState{}) {
				t.Errorf("blinkMsg at %s left anim = %+v, want reset to zero", tt.name, got.anim)
			}
		})
	}
}

// TestStartBlinkGuard covers the double-start guard: starting a turn from a
// not-ticking state returns a tick cmd and sets ticking; starting again while already
// ticking returns nil (no parallel loop) and leaves ticking set.
func TestStartBlinkGuard(t *testing.T) {
	t.Parallel()

	t.Run("first start kicks off the tick", func(t *testing.T) {
		t.Parallel()

		agent := &fakeAgent{}
		m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
		if m.anim.ticking {
			t.Fatal("fresh Screen already ticking")
		}
		cmd := m.startBlink()
		if cmd == nil {
			t.Fatal("first startBlink returned nil cmd, want a tick")
		}
		if !m.anim.ticking {
			t.Error("first startBlink did not set ticking")
		}
	})

	t.Run("second start does not double-start", func(t *testing.T) {
		t.Parallel()

		agent := &fakeAgent{}
		m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
		m.anim.ticking = true // a loop is already in flight
		cmd := m.startBlink()
		if cmd != nil {
			t.Error("startBlink while already ticking returned non-nil cmd, want nil (no parallel loop)")
		}
		if !m.anim.ticking {
			t.Error("startBlink cleared ticking; it must stay set")
		}
	})
}

// TestTurnStartTicking covers the integration point: a turn that actually
// starts transitions to Running and kicks off the animation tick (ticking set, and a
// non-nil cmd batching readNext + the tick). A failed start stays Idle and never
// starts ticking.
func TestTurnStartTicking(t *testing.T) {
	t.Parallel()

	t.Run("successful start ticks", func(t *testing.T) {
		t.Parallel()

		agent := &fakeAgent{streamReader: scriptedReader()}
		m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})

		cmd, ok := m.startTurn([]content.Block{&content.TextBlock{Text: "hi"}})
		if !ok {
			t.Fatal("startTurn ok = false, want true")
		}
		if m.status != StatusRunning {
			t.Errorf("startTurn status = %d, want StatusRunning", m.status)
		}
		if !m.anim.ticking {
			t.Error("startTurn did not start the animation tick (ticking false)")
		}
		if cmd == nil {
			t.Error("startTurn cmd = nil, want batched readNext + tick")
		}
	})

	t.Run("failed start does not tick", func(t *testing.T) {
		t.Parallel()

		agent := &fakeAgent{streamErr: errStub{}}
		m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})

		_, ok := m.startTurn([]content.Block{&content.TextBlock{Text: "hi"}})
		if ok {
			t.Fatal("startTurn ok = true on stream error, want false")
		}
		if m.status != StatusIdle {
			t.Errorf("failed startTurn status = %d, want StatusIdle", m.status)
		}
		if m.anim.ticking {
			t.Error("failed startTurn started the animation tick; it must not")
		}
	})
}

// TestBlinkDoesNotFlushScrollback is the load-bearing invariant: a blinkMsg is a PURE
// active-surface re-render and must NEVER write to scrollback. With a committed-but-
// unflushed entry present, a blinkMsg must leave the print-once set untouched (it did
// not flush) and must not return a print command — only the re-scheduled tick.
func TestBlinkDoesNotFlushScrollback(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := runningScreen(t, agent)
	m.anim = animState{ticking: true}
	m.width = 80
	m.scrollback = newScrollbackModel(80)

	// Commit an entry WITHOUT flushing it — so a stray flush would be observable as a
	// growth in the print-once set.
	m.transcript = m.transcript.CommitUser([]content.Block{&content.TextBlock{Text: "pending"}})
	if len(m.transcript.committed) == 0 {
		t.Fatal("setup: no committed entry to detect a flush against")
	}
	printedBefore := len(m.scrollback.printed)

	got, _ := updateScreen(t, m, blinkMsg{})

	if len(got.scrollback.printed) != printedBefore {
		t.Errorf("blinkMsg flushed to scrollback: printed set grew %d → %d; a tick must never flush",
			printedBefore, len(got.scrollback.printed))
	}
}

// errStub is a typed stub error for the failed-start table row.
type errStub struct{}

func (errStub) Error() string { return "stub stream error" }
