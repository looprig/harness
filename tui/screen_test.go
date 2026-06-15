package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/inventivepotter/urvi/tui/components"
)

// compile-time assertion that the test double satisfies the (widened) Agent
// interface; if a method is added or its signature drifts, this fails to build.
var _ Agent = (*fakeAgent)(nil)

// fakeAgent is a scriptable Agent test double. It records calls and returns the
// configured reader/error/bool so Screen behavior can be exercised without a real
// session.
type fakeAgent struct {
	streamReader *llm.StreamReader[event.Event]
	streamErr    error

	interruptCancelled bool
	interruptErr       error

	closeCalled  bool
	closeErr     error
	acceptsImage bool

	// gate-trio recorders: the configured error is returned, and the last call's
	// arguments are captured so a test can assert the wrapper forwarded them.
	approveErr error
	denyErr    error
	answerErr  error
	lastCallID uuid.UUID
	lastScope  tool.ApprovalScope
	lastAnswer string
}

func (f *fakeAgent) StreamBlocks(_ context.Context, _ []content.Block) (*llm.StreamReader[event.Event], error) {
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	return f.streamReader, nil
}

func (f *fakeAgent) Interrupt(_ context.Context) (bool, error) {
	return f.interruptCancelled, f.interruptErr
}

func (f *fakeAgent) Close(_ context.Context) error {
	f.closeCalled = true
	return f.closeErr
}

func (f *fakeAgent) AcceptsImages() bool { return f.acceptsImage }

func (f *fakeAgent) Approve(_ context.Context, callID uuid.UUID, scope tool.ApprovalScope) error {
	f.lastCallID = callID
	f.lastScope = scope
	return f.approveErr
}

func (f *fakeAgent) Deny(_ context.Context, callID uuid.UUID) error {
	f.lastCallID = callID
	return f.denyErr
}

func (f *fakeAgent) ProvideAnswer(_ context.Context, callID uuid.UUID, answer string) error {
	f.lastCallID = callID
	f.lastAnswer = answer
	return f.answerErr
}

// scriptedReader builds a StreamReader that yields the given events in order,
// then io.EOF on every subsequent call.
func scriptedReader(evs ...event.Event) *llm.StreamReader[event.Event] {
	i := 0
	next := func() (event.Event, error) {
		if i >= len(evs) {
			return nil, io.EOF
		}
		ev := evs[i]
		i++
		return ev, nil
	}
	return llm.NewStreamReader(next, nil)
}

// fakeOpen returns an OpenAgent thunk that yields the given agent.
func fakeOpen(a Agent) OpenAgent {
	return func(context.Context) (Agent, error) { return a, nil }
}

func TestNew(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))

	if m.status != StatusIdle {
		t.Errorf("New status = %d, want StatusIdle (%d)", m.status, StatusIdle)
	}
	if m.Agent() != agent {
		t.Errorf("New Agent() = %p, want %p", m.Agent(), agent)
	}
}

func TestInit(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))

	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init() = nil, want non-nil cmd")
	}
}

func TestScreenIsTeaModel(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	var _ tea.Model = New(context.Background(), agent, fakeOpen(agent))
}

func TestStartTurn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		agent      *fakeAgent
		wantCmd    bool
		wantOK     bool
		wantStatus Status
		wantReader bool
		wantErrRow bool
	}{
		{
			name:       "success returns cmd and running",
			agent:      &fakeAgent{streamReader: scriptedReader(event.TurnStarted{})},
			wantCmd:    true,
			wantOK:     true,
			wantStatus: StatusRunning,
			wantReader: true,
			wantErrRow: false,
		},
		{
			name:       "failure appends error and stays idle",
			agent:      &fakeAgent{streamErr: errors.New("boom")},
			wantCmd:    false,
			wantOK:     false,
			wantStatus: StatusIdle,
			wantReader: false,
			wantErrRow: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := New(context.Background(), tt.agent, fakeOpen(tt.agent))
			cmd, ok := m.startTurn([]content.Block{&content.TextBlock{Text: "hi"}})

			if (cmd != nil) != tt.wantCmd {
				t.Errorf("startTurn cmd != nil = %v, want %v", cmd != nil, tt.wantCmd)
			}
			if ok != tt.wantOK {
				t.Errorf("startTurn ok = %v, want %v", ok, tt.wantOK)
			}
			if m.status != tt.wantStatus {
				t.Errorf("status = %d, want %d", m.status, tt.wantStatus)
			}
			if (m.reader != nil) != tt.wantReader {
				t.Errorf("reader != nil = %v, want %v", m.reader != nil, tt.wantReader)
			}
			if tt.wantErrRow {
				if len(m.messages) == 0 {
					t.Fatal("expected a RoleError row, got no messages")
				}
				last := m.messages[len(m.messages)-1]
				if last.Role != RoleError {
					t.Errorf("last message role = %d, want RoleError (%d)", last.Role, RoleError)
				}
			} else if len(m.messages) != 0 {
				t.Errorf("expected no messages, got %d", len(m.messages))
			}
		})
	}
}

func TestWindowSizeMsg(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))

	// Before any WindowSizeMsg, the view is empty (not ready).
	if v := m.View().Content; v != "" {
		t.Errorf("View() before ready = %q, want empty", v)
	}

	model, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got, ok := model.(Screen)
	if !ok {
		t.Fatalf("Update returned %T, want Screen", model)
	}
	if got.width != 80 || got.height != 24 {
		t.Errorf("width,height = %d,%d, want 80,24", got.width, got.height)
	}
	if !got.ready {
		t.Error("ready = false after WindowSizeMsg, want true")
	}
	if v := got.View().Content; v == "" {
		t.Error("View() after ready = empty, want non-empty")
	}
}

// TestViewScrollbackFirstInvariant pins the scrollback-first guarantee at the
// place it now lives: Screen.View() must return a tea.View that keeps the program
// on the NORMAL screen (AltScreen == false) and never captures the mouse
// (MouseMode == tea.MouseModeNone). Both are the v2 zero values, but asserting them
// here — on the actual View() output — proves the intent is realized rather than
// merely defaulted, and catches any future code that flips an alt-screen/mouse field.
// Checked both before any WindowSizeMsg (the not-ready empty view) and after one
// (the composed view), since either could in principle set those fields.
func TestViewScrollbackFirstInvariant(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		resize *tea.WindowSizeMsg // nil = no WindowSizeMsg (view not yet ready)
	}{
		{name: "before window size (not ready)", resize: nil},
		{name: "after window size (ready, composed)", resize: &tea.WindowSizeMsg{Width: 80, Height: 24}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent))
			if tt.resize != nil {
				m, _ = updateScreen(t, m, *tt.resize)
			}

			v := m.View()
			if v.AltScreen {
				t.Error("View().AltScreen = true, want false (scrollback-first stays on the normal screen so tea.Println writes to native scrollback)")
			}
			if v.MouseMode != tea.MouseModeNone {
				t.Errorf("View().MouseMode = %v, want tea.MouseModeNone (scrollback-first never captures the mouse)", v.MouseMode)
			}
		})
	}
}

func TestAppendError(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.appendError(errors.New("kaboom"))

	if len(m.messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(m.messages))
	}
	row := m.messages[0]
	if row.Role != RoleError {
		t.Errorf("role = %d, want RoleError", row.Role)
	}
	tb, ok := row.Blocks[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("block[0] = %T, want *content.TextBlock", row.Blocks[0])
	}
	if tb.Text != "kaboom" {
		t.Errorf("text = %q, want %q", tb.Text, "kaboom")
	}
}

// updateScreen drives m.Update with msg and returns the concrete Screen plus the
// cmd, failing the test if the model is not a Screen.
func updateScreen(t *testing.T, m Screen, msg tea.Msg) (Screen, tea.Cmd) {
	t.Helper()
	model, cmd := m.Update(msg)
	got, ok := model.(Screen)
	if !ok {
		t.Fatalf("Update returned %T, want Screen", model)
	}
	return got, cmd
}

// firstTextBlock returns the text of the first *content.TextBlock in row, or "".
func firstTextBlock(t *testing.T, row DisplayMessage) string {
	t.Helper()
	if len(row.Blocks) == 0 {
		return ""
	}
	tb, ok := row.Blocks[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("block[0] = %T, want *content.TextBlock", row.Blocks[0])
	}
	return tb.Text
}

// TestViewNoLineExceedsWidth guards against a rendered line wider than the
// terminal: such a line wraps visually (which lipgloss.Height cannot see), pushing
// the real frame past the terminal height and stacking stale chrome (the
// "multiplying" artifact). Every composed line must fit the width.
// TestViewHeightIsConstant covers the anti-stacking invariant: the composed View is
// always exactly the terminal height regardless of turn status. If the frame height
// fluctuates (e.g. a status line that appears only while running), shrinking frames
// leave stale chrome that bubbletea does not clear — the "multiplying"/doubled-input
// artifact.
func TestViewHeightIsConstant(t *testing.T) {
	t.Parallel()

	const w, h = 60, 18
	for _, st := range []Status{StatusIdle, StatusRunning, StatusInterrupting, StatusResetting} {
		agent := &fakeAgent{}
		m := New(context.Background(), agent, fakeOpen(agent))
		m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: w, Height: h})
		m.status = st
		m.refreshHistory()
		if got := lipgloss.Height(m.View().Content); got != h {
			t.Errorf("status %v: View height = %d, want exactly %d", st, got, h)
		}
	}
}

func TestViewNoLineExceedsWidth(t *testing.T) {
	t.Parallel()

	const w, h = 40, 20
	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: w, Height: h})

	long := strings.Repeat("supercalifragilistic ", 20)
	m.messages = append(m.messages,
		DisplayMessage{Role: RoleUser, Blocks: []content.Block{&content.TextBlock{Text: long}}},
		DisplayMessage{Role: RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: long}}},
	)
	m.refreshHistory()

	for i, ln := range strings.Split(m.View().Content, "\n") {
		if got := lipgloss.Width(ln); got > w {
			t.Errorf("View line %d width = %d, want <= %d: %q", i, got, w, ln)
		}
	}
}

func TestUpdateTokenDeltaAccumulates(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.reader = scriptedReader() // readNext targets must be non-nil
	m.status = StatusRunning

	m, cmd := updateScreen(t, m, eventMsg{ev: event.TokenDelta{Chunk: &content.TextChunk{Text: "ab"}}})
	if cmd == nil {
		t.Error("TokenDelta cmd = nil, want non-nil (readNext)")
	}
	m, _ = updateScreen(t, m, eventMsg{ev: event.TokenDelta{Chunk: &content.TextChunk{Text: "cd"}}})

	if m.live.text != "abcd" {
		t.Errorf("live.text = %q, want %q", m.live.text, "abcd")
	}
}

// TestUpdateThinkingChunkAccumulates covers the redesign: ThinkingChunk TokenDeltas
// accumulate into the live segment's thinking buffer (they used to be discarded) and
// leave the narration text untouched.
func TestUpdateThinkingChunkAccumulates(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.reader = scriptedReader()
	m.status = StatusRunning
	m.live.text = "keep"

	m, _ = updateScreen(t, m, eventMsg{ev: event.TokenDelta{Chunk: &content.ThinkingChunk{Thinking: "rea"}}})
	m, _ = updateScreen(t, m, eventMsg{ev: event.TokenDelta{Chunk: &content.ThinkingChunk{Thinking: "soning"}}})

	if m.live.thinking != "reasoning" {
		t.Errorf("live.thinking = %q, want %q", m.live.thinking, "reasoning")
	}
	if m.live.text != "keep" {
		t.Errorf("live.text = %q, want unchanged %q", m.live.text, "keep")
	}
}

// TestThinkingCommittedAsBlock covers committing a streamed thinking+text segment:
// the committed assistant row carries a ThinkingBlock (the reasoning) and a TextBlock
// (the narration), so both the streamed and final-message paths render thinking the
// same way.
func TestThinkingCommittedAsBlock(t *testing.T) {
	t.Parallel()

	m := runningScreen(t)
	m = feed(t, m, event.TokenDelta{Chunk: &content.ThinkingChunk{Thinking: "because reasons"}})
	m = feed(t, m, event.TokenDelta{Chunk: &content.TextChunk{Text: "the answer"}})
	m = feed(t, m, event.TurnDone{})

	if len(m.messages) == 0 {
		t.Fatal("no committed messages after TurnDone")
	}
	last := m.messages[len(m.messages)-1]
	var thinking, text string
	for _, b := range last.Blocks {
		switch bb := b.(type) {
		case *content.ThinkingBlock:
			thinking = bb.Thinking
		case *content.TextBlock:
			text = bb.Text
		}
	}
	if thinking != "because reasons" {
		t.Errorf("committed ThinkingBlock = %q, want %q (blocks=%+v)", thinking, "because reasons", last.Blocks)
	}
	if text != "the answer" {
		t.Errorf("committed TextBlock = %q, want %q (blocks=%+v)", text, "the answer", last.Blocks)
	}
}

// TestUpdateTurnDone exercises the Task 2.4 rule: the live segment is
// authoritative and ev.Message is a fallback only — never both — so a streamed
// turn never duplicates its final text from the carried message (design §6).
// callID returns a deterministic non-zero UUID for a test, distinguishing tool
// calls by a single byte so CallID correlation can be asserted.
func callID(b byte) uuid.UUID {
	var u uuid.UUID
	u[0] = b
	return u
}

// runningScreen returns a fresh Screen wired for handleEvent: a non-nil reader
// (readNext targets must be non-nil) and StatusRunning.
func runningScreen(t *testing.T) Screen {
	t.Helper()
	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.reader = scriptedReader()
	m.status = StatusRunning
	return m
}

// feed drives one synthetic event through Update and returns the new Screen.
func feed(t *testing.T, m Screen, ev event.Event) Screen {
	t.Helper()
	m, _ = updateScreen(t, m, eventMsg{ev: ev})
	return m
}

// assistantSegments returns the RoleAssistant rows of the transcript in order.
func assistantSegments(msgs []DisplayMessage) []DisplayMessage {
	var out []DisplayMessage
	for _, msg := range msgs {
		if msg.Role == RoleAssistant {
			out = append(out, msg)
		}
	}
	return out
}

// TestSplitLines covers the result-preview splitter.
func TestSplitLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty yields nil", in: "", want: nil},
		{name: "single line", in: "one", want: []string{"one"}},
		{name: "two lines", in: "a\nb", want: []string{"a", "b"}},
		{name: "trailing newline keeps empty tail", in: "a\n", want: []string{"a", ""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := splitLines(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("splitLines(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("splitLines(%q)[%d] = %q, want %q", tt.in, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestCommitLive covers the commit helper directly: it carries both text and
// calls, commits a calls-only (bare) segment, resets live, and no-ops when empty.
func TestCommitLive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		live        liveSegment
		wantSegment bool
		wantText    string
		wantCalls   int
	}{
		{
			name:        "text and calls both survive",
			live:        liveSegment{text: "narration", calls: []ToolCallView{{CallID: callID(1)}}},
			wantSegment: true,
			wantText:    "narration",
			wantCalls:   1,
		},
		{
			name:        "calls only commits bare segment with nil blocks",
			live:        liveSegment{calls: []ToolCallView{{CallID: callID(2)}}},
			wantSegment: true,
			wantText:    "",
			wantCalls:   1,
		},
		{
			name:        "text only",
			live:        liveSegment{text: "hi"},
			wantSegment: true,
			wantText:    "hi",
			wantCalls:   0,
		},
		{
			name:        "empty is no-op",
			live:        liveSegment{},
			wantSegment: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent))
			m.live = tt.live

			m.commitLive()

			if m.live.text != "" || len(m.live.calls) > 0 {
				t.Errorf("live not reset: %+v", m.live)
			}
			if !tt.wantSegment {
				if len(m.messages) != 0 {
					t.Fatalf("expected no segment, got %d", len(m.messages))
				}
				return
			}
			if len(m.messages) != 1 {
				t.Fatalf("messages len = %d, want 1", len(m.messages))
			}
			seg := m.messages[0]
			if seg.Role != RoleAssistant {
				t.Errorf("role = %d, want RoleAssistant", seg.Role)
			}
			if got := firstTextBlock(t, seg); got != tt.wantText {
				t.Errorf("text = %q, want %q", got, tt.wantText)
			}
			if tt.wantText == "" && seg.Blocks != nil {
				t.Errorf("bare segment Blocks = %v, want nil", seg.Blocks)
			}
			if len(seg.ToolCalls) != tt.wantCalls {
				t.Errorf("ToolCalls len = %d, want %d", len(seg.ToolCalls), tt.wantCalls)
			}
		})
	}
}

// TestTokenDeltaCommitsAfterTools covers Task 2.1: narration after a tool batch
// commits the prior segment then starts fresh; plain text→text just accumulates.
func TestTokenDeltaCommitsAfterTools(t *testing.T) {
	t.Parallel()

	t.Run("text then tools then text splits", func(t *testing.T) {
		t.Parallel()

		m := runningScreen(t)
		m = feed(t, m, event.TokenDelta{Chunk: &content.TextChunk{Text: "before "}})
		m = feed(t, m, event.ToolCallStarted{CallID: callID(1), ToolName: "ReadFile"})
		m = feed(t, m, event.ToolCallCompleted{CallID: callID(1)})
		// New narration after the (completed) batch must commit the first segment.
		m = feed(t, m, event.TokenDelta{Chunk: &content.TextChunk{Text: "after"}})

		if len(m.messages) != 1 {
			t.Fatalf("committed segments = %d, want 1", len(m.messages))
		}
		first := m.messages[0]
		if got := firstTextBlock(t, first); got != "before " {
			t.Errorf("first segment text = %q, want %q", got, "before ")
		}
		if len(first.ToolCalls) != 1 {
			t.Fatalf("first segment cards = %d, want 1", len(first.ToolCalls))
		}
		if m.live.text != "after" {
			t.Errorf("live.text = %q, want %q", m.live.text, "after")
		}
		if len(m.live.calls) != 0 {
			t.Errorf("live.calls len = %d, want 0 (fresh segment)", len(m.live.calls))
		}
	})

	t.Run("plain text accumulates into one live", func(t *testing.T) {
		t.Parallel()

		m := runningScreen(t)
		m = feed(t, m, event.TokenDelta{Chunk: &content.TextChunk{Text: "ab"}})
		m = feed(t, m, event.TokenDelta{Chunk: &content.TextChunk{Text: "cd"}})

		if len(m.messages) != 0 {
			t.Errorf("committed segments = %d, want 0 (no calls present)", len(m.messages))
		}
		if m.live.text != "abcd" {
			t.Errorf("live.text = %q, want %q", m.live.text, "abcd")
		}
	})
}

// TestToolCallStarted covers Task 2.2: parallel batches stay in one segment;
// back-to-back terminal batches (no narration between) split.
func TestToolCallStarted(t *testing.T) {
	t.Parallel()

	t.Run("parallel batch attaches to one segment", func(t *testing.T) {
		t.Parallel()

		m := runningScreen(t)
		// Two starts before any completion: both still running → same segment.
		m = feed(t, m, event.ToolCallStarted{CallID: callID(1), ToolName: "A"})
		m = feed(t, m, event.ToolCallStarted{CallID: callID(2), ToolName: "B"})

		if len(m.messages) != 0 {
			t.Fatalf("committed segments = %d, want 0 (parallel batch not split)", len(m.messages))
		}
		if len(m.live.calls) != 2 {
			t.Fatalf("live.calls len = %d, want 2", len(m.live.calls))
		}
	})

	t.Run("back-to-back terminal batches split", func(t *testing.T) {
		t.Parallel()

		// text → tool(completed) → tool(no text between) → text → done = 3 segments.
		m := runningScreen(t)
		m = feed(t, m, event.TokenDelta{Chunk: &content.TextChunk{Text: "narr"}})
		m = feed(t, m, event.ToolCallStarted{CallID: callID(1), ToolName: "A"})
		m = feed(t, m, event.ToolCallCompleted{CallID: callID(1)})
		// Second batch starts with the first all-terminal and NO narration → split.
		m = feed(t, m, event.ToolCallStarted{CallID: callID(2), ToolName: "B"})
		m = feed(t, m, event.ToolCallCompleted{CallID: callID(2)})
		m = feed(t, m, event.TokenDelta{Chunk: &content.TextChunk{Text: "tail"}})
		m = feed(t, m, event.TurnDone{})

		segs := assistantSegments(m.messages)
		if len(segs) != 3 {
			t.Fatalf("assistant segments = %d, want 3", len(segs))
		}
		if got := firstTextBlock(t, segs[0]); got != "narr" || len(segs[0].ToolCalls) != 1 {
			t.Errorf("seg0 = text %q cards %d, want %q / 1", got, len(segs[0].ToolCalls), "narr")
		}
		// seg1 is the bare back-to-back batch: no text, one card.
		if firstTextBlock(t, segs[1]) != "" || len(segs[1].ToolCalls) != 1 {
			t.Errorf("seg1 = text %q cards %d, want bare with 1 card", firstTextBlock(t, segs[1]), len(segs[1].ToolCalls))
		}
		if got := firstTextBlock(t, segs[2]); got != "tail" || len(segs[2].ToolCalls) != 0 {
			t.Errorf("seg2 = text %q cards %d, want %q / 0", got, len(segs[2].ToolCalls), "tail")
		}
	})
}

// TestToolCallCompleted covers Task 2.3: status flip + result by CallID, the
// committed-segment fallback, unknown-CallID no-op, and the failure card.
func TestToolCallCompleted(t *testing.T) {
	t.Parallel()

	t.Run("success flips matching card to OK with result", func(t *testing.T) {
		t.Parallel()

		m := runningScreen(t)
		m = feed(t, m, event.ToolCallStarted{CallID: callID(1), ToolName: "ReadFile"})
		m = feed(t, m, event.ToolCallStarted{CallID: callID(2), ToolName: "Bash"})
		m = feed(t, m, event.ToolCallCompleted{CallID: callID(2), ResultPreview: "out1\nout2"})

		if len(m.live.calls) != 2 {
			t.Fatalf("live.calls len = %d, want 2", len(m.live.calls))
		}
		if m.live.calls[0].Status != ToolRunning {
			t.Errorf("card 0 status = %d, want ToolRunning (untouched)", m.live.calls[0].Status)
		}
		c := m.live.calls[1]
		if c.Status != ToolOK {
			t.Errorf("card 1 status = %d, want ToolOK", c.Status)
		}
		if len(c.Result) != 2 || c.Result[0] != "out1" || c.Result[1] != "out2" {
			t.Errorf("card 1 Result = %#v, want [out1 out2]", c.Result)
		}
	})

	t.Run("error flips card to ToolError", func(t *testing.T) {
		t.Parallel()

		m := runningScreen(t)
		m = feed(t, m, event.ToolCallStarted{CallID: callID(1), ToolName: "Bash"})
		m = feed(t, m, event.ToolCallCompleted{CallID: callID(1), IsError: true, ResultPreview: "boom"})

		if m.live.calls[0].Status != ToolError {
			t.Errorf("status = %d, want ToolError", m.live.calls[0].Status)
		}
		if len(m.live.calls[0].Result) != 1 || m.live.calls[0].Result[0] != "boom" {
			t.Errorf("Result = %#v, want [boom]", m.live.calls[0].Result)
		}
	})

	t.Run("unknown CallID is a no-op", func(t *testing.T) {
		t.Parallel()

		m := runningScreen(t)
		m = feed(t, m, event.ToolCallStarted{CallID: callID(1), ToolName: "A"})
		m = feed(t, m, event.ToolCallCompleted{CallID: callID(9), ResultPreview: "ignored"})

		// The known card is untouched: still running, no result.
		c := m.live.calls[0]
		if c.Status != ToolRunning || c.Result != nil {
			t.Errorf("card mutated by unknown CallID: status %d result %#v, want ToolRunning nil", c.Status, c.Result)
		}
	})

	t.Run("completed updates committed-segment fallback", func(t *testing.T) {
		t.Parallel()

		// Commit a segment carrying a still-running card, then complete it.
		m := runningScreen(t)
		m = feed(t, m, event.ToolCallStarted{CallID: callID(1), ToolName: "A"})
		m.commitLive() // simulate the card living in a committed segment
		m = feed(t, m, event.ToolCallCompleted{CallID: callID(1), IsError: false, ResultPreview: "done"})

		segs := assistantSegments(m.messages)
		if len(segs) != 1 || len(segs[0].ToolCalls) != 1 {
			t.Fatalf("want 1 committed segment with 1 card; got %d segs", len(segs))
		}
		c := segs[0].ToolCalls[0]
		if c.Status != ToolOK || len(c.Result) != 1 || c.Result[0] != "done" {
			t.Errorf("fallback card = %+v, want ToolOK [done]", c)
		}
	})

	t.Run("pre-execution failure card", func(t *testing.T) {
		t.Parallel()

		// Started + Completed{IsError} with no execution → a ✗ card with the error
		// (covers denied/invalid/unknown/WriteTarget — design §5).
		m := runningScreen(t)
		m = feed(t, m, event.ToolCallStarted{CallID: callID(1), ToolName: "WriteFile", Summary: "config.yaml"})
		m = feed(t, m, event.ToolCallCompleted{CallID: callID(1), IsError: true, ResultPreview: "error: permission denied"})

		c := m.live.calls[0]
		if c.Status != ToolError {
			t.Errorf("failure card status = %d, want ToolError", c.Status)
		}
		if len(c.Result) != 1 || c.Result[0] != "error: permission denied" {
			t.Errorf("failure card Result = %#v, want [error: permission denied]", c.Result)
		}
	})
}

// TestTurnFailedCommitsLive covers Task 2.5: a TurnFailed after a tool batch
// commits the segment (tool work visible) then appends the RoleError.
func TestTurnFailedCommitsLive(t *testing.T) {
	t.Parallel()

	m := runningScreen(t)
	m = feed(t, m, event.TokenDelta{Chunk: &content.TextChunk{Text: "working"}})
	m = feed(t, m, event.ToolCallStarted{CallID: callID(1), ToolName: "Bash"})
	m = feed(t, m, event.ToolCallCompleted{CallID: callID(1), ResultPreview: "ok"})
	m = feed(t, m, event.TurnFailed{Err: errors.New("boom")})

	if len(m.messages) != 2 {
		t.Fatalf("messages len = %d, want 2 (assistant segment + error)", len(m.messages))
	}
	seg := m.messages[0]
	if seg.Role != RoleAssistant || firstTextBlock(t, seg) != "working" || len(seg.ToolCalls) != 1 {
		t.Errorf("seg = role %d text %q cards %d, want assistant 'working' 1 card", seg.Role, firstTextBlock(t, seg), len(seg.ToolCalls))
	}
	if m.messages[1].Role != RoleError || firstTextBlock(t, m.messages[1]) != "boom" {
		t.Errorf("err row = role %d text %q, want RoleError 'boom'", m.messages[1].Role, firstTextBlock(t, m.messages[1]))
	}
	if m.live.text != "" || len(m.live.calls) != 0 {
		t.Errorf("live not cleared: %+v", m.live)
	}
}

// TestTurnInterruptedKeepsCards covers Task 2.5: interrupt mid-tool marks the
// running card ToolCancelled and commits it (cards survive) before the tombstone.
func TestTurnInterruptedKeepsCards(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.reader = scriptedReader()
	m.status = StatusInterrupting
	m = feed(t, m, event.TokenDelta{Chunk: &content.TextChunk{Text: "let me check"}})
	m = feed(t, m, event.ToolCallStarted{CallID: callID(1), ToolName: "Bash"})
	// Interrupt while the tool is still running.
	m = feed(t, m, event.TurnInterrupted{})

	if len(m.messages) != 2 {
		t.Fatalf("messages len = %d, want 2 (segment + tombstone)", len(m.messages))
	}
	seg := m.messages[0]
	if seg.Role != RoleAssistant {
		t.Fatalf("messages[0] role = %d, want RoleAssistant", seg.Role)
	}
	if firstTextBlock(t, seg) != "let me check" {
		t.Errorf("segment text = %q, want %q", firstTextBlock(t, seg), "let me check")
	}
	if len(seg.ToolCalls) != 1 {
		t.Fatalf("segment cards = %d, want 1 (card survives interrupt)", len(seg.ToolCalls))
	}
	if seg.ToolCalls[0].Status != ToolCancelled {
		t.Errorf("card status = %d, want ToolCancelled", seg.ToolCalls[0].Status)
	}
	if m.messages[1].Role != RoleInterrupted || m.messages[1].Blocks != nil {
		t.Errorf("tombstone = role %d blocks %v, want RoleInterrupted nil", m.messages[1].Role, m.messages[1].Blocks)
	}
	if m.live.text != "" || len(m.live.calls) != 0 {
		t.Errorf("live not cleared: %+v", m.live)
	}
}

// TestQueueIndicesSurviveSegmentCommits covers Task 2.5's invariant: the extra
// assistant-segment commits during a turn must not disturb queued RoleUser rows'
// DisplayIndex mapping. A queued input's DisplayIndex must still point at its
// RoleUser row after segments commit ahead of, and the turn ends behind, it.
func TestQueueIndicesSurviveSegmentCommits(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.reader = scriptedReader()
	m.status = StatusRunning

	// The active user row (index 0) already exists for the running turn.
	m.messages = []DisplayMessage{{Role: RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "active"}}}}

	// Stream the first segment of the turn (text + a completed tool batch),
	// then a second segment of narration — two commits land at indices 1 and 2.
	m = feed(t, m, event.TokenDelta{Chunk: &content.TextChunk{Text: "step 1"}})
	m = feed(t, m, event.ToolCallStarted{CallID: callID(1), ToolName: "Bash"})
	m = feed(t, m, event.ToolCallCompleted{CallID: callID(1), ResultPreview: "ok"})
	m = feed(t, m, event.TokenDelta{Chunk: &content.TextChunk{Text: "step 2"}})

	// Now the user queues an input mid-turn (Running): its RoleUser row appends
	// at the current tail and queue[0].DisplayIndex records that tail index.
	m.input.SetValue("queued question")
	m, _ = updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(m.queue) != 1 {
		t.Fatalf("queue len = %d, want 1", len(m.queue))
	}
	idx := m.queue[0].DisplayIndex
	if idx < 0 || idx >= len(m.messages) {
		t.Fatalf("queue DisplayIndex = %d, out of range len %d", idx, len(m.messages))
	}
	if m.messages[idx].Role != RoleUser {
		t.Errorf("messages[%d].Role = %d, want RoleUser (queue index drifted)", idx, m.messages[idx].Role)
	}
	if got := firstTextBlock(t, m.messages[idx]); got != "queued question" {
		t.Errorf("queued row text = %q, want %q", got, "queued question")
	}

	// Finish the turn with a final segment — another commit lands AHEAD of the
	// queued row only if we appended out of order; since commits append at the
	// tail, the queued row's index must still resolve to it.
	m = feed(t, m, event.TurnDone{Message: nil})
	if m.messages[idx].Role != RoleUser {
		t.Errorf("after TurnDone messages[%d].Role = %d, want RoleUser", idx, m.messages[idx].Role)
	}
	if got := firstTextBlock(t, m.messages[idx]); got != "queued question" {
		t.Errorf("after TurnDone queued row text = %q, want %q", got, "queued question")
	}
}

func TestUpdateTurnDone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		msg         *content.AIMessage
		live        string
		wantSegment bool   // expect a final RoleAssistant row
		wantText    string // its first text-block text ("" → expect nil Blocks)
	}{
		{
			// live authoritative: the streamed text wins, ev.Message is ignored.
			name:        "live wins over message",
			msg:         &content.AIMessage{Message: content.Message{Blocks: []content.Block{&content.TextBlock{Text: "msg"}}}},
			live:        "streamed",
			wantSegment: true,
			wantText:    "streamed",
		},
		{
			// fallback: empty live + a non-nil message → one segment from Message.Blocks.
			name:        "empty live falls back to message blocks",
			msg:         &content.AIMessage{Message: content.Message{Blocks: []content.Block{&content.TextBlock{Text: "fallback"}}}},
			live:        "",
			wantSegment: true,
			wantText:    "fallback",
		},
		{
			// empty live + nil message → NO final segment.
			name:        "empty live nil message no segment",
			msg:         nil,
			live:        "",
			wantSegment: false,
		},
		{
			// empty live + message with no blocks → NO final segment (nothing to show).
			name:        "empty live empty message no segment",
			msg:         &content.AIMessage{Message: content.Message{Blocks: nil}},
			live:        "",
			wantSegment: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent))
			m.reader = scriptedReader()
			m.status = StatusRunning
			m.live.text = tt.live

			m, cmd := updateScreen(t, m, eventMsg{ev: event.TurnDone{Message: tt.msg}})
			if cmd == nil {
				t.Error("TurnDone cmd = nil, want non-nil (readNext for trailing EOF)")
			}
			if m.live.text != "" {
				t.Errorf("live.text = %q, want cleared", m.live.text)
			}
			if !tt.wantSegment {
				if len(m.messages) != 0 {
					t.Fatalf("expected no final segment, got %d messages", len(m.messages))
				}
				return
			}
			if len(m.messages) != 1 {
				t.Fatalf("messages len = %d, want exactly 1 (no duplication)", len(m.messages))
			}
			last := m.messages[0]
			if last.Role != RoleAssistant {
				t.Errorf("role = %d, want RoleAssistant (%d)", last.Role, RoleAssistant)
			}
			if got := firstTextBlock(t, last); got != tt.wantText {
				t.Errorf("text = %q, want %q", got, tt.wantText)
			}
		})
	}
}

// TestUpdateTurnDoneNoDuplication is the §6 anchor case: a fully streamed turn
// whose TurnDone.Message carries that SAME final text must produce exactly ONE
// assistant segment, with the text appearing once.
func TestUpdateTurnDoneNoDuplication(t *testing.T) {
	t.Parallel()

	const final = "the final answer"
	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.reader = scriptedReader()
	m.status = StatusRunning

	// Stream the final text, then TurnDone carries the same text in its Message.
	m, _ = updateScreen(t, m, eventMsg{ev: event.TokenDelta{Chunk: &content.TextChunk{Text: final}}})
	msg := &content.AIMessage{Message: content.Message{Blocks: []content.Block{&content.TextBlock{Text: final}}}}
	m, _ = updateScreen(t, m, eventMsg{ev: event.TurnDone{Message: msg}})

	if len(m.messages) != 1 {
		t.Fatalf("messages len = %d, want exactly 1 segment (no duplication)", len(m.messages))
	}
	if got := firstTextBlock(t, m.messages[0]); got != final {
		t.Errorf("final text = %q, want %q", got, final)
	}
	// The text appears exactly once across the whole transcript.
	if n := strings.Count(renderAll(t, m), final); n != 1 {
		t.Errorf("final text appears %d times in render, want 1", n)
	}
}

// renderAll concatenates the first text block of every message into one string —
// a cheap way to assert how many times a piece of text appears in the transcript
// model (the render is flat this phase, so model text == rendered text).
func renderAll(t *testing.T, m Screen) string {
	t.Helper()
	var b strings.Builder
	for _, msg := range m.messages {
		b.WriteString(firstTextBlock(t, msg))
		b.WriteByte('\n')
	}
	return b.String()
}

func TestUpdateTurnFailed(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.reader = scriptedReader()
	m.status = StatusRunning
	m.live.text = "partial"

	m, cmd := updateScreen(t, m, eventMsg{ev: event.TurnFailed{Err: errors.New("boom")}})
	if cmd == nil {
		t.Error("TurnFailed cmd = nil, want non-nil")
	}
	if m.live.text != "" {
		t.Errorf("live.text = %q, want cleared", m.live.text)
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != RoleError {
		t.Errorf("role = %d, want RoleError", last.Role)
	}
	if firstTextBlock(t, last) != "boom" {
		t.Errorf("text = %q, want %q", firstTextBlock(t, last), "boom")
	}
}

func TestUpdateTurnInterrupted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		live      string
		wantRoles []DisplayRole
		wantText  string
	}{
		{
			name:      "with partial flushes partial then tombstone",
			live:      "partial",
			wantRoles: []DisplayRole{RoleAssistant, RoleInterrupted},
			wantText:  "partial",
		},
		{
			name:      "empty stream appends only tombstone",
			live:      "",
			wantRoles: []DisplayRole{RoleInterrupted},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent))
			m.reader = scriptedReader()
			m.status = StatusInterrupting
			m.live.text = tt.live

			m, cmd := updateScreen(t, m, eventMsg{ev: event.TurnInterrupted{}})
			if cmd == nil {
				t.Error("TurnInterrupted cmd = nil, want non-nil")
			}
			if m.live.text != "" {
				t.Errorf("live.text = %q, want cleared", m.live.text)
			}
			if len(m.messages) != len(tt.wantRoles) {
				t.Fatalf("messages len = %d, want %d", len(m.messages), len(tt.wantRoles))
			}
			for i, role := range tt.wantRoles {
				if m.messages[i].Role != role {
					t.Errorf("messages[%d].Role = %d, want %d", i, m.messages[i].Role, role)
				}
			}
			if tt.wantText != "" {
				if got := firstTextBlock(t, m.messages[0]); got != tt.wantText {
					t.Errorf("partial text = %q, want %q", got, tt.wantText)
				}
			}
			// Tombstone carries nil Blocks.
			tomb := m.messages[len(m.messages)-1]
			if tomb.Blocks != nil {
				t.Errorf("tombstone blocks = %v, want nil", tomb.Blocks)
			}
		})
	}
}

func TestUpdateTurnStarted(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.reader = scriptedReader()
	m.status = StatusRunning

	m, cmd := updateScreen(t, m, eventMsg{ev: event.TurnStarted{}})
	if cmd == nil {
		t.Error("TurnStarted cmd = nil, want non-nil (readNext)")
	}
	if len(m.messages) != 0 {
		t.Errorf("TurnStarted appended %d messages, want 0", len(m.messages))
	}
	if m.status != StatusRunning {
		t.Errorf("status = %d, want StatusRunning", m.status)
	}
}

func TestUpdateStreamEOFAdvancesQueue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		queue       []queuedInput
		streamErr   error
		wantStatus  Status
		wantCmd     bool
		wantQueue   int
		wantErrLast bool
	}{
		{
			name:       "empty queue goes idle",
			queue:      nil,
			wantStatus: StatusIdle,
			wantCmd:    false,
			wantQueue:  0,
		},
		{
			name:       "non-empty queue starts next turn",
			queue:      []queuedInput{{Blocks: []content.Block{&content.TextBlock{Text: "q"}}, DisplayIndex: 0}},
			wantStatus: StatusRunning,
			wantCmd:    true,
			wantQueue:  0,
		},
		{
			name:        "queued start failure keeps head, stays idle, error appended",
			queue:       []queuedInput{{Blocks: []content.Block{&content.TextBlock{Text: "q"}}, DisplayIndex: 0}},
			streamErr:   errors.New("busy"),
			wantStatus:  StatusIdle,
			wantCmd:     false,
			wantQueue:   1,
			wantErrLast: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{streamReader: scriptedReader(event.TurnStarted{}), streamErr: tt.streamErr}
			m := New(context.Background(), agent, fakeOpen(agent))
			m.reader = scriptedReader()
			m.status = StatusRunning
			m.queue = tt.queue

			m, cmd := updateScreen(t, m, streamEOFMsg{})

			if m.status != tt.wantStatus {
				t.Errorf("status = %d, want %d", m.status, tt.wantStatus)
			}
			if (cmd != nil) != tt.wantCmd {
				t.Errorf("cmd != nil = %v, want %v", cmd != nil, tt.wantCmd)
			}
			if len(m.queue) != tt.wantQueue {
				t.Errorf("queue len = %d, want %d", len(m.queue), tt.wantQueue)
			}
			if m.reader != nil && tt.wantStatus == StatusIdle {
				t.Errorf("reader != nil after idle EOF, want nil")
			}
			if tt.wantErrLast {
				last := m.messages[len(m.messages)-1]
				if last.Role != RoleError {
					t.Errorf("last role = %d, want RoleError", last.Role)
				}
			}
		})
	}
}

func TestUpdateStreamErr(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.reader = scriptedReader()
	m.status = StatusRunning

	m, cmd := updateScreen(t, m, streamErrMsg{err: errors.New("read fail")})
	if cmd != nil {
		t.Errorf("cmd = non-nil, want nil (empty queue)")
	}
	if m.status != StatusIdle {
		t.Errorf("status = %d, want StatusIdle", m.status)
	}
	if m.reader != nil {
		t.Error("reader != nil, want nil")
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != RoleError || firstTextBlock(t, last) != "read fail" {
		t.Errorf("last = %+v, want RoleError 'read fail'", last)
	}
}

func TestUpdateInterruptResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		msg         interruptResultMsg
		startStatus Status
		wantStatus  Status
		wantErrRow  bool
	}{
		{
			name:        "error sets running and appends error",
			msg:         interruptResultMsg{err: errors.New("x")},
			startStatus: StatusInterrupting,
			wantStatus:  StatusRunning,
			wantErrRow:  true,
		},
		{
			name:        "success stays interrupting",
			msg:         interruptResultMsg{cancelled: true},
			startStatus: StatusInterrupting,
			wantStatus:  StatusInterrupting,
		},
		{
			name:        "cancelled false stays interrupting (EOF returns to idle)",
			msg:         interruptResultMsg{cancelled: false},
			startStatus: StatusInterrupting,
			wantStatus:  StatusInterrupting,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent))
			m.status = tt.startStatus

			m, cmd := updateScreen(t, m, tt.msg)
			if cmd != nil {
				t.Errorf("cmd = non-nil, want nil")
			}
			if m.status != tt.wantStatus {
				t.Errorf("status = %d, want %d", m.status, tt.wantStatus)
			}
			if tt.wantErrRow {
				if len(m.messages) == 0 || m.messages[len(m.messages)-1].Role != RoleError {
					t.Errorf("expected RoleError row")
				}
			} else if len(m.messages) != 0 {
				t.Errorf("expected no messages, got %d", len(m.messages))
			}
		})
	}
}

func TestUpdateReopenResult(t *testing.T) {
	t.Parallel()

	t.Run("error keeps old agent and goes idle", func(t *testing.T) {
		t.Parallel()

		old := &fakeAgent{}
		m := New(context.Background(), old, fakeOpen(old))
		m.status = StatusResetting

		m, cmd := updateScreen(t, m, reopenResultMsg{err: errors.New("x")})
		if cmd != nil {
			t.Errorf("cmd = non-nil, want nil")
		}
		if m.Agent() != old {
			t.Errorf("agent swapped on error, want unchanged")
		}
		if m.status != StatusIdle {
			t.Errorf("status = %d, want StatusIdle", m.status)
		}
		if len(m.messages) == 0 || m.messages[len(m.messages)-1].Role != RoleError {
			t.Errorf("expected RoleError row")
		}
	})

	t.Run("success swaps agent clears state and closes old", func(t *testing.T) {
		t.Parallel()

		old := &fakeAgent{}
		fresh := &fakeAgent{}
		m := New(context.Background(), old, fakeOpen(old))
		m.status = StatusResetting
		m.messages = []DisplayMessage{{Role: RoleUser}}
		m.live.text = "x"
		m.queue = []queuedInput{{DisplayIndex: 0}}

		m, cmd := updateScreen(t, m, reopenResultMsg{agent: fresh})
		if m.Agent() != fresh {
			t.Errorf("agent = %p, want fresh %p", m.Agent(), fresh)
		}
		if len(m.messages) != 0 {
			t.Errorf("messages len = %d, want 0", len(m.messages))
		}
		if m.live.text != "" {
			t.Errorf("live.text = %q, want cleared", m.live.text)
		}
		if len(m.queue) != 0 {
			t.Errorf("queue len = %d, want 0", len(m.queue))
		}
		if m.status != StatusIdle {
			t.Errorf("status = %d, want StatusIdle", m.status)
		}
		if cmd == nil {
			t.Fatal("cmd = nil, want non-nil (closeAgent for old)")
		}
		// Executing the returned cmd must Close the OLD agent.
		cmd()
		if !old.closeCalled {
			t.Error("old agent Close() not called")
		}
		if fresh.closeCalled {
			t.Error("fresh agent Close() called, want not closed")
		}
	})
}

func TestUpdateSystemReady(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))

	m, cmd := updateScreen(t, m, systemReadyMsg{})
	if cmd != nil {
		t.Errorf("cmd = non-nil, want nil")
	}
	if len(m.messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(m.messages))
	}
	row := m.messages[0]
	if row.Role != RoleSystem {
		t.Errorf("role = %d, want RoleSystem", row.Role)
	}
	if firstTextBlock(t, row) != "session ready" {
		t.Errorf("text = %q, want %q", firstTextBlock(t, row), "session ready")
	}
}

func TestFirstToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "single word", in: "/help", want: "/help"},
		{name: "leading word with args", in: "/clear now", want: "/clear"},
		{name: "leading whitespace", in: "  /help foo", want: "/help"},
		{name: "empty", in: "", want: ""},
		{name: "whitespace only", in: "   ", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := firstToken(tt.in); got != tt.want {
				t.Errorf("firstToken(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestHandleKeyTypingForwardsToInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		value       string
		wantSlash   bool // expect slashComplete non-nil after rebuild
		description string
	}{
		{name: "plain text no panel", value: "hi", wantSlash: false},
		{name: "single slash matches both", value: "/", wantSlash: true},
		{name: "slash c matches clear", value: "/c", wantSlash: true},
		{name: "no match", value: "/zz", wantSlash: false},
		{name: "slash with space hides panel", value: "/clear ", wantSlash: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent))
			// SetValue then drive a no-op key so the default branch rebuilds the panel.
			m.input.SetValue(tt.value)
			m, _ = updateScreen(t, m, tea.KeyPressMsg{})

			if m.input.Value() != tt.value {
				t.Errorf("input value = %q, want %q", m.input.Value(), tt.value)
			}
			if (m.slashComplete != nil) != tt.wantSlash {
				t.Errorf("slashComplete != nil = %v, want %v", m.slashComplete != nil, tt.wantSlash)
			}
		})
	}
}

func TestHandleKeyEnterPlainSubmitIdle(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{streamReader: scriptedReader(event.TurnStarted{})}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.input.SetValue("hello there")

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd == nil {
		t.Error("Enter submit cmd = nil, want non-nil (readNext)")
	}
	if m.status != StatusRunning {
		t.Errorf("status = %d, want StatusRunning", m.status)
	}
	if m.input.Value() != "" {
		t.Errorf("input = %q, want reset to empty", m.input.Value())
	}
	if len(m.messages) == 0 {
		t.Fatal("expected a RoleUser row")
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != RoleUser {
		t.Errorf("last role = %d, want RoleUser", last.Role)
	}
}

func TestHandleKeyEnterQueueWhileRunning(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.status = StatusRunning
	m.reader = scriptedReader()
	// Pre-existing rows so the DisplayIndex is not trivially 0.
	m.messages = []DisplayMessage{{Role: RoleUser}, {Role: RoleAssistant}}
	m.input.SetValue("queued one")

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd != nil {
		t.Errorf("queue Enter cmd = non-nil, want nil")
	}
	if len(m.queue) != 1 {
		t.Fatalf("queue len = %d, want 1", len(m.queue))
	}
	wantIdx := len(m.messages) - 1
	if m.queue[0].DisplayIndex != wantIdx {
		t.Errorf("queue[0].DisplayIndex = %d, want %d", m.queue[0].DisplayIndex, wantIdx)
	}
	if m.messages[wantIdx].Role != RoleUser {
		t.Errorf("queued row role = %d, want RoleUser", m.messages[wantIdx].Role)
	}
	if m.input.Value() != "" {
		t.Errorf("input = %q, want reset", m.input.Value())
	}
	if m.status != StatusRunning {
		t.Errorf("status = %d, want StatusRunning", m.status)
	}
}

func TestHandleKeyEnterBadAttachmentKeepsInput(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.input.SetValue("@nope.pem") // .pem is a denied extension → buildBlocks error

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd != nil {
		t.Errorf("cmd = non-nil, want nil (no turn started)")
	}
	if m.input.Value() != "@nope.pem" {
		t.Errorf("input = %q, want intact %q", m.input.Value(), "@nope.pem")
	}
	if m.status != StatusIdle {
		t.Errorf("status = %d, want StatusIdle", m.status)
	}
	if len(m.messages) == 0 || m.messages[len(m.messages)-1].Role != RoleError {
		t.Error("expected a RoleError row")
	}
	if agent.closeCalled {
		t.Error("agent should not be touched on a buildBlocks error")
	}
}

func TestHandleKeyEnterEmptyIsNoop(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.input.SetValue("   ") // whitespace only

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd != nil {
		t.Errorf("cmd = non-nil, want nil")
	}
	if len(m.messages) != 0 {
		t.Errorf("messages len = %d, want 0", len(m.messages))
	}
	if m.status != StatusIdle {
		t.Errorf("status = %d, want StatusIdle", m.status)
	}
}

func TestHandleKeyEnterHelp(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.input.SetValue("/help")

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd != nil {
		t.Errorf("/help cmd = non-nil, want nil")
	}
	if len(m.messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(m.messages))
	}
	row := m.messages[0]
	if row.Role != RoleSystem {
		t.Errorf("role = %d, want RoleSystem", row.Role)
	}
	text := firstTextBlock(t, row)
	for _, c := range components.SlashCommands {
		if !strings.Contains(text, c.Name) {
			t.Errorf("help text missing %q; got %q", c.Name, text)
		}
	}
	if m.input.Value() != "" {
		t.Errorf("input = %q, want reset after /help ran", m.input.Value())
	}
}

func TestHandleKeyEnterClearWhileIdle(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.input.SetValue("/clear")

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd == nil {
		t.Error("/clear cmd = nil, want non-nil (reopen)")
	}
	if m.status != StatusResetting {
		t.Errorf("status = %d, want StatusResetting", m.status)
	}
	if m.input.Value() != "" {
		t.Errorf("input = %q, want reset after /clear ran", m.input.Value())
	}
}

func TestHandleKeyEnterClearWhileRunningIsNoop(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.status = StatusRunning
	m.reader = scriptedReader()
	m.input.SetValue("/clear")

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd != nil {
		t.Errorf("/clear-while-running cmd = non-nil, want nil (no-op)")
	}
	if m.status != StatusRunning {
		t.Errorf("status = %d, want StatusRunning (unchanged)", m.status)
	}
	if m.input.Value() != "/clear" {
		t.Errorf("input = %q, want intact %q", m.input.Value(), "/clear")
	}
}

func TestHandleKeyEnterUnknownSlashFallsToSubmit(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{streamReader: scriptedReader(event.TurnStarted{})}
	m := New(context.Background(), agent, fakeOpen(agent))
	// /unknown is not a known command, so it submits as plain text.
	m.input.SetValue("/unknown stuff")

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd == nil {
		t.Error("unknown-slash cmd = nil, want non-nil (plain submit)")
	}
	if m.status != StatusRunning {
		t.Errorf("status = %d, want StatusRunning", m.status)
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != RoleUser {
		t.Errorf("last role = %d, want RoleUser", last.Role)
	}
}

func TestHandleKeyEscWhileRunningClearsQueue(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{interruptCancelled: true}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.status = StatusRunning
	m.reader = scriptedReader()
	// messages: [0]=user (active), [1]=system (interleaved), [2]=user (queued)
	m.messages = []DisplayMessage{
		{Role: RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "active"}}},
		{Role: RoleSystem, Blocks: []content.Block{&content.TextBlock{Text: "help"}}},
		{Role: RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "queued"}}},
	}
	m.queue = []queuedInput{{Blocks: []content.Block{&content.TextBlock{Text: "queued"}}, DisplayIndex: 2}}

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})

	if cmd == nil {
		t.Error("Esc cmd = nil, want non-nil (interruptTurn)")
	}
	if m.status != StatusInterrupting {
		t.Errorf("status = %d, want StatusInterrupting", m.status)
	}
	if len(m.queue) != 0 {
		t.Errorf("queue len = %d, want 0", len(m.queue))
	}
	if len(m.messages) != 2 {
		t.Fatalf("messages len = %d, want 2 (queued row removed)", len(m.messages))
	}
	// The remaining rows are the active user row and the interleaved system row.
	if firstTextBlock(t, m.messages[0]) != "active" {
		t.Errorf("messages[0] text = %q, want %q", firstTextBlock(t, m.messages[0]), "active")
	}
	if m.messages[1].Role != RoleSystem {
		t.Errorf("messages[1] role = %d, want RoleSystem", m.messages[1].Role)
	}
}

func TestHandleKeyEscWhileIdleIsNoop(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.status = StatusIdle

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})

	if cmd != nil {
		t.Errorf("Esc-while-idle cmd = non-nil, want nil")
	}
	if m.status != StatusIdle {
		t.Errorf("status = %d, want StatusIdle", m.status)
	}
}

func TestHandleKeyCtrlC(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))

	_, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})

	if cmd == nil {
		t.Fatal("Ctrl+C cmd = nil, want non-nil (close + quit sequence)")
	}
}

func TestHandleKeySlashCompleteEnter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        Status
		selected      string // command Name under cursor
		wantRan       bool   // ran → input reset + panel hidden
		wantStatus    Status
		wantInputKept string // when !wantRan, the input should stay this
	}{
		{
			name:       "help runs resets and hides panel",
			status:     StatusIdle,
			selected:   "/help",
			wantRan:    true,
			wantStatus: StatusIdle,
		},
		{
			name:       "clear idle runs resets and hides panel",
			status:     StatusIdle,
			selected:   "/clear",
			wantRan:    true,
			wantStatus: StatusResetting,
		},
		{
			name:          "clear while running is noop keeps panel and input",
			status:        StatusRunning,
			selected:      "/clear",
			wantRan:       false,
			wantStatus:    StatusRunning,
			wantInputKept: "/cl",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent))
			m.status = tt.status
			if tt.status == StatusRunning {
				m.reader = scriptedReader()
			}
			// Build a panel whose Selected() is the wanted command.
			m.input.SetValue("/cl")
			m.slashComplete = components.NewSlashComplete(tt.selected)
			if m.slashComplete == nil {
				t.Fatalf("NewSlashComplete(%q) = nil", tt.selected)
			}

			m, _ = updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

			if m.status != tt.wantStatus {
				t.Errorf("status = %d, want %d", m.status, tt.wantStatus)
			}
			if tt.wantRan {
				if m.input.Value() != "" {
					t.Errorf("input = %q, want reset (ran)", m.input.Value())
				}
				if m.slashComplete != nil {
					t.Error("slashComplete != nil, want hidden (ran)")
				}
			} else {
				if m.input.Value() != tt.wantInputKept {
					t.Errorf("input = %q, want kept %q (no-op)", m.input.Value(), tt.wantInputKept)
				}
				if m.slashComplete == nil {
					t.Error("slashComplete = nil, want kept (no-op)")
				}
			}
		})
	}
}

func TestHandleKeyTab(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.input.SetValue("/h")
	m.slashComplete = components.NewSlashComplete("/h")
	if m.slashComplete == nil {
		t.Fatal("NewSlashComplete(/h) = nil")
	}
	want := m.slashComplete.Selected().Name

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyTab})

	if cmd != nil {
		t.Errorf("Tab cmd = non-nil, want nil")
	}
	if m.input.Value() != want {
		t.Errorf("input = %q, want %q", m.input.Value(), want)
	}
	if m.slashComplete != nil {
		t.Error("slashComplete != nil, want hidden after Tab")
	}
}

func TestHandleKeyTabNoPanelIsForwarded(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.input.SetValue("hi")
	// No slashComplete; Tab falls through to the input editor (default branch).
	m, _ = updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	if m.slashComplete != nil {
		t.Error("slashComplete should remain nil")
	}
}

func TestHandleKeyScrollKeysRouteToHistory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{name: "pgup", key: tea.KeyPressMsg{Code: tea.KeyPgUp}},
		{name: "pgdown", key: tea.KeyPressMsg{Code: tea.KeyPgDown}},
		{name: "ctrl+u", key: tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}},
		{name: "ctrl+d", key: tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent))
			// Give the viewport real dimensions and content so scrolling is live.
			m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 40, Height: 20})
			m.messages = make([]DisplayMessage, 0, 50)
			for i := 0; i < 50; i++ {
				m.messages = append(m.messages, DisplayMessage{
					Role:   RoleSystem,
					Blocks: []content.Block{&content.TextBlock{Text: "line of transcript content"}},
				})
			}
			m.refreshHistory()
			m.input.SetValue("draft")

			m, cmd := updateScreen(t, m, tt.key)

			// The scroll key must reach the viewport, not the textarea: the
			// draft input is untouched.
			if m.input.Value() != "draft" {
				t.Errorf("input value = %q, want unchanged %q (scroll key leaked to textarea)", m.input.Value(), "draft")
			}
			_ = cmd // cmd may be nil or a viewport cmd; either is fine.
		})
	}
}

func TestViewNeverExceedsHeight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		withPanel bool
	}{
		{name: "no panel", withPanel: false},
		{name: "with slash panel", withPanel: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent))
			m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 40, Height: 20})

			// Fill the transcript so the viewport content is taller than the height.
			m.messages = make([]DisplayMessage, 0, 50)
			for i := 0; i < 50; i++ {
				m.messages = append(m.messages, DisplayMessage{
					Role:   RoleSystem,
					Blocks: []content.Block{&content.TextBlock{Text: "line of transcript content"}},
				})
			}

			if tt.withPanel {
				// "/" matches ≥2 commands → a multi-row panel.
				m.input.SetValue("/")
				m.slashComplete = components.NewSlashComplete("/")
				if m.slashComplete == nil {
					t.Fatal("NewSlashComplete(/) = nil")
				}
				m.resizeHistory() // re-budget the viewport for the panel
			}
			m.refreshHistory()

			if got := lipgloss.Height(m.View().Content); got > m.height {
				t.Errorf("View() height = %d, want <= %d (overflow)", got, m.height)
			}
		})
	}
}

// renderScreen renders the screen's current transcript exactly as refreshHistory
// would, using the screen's own expandTools flag, so a test can assert on what the
// Ctrl+T toggle changes about the rendered output.
func renderScreen(m Screen, width int) string {
	queued := make(map[int]bool, len(m.queue))
	for _, q := range m.queue {
		queued[q.DisplayIndex] = true
	}
	return renderMessages(m.messages, m.live, queued, m.expandTools, width)
}

// TestHandleKeyCtrlTTogglesExpand covers Task 4.1: ctrl+t flips expandTools and
// re-renders, in any status, with no key conflict.
func TestHandleKeyCtrlTTogglesExpand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status Status
	}{
		{name: "idle", status: StatusIdle},
		{name: "running", status: StatusRunning},
		{name: "interrupting", status: StatusInterrupting},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent))
			m.status = tt.status
			if tt.status != StatusIdle {
				m.reader = scriptedReader()
			}

			if m.expandTools {
				t.Fatal("expandTools = true at construction, want false")
			}
			m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
			if cmd != nil {
				t.Errorf("ctrl+t cmd = non-nil, want nil (re-render only)")
			}
			if !m.expandTools {
				t.Errorf("expandTools = false after first ctrl+t, want true")
			}
			// Status is unchanged — the toggle is status-agnostic.
			if m.status != tt.status {
				t.Errorf("status = %d, want unchanged %d", m.status, tt.status)
			}
			m, _ = updateScreen(t, m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
			if m.expandTools {
				t.Errorf("expandTools = true after second ctrl+t, want false (toggled back)")
			}
		})
	}
}

// TestHandleKeyCtrlTRerendersPreview covers Task 4.1's render effect: a transcript
// with a long tool result renders folded (first K lines + marker) before the toggle
// and fully (all lines, no marker) after.
func TestHandleKeyCtrlTRerendersPreview(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.messages = []DisplayMessage{{
		Role:   RoleAssistant,
		Blocks: []content.Block{&content.TextBlock{Text: "reading"}},
		ToolCalls: []ToolCallView{{
			ToolName: "ReadFile",
			Status:   ToolOK,
			Result:   makeLines(10), // 10 > previewLineCap(6)
		}},
	}}

	before := renderScreen(m, 80)
	if !strings.Contains(before, "line5") || strings.Contains(before, "line6") {
		t.Errorf("collapsed render should show line5 and hide line6; got %q", before)
	}
	if !strings.Contains(before, "more lines (Ctrl+T)") {
		t.Errorf("collapsed render missing more-lines marker; got %q", before)
	}

	m, _ = updateScreen(t, m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})

	after := renderScreen(m, 80)
	if !strings.Contains(after, "line6") || !strings.Contains(after, "line9") {
		t.Errorf("expanded render should show all lines incl line6 and line9; got %q", after)
	}
	if strings.Contains(after, "more lines (Ctrl+T)") {
		t.Errorf("expanded render should drop the more-lines marker; got %q", after)
	}
}

func TestHandleKeyUpDownMovesSelection(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	// Prefix "/" matches both /clear and /help (≥2 items).
	m.slashComplete = components.NewSlashComplete("/")
	if m.slashComplete == nil || len(components.SlashCommands) < 2 {
		t.Fatal("need ≥2 matching commands for this test")
	}
	first := m.slashComplete.Selected().Name

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if cmd != nil {
		t.Errorf("Down cmd = non-nil, want nil")
	}
	afterDown := m.slashComplete.Selected().Name
	if afterDown == first {
		t.Errorf("Down did not move selection from %q", first)
	}

	m, _ = updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	afterUp := m.slashComplete.Selected().Name
	if afterUp != first {
		t.Errorf("Up did not return to %q, got %q", first, afterUp)
	}
}
