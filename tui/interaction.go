package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/tui/components"
)

// interactionMode is the bottom surface's current mode. Compose is the default
// (editing the next message); the three prompt modes are entered when a prompt is
// pending and select how keys are routed (Task 8 implements that routing).
type interactionMode uint8

const (
	// modeCompose edits the next user message in the input box.
	modeCompose interactionMode = iota
	// modePermissionPrompt shows the active permission gate (approve/deny/scope).
	modePermissionPrompt
	// modeChoicePrompt shows an AskUser request with selectable choices.
	modeChoicePrompt
	// modeAnswerPrompt shows a free-text AskUser request (no choices).
	modeAnswerPrompt
)

// interactionModel owns the bottom interaction surface: the compose editor, the
// slash-completion panel, the FIFO queue of pending prompts (keyed by CallID), and
// the saved compose draft restored when the queue drains. It is a value type
// driven Elm-style: Update/ApplyEvent return a new interactionModel. It holds NO
// agent reference — it only PRODUCES a typed uiAction for Screen to act on.
type interactionModel struct {
	mode         interactionMode
	pending      []prompt // FIFO; pending[0] is the active prompt
	input        components.InputBox
	slash        *components.SlashComplete // nil = hidden
	composeDraft string                    // editor text saved when a prompt preempts compose
}

// newInteractionModel returns an idle model in compose mode with a focused input.
func newInteractionModel() interactionModel {
	return interactionModel{
		mode:  modeCompose,
		input: components.NewInputBox(),
	}
}

// ActivePrompt returns the head (active) pending prompt, or nil when none pend.
// The pointer is into the model's own slice; callers must not retain it past the
// next ApplyEvent/pop.
func (m *interactionModel) ActivePrompt() *prompt {
	if len(m.pending) == 0 {
		return nil
	}
	return &m.pending[0]
}

// PendingCount is the number of queued prompts (active + waiting).
func (m interactionModel) PendingCount() int { return len(m.pending) }

// ApplyEvent folds one turn-stream event into the interaction surface. A
// PermissionRequested/UserInputRequested enqueues a prompt (append-once by
// CallID) and reveals the head; the terminal events clear every pending prompt
// and restore compose. All other events are no-ops here (the transcript owns
// them). It returns the updated model by value.
func (m interactionModel) ApplyEvent(ev event.Event) interactionModel {
	switch ev := ev.(type) {
	case event.PermissionRequested:
		m.enqueue(promptFromPermission(ev.CallID, ev.Request))
	case event.UserInputRequested:
		m.enqueue(promptFromUserInput(ev.CallID, ev.Question, ev.Choices))
	case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
		m = m.ClearPrompts()
	}
	return m
}

// enqueue appends p unless a prompt with the same CallID is already pending
// (append-once: a duplicate gate event must not double-queue). The first prompt to
// land saves the current compose draft and switches the mode to the head's mode;
// subsequent appends leave the active head and mode untouched.
func (m *interactionModel) enqueue(p prompt) {
	for i := range m.pending {
		if m.pending[i].CallID == p.CallID {
			return // already pending — ignore the duplicate
		}
	}
	if len(m.pending) == 0 {
		m.composeDraft = m.input.Value()
	}
	m.pending = append(m.pending, p)
	m.syncModeToHead()
}

// pop removes the active (head) prompt and reveals the next one. When the queue
// drains it returns to compose mode and restores the saved draft. It is the
// resolution path the modal router (Task 8) calls after approve/deny/answer.
func (m interactionModel) pop() interactionModel {
	if len(m.pending) == 0 {
		return m
	}
	m.pending = m.pending[1:]
	m.syncModeToHead()
	return m
}

// ClearPrompts drops every pending prompt and restores compose mode plus the
// saved draft. It is the terminal-event path: when the turn ends, any unresolved
// prompts are abandoned and the user is returned to their composer exactly as they
// left it. Value receiver returning a new model, matching pop's Elm-style contract.
func (m interactionModel) ClearPrompts() interactionModel {
	m.pending = nil
	m.restoreCompose()
	return m
}

// syncModeToHead sets the mode from the active prompt: permission → permission
// mode; user-input → choice or answer depending on freeText. With no pending
// prompt it returns to compose and restores the saved draft.
func (m *interactionModel) syncModeToHead() {
	p := m.ActivePrompt()
	if p == nil {
		m.restoreCompose()
		return
	}
	switch {
	case p.Kind == promptPermission:
		m.mode = modePermissionPrompt
	case p.freeText:
		m.mode = modeAnswerPrompt
	default:
		m.mode = modeChoicePrompt
	}
}

// restoreCompose returns to compose mode and refills the editor with the saved
// draft, clearing any stale slash panel.
func (m *interactionModel) restoreCompose() {
	m.mode = modeCompose
	m.input.SetValue(m.composeDraft)
	m.slash = nil
}

// Update advances the model on a key press and returns the new model plus a typed
// uiAction. THIS TASK implements modeCompose only: printable keys edit the editor
// (uiNoop), Enter submits non-empty prose (uiSubmit) or runs a known slash
// (uiRunSlash). For the prompt modes it returns uiNoop — modal key routing is the
// next task (Task 8).
func (m interactionModel) Update(msg tea.KeyPressMsg) (interactionModel, uiAction) {
	if m.mode != modeCompose {
		return m, uiAction{Kind: uiNoop}
	}
	if msg.Code == tea.KeyEnter && msg.Mod == 0 {
		return m.composeEnter()
	}
	m.forwardToInput(msg)
	return m, uiAction{Kind: uiNoop}
}

// composeEnter resolves an Enter in compose mode by re-parsing the typed text. An
// empty/whitespace draft is a no-op (input kept). A leading-slash known command
// resets the input and returns uiRunSlash; an unknown slash falls through to a
// plain submit. A plain submit resets the input and returns uiSubmit carrying the
// composed text. This mirrors only screen.go's typed-text submit/slash path:
// dispatching the highlighted slash-completion entry (m.slash.Selected) and the
// Tab/Up/Down panel navigation are deferred to the modal-routing task (Task 8).
func (m interactionModel) composeEnter() (interactionModel, uiAction) {
	v := m.input.Value()
	if strings.TrimSpace(v) == "" {
		return m, uiAction{Kind: uiNoop}
	}
	if strings.HasPrefix(v, "/") {
		name := firstToken(v)
		if isSlashCommand(name) {
			m.input.Reset()
			m.slash = nil
			return m, uiAction{Kind: uiRunSlash, Slash: name}
		}
		// Unknown command: fall through to a plain-text submit.
	}
	m.input.Reset()
	m.slash = nil
	return m, uiAction{Kind: uiSubmit, Text: v}
}

// forwardToInput sends the key to the editor and rebuilds the slash-completion
// panel from the new value: a leading-slash word (no whitespace) rebuilds it from
// the prefix (nil if nothing matches); anything else hides it. It mirrors
// screen.go's forwardToInput so compose behavior is identical.
func (m *interactionModel) forwardToInput(msg tea.KeyPressMsg) {
	_ = m.input.Update(msg)
	v := m.input.Value()
	if strings.HasPrefix(v, "/") && !strings.ContainsAny(v, " \t\n") {
		m.slash = components.NewSlashComplete(firstToken(v))
	} else {
		m.slash = nil
	}
}
