package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/tui/components"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// otherChoice is the literal escape-hatch answer the 'o' accelerator sends in
// choice mode. It MUST equal tools.otherChoice (the AskUser validateAnswer
// contract): with choices present, an answer is valid only if it is a listed
// choice or this exact literal. It is duplicated here rather than imported because
// package tools is a higher layer (the tui must not depend on it).
const otherChoice = "other"

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
// slash-completion panel, the FIFO queue of pending prompts (keyed by ToolExecutionID), and
// the saved compose draft restored when the queue drains. It is a value type
// driven Elm-style: Update/ApplyEvent return a new interactionModel. It holds NO
// agent reference — it only PRODUCES a typed uiAction for Screen to act on.
type interactionModel struct {
	mode         interactionMode
	pending      []prompt // FIFO; pending[0] is the active prompt
	input        components.InputBox
	slash        *components.SlashComplete // slash-command panel; nil = hidden
	files        *components.FileComplete  // @path completion panel; nil = hidden
	composeDraft string                    // editor text saved when a prompt preempts compose
}

// newInteractionModel returns an idle model in compose mode with a focused input.
func newInteractionModel() interactionModel {
	return interactionModel{
		mode:  modeCompose,
		input: components.NewInputBox(),
	}
}

// cloneHead returns a copy of pending whose head element is a distinct value, so
// mutating index 0 of the result never writes through a slice the caller still
// holds. interactionModel is driven by value, but a slice header copy shares its
// backing array; the selection handlers mutate the head in place, so they must
// clone it first to stay sound under that value-copy contract. copy duplicates the
// slice of prompt value structs into a fresh backing array, so every element —
// head and tail — is an independent copy. pending must be non-empty.
func cloneHead(pending []prompt) []prompt {
	out := make([]prompt, len(pending))
	copy(out, pending)
	return out
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
// ToolExecutionID), stamped with its producing loop's id (ev.EventHeader().LoopID), and
// reveals the head; the terminal events clear only the FINISHING loop's pending
// prompts (ClearPromptsForLoop) and, if that drains the queue, restore compose —
// a sibling loop's pending gate is left intact (design §7). All other events are
// no-ops here (the transcript owns them). It returns the updated model by value.
func (m interactionModel) ApplyEvent(ev event.Event) interactionModel {
	switch ev := ev.(type) {
	case event.PermissionRequested:
		m.enqueueForLoop(promptFromPermission(ev.ToolExecutionID, ev.Request), ev.EventHeader().LoopID)
	case event.UserInputRequested:
		m.enqueueForLoop(promptFromUserInput(ev.ToolExecutionID, ev.Question, ev.Choices), ev.EventHeader().LoopID)
	case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
		m = m.ClearPromptsForLoop(ev.EventHeader().LoopID)
	}
	return m
}

// enqueueForLoop stamps p with its producing loop's id and enqueues it. The
// LoopID scopes terminal-event clearing per loop (ClearPromptsForLoop, design
// §7); it is set here at the single enqueue site rather than threaded through the
// prompt constructors, so prompt construction stays purely about the gate's
// view-model and the producer-identity concern lives at the event boundary.
func (m *interactionModel) enqueueForLoop(p prompt, loopID uuid.UUID) {
	p.LoopID = loopID
	m.enqueue(p)
}

// enqueue appends p unless a prompt with the same ToolExecutionID is already pending
// (append-once: a duplicate gate event must not double-queue). The first prompt to
// land saves the current compose draft and switches the mode to the head's mode;
// subsequent appends leave the active head and mode untouched.
func (m *interactionModel) enqueue(p prompt) {
	for i := range m.pending {
		if m.pending[i].ToolExecutionID == p.ToolExecutionID {
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
//
// When no prompt was active it is a no-op. Terminal events fire on every turn end,
// and restoring the (empty) saved draft would clobber text the user typed into the
// composer while the turn was streaming. Only an actually-preempted compose is restored.
func (m interactionModel) ClearPrompts() interactionModel {
	if len(m.pending) == 0 {
		return m
	}
	m.pending = nil
	m.restoreCompose()
	return m
}

// ClearPromptsForLoop drops only the pending prompts produced by loopID and
// reveals what remains. It is the per-turn terminal-event path (design §7): when
// one loop's turn ends (TurnDone/TurnFailed/TurnInterrupted), only THAT loop's
// unresolved gates are abandoned — a sibling loop's pending prompt survives,
// where the blanket ClearPrompts would have wrongly dropped it. Value receiver
// returning a new model, matching ClearPrompts/pop's Elm-style contract.
//
// It is a no-op (returns m unchanged) when nothing pends or when no pending
// prompt belongs to loopID — terminal events fire on every turn end, and an
// unrelated loop's end must not restore the (possibly empty) saved draft over
// text the user typed into the composer while the turn streamed.
//
// When some prompts match, it builds a FRESH slice of the survivors (LoopID !=
// loopID) — never an in-place filter, since the value-copy model shares pending's
// backing array with the caller (see cloneHead). If the queue then drains it
// restores compose; otherwise it re-syncs the mode to the new head, which may
// have changed when the prior active head was one of the cleared prompts.
func (m interactionModel) ClearPromptsForLoop(loopID uuid.UUID) interactionModel {
	if len(m.pending) == 0 {
		return m
	}
	kept := make([]prompt, 0, len(m.pending))
	for _, p := range m.pending {
		if p.LoopID != loopID {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(m.pending) {
		return m // nothing belonged to loopID — no compose clobber
	}
	m.pending = kept
	if len(m.pending) == 0 {
		m.restoreCompose()
	} else {
		m.syncModeToHead()
	}
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
		// The input box IS the answer field, so it must start empty: the compose
		// draft was already saved to composeDraft at enqueue and is restored by
		// restoreCompose when the queue drains. A back-to-back free-text prompt
		// likewise opens on an empty field.
		m.input.SetValue("")
	default:
		m.mode = modeChoicePrompt
	}
}

// restoreCompose returns to compose mode and refills the editor with the saved
// draft, clearing any stale completion panel.
func (m *interactionModel) restoreCompose() {
	m.mode = modeCompose
	m.input.SetValue(m.composeDraft)
	m.slash, m.files = nil, nil
}

// noop is the consumed-key result: nothing for Screen to act on, re-render only.
var noop = uiAction{Kind: uiNoop}

// isEnter reports whether msg is a submit Enter — main Enter or keypad Enter,
// both of which stringify to "enter" (KeyEnter and KeyKpEnter share that
// keystroke). shift+enter stringifies to "shift+enter", so it is naturally
// excluded here and forwards to the textarea's newline binding. Routing Enter
// through this one helper keeps the decision identical across every mode
// (compose + the three prompt modes) so a real key like keypad Enter cannot
// submit in one mode yet be typed literally in another.
func isEnter(msg tea.KeyPressMsg) bool { return msg.String() == "enter" }

// Update advances the model on a key press and returns the new model, a typed
// uiAction, and the editor's cursor-blink Cmd. It dispatches on the current mode:
// compose edits/submits the next message; the three prompt modes route the key to a
// per-mode handler that produces the typed action (approve/deny/answer/interrupt).
// When a prompt-mode handler resolves the head (approve/deny/answer) the head is
// popped optimistically in the returned model — fire-and-route, no ack — revealing
// the next prompt or returning to compose. esc precedence is encoded per mode: deny
// in permission mode, interrupt (no pop) in choice/answer mode, existing behavior in
// compose. The third return is the textarea's blink Cmd from the editing modes
// (compose + free-text answer); the prompt control modes return a nil Cmd. Screen
// batches it so the cursor keeps blinking wherever the composer is the active field.
func (m interactionModel) Update(msg tea.KeyPressMsg) (interactionModel, uiAction, tea.Cmd) {
	switch m.mode {
	case modePermissionPrompt:
		model, action := m.permissionKey(msg)
		return model, action, nil
	case modeChoicePrompt:
		model, action := m.choiceKey(msg)
		return model, action, nil
	case modeAnswerPrompt:
		return m.answerKey(msg)
	default:
		return m.composeKey(msg)
	}
}

// permissionKey routes a key in modePermissionPrompt (head is a promptPermission).
// y/s/w approve at once/session/workspace but ONLY when the head offers that
// scope (else a no-op); n or esc deny (fail-secure); any other key re-renders.
// An approve/deny resolves the head, so it pops optimistically.
func (m interactionModel) permissionKey(msg tea.KeyPressMsg) (interactionModel, uiAction) {
	head := *m.ActivePrompt()
	if msg.Code == tea.KeyEsc {
		return m.pop(), uiAction{Kind: uiDeny, LoopID: head.LoopID, ToolExecutionID: head.ToolExecutionID}
	}
	switch msg.Code {
	case 'y':
		return m.approveAt(head, tool.ScopeOnce)
	case 's':
		return m.approveAt(head, tool.ScopeSession)
	case 'w':
		return m.approveAt(head, tool.ScopeWorkspace)
	case 'n':
		return m.pop(), uiAction{Kind: uiDeny, LoopID: head.LoopID, ToolExecutionID: head.ToolExecutionID}
	}
	return m, noop
}

// approveAt approves head at scope when the head offers it (pop + uiApprove),
// otherwise it is a no-op (the key names a scope the request never granted).
func (m interactionModel) approveAt(head prompt, scope tool.ApprovalScope) (interactionModel, uiAction) {
	if !head.offersScope(scope) {
		return m, noop
	}
	return m.pop(), uiAction{Kind: uiApprove, LoopID: head.LoopID, ToolExecutionID: head.ToolExecutionID, Scope: scope}
}

// choiceKey routes a key in modeChoicePrompt (head is a promptUserInput with
// choices). up/down move the selection (no-op action, no pop); enter answers the
// selected choice; 1–9 are accelerators for choices at that index; o answers the
// literal "other" escape hatch; esc interrupts WITHOUT popping (the terminal event
// clears). There is no free-text capture in choice mode — any other key re-renders.
func (m interactionModel) choiceKey(msg tea.KeyPressMsg) (interactionModel, uiAction) {
	head := *m.ActivePrompt()
	if msg.Code == tea.KeyEsc {
		return m, uiAction{Kind: uiInterrupt}
	}
	if isEnter(msg) {
		return m.pop(), uiAction{Kind: uiAnswer, LoopID: head.LoopID, ToolExecutionID: head.ToolExecutionID, Text: head.Choices[head.selected]}
	}
	switch msg.Code {
	case tea.KeyUp:
		return m.selectBy(-1)
	case tea.KeyDown:
		return m.selectBy(1)
	case 'o':
		return m.pop(), uiAction{Kind: uiAnswer, LoopID: head.LoopID, ToolExecutionID: head.ToolExecutionID, Text: otherChoice}
	}
	if i := int(msg.Code - '1'); msg.Code >= '1' && msg.Code <= '9' && i < len(head.Choices) {
		return m.pop(), uiAction{Kind: uiAnswer, LoopID: head.LoopID, ToolExecutionID: head.ToolExecutionID, Text: head.Choices[i]}
	}
	return m, noop
}

// selectBy moves the head choice cursor by delta and returns a no-op (selection
// is local state, nothing for Screen to act on). Under the value-copy model the
// returned model's pending slice shares its backing array with the caller, so the
// head is cloned before mutating to avoid writing through the caller's slice.
func (m interactionModel) selectBy(delta int) (interactionModel, uiAction) {
	m.pending = cloneHead(m.pending)
	m.pending[0].moveSelection(delta)
	return m, noop
}

// answerKey routes a key in modeAnswerPrompt (head is a free-text promptUserInput;
// the input box IS the answer field). enter submits the typed answer when
// non-empty (pop + uiAnswer; the queue-drain restore puts the compose draft back);
// an empty enter re-prompts (no-op); esc interrupts WITHOUT popping; every other
// key (including shift+enter's newline via the textarea binding) forwards to the
// editor and returns its blink Cmd so the answer field's cursor keeps blinking.
func (m interactionModel) answerKey(msg tea.KeyPressMsg) (interactionModel, uiAction, tea.Cmd) {
	head := *m.ActivePrompt()
	if msg.Code == tea.KeyEsc {
		return m, uiAction{Kind: uiInterrupt}, nil
	}
	if isEnter(msg) {
		v := m.input.Value()
		if strings.TrimSpace(v) == "" {
			return m, noop, nil
		}
		return m.pop(), uiAction{Kind: uiAnswer, LoopID: head.LoopID, ToolExecutionID: head.ToolExecutionID, Text: v}, nil
	}
	cmd := m.input.Update(msg)
	return m, noop, cmd
}

// composeKey routes a key in modeCompose. When the slash panel is visible it owns
// tab/up/down/enter (mirroring screen.go's handleKey/handleEnter): tab fills the
// input with the highlighted command, up/down navigate the panel, and enter
// dispatches the HIGHLIGHTED command. With no panel, a bare Enter submits/runs via
// composeEnter; any other key edits the editor and rebuilds the panel, returning the
// editor's blink Cmd so the composer cursor keeps blinking. The panel-navigation and
// submit/run keys are pure state changes — they return a nil Cmd.
func (m interactionModel) composeKey(msg tea.KeyPressMsg) (interactionModel, uiAction, tea.Cmd) {
	if m.slash != nil {
		if isEnter(msg) {
			name := m.slash.Selected().Name
			m.input.Reset()
			m.slash = nil
			return m, slashAction(name), nil
		}
		switch msg.String() {
		case "tab":
			m.input.SetValue(m.slash.Selected().Name)
			m.slash = nil
			return m, noop, nil
		case "up":
			m.slash.Up()
			return m, noop, nil
		case "down":
			m.slash.Down()
			return m, noop, nil
		}
	}
	// The @path panel owns tab/enter (complete the highlighted entry) and up/down
	// (navigate). Tab and Enter both COMPLETE here (they never submit while the panel is
	// open); completing a directory keeps the panel open one level in (completeAtPath).
	if m.files != nil {
		switch {
		case isEnter(msg), msg.String() == "tab":
			m.completeAtPath(m.files.Selected())
			return m, noop, nil
		case msg.String() == "up":
			m.files.Up()
			return m, noop, nil
		case msg.String() == "down":
			m.files.Down()
			return m, noop, nil
		}
	}
	if isEnter(msg) {
		model, action := m.composeEnter()
		return model, action, nil
	}
	cmd := m.forwardToInput(msg)
	return m, noop, cmd
}

// completeAtPath replaces the @path partial being typed at the end of the editor with
// the selected entry: "<text before>@<path>", appending "/" for a directory. After
// completing a directory the panel re-lists that directory's contents (drill in); after
// a file it hides. SetValue keeps the editor cursor at the end, ready to keep typing.
func (m *interactionModel) completeAtPath(sel components.FileItem) {
	v := m.input.Value()
	before := strings.TrimSuffix(v, lastField(v)) // text before the trailing @token
	path := sel.Path
	if sel.IsDir {
		path += "/"
	}
	m.input.SetValue(before + "@" + path)
	if sel.IsDir {
		m.files = components.NewFileComplete(listFiles(path))
	} else {
		m.files = nil
	}
}

// composeEnter resolves a bare Enter in compose mode WITH NO slash panel by
// re-parsing the typed text. An empty/whitespace draft is a no-op (input kept). A
// leading-slash known command resets the input and returns uiRunSlash; an unknown
// slash falls through to a plain submit. A plain submit resets the input and
// returns uiSubmit carrying the composed text. The panel-visible slash path
// (dispatching the highlighted m.slash.Selected entry and Tab/Up/Down navigation)
// lives in composeKey, mirroring screen.go's handleEnter/handleKey slash dispatch.
func (m interactionModel) composeEnter() (interactionModel, uiAction) {
	v := m.input.Value()
	if strings.TrimSpace(v) == "" {
		return m, noop
	}
	if strings.HasPrefix(v, "/") {
		name := firstToken(v)
		if isSlashCommand(name) {
			m.input.Reset()
			m.slash = nil
			return m, slashAction(name)
		}
		// Unknown command: fall through to a plain-text submit.
	}
	m.input.Reset()
	m.slash, m.files = nil, nil
	return m, uiAction{Kind: uiSubmit, Text: v}
}

// forwardToInput sends the key to the editor and rebuilds the slash-completion
// panel from the new value: a leading-slash word (no whitespace) rebuilds it from
// the prefix (nil if nothing matches); anything else hides it. It returns the
// editor's Cmd (cursor blink) so the caller can keep the composer cursor alive. It
// mirrors screen.go's forwardToInput so compose behavior is identical.
func (m *interactionModel) forwardToInput(msg tea.KeyPressMsg) tea.Cmd {
	cmd := m.input.Update(msg)
	v := m.input.Value()
	// A leading-slash word opens the command panel; otherwise an @path being typed at
	// the end of the editor opens the file panel. They are mutually exclusive, and any
	// other text hides both.
	m.slash, m.files = nil, nil
	switch {
	case strings.HasPrefix(v, "/") && !strings.ContainsAny(v, " \t\n"):
		m.slash = components.NewSlashComplete(firstToken(v))
	default:
		if partial, ok := activeAtToken(v); ok {
			m.files = components.NewFileComplete(listFiles(partial))
		}
	}
	return cmd
}

// helpText builds the /help listing from the canonical command table. Screen
// commits the result as a system entry when /help runs.
func helpText() string {
	var b strings.Builder
	b.WriteString("commands:")
	for _, c := range components.SlashCommands {
		b.WriteString("\n  " + c.Name + " — " + c.Desc)
	}
	return b.String()
}

// slashAction maps a recognized slash-command name to the typed uiAction Screen acts
// on. Most commands share the generic uiRunSlash carrier (Screen.runSlash switches on
// the name); /export is the one exception with its own uiExport kind so its
// snapshot-anytime semantics read distinctly from /help and /clear at the action layer.
// Screen still funnels uiExport back through runSlash("/export"), keeping all
// status-gated slash execution in one place.
func slashAction(name string) uiAction {
	if name == components.CmdExport {
		return uiAction{Kind: uiExport}
	}
	return uiAction{Kind: uiRunSlash, Slash: name}
}

// isSlashCommand reports whether name is one of the canonical slash commands.
func isSlashCommand(name string) bool {
	for _, c := range components.SlashCommands {
		if c.Name == name {
			return true
		}
	}
	return false
}

// firstToken returns the first whitespace-delimited token of s, or "" if none.
func firstToken(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
