package tui

import (
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
)

// tailHeight is the physical row count of the active surface's tail region (held + live),
// the quantity that must stay stable across commit and send so the input box never jumps.
func tailHeight(m Screen) int {
	tail := stripANSI(m.tailView())
	if tail == "" {
		return 0
	}
	return strings.Count(tail, "\n") + 1
}

// TestFinalizedStepHeldInSurface is the freeze-in-place invariant: a step that finalizes
// (StepDone, then the turn's terminal) is HELD in the active surface — rendered as the tail
// above the input — instead of being emitted to native scrollback immediately. Holding it
// keeps the managed region the same height, so the input box does not jump upward when the
// live tail would otherwise collapse.
func TestFinalizedStepHeldInSurface(t *testing.T) {
	t.Parallel()

	m := runningScreen(t, &fakeAgent{})
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 40})

	m = feed(t, m, event.TokenDelta{Chunk: &content.TextChunk{Text: "the final answer"}})
	m = feed(t, m, stepDone(aiMessage("", "the final answer")))
	m = feed(t, m, event.TurnDone{})

	if len(m.heldLines) == 0 {
		t.Fatal("finalized step was not held: heldLines is empty (it flushed to scrollback, collapsing the surface)")
	}
	if m.status != StatusIdle {
		t.Fatalf("status = %d, want StatusIdle after TurnDone", m.status)
	}
	if v := stripANSI(m.View().Content); !strings.Contains(v, "the final answer") {
		t.Errorf("held step not shown in the surface after commit; view = %q", v)
	}
}

// TestSendDoesNotShrinkSurface is the send-time regression lock. After a response finalizes
// (held frozen above the input), sending a new message and streaming its response must not
// collapse the surface: the previous response spills to scrollback GRADUALLY as the new one
// grows, so the combined tail height stays within the budget and never drops toward zero —
// the all-at-once release did the latter, springing the input box to the top with blank
// space below it.
func TestSendDoesNotShrinkSurface(t *testing.T) {
	t.Parallel()

	m := runningScreen(t, &fakeAgent{})
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 40})
	budget := liveTailDisplayCap(m.surfaceInputs(""))
	if budget <= 0 {
		t.Fatalf("test setup: non-positive live-tail budget %d", budget)
	}

	// First turn: a response taller than the tail budget, then finalize.
	first := make([]string, 0, budget*2)
	for i := 0; i < budget*2; i++ {
		first = append(first, "first-response line "+strconv.Itoa(i))
	}
	firstText := strings.Join(first, "\n")
	m = feed(t, m, event.TokenDelta{Chunk: &content.TextChunk{Text: firstText}})
	m = feed(t, m, stepDone(aiMessage("", firstText)))
	m = feed(t, m, event.TurnDone{})
	frozen := len(m.heldLines)
	if frozen == 0 {
		t.Fatal("first response not held after finalize")
	}
	idleHeight := tailHeight(m)

	// Second turn: send, then stream a new response token by token. Track the tail height —
	// it must stay bounded by the budget and never collapse toward zero.
	m = feed(t, m, event.TurnStarted{Message: userMsg("second question")})
	minHeight := tailHeight(m)
	for i := 0; i < budget*2; i++ {
		m = feed(t, m, event.TokenDelta{Chunk: &content.TextChunk{Text: "second-response line " + strconv.Itoa(i) + "\n"}})
		h := tailHeight(m)
		if h < minHeight {
			minHeight = h
		}
		if h > budget+2 {
			t.Fatalf("tail overflowed the budget at token %d: height %d > budget %d (content would be clamped/lost)", i, h, budget)
		}
	}

	if minHeight < idleHeight/2 {
		t.Errorf("surface collapsed during the send handoff: min tail height %d, idle height %d (the input box would jump to the top)", minHeight, idleHeight)
	}
	if len(m.heldLines) >= frozen {
		t.Errorf("held tail did not drain while the new response streamed: %d -> %d (expected a gradual spill to scrollback)", frozen, len(m.heldLines))
	}
	if v := stripANSI(m.View().Content); !strings.Contains(v, "second-response line") {
		t.Errorf("new response not shown in the surface; view = %q", v)
	}
}

// TestNoOpEventLeavesStepHeld guards the hold logic: a finalizing event that commits nothing
// (a second TurnDone for the already-idle turn) must leave the held tail untouched — not
// flush it (which would collapse the surface) and not lose it.
func TestNoOpEventLeavesStepHeld(t *testing.T) {
	t.Parallel()

	m := runningScreen(t, &fakeAgent{})
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 40})
	m = feed(t, m, event.TokenDelta{Chunk: &content.TextChunk{Text: "held answer"}})
	m = feed(t, m, stepDone(aiMessage("", "held answer")))
	m = feed(t, m, event.TurnDone{})
	before := len(m.heldLines)
	if before == 0 {
		t.Fatal("precondition failed: step not held")
	}

	m = feed(t, m, event.TurnDone{})
	if len(m.heldLines) != before {
		t.Errorf("held tail changed on a no-op event: heldLines %d -> %d (must stay held)", before, len(m.heldLines))
	}
	if v := stripANSI(m.View().Content); !strings.Contains(v, "held answer") {
		t.Errorf("held step dropped from the surface; view = %q", v)
	}
}

// TestTallFinalizedStepBudgets covers the spill-equivalent for a finalized step delivered
// all at once (no token streaming, so the streaming spill never ran): a step TALLER than the
// live-tail budget must flush its oldest overflow to scrollback at commit and hold only the
// last budget lines, so the surface never overflows the terminal. A step that FITS the
// budget is held whole and flushes nothing.
func TestTallFinalizedStepBudgets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		lines        int  // narration lines delivered at the step boundary
		wantOverflow bool // an overflow flush command (to scrollback) is expected
	}{
		{name: "short step held whole", lines: 1, wantOverflow: false},
		{name: "tall step spills overflow, holds tail", lines: 200, wantOverflow: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := runningScreen(t, &fakeAgent{})
			m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
			budget := liveTailDisplayCap(m.surfaceInputs(""))
			if budget <= 0 {
				t.Fatalf("test setup: non-positive live-tail budget %d", budget)
			}

			narration := make([]string, 0, tt.lines)
			for i := 0; i < tt.lines; i++ {
				narration = append(narration, "narration line "+strconv.Itoa(i))
			}
			// Commit the step into the transcript, hold it, then drain to the budget. spillHeld's
			// return is the overflow print command (nil when the step is held whole).
			m.transcript = m.transcript.ApplyEvent(event.TurnStarted{})
			m.transcript = m.transcript.ApplyEvent(stepDone(aiMessage("", strings.Join(narration, "\n"))))
			m.hold(true)
			cmd := m.spillHeld()

			if got := cmd != nil; got != tt.wantOverflow {
				t.Errorf("overflow flush cmd present = %v, want %v (tall steps must spill their overflow to scrollback)", got, tt.wantOverflow)
			}
			if n := len(m.heldLines); n > budget {
				t.Errorf("held tail = %d lines, want <= budget %d (surface would overflow the terminal)", n, budget)
			}
		})
	}
}

// TestQuitFlushesHeldTail proves ctrl+c emits the held tail to scrollback before quitting:
// it lives in the managed region, which the renderer erases on close, so it must hand off to
// native history first or the last response vanishes on exit.
func TestQuitFlushesHeldTail(t *testing.T) {
	t.Parallel()

	m := runningScreen(t, &fakeAgent{})
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 40})
	m = feed(t, m, event.TokenDelta{Chunk: &content.TextChunk{Text: "last words"}})
	m = feed(t, m, stepDone(aiMessage("", "last words")))
	m = feed(t, m, event.TurnDone{})
	if len(m.heldLines) == 0 {
		t.Fatal("precondition failed: step not held before quit")
	}

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("ctrl+c produced no command")
	}
	if len(m.heldLines) != 0 {
		t.Errorf("held tail not consumed on quit: heldLines = %d (last response would vanish on exit)", len(m.heldLines))
	}
}
