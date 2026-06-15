package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
	if v := m.View(); v != "" {
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
	if v := got.View(); v == "" {
		t.Error("View() after ready = empty, want non-empty")
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

	if m.stream != "abcd" {
		t.Errorf("stream = %q, want %q", m.stream, "abcd")
	}
}

func TestUpdateThinkingChunkSkipped(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.reader = scriptedReader()
	m.stream = "keep"

	m, _ = updateScreen(t, m, eventMsg{ev: event.TokenDelta{Chunk: &content.ThinkingChunk{Thinking: "x"}}})

	if m.stream != "keep" {
		t.Errorf("stream = %q, want unchanged %q", m.stream, "keep")
	}
}

func TestUpdateTurnDone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		msg       *content.AIMessage
		stream    string
		wantText  string
		wantNil   bool
		wantBlock bool
	}{
		{
			name:      "with message blocks",
			msg:       &content.AIMessage{Message: content.Message{Blocks: []content.Block{&content.TextBlock{Text: "hi"}}}},
			stream:    "partial",
			wantText:  "hi",
			wantBlock: true,
		},
		{
			name:    "nil message appends empty assistant turn",
			msg:     nil,
			stream:  "partial",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent))
			m.reader = scriptedReader()
			m.status = StatusRunning
			m.stream = tt.stream

			m, cmd := updateScreen(t, m, eventMsg{ev: event.TurnDone{Message: tt.msg}})
			if cmd == nil {
				t.Error("TurnDone cmd = nil, want non-nil (readNext for trailing EOF)")
			}
			if m.stream != "" {
				t.Errorf("stream = %q, want cleared", m.stream)
			}
			if len(m.messages) == 0 {
				t.Fatal("expected an assistant row")
			}
			last := m.messages[len(m.messages)-1]
			if last.Role != RoleAssistant {
				t.Errorf("role = %d, want RoleAssistant (%d)", last.Role, RoleAssistant)
			}
			if tt.wantNil && last.Blocks != nil {
				t.Errorf("nil-message blocks = %v, want nil", last.Blocks)
			}
			if tt.wantBlock && firstTextBlock(t, last) != tt.wantText {
				t.Errorf("text = %q, want %q", firstTextBlock(t, last), tt.wantText)
			}
		})
	}
}

func TestUpdateTurnFailed(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.reader = scriptedReader()
	m.status = StatusRunning
	m.stream = "partial"

	m, cmd := updateScreen(t, m, eventMsg{ev: event.TurnFailed{Err: errors.New("boom")}})
	if cmd == nil {
		t.Error("TurnFailed cmd = nil, want non-nil")
	}
	if m.stream != "" {
		t.Errorf("stream = %q, want cleared", m.stream)
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
		stream    string
		wantRoles []DisplayRole
		wantText  string
	}{
		{
			name:      "with partial flushes partial then tombstone",
			stream:    "partial",
			wantRoles: []DisplayRole{RoleAssistant, RoleInterrupted},
			wantText:  "partial",
		},
		{
			name:      "empty stream appends only tombstone",
			stream:    "",
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
			m.stream = tt.stream

			m, cmd := updateScreen(t, m, eventMsg{ev: event.TurnInterrupted{}})
			if cmd == nil {
				t.Error("TurnInterrupted cmd = nil, want non-nil")
			}
			if m.stream != "" {
				t.Errorf("stream = %q, want cleared", m.stream)
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
		m.stream = "x"
		m.queue = []queuedInput{{DisplayIndex: 0}}

		m, cmd := updateScreen(t, m, reopenResultMsg{agent: fresh})
		if m.Agent() != fresh {
			t.Errorf("agent = %p, want fresh %p", m.Agent(), fresh)
		}
		if len(m.messages) != 0 {
			t.Errorf("messages len = %d, want 0", len(m.messages))
		}
		if m.stream != "" {
			t.Errorf("stream = %q, want cleared", m.stream)
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
			m, _ = updateScreen(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("")})

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

	m, cmd := updateScreen(t, m, tea.KeyMsg{Type: tea.KeyEnter})

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

	m, cmd := updateScreen(t, m, tea.KeyMsg{Type: tea.KeyEnter})

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

	m, cmd := updateScreen(t, m, tea.KeyMsg{Type: tea.KeyEnter})

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

	m, cmd := updateScreen(t, m, tea.KeyMsg{Type: tea.KeyEnter})

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

	m, cmd := updateScreen(t, m, tea.KeyMsg{Type: tea.KeyEnter})

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

	m, cmd := updateScreen(t, m, tea.KeyMsg{Type: tea.KeyEnter})

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

	m, cmd := updateScreen(t, m, tea.KeyMsg{Type: tea.KeyEnter})

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

	m, cmd := updateScreen(t, m, tea.KeyMsg{Type: tea.KeyEnter})

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

	m, cmd := updateScreen(t, m, tea.KeyMsg{Type: tea.KeyEsc})

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

	m, cmd := updateScreen(t, m, tea.KeyMsg{Type: tea.KeyEsc})

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

	_, cmd := updateScreen(t, m, tea.KeyMsg{Type: tea.KeyCtrlC})

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

			m, _ = updateScreen(t, m, tea.KeyMsg{Type: tea.KeyEnter})

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

	m, cmd := updateScreen(t, m, tea.KeyMsg{Type: tea.KeyTab})

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
	m, _ = updateScreen(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if m.slashComplete != nil {
		t.Error("slashComplete should remain nil")
	}
}

func TestHandleKeyScrollKeysRouteToHistory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  tea.KeyMsg
	}{
		{name: "pgup", key: tea.KeyMsg{Type: tea.KeyPgUp}},
		{name: "pgdown", key: tea.KeyMsg{Type: tea.KeyPgDown}},
		{name: "ctrl+u", key: tea.KeyMsg{Type: tea.KeyCtrlU}},
		{name: "ctrl+d", key: tea.KeyMsg{Type: tea.KeyCtrlD}},
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

			if got := lipgloss.Height(m.View()); got > m.height {
				t.Errorf("View() height = %d, want <= %d (overflow)", got, m.height)
			}
		})
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

	m, cmd := updateScreen(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if cmd != nil {
		t.Errorf("Down cmd = non-nil, want nil")
	}
	afterDown := m.slashComplete.Selected().Name
	if afterDown == first {
		t.Errorf("Down did not move selection from %q", first)
	}

	m, _ = updateScreen(t, m, tea.KeyMsg{Type: tea.KeyUp})
	afterUp := m.slashComplete.Selected().Name
	if afterUp != first {
		t.Errorf("Up did not return to %q, got %q", first, afterUp)
	}
}
