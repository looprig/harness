package tui

import (
	"context"
	"errors"
	"io"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
)

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
