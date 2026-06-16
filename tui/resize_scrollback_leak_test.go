package tui

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// TestResizeScrollbackLeak locks the vendored ultraviolet renderer patch that
// fixes the inline-mode resize scrollback leak (see
// third_party/github.com/charmbracelet/ultraviolet/PATCH.md and CLAUDE.md).
//
// The bug: on a terminal resize, the renderer's current buffer (curbuf) was
// resized but only its newly-grown tail rows were synced from the new frame,
// leaving the rest holding STALE content from the previous (different-width)
// frame. The next render then diffed against that stale curbuf and
// partial-redrew full-width lines — e.g. the composer box's ┌…┐ / └…┘ borders —
// at an absolute column offset (CSI <col> C), stranding the prior, narrower
// border rows in native scrollback on every resize step.
//
// This test drives a real tea.Program (the patched renderer end-to-end) through
// a resize drag of a bordered-box composer frame and asserts that no horizontal
// border run is ever emitted at a CursorForward (CSI <col> C) column offset — a
// clean full redraw emits each border line whole from column 0.
func TestResizeScrollbackLeak(t *testing.T) {
	t.Parallel()

	// cufBorder matches a CSI <n> C (CursorForward) immediately followed by a
	// horizontal-rule / corner glyph. The vertical bar │ legitimately follows a
	// CUF on body lines (│<ECH-cleared interior>│), so it is intentionally
	// excluded; only an offset HORIZONTAL border run is the leak signature.
	cufBorder := regexp.MustCompile("\x1b\\[[0-9]+C[─┌┐└┘]")

	tests := []struct {
		name   string
		widths []int // resize drag sequence (each followed by a content tick)
	}{
		{name: "narrowing drag", widths: []int{78, 70, 64, 50, 40}},
		{name: "widening drag", widths: []int{40, 50, 64, 70, 78, 80}},
		{name: "oscillating drag", widths: []int{78, 70, 64, 50, 40, 64, 80, 50}},
		{name: "single narrow step", widths: []int{40}},
		{name: "single widen step", widths: []int{120}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			emitted := driveResizeDrag(t, tt.widths)
			if matches := cufBorder.FindAllString(emitted, -1); len(matches) > 0 {
				t.Fatalf("resize scrollback leak: %d CUF-offset border run(s) emitted %v\nfull output: %q",
					len(matches), matches, emitted)
			}
		})
	}
}

// driveResizeDrag runs a bordered-box composer model through a real tea.Program
// over the given width steps (each followed by a content tick that forces a
// diff render at unchanged bounds) and returns all bytes the renderer emitted.
// It sends one message at a time, waiting for each frame to flush, so the
// render sequence is deterministic.
func driveResizeDrag(t *testing.T, widths []int) string {
	t.Helper()

	out := &syncBuf{}
	in := newBlockingReader()
	prog := tea.NewProgram(
		leakBoxModel{},
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

	send := func(msg tea.Msg) {
		before := out.Len()
		prog.Send(msg)
		deadline := time.Now().Add(5 * time.Second)
		for out.Len() == before && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(5 * time.Millisecond) // allow the Flush to complete
	}

	// Establish an initial wide frame, then drive the drag.
	send(tea.WindowSizeMsg{Width: 80, Height: 24})
	for _, w := range widths {
		send(tea.WindowSizeMsg{Width: w, Height: 24})
		send(leakTickMsg{})
	}

	prog.Quit()
	in.Close()
	<-done
	return out.String()
}

// leakBoxModel renders a composer-like frame: a wrapped transcript (whose row
// count — and thus the box's vertical offset and total frame height — depends
// on width, exactly like the real transcript), a separator rule, and a
// full-width bordered input box at the bottom.
type leakBoxModel struct {
	w, h int
	tick int
}

func (m leakBoxModel) Init() tea.Cmd { return nil }

func (m leakBoxModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case leakTickMsg:
		m.tick++
	case tea.QuitMsg:
		return m, tea.Quit
	}
	return m, nil
}

func (m leakBoxModel) View() tea.View {
	if m.w < 4 {
		return tea.NewView("")
	}
	inner := m.w - 2
	top := "┌" + strings.Repeat("─", inner) + "┐"
	mid := "│" + strings.Repeat(" ", inner) + "│"
	bot := "└" + strings.Repeat("─", inner) + "┘"
	sep := strings.Repeat("─", m.w)

	words := strings.Repeat("alpha beta gamma delta epsilon zeta eta theta ", 6+m.tick)
	lines := wrapWords(strings.TrimSpace(words), m.w)
	lines = append(lines, sep, top, mid, bot)

	v := tea.NewView(strings.Join(lines, "\n"))
	v.AltScreen = false
	return v
}

type leakTickMsg struct{}

// wrapWords hard-wraps text into lines no wider than w columns (word-based).
func wrapWords(text string, w int) []string {
	if w < 4 {
		w = 4
	}
	var lines []string
	var cur strings.Builder
	for _, word := range strings.Fields(text) {
		switch {
		case cur.Len() == 0:
			cur.WriteString(word)
		case cur.Len()+1+len(word) > w:
			lines = append(lines, cur.String())
			cur.Reset()
			cur.WriteString(word)
		default:
			cur.WriteString(" ")
			cur.WriteString(word)
		}
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	return lines
}

// syncBuf is a goroutine-safe bytes.Buffer wrapper: the program writes frames
// from its render goroutine while the test reads length/contents.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Len()
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// blockingReader blocks Read until closed, keeping the program alive until the
// test calls Quit (so the program never exits on an early input EOF).
type blockingReader struct {
	mu     sync.Mutex
	ch     chan struct{}
	closed bool
}

func newBlockingReader() *blockingReader { return &blockingReader{ch: make(chan struct{})} }

func (b *blockingReader) Read(p []byte) (int, error) {
	<-b.ch
	return 0, fmt.Errorf("input closed")
}

func (b *blockingReader) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.closed = true
		close(b.ch)
	}
}
