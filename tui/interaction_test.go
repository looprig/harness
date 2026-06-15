package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/tool"
)

// TestInteractionComposeUpdate covers modeCompose key handling: a printable key
// edits the editor (Value reflects it); a non-empty Enter returns a submit action
// carrying the composed text; an empty/whitespace Enter is a no-op (no submit).
func TestInteractionComposeUpdate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		seed      string          // value set before the key
		key       tea.KeyPressMsg // key to apply
		wantKind  uiActionKind
		wantText  string // expected action Text (for uiSubmit)
		wantSlash string // expected action Slash (for uiRunSlash)
		wantValue string // expected editor Value after the key
	}{
		{
			name:      "printable key edits the editor",
			seed:      "",
			key:       tea.KeyPressMsg{Text: "h", Code: 'h'},
			wantKind:  uiNoop,
			wantValue: "h",
		},
		{
			name:      "enter submits non-empty composed text",
			seed:      "hello there",
			key:       tea.KeyPressMsg{Code: tea.KeyEnter},
			wantKind:  uiSubmit,
			wantText:  "hello there",
			wantValue: "", // editor reset on submit
		},
		{
			name:      "empty enter is a no-op",
			seed:      "",
			key:       tea.KeyPressMsg{Code: tea.KeyEnter},
			wantKind:  uiNoop,
			wantValue: "",
		},
		{
			name:      "whitespace-only enter is a no-op",
			seed:      "   ",
			key:       tea.KeyPressMsg{Code: tea.KeyEnter},
			wantKind:  uiNoop,
			wantValue: "   ", // input kept intact on a no-op
		},
		{
			name:      "known slash enter runs the command",
			seed:      "/help",
			key:       tea.KeyPressMsg{Code: tea.KeyEnter},
			wantKind:  uiRunSlash,
			wantSlash: "/help",
			wantValue: "", // editor reset on a run
		},
		{
			name:      "unknown slash enter falls through to submit",
			seed:      "/nope and more",
			key:       tea.KeyPressMsg{Code: tea.KeyEnter},
			wantKind:  uiSubmit,
			wantText:  "/nope and more",
			wantValue: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newInteractionModel()
			m.input.SetValue(tt.seed)

			m, action := m.Update(tt.key)

			if action.Kind != tt.wantKind {
				t.Errorf("action.Kind = %d, want %d", action.Kind, tt.wantKind)
			}
			if tt.wantKind == uiSubmit && action.Text != tt.wantText {
				t.Errorf("submit Text = %q, want %q", action.Text, tt.wantText)
			}
			if tt.wantKind == uiRunSlash && action.Slash != tt.wantSlash {
				t.Errorf("runSlash Slash = %q, want %q", action.Slash, tt.wantSlash)
			}
			if got := m.input.Value(); got != tt.wantValue {
				t.Errorf("editor Value = %q, want %q", got, tt.wantValue)
			}
		})
	}
}

// TestInteractionEnqueuePermission covers enqueuing a PermissionRequested event:
// one promptPermission is appended with the request's ToolName/Description and
// AllowedScopes, the mode switches to modePermissionPrompt, and the head is active.
func TestInteractionEnqueuePermission(t *testing.T) {
	t.Parallel()

	m := newInteractionModel()
	id := callID(1)
	m = m.ApplyEvent(event.PermissionRequested{
		CallID:  id,
		Request: tool.BashRequest{Command: "go build"},
	})

	if m.PendingCount() != 1 {
		t.Fatalf("PendingCount = %d, want 1", m.PendingCount())
	}
	if m.mode != modePermissionPrompt {
		t.Errorf("mode = %d, want modePermissionPrompt (%d)", m.mode, modePermissionPrompt)
	}
	p := m.ActivePrompt()
	if p == nil {
		t.Fatal("ActivePrompt = nil, want the head permission prompt")
	}
	if p.CallID != id {
		t.Errorf("active CallID = %v, want %v", p.CallID, id)
	}
	if p.Kind != promptPermission || p.ToolName != "Bash" || p.Description != "go build" {
		t.Errorf("active prompt = %+v, want Bash/go build permission", *p)
	}
	want := tool.BashRequest{}.AllowedScopes()
	if !scopesEqual(p.Scopes, want) {
		t.Errorf("Scopes = %v, want %v", p.Scopes, want)
	}
}

// TestInteractionEnqueueUserInput covers enqueuing a UserInputRequested event:
// a promptUserInput is appended carrying the Question/Choices, freeText reflects
// whether choices exist, and the mode switches to the head's mode.
func TestInteractionEnqueueUserInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		choices      []string
		wantFreeText bool
		wantMode     interactionMode
	}{
		{
			name:         "choices switch to choice prompt",
			choices:      []string{"yes", "no"},
			wantFreeText: false,
			wantMode:     modeChoicePrompt,
		},
		{
			name:         "no choices switch to free-text answer prompt",
			choices:      nil,
			wantFreeText: true,
			wantMode:     modeAnswerPrompt,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newInteractionModel()
			id := callID(2)
			m = m.ApplyEvent(event.UserInputRequested{
				CallID:   id,
				Question: "proceed?",
				Choices:  tt.choices,
			})

			if m.PendingCount() != 1 {
				t.Fatalf("PendingCount = %d, want 1", m.PendingCount())
			}
			if m.mode != tt.wantMode {
				t.Errorf("mode = %d, want %d", m.mode, tt.wantMode)
			}
			p := m.ActivePrompt()
			if p == nil {
				t.Fatal("ActivePrompt = nil")
			}
			if p.Kind != promptUserInput || p.Question != "proceed?" {
				t.Errorf("active prompt = %+v, want user-input 'proceed?'", *p)
			}
			if p.freeText != tt.wantFreeText {
				t.Errorf("freeText = %v, want %v", p.freeText, tt.wantFreeText)
			}
		})
	}
}

// TestInteractionEnqueueFIFOAndAppendOnce covers the queue mechanics: two
// distinct CallIDs leave two pending with the head (index 0) active first; a
// duplicate CallID (already pending) is ignored (append-once).
func TestInteractionEnqueueFIFOAndAppendOnce(t *testing.T) {
	t.Parallel()

	m := newInteractionModel()
	first := callID(10)
	second := callID(20)

	m = m.ApplyEvent(event.UserInputRequested{CallID: first, Question: "Q1", Choices: []string{"x"}})
	m = m.ApplyEvent(event.UserInputRequested{CallID: second, Question: "Q2", Choices: []string{"y"}})

	if m.PendingCount() != 2 {
		t.Fatalf("PendingCount = %d, want 2 (distinct CallIDs)", m.PendingCount())
	}
	if head := m.ActivePrompt(); head == nil || head.CallID != first {
		t.Fatalf("head CallID = %v, want %v (FIFO)", head, first)
	}

	// A duplicate of an already-pending CallID is ignored (append-once).
	m = m.ApplyEvent(event.UserInputRequested{CallID: first, Question: "DUP", Choices: []string{"z"}})
	if m.PendingCount() != 2 {
		t.Fatalf("PendingCount after dup = %d, want 2 (append-once)", m.PendingCount())
	}
	if head := m.ActivePrompt(); head == nil || head.Question != "Q1" {
		t.Errorf("head Question = %v, want Q1 (dup did not overwrite)", head)
	}

	// A duplicate permission CallID is likewise ignored.
	m = m.ApplyEvent(event.PermissionRequested{CallID: second, Request: tool.BashRequest{Command: "rm"}})
	if m.PendingCount() != 2 {
		t.Errorf("PendingCount after dup permission = %d, want 2", m.PendingCount())
	}
}

// TestInteractionClearOnTerminal covers terminal-event clearing: a TurnDone /
// TurnFailed / TurnInterrupted clears pending and restores compose mode plus the
// saved compose draft.
func TestInteractionClearOnTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		term event.Event
	}{
		{name: "turn done clears", term: event.TurnDone{}},
		{name: "turn failed clears", term: event.TurnFailed{}},
		{name: "turn interrupted clears", term: event.TurnInterrupted{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newInteractionModel()
			// The user had a draft in the composer before a prompt interrupted them.
			m.input.SetValue("my draft")
			m.composeDraft = "my draft"

			m = m.ApplyEvent(event.PermissionRequested{
				CallID:  callID(1),
				Request: tool.BashRequest{Command: "go test"},
			})
			if m.mode != modePermissionPrompt || m.PendingCount() != 1 {
				t.Fatalf("pre-terminal state wrong: mode %d pending %d", m.mode, m.PendingCount())
			}

			m = m.ApplyEvent(tt.term)

			if m.PendingCount() != 0 {
				t.Errorf("PendingCount = %d, want 0 (cleared)", m.PendingCount())
			}
			if m.ActivePrompt() != nil {
				t.Errorf("ActivePrompt = %+v, want nil", m.ActivePrompt())
			}
			if m.mode != modeCompose {
				t.Errorf("mode = %d, want modeCompose (%d)", m.mode, modeCompose)
			}
			if m.input.Value() != "my draft" {
				t.Errorf("restored draft = %q, want %q", m.input.Value(), "my draft")
			}
		})
	}
}

// TestInteractionNonComposeUpdateIsNoop covers the deferral to Task 8: while a
// prompt is active (non-compose mode), Update returns noop for now (modal routing
// is the next task) and never submits.
func TestInteractionNonComposeUpdateIsNoop(t *testing.T) {
	t.Parallel()

	m := newInteractionModel()
	m = m.ApplyEvent(event.PermissionRequested{
		CallID:  callID(1),
		Request: tool.BashRequest{Command: "go build"},
	})

	m, action := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if action.Kind != uiNoop {
		t.Errorf("non-compose Update Kind = %d, want uiNoop (%d)", action.Kind, uiNoop)
	}
	// The prompt is still pending — non-compose Update did not resolve it.
	if m.PendingCount() != 1 {
		t.Errorf("PendingCount = %d, want 1 (unchanged)", m.PendingCount())
	}
}

// TestInteractionPopRevealsNextThenCompose covers the pop mechanic: popping the
// head reveals the next pending prompt; popping the last returns to compose mode
// and restores the saved draft.
func TestInteractionPopRevealsNextThenCompose(t *testing.T) {
	t.Parallel()

	m := newInteractionModel()
	// The draft is captured from the input box when the first prompt enqueues.
	m.input.SetValue("saved")
	first := callID(1)
	second := callID(2)
	m = m.ApplyEvent(event.PermissionRequested{CallID: first, Request: tool.BashRequest{Command: "a"}})
	m = m.ApplyEvent(event.PermissionRequested{CallID: second, Request: tool.UnknownRequest{Tool: "T", Summary: "s"}})

	m = m.pop()
	if m.PendingCount() != 1 {
		t.Fatalf("PendingCount after pop = %d, want 1", m.PendingCount())
	}
	if head := m.ActivePrompt(); head == nil || head.CallID != second {
		t.Fatalf("head after pop = %v, want second %v", head, second)
	}

	m = m.pop()
	if m.PendingCount() != 0 {
		t.Errorf("PendingCount after final pop = %d, want 0", m.PendingCount())
	}
	if m.mode != modeCompose {
		t.Errorf("mode after final pop = %d, want modeCompose", m.mode)
	}
	if m.input.Value() != "saved" {
		t.Errorf("restored draft = %q, want %q", m.input.Value(), "saved")
	}
}
