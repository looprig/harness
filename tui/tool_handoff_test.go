package tui

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
)

// runningGlyphs is the spinner cell set (anim.spinnerFrames) — its presence in the
// emitted byte stream proves the running tool card actually painted live.
const runningGlyphs = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"

// TestToolRunningToCompletedHandoff drives a REAL Screen end-to-end through a
// tea.Program with the patched ultraviolet renderer and asserts the running→completed
// tool-card handoff composes as a CLEAN CONTINUATION, not a split. It is the
// regression lock for the live→committed seam (design Option B):
//
//   - The running tool card paints live (spinner glyph appears in the stream) and is a
//     compact ONE-LINE indicator — the multi-line live card (header + result body) is
//     what used to be removed all at once on commit, fracturing the handoff.
//   - On ToolCallCompleted the live tail shrinks (running card removed) AND the full
//     card is tea.Println-committed to scrollback in the same pass. The completed card
//     is inserted exactly ONCE (no duplicate), and the running-card body placeholder
//     ("(no output)") never reaches scrollback.
//
// The test is deterministic: it hand-delivers each turn event as an eventMsg (the
// agent reader blocks, keeping the turn Running) and settles between sends so each
// frame paints. It is the same end-to-end harness shape as the resize-leak test.
func TestToolRunningToCompletedHandoff(t *testing.T) {
	out := &syncBuf{}
	in := newBlockingReader()

	// Reader BLOCKS forever so the turn stays Running while events are hand-delivered
	// as eventMsg (so readNext never EOFs the turn or competes for ordering).
	block := make(chan struct{})
	defer close(block)
	agent := &fakeAgent{streamReader: llm.NewStreamReader(
		func() (event.Event, error) { <-block; return nil, io.EOF }, nil)}
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

	// Establish the frame. bubbletea's startup checkResize reads the non-TTY output
	// as 0x0 and clobbers the size, so send it, settle past that, then re-assert it.
	prog.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
	settle()
	prog.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
	settle()

	// Submit a message → starts the (blocked) turn, so status=Running.
	for _, r := range "weather?" {
		prog.Send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	prog.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	settle()

	deliver := func(ev event.Event) {
		prog.Send(eventMsg{ev: ev})
		settle()
	}

	deliver(event.TurnStarted{})
	deliver(event.TokenDelta{Chunk: &content.TextChunk{Text: "Let me fetch the weather.\n"}})
	deliver(event.ToolCallStarted{CallID: callID(1), ToolName: "Fetch", Summary: "GET weather.com"})

	// Drive blink ticks so the running card visibly paints (as it does live).
	for i := 0; i < 3; i++ {
		prog.Send(blinkMsg(time.Now()))
		settle()
	}
	preComplete := out.String()

	deliver(event.ToolCallCompleted{
		CallID: callID(1),
		ResultPreview: "HTTP 200 OK\nContent-Type: text/html\nServer: nginx\n" +
			"Date: today\nX-Cache: HIT\nbody body body",
	})

	prog.Quit()
	in.Close()
	<-done
	full := out.String()

	// 1. The running card painted live (the spinner reached the screen).
	if !strings.ContainsAny(preComplete, runningGlyphs) {
		t.Fatalf("running tool card never painted: no spinner glyph in pre-completion stream\n%q", preComplete)
	}

	// 2. The running card was a COMPACT one-line indicator: while running, the live
	//    tail must NOT carry the "(no output)" result-body placeholder. That body is
	//    the multi-line live content whose all-at-once removal on commit fractured the
	//    handoff; Option B drops it so the live indicator is a single line.
	if strings.Contains(preComplete, noOutput) {
		t.Errorf("running tool card showed the %q body placeholder live; want a compact one-line indicator\n%q",
			noOutput, preComplete)
	}

	// 3. The completed card committed to scrollback exactly once (no duplicate/split),
	//    and only the RESOLVED (✓) card — never a running glyph — reaches scrollback.
	if got := strings.Count(full, "└ Fetch  GET weather.com"); got != 2 {
		// Two header occurrences total: the live running indicator + the one committed
		// card. A third would be a duplicate/split.
		t.Errorf("Fetch card header appeared %d times in the full stream, want 2 (one live, one committed)\n%q",
			got, full)
	}
	if !strings.Contains(full, glyphOK) {
		t.Errorf("completed card with %q glyph never committed to scrollback\n%q", glyphOK, full)
	}
}
