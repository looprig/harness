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

			m, action, _ := m.Update(tt.key)

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

// TestInteractionTerminalPreservesComposeWhenNoPrompt covers the input-loss fix:
// when a turn ends and NO prompt was active, the user's in-progress compose text
// must be preserved, not clobbered by restoring the empty saved draft.
func TestInteractionTerminalPreservesComposeWhenNoPrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		term event.Event
	}{
		{name: "turn done", term: event.TurnDone{}},
		{name: "turn failed", term: event.TurnFailed{}},
		{name: "turn interrupted", term: event.TurnInterrupted{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newInteractionModel()
			// The user is typing their next message while the AI streams; no
			// permission/AskUser prompt is active.
			m.input.SetValue("half-typed next message")

			m = m.ApplyEvent(tt.term) // the AI turn ends

			if got := m.input.Value(); got != "half-typed next message" {
				t.Errorf("compose input wiped on %s: got %q, want preserved", tt.name, got)
			}
			if m.PendingCount() != 0 {
				t.Errorf("PendingCount = %d, want 0", m.PendingCount())
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

	m, action, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if action.Kind != uiNoop {
		t.Errorf("non-compose Update Kind = %d, want uiNoop (%d)", action.Kind, uiNoop)
	}
	// The prompt is still pending — non-compose Update did not resolve it.
	if m.PendingCount() != 1 {
		t.Errorf("PendingCount = %d, want 1 (unchanged)", m.PendingCount())
	}
}

// runeKey builds a v2 printable-key press for a single rune (e.g. 'y', '1', 'o'):
// Code is the rune and Text is its string, matching how a terminal reports a
// printable key.
func runeKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// permissionModel returns an interaction model already in permission mode for one
// gate (callID 1) built from req.
func permissionModel(req tool.PermissionRequest) interactionModel {
	m := newInteractionModel()
	return m.ApplyEvent(event.PermissionRequested{CallID: callID(1), Request: req})
}

// choiceModel returns an interaction model in choice mode for one gate (callID 2)
// with the given choices.
func choiceModel(choices []string) interactionModel {
	m := newInteractionModel()
	return m.ApplyEvent(event.UserInputRequested{CallID: callID(2), Question: "pick", Choices: choices})
}

// TestInteractionPermissionRouting covers modePermissionPrompt key routing: the
// scope keys (y/s/w) are gated by scope membership — offered → approve+pop, not
// offered → noop; n and esc both deny+pop; any other key is a no-op re-render.
func TestInteractionPermissionRouting(t *testing.T) {
	t.Parallel()

	bash := tool.BashRequest{Command: "go build"}           // once/session/workspace
	unknown := tool.UnknownRequest{Tool: "T", Summary: "s"} // once only

	tests := []struct {
		name      string
		req       tool.PermissionRequest
		key       tea.KeyPressMsg
		wantKind  uiActionKind
		wantScope tool.ApprovalScope
		wantPop   bool // true → head resolved (pending drops to 0)
	}{
		{name: "y approves once (offered)", req: bash, key: runeKey('y'), wantKind: uiApprove, wantScope: tool.ScopeOnce, wantPop: true},
		{name: "s approves session (offered)", req: bash, key: runeKey('s'), wantKind: uiApprove, wantScope: tool.ScopeSession, wantPop: true},
		{name: "w approves workspace (offered)", req: bash, key: runeKey('w'), wantKind: uiApprove, wantScope: tool.ScopeWorkspace, wantPop: true},
		{name: "y approves once for unknown", req: unknown, key: runeKey('y'), wantKind: uiApprove, wantScope: tool.ScopeOnce, wantPop: true},
		{name: "s not offered is a no-op", req: unknown, key: runeKey('s'), wantKind: uiNoop, wantPop: false},
		{name: "w not offered is a no-op", req: unknown, key: runeKey('w'), wantKind: uiNoop, wantPop: false},
		{name: "n denies", req: bash, key: runeKey('n'), wantKind: uiDeny, wantPop: true},
		{name: "esc denies", req: bash, key: tea.KeyPressMsg{Code: tea.KeyEsc}, wantKind: uiDeny, wantPop: true},
		{name: "other key is a no-op", req: bash, key: runeKey('z'), wantKind: uiNoop, wantPop: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := permissionModel(tt.req)
			m, action, _ := m.Update(tt.key)

			if action.Kind != tt.wantKind {
				t.Fatalf("action.Kind = %d, want %d", action.Kind, tt.wantKind)
			}
			if tt.wantKind == uiApprove {
				if action.CallID != callID(1) {
					t.Errorf("approve CallID = %v, want %v", action.CallID, callID(1))
				}
				if action.Scope != tt.wantScope {
					t.Errorf("approve Scope = %d, want %d", action.Scope, tt.wantScope)
				}
			}
			if tt.wantKind == uiDeny && action.CallID != callID(1) {
				t.Errorf("deny CallID = %v, want %v", action.CallID, callID(1))
			}
			wantPending := 1
			if tt.wantPop {
				wantPending = 0
			}
			if m.PendingCount() != wantPending {
				t.Errorf("PendingCount = %d, want %d (pop=%v)", m.PendingCount(), wantPending, tt.wantPop)
			}
		})
	}
}

// TestInteractionChoiceNavigation covers up/down selection movement in choice
// mode, including reaching index ≥ 9 in a 12-choice prompt and clamping at both
// ends. Selection is no-op-action (uiNoop) and never pops.
func TestInteractionChoiceNavigation(t *testing.T) {
	t.Parallel()

	twelve := []string{"c0", "c1", "c2", "c3", "c4", "c5", "c6", "c7", "c8", "c9", "c10", "c11"}

	tests := []struct {
		name         string
		choices      []string
		keys         []tea.KeyPressMsg
		wantSelected int
	}{
		{
			name:         "down moves selection forward",
			choices:      twelve,
			keys:         []tea.KeyPressMsg{{Code: tea.KeyDown}, {Code: tea.KeyDown}},
			wantSelected: 2,
		},
		{
			name:    "down reaches index >= 9 (row 10+)",
			choices: twelve,
			keys: []tea.KeyPressMsg{
				{Code: tea.KeyDown}, {Code: tea.KeyDown}, {Code: tea.KeyDown}, {Code: tea.KeyDown},
				{Code: tea.KeyDown}, {Code: tea.KeyDown}, {Code: tea.KeyDown}, {Code: tea.KeyDown},
				{Code: tea.KeyDown}, {Code: tea.KeyDown}, {Code: tea.KeyDown},
			},
			wantSelected: 11,
		},
		{
			name:         "down clamps at the last index",
			choices:      []string{"a", "b"},
			keys:         []tea.KeyPressMsg{{Code: tea.KeyDown}, {Code: tea.KeyDown}, {Code: tea.KeyDown}},
			wantSelected: 1,
		},
		{
			name:         "up clamps at the first index",
			choices:      []string{"a", "b"},
			keys:         []tea.KeyPressMsg{{Code: tea.KeyUp}, {Code: tea.KeyUp}},
			wantSelected: 0,
		},
		{
			name:         "up then down nets back",
			choices:      twelve,
			keys:         []tea.KeyPressMsg{{Code: tea.KeyDown}, {Code: tea.KeyDown}, {Code: tea.KeyUp}},
			wantSelected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := choiceModel(tt.choices)
			for _, k := range tt.keys {
				var action uiAction
				m, action, _ = m.Update(k)
				if action.Kind != uiNoop {
					t.Fatalf("navigation Kind = %d, want uiNoop", action.Kind)
				}
				if m.PendingCount() != 1 {
					t.Fatalf("PendingCount = %d, want 1 (navigation never pops)", m.PendingCount())
				}
			}
			if got := m.ActivePrompt().selected; got != tt.wantSelected {
				t.Errorf("selected = %d, want %d", got, tt.wantSelected)
			}
		})
	}
}

// TestInteractionChoiceSubmit covers choice-mode submission: enter answers the
// selected choice and pops; digits 1–9 are accelerators (digit beyond len is a
// no-op); o answers the LITERAL "other"; esc interrupts WITHOUT popping (the
// terminal event will clear).
func TestInteractionChoiceSubmit(t *testing.T) {
	t.Parallel()

	three := []string{"alpha", "beta", "gamma"}

	tests := []struct {
		name     string
		choices  []string
		preDowns int // down-presses before the key under test (to move selection)
		key      tea.KeyPressMsg
		wantKind uiActionKind
		wantText string
		wantPop  bool
	}{
		{name: "enter answers selected (head)", choices: three, key: tea.KeyPressMsg{Code: tea.KeyEnter}, wantKind: uiAnswer, wantText: "alpha", wantPop: true},
		// Keypad Enter (Code KeyKpEnter, distinct from KeyEnter) stringifies to
		// "enter", so isEnter routes it identically to main Enter — it must submit
		// the selected choice, not be typed literally.
		{name: "keypad enter answers selected (head)", choices: three, key: tea.KeyPressMsg{Code: tea.KeyKpEnter}, wantKind: uiAnswer, wantText: "alpha", wantPop: true},
		{name: "enter answers selected after down", choices: three, preDowns: 2, key: tea.KeyPressMsg{Code: tea.KeyEnter}, wantKind: uiAnswer, wantText: "gamma", wantPop: true},
		{name: "digit 1 answers first choice", choices: three, key: runeKey('1'), wantKind: uiAnswer, wantText: "alpha", wantPop: true},
		{name: "digit 3 answers third choice", choices: three, key: runeKey('3'), wantKind: uiAnswer, wantText: "gamma", wantPop: true},
		{name: "digit beyond len is a no-op", choices: three, key: runeKey('4'), wantKind: uiNoop, wantPop: false},
		{name: "o answers literal other", choices: three, key: runeKey('o'), wantKind: uiAnswer, wantText: "other", wantPop: true},
		{name: "esc interrupts without pop", choices: three, key: tea.KeyPressMsg{Code: tea.KeyEsc}, wantKind: uiInterrupt, wantPop: false},
		{name: "other printable key is a no-op (no free text)", choices: three, key: runeKey('q'), wantKind: uiNoop, wantPop: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := choiceModel(tt.choices)
			for i := 0; i < tt.preDowns; i++ {
				m, _, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			}
			m, action, _ := m.Update(tt.key)

			if action.Kind != tt.wantKind {
				t.Fatalf("action.Kind = %d, want %d", action.Kind, tt.wantKind)
			}
			if tt.wantKind == uiAnswer {
				if action.Text != tt.wantText {
					t.Errorf("answer Text = %q, want %q", action.Text, tt.wantText)
				}
				if action.CallID != callID(2) {
					t.Errorf("answer CallID = %v, want %v", action.CallID, callID(2))
				}
			}
			wantPending := 1
			if tt.wantPop {
				wantPending = 0
			}
			if m.PendingCount() != wantPending {
				t.Errorf("PendingCount = %d, want %d (pop=%v)", m.PendingCount(), wantPending, tt.wantPop)
			}
		})
	}
}

// TestInteractionAnswerMode covers free-text answer mode: entering the mode
// clears the saved compose draft from the box so a fresh answer is typed;
// printable keys edit the box; a non-empty enter answers the typed text and pops
// (restoring the draft); an empty enter re-prompts (no-op); esc interrupts.
func TestInteractionAnswerMode(t *testing.T) {
	t.Parallel()

	// Entering answer mode must clear the draft so the box starts empty.
	t.Run("entering answer mode clears the draft from the box", func(t *testing.T) {
		t.Parallel()
		m := newInteractionModel()
		m.input.SetValue("my draft")
		m = m.ApplyEvent(event.UserInputRequested{CallID: callID(3), Question: "name?", Choices: nil})
		if m.mode != modeAnswerPrompt {
			t.Fatalf("mode = %d, want modeAnswerPrompt", m.mode)
		}
		if got := m.input.Value(); got != "" {
			t.Errorf("answer box = %q, want empty (fresh answer field)", got)
		}
		if m.composeDraft != "my draft" {
			t.Errorf("composeDraft = %q, want preserved 'my draft'", m.composeDraft)
		}
	})

	t.Run("printable key edits the answer box", func(t *testing.T) {
		t.Parallel()
		m := newInteractionModel()
		m = m.ApplyEvent(event.UserInputRequested{CallID: callID(3), Question: "name?", Choices: nil})
		m, action, _ := m.Update(runeKey('h'))
		if action.Kind != uiNoop {
			t.Errorf("Kind = %d, want uiNoop", action.Kind)
		}
		if got := m.input.Value(); got != "h" {
			t.Errorf("answer box = %q, want %q", got, "h")
		}
	})

	t.Run("non-empty enter answers typed text and pops, restoring draft", func(t *testing.T) {
		t.Parallel()
		m := newInteractionModel()
		m.input.SetValue("draft")
		m = m.ApplyEvent(event.UserInputRequested{CallID: callID(3), Question: "name?", Choices: nil})
		m, _, _ = m.Update(runeKey('h'))
		m, _, _ = m.Update(runeKey('i'))
		m, action, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if action.Kind != uiAnswer || action.Text != "hi" || action.CallID != callID(3) {
			t.Fatalf("action = %+v, want uiAnswer 'hi' callID 3", action)
		}
		if m.PendingCount() != 0 {
			t.Errorf("PendingCount = %d, want 0 (popped)", m.PendingCount())
		}
		if m.mode != modeCompose {
			t.Errorf("mode = %d, want modeCompose (queue drained)", m.mode)
		}
		if m.input.Value() != "draft" {
			t.Errorf("restored draft = %q, want %q", m.input.Value(), "draft")
		}
	})

	t.Run("empty enter re-prompts (no-op)", func(t *testing.T) {
		t.Parallel()
		m := newInteractionModel()
		m = m.ApplyEvent(event.UserInputRequested{CallID: callID(3), Question: "name?", Choices: nil})
		m, action, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if action.Kind != uiNoop {
			t.Errorf("Kind = %d, want uiNoop (empty answer re-prompts)", action.Kind)
		}
		if m.PendingCount() != 1 {
			t.Errorf("PendingCount = %d, want 1 (not resolved)", m.PendingCount())
		}
	})

	t.Run("esc interrupts without popping", func(t *testing.T) {
		t.Parallel()
		m := newInteractionModel()
		m = m.ApplyEvent(event.UserInputRequested{CallID: callID(3), Question: "name?", Choices: nil})
		m, action, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
		if action.Kind != uiInterrupt {
			t.Errorf("Kind = %d, want uiInterrupt", action.Kind)
		}
		if m.PendingCount() != 1 {
			t.Errorf("PendingCount = %d, want 1 (esc does not pop; terminal clears)", m.PendingCount())
		}
	})
}

// TestInteractionAnswerModeEnterVariants covers how the Enter family routes in
// free-text answer mode via isEnter: main Enter and keypad Enter (KeyKpEnter,
// which stringifies to "enter") both submit the typed text and pop; shift+enter
// does NOT submit — it forwards to the input box for the textarea's newline
// binding, so no uiAnswer is produced and the prompt stays pending.
func TestInteractionAnswerModeEnterVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		key        tea.KeyPressMsg
		wantKind   uiActionKind
		wantSubmit bool // true → uiAnswer with the typed text, head popped
	}{
		{
			name:       "main enter submits typed text",
			key:        tea.KeyPressMsg{Code: tea.KeyEnter},
			wantKind:   uiAnswer,
			wantSubmit: true,
		},
		{
			name:       "keypad enter submits typed text",
			key:        tea.KeyPressMsg{Code: tea.KeyKpEnter},
			wantKind:   uiAnswer,
			wantSubmit: true,
		},
		{
			name:       "shift+enter does not submit (forwards to input)",
			key:        tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift},
			wantKind:   uiNoop,
			wantSubmit: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newInteractionModel()
			m = m.ApplyEvent(event.UserInputRequested{CallID: callID(3), Question: "name?", Choices: nil})
			// Type a non-empty answer so a submit would fire (and so a shift+enter
			// forward is distinguishable from an empty-enter no-op).
			m, _, _ = m.Update(runeKey('h'))
			m, _, _ = m.Update(runeKey('i'))

			m, action, _ := m.Update(tt.key)

			if action.Kind != tt.wantKind {
				t.Fatalf("action.Kind = %d, want %d", action.Kind, tt.wantKind)
			}
			if tt.wantSubmit {
				if action.Text != "hi" || action.CallID != callID(3) {
					t.Errorf("submit action = %+v, want uiAnswer 'hi' callID 3", action)
				}
				if m.PendingCount() != 0 {
					t.Errorf("PendingCount = %d, want 0 (submitted + popped)", m.PendingCount())
				}
			} else {
				if m.PendingCount() != 1 {
					t.Errorf("PendingCount = %d, want 1 (shift+enter did not submit)", m.PendingCount())
				}
			}
		})
	}
}

// TestInteractionAnswerModeNextFieldEmpty covers the lifecycle when one free-text
// answer is followed by another queued free-text prompt: after answering the
// first, the second's answer field starts empty (not pre-filled with the draft).
func TestInteractionAnswerModeNextFieldEmpty(t *testing.T) {
	t.Parallel()

	m := newInteractionModel()
	m.input.SetValue("draft")
	m = m.ApplyEvent(event.UserInputRequested{CallID: callID(3), Question: "q1?", Choices: nil})
	m = m.ApplyEvent(event.UserInputRequested{CallID: callID(4), Question: "q2?", Choices: nil})

	m, _, _ = m.Update(runeKey('a'))
	m, action, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if action.Kind != uiAnswer || action.Text != "a" {
		t.Fatalf("first answer = %+v, want uiAnswer 'a'", action)
	}
	if m.mode != modeAnswerPrompt || m.PendingCount() != 1 {
		t.Fatalf("after first answer: mode %d pending %d, want answer/1", m.mode, m.PendingCount())
	}
	if m.input.Value() != "" {
		t.Errorf("second answer field = %q, want empty", m.input.Value())
	}
}

// TestInteractionComposeSlashNavigation covers the compose-mode slash panel
// navigation deferred from Task 7: with the panel visible, up/down move the
// highlight, tab fills the input with the highlighted command, and enter
// dispatches the HIGHLIGHTED command (not the re-parsed typed text).
func TestInteractionComposeSlashNavigation(t *testing.T) {
	t.Parallel()

	t.Run("down then enter dispatches the highlighted command", func(t *testing.T) {
		t.Parallel()
		// Typing "/" matches all commands (/clear, /help); the panel is visible.
		m := newInteractionModel()
		m, _, _ = m.Update(runeKey('/'))
		if m.slash == nil {
			t.Fatal("slash panel = nil after typing '/', want visible")
		}
		// Head highlight is /clear; down moves to /help.
		m, navAction, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		if navAction.Kind != uiNoop {
			t.Errorf("down Kind = %d, want uiNoop", navAction.Kind)
		}
		m, action, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if action.Kind != uiRunSlash || action.Slash != "/help" {
			t.Fatalf("action = %+v, want uiRunSlash '/help' (highlighted, not typed)", action)
		}
		if m.slash != nil {
			t.Errorf("slash panel = %+v, want nil after dispatch", m.slash)
		}
	})

	t.Run("up wraps and enter dispatches the highlighted command", func(t *testing.T) {
		t.Parallel()
		m := newInteractionModel()
		m, _, _ = m.Update(runeKey('/'))
		// Up from /clear wraps to /help (the completer wraps).
		m, _, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		_, action, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if action.Kind != uiRunSlash || action.Slash != "/help" {
			t.Fatalf("action = %+v, want uiRunSlash '/help' (wrapped highlight)", action)
		}
	})

	t.Run("tab fills the input with the highlighted command", func(t *testing.T) {
		t.Parallel()
		m := newInteractionModel()
		m, _, _ = m.Update(runeKey('/'))
		m, action, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		if action.Kind != uiNoop {
			t.Errorf("tab Kind = %d, want uiNoop", action.Kind)
		}
		if m.input.Value() != "/clear" {
			t.Errorf("input after tab = %q, want %q", m.input.Value(), "/clear")
		}
		if m.slash != nil {
			t.Errorf("slash panel = %+v, want nil after tab fill", m.slash)
		}
	})
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
