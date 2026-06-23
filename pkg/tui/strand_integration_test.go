package tui

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
)

// cursorUpRe matches a CursorUp (CUU) escape — ESC[<n>A — emitted by ansi.CursorUp.
// The numeric parameter is optional: ansi.CursorUp(1) emits ESC[A (implicit 1), and
// ansi.CursorUp(n>1) emits ESC[<n>A. An absent number is therefore 1.
var cursorUpRe = regexp.MustCompile(`\x1b\[(\d*)A`)

// maxCursorUp returns the largest CursorUp distance n in the stream (the count of
// ESC[<n>A matches mapped to their n), and every n observed. An empty parameter is the
// implicit n=1. A stream with no CursorUp returns (0, nil).
func maxCursorUp(stream string) (int, []int) {
	matches := cursorUpRe.FindAllStringSubmatch(stream, -1)
	ups := make([]int, 0, len(matches))
	max := 0
	for _, m := range matches {
		n := 1 // ESC[A is implicit n=1
		if m[1] != "" {
			n, _ = strconv.Atoi(m[1])
		}
		ups = append(ups, n)
		if n > max {
			max = n
		}
	}
	return max, ups
}

// TestStrandNoClampingCursorUpOnTallCommit is the INTEGRATION lock for the TUI
// scrollback-strand fix: it drives the REAL Screen model through a tea.Program with the
// vendored, paged ultraviolet renderer, commits a step whose payload is TALLER than the
// terminal, and asserts the integrated system emits NO clamping cursor movement.
//
// The strand IS a clamped CursorUp. insertAbove (cursed_renderer.go) commits a chunk
// with CursorUp(offset+h-1); when offset+h > termRows the terminal clamps that move at
// the top of the screen and strands the managed input region into native scrollback. The
// fork now pages so every emitted chunk holds offset+h-1 <= termRows-1, and the app
// (clampSurfaceHeight) caps the rendered surface at termRows-1 so the normal frame
// repaint moves up at most h-1 <= termRows-2. So the exact, emulator-free no-strand
// invariant on the program's whole output byte stream is:
//
//	every CursorUp(n) (ESC[<n>A) emitted by the program has n <= termRows-1.
//
// A clamp-causing n >= termRows is the bug. This test is GREEN immediately because the
// fix is already vendored; RED was proven at the renderer level by the fork's
// TestInsertAboveSingleChunkWouldClamp (an unpaged single chunk emits CursorUp(19) >
// termRows-1). The in-test tallness sanity check below keeps THIS test meaningfully
// regression-locked: if a future change stops committing tall content the invariant
// would pass vacuously, so we assert the committed payload genuinely spans > Height rows.
//
// It reuses the program-driver harness from tool_handoff_test.go (syncBuf +
// blockingReader + a hand-delivered eventMsg turn).
func TestStrandNoClampingCursorUpOnTallCommit(t *testing.T) {
	const (
		termHeight = 24
		termWidth  = 80
	)

	// Build a step payload CLEARLY taller than termHeight rows: a long assistant
	// narration plus a tool call with a many-line result. The whole finalized group goes
	// through one StepDone → one tea.Println commit, which is the payload that used to
	// strand.
	narration := make([]string, 0, 40)
	for i := 1; i <= 40; i++ {
		narration = append(narration, "Narration line "+strconv.Itoa(i)+
			": detail about how the build step proceeded and what it found.")
	}
	narrationText := strings.Join(narration, "\n")

	resultLines := make([]string, 0, 30)
	for i := 1; i <= 30; i++ {
		resultLines = append(resultLines, "tool-result row "+strconv.Itoa(i))
	}
	resultText := strings.Join(resultLines, "\n")

	const sentinel = "Narration line 40"
	tallStep := event.StepDone{Messages: content.AgenticMessages{
		aiMessage("", narrationText, toolUse("tu-tall", "Build", `{}`)),
		toolResult("tu-tall", resultText),
	}}

	// Tallness sanity check (regression lock): render the SAME committed entries the
	// program's flush() prints to scrollback (renderEntry at expand=true so nothing is
	// folded) and count the physical rows. If a future change stops committing tall
	// content this drops to <= Height and fails LOUDLY rather than letting the CursorUp
	// invariant pass vacuously. RED for the underlying clamp was proven at the renderer
	// level by the fork's TestInsertAboveSingleChunkWouldClamp (an unpaged single chunk
	// emits CursorUp(19) > termRows-1).
	var tm transcriptModel
	tm = tm.ApplyEvent(event.TurnStarted{})
	tm = tm.ApplyEvent(tallStep)
	committedRows := 0
	for _, e := range tm.committed {
		committedRows += len(renderEntry(e, true, termWidth))
	}
	if committedRows <= termHeight {
		t.Fatalf("scenario not tall: committed payload renders %d rows <= Height %d "+
			"(no longer a strand repro — restore a payload taller than the terminal)",
			committedRows, termHeight)
	}

	out := &syncBuf{}
	in := newBlockingReader()

	sub := newFakeSubscription()
	defer func() { _ = sub.Close() }()
	agent := &fakeAgent{subStream: sub}
	screen := New(context.Background(), agent, fakeOpen(agent), AgentBanner{Name: "test"})

	prog := tea.NewProgram(
		screen,
		tea.WithInput(in),
		tea.WithOutput(out),
		tea.WithEnvironment([]string{"TERM=xterm-256color"}),
		tea.WithoutSignalHandler(),
		tea.WithFPS(60),
	)

	done := make(chan struct{})
	go func() {
		_, _ = prog.Run()
		close(done)
	}()

	settle := func() { time.Sleep(120 * time.Millisecond) }

	// Establish the frame. bubbletea's startup checkResize reads the non-TTY output as
	// 0x0 and clobbers the size, so send it, settle past that, then re-assert it.
	prog.Send(tea.WindowSizeMsg{Width: termWidth, Height: termHeight})
	settle()
	prog.Send(tea.WindowSizeMsg{Width: termWidth, Height: termHeight})
	settle()

	// Submit a message → starts the (blocked) turn, so status=Running.
	for _, r := range "tell me about the build" {
		prog.Send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	prog.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	settle()

	deliver := func(ev event.Event) {
		prog.Send(eventMsg{ev: ev})
		settle()
	}
	deliver(event.TurnStarted{})

	// The step boundary: StepDone snaps the finalized group into scrollback through one
	// tea.Println — the tall payload that used to strand the input box.
	deliver(tallStep)

	// Drive a couple of frames so the committed surface paints after the commit.
	for i := 0; i < 3; i++ {
		prog.Send(blinkMsg(time.Now()))
		settle()
	}

	prog.Quit()
	in.Close()
	<-done
	full := out.String()

	// 1. The committed content actually reached the output (not vacuously green): both
	//    the first and last narration lines must appear, proving the full span was
	//    emitted, not truncated by the app.
	if !strings.Contains(full, sentinel) {
		t.Fatalf("committed tall payload never reached scrollback: sentinel %q absent\n%q", sentinel, full)
	}
	if !strings.Contains(full, "Narration line 1:") {
		t.Fatalf("first narration line missing — committed payload was truncated, not tall\n%q", full)
	}

	// 2. THE INVARIANT: every CursorUp(n) emitted by the driven program satisfies
	//    n <= termHeight-1. A larger n is the clamp that strands the managed region.
	maxUp, ups := maxCursorUp(full)
	if maxUp > termHeight-1 {
		t.Fatalf("clamping CursorUp emitted: max n=%d > termHeight-1=%d (strand bug)\nall ups: %v",
			maxUp, termHeight-1, ups)
	}
	t.Logf("no-strand OK: tall payload (%d committed rows vs Height %d), %d CursorUp moves, max n=%d <= %d",
		committedRows, termHeight, len(ups), maxUp, termHeight-1)
}
