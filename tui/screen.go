package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/tui/components"
)

// reservedLines is the vertical space the status line (1) plus the input box (3)
// occupy below the history viewport.
const reservedLines = 4

// liveSegment is the in-progress assistant segment for the current turn: the
// streamed narration text plus the tool calls reconstructed from the event
// stream. It is committed to a DisplayMessage when the segment ends. calls stays
// empty until the event-reconstruction state machine populates it (later phase).
type liveSegment struct {
	text  string
	calls []ToolCallView
}

// Screen is the Elm model for the chat TUI. It owns all display state — the
// transcript, the live segment accumulator, the turn status, the input queue, and
// the active stream reader — and drives a single Agent. There is no separate
// goroutine: streaming and interrupts are tea.Cmds whose results return as msgs.
type Screen struct {
	agent     Agent
	openAgent OpenAgent       // builds a replacement agent on /clear
	appCtx    context.Context // long-lived; cancelled on quit

	messages []DisplayMessage               // display history
	live     liveSegment                    // in-progress segment (current turn)
	status   Status                         // Idle | Running | Interrupting | Resetting
	queue    []queuedInput                  // inputs submitted while Running, FIFO
	reader   *llm.StreamReader[event.Event] // active turn's stream; nil when idle

	history       components.ChatHistory
	input         components.InputBox
	slashComplete *components.SlashComplete // nil = hidden
	expandTools   bool                      // Ctrl+T toggle; false = collapsed tool previews
	width, height int
	ready         bool
}

// New constructs an idle Screen driving agent, with open as the /clear thunk.
func New(ctx context.Context, agent Agent, open OpenAgent) Screen {
	return Screen{
		agent:     agent,
		openAgent: open,
		appCtx:    ctx,
		status:    StatusIdle,
		input:     components.NewInputBox(),
		history:   components.NewChatHistory(0, 0),
	}
}

// Init focuses the input (cursor blink) and emits the initial system "ready" row.
func (m Screen) Init() tea.Cmd {
	return tea.Batch(m.input.Focus(), func() tea.Msg { return systemReadyMsg{} })
}

// Agent returns the live agent. cmd/cli uses this for a bounded backstop Close
// of whichever agent /clear may have swapped in.
func (m Screen) Agent() Agent { return m.agent }

// Update advances the model. It is a value receiver so Screen satisfies tea.Model;
// helpers that mutate (startTurn/appendError/refreshHistory/handleKey) take a
// pointer to the addressable receiver and the updated value is returned.
func (m Screen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		m.input.Resize(msg.Width)
		m.resizeHistory()
		m.refreshHistory()
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	case eventMsg:
		return m, m.handleEvent(msg.ev)
	case streamEOFMsg:
		return m, m.finishTurnAdvanceQueue()
	case streamErrMsg:
		m.appendError(msg.err)
		return m, m.finishTurnAdvanceQueue()
	case interruptResultMsg:
		return m, m.handleInterruptResult(msg)
	case reopenResultMsg:
		return m, m.handleReopenResult(msg)
	case systemReadyMsg:
		m.messages = append(m.messages, DisplayMessage{
			Role:   RoleSystem,
			Blocks: []content.Block{&content.TextBlock{Text: "session ready"}},
		})
		m.refreshHistory()
		return m, nil
	}
	return m, nil
}

// handleEvent applies one turn-stream event to the model and returns the command
// that pulls the next event (readNext). Unknown events are no-ops. It reconstructs
// per-segment tool-call structure from the ordered event stream: an assistant text
// segment is committed (with its tool-call children) when a new segment begins —
// either when narration arrives after a tool batch (TokenDelta) or when a fresh,
// narration-less batch starts (ToolCallStarted) — and at every terminal event.
func (m *Screen) handleEvent(ev event.Event) tea.Cmd {
	switch ev := ev.(type) {
	case event.TurnStarted:
		// Already Running; nothing to display.
	case event.TokenDelta:
		if tc, ok := ev.Chunk.(*content.TextChunk); ok {
			// Narration following a completed tool batch begins a NEW segment:
			// commit the prior segment (its text + cards) before accumulating.
			if len(m.live.calls) > 0 {
				m.commitLive()
			}
			m.live.text += tc.Text
			m.refreshHistory()
		}
		// Any other chunk variant (e.g. *content.ThinkingChunk) is skipped.
	case event.ToolCallStarted:
		m.handleToolStarted(ev)
	case event.ToolCallCompleted:
		m.handleToolCompleted(ev)
	case event.TurnDone:
		m.handleTurnDone(ev)
	case event.TurnFailed:
		if m.liveNonEmpty() {
			m.commitLive() // keep completed tool work + partial text visible
		}
		m.appendError(ev.Err)
		m.live = liveSegment{}
		m.refreshHistory()
	case event.TurnInterrupted:
		m.handleTurnInterrupted()
	}
	return readNext(m.reader)
}

// liveNonEmpty reports whether the live segment carries committable content —
// either streamed narration text or any reconstructed tool call.
func (m *Screen) liveNonEmpty() bool {
	return m.live.text != "" || len(m.live.calls) > 0
}

// commitLive appends the live segment to the transcript as one RoleAssistant row
// carrying BOTH its narration text and its tool-call children, then resets the
// live segment. It is a no-op when the live segment is empty. A segment with no
// text but with calls still commits (a bare assistant row whose only content is
// its tool cards, per design §3): the empty TextBlock is omitted so the row's
// Blocks are nil and only ToolCalls survive.
func (m *Screen) commitLive() {
	if !m.liveNonEmpty() {
		return
	}
	var blocks []content.Block
	if m.live.text != "" {
		blocks = []content.Block{&content.TextBlock{Text: m.live.text}}
	}
	m.messages = append(m.messages, DisplayMessage{
		Role:      RoleAssistant,
		Blocks:    blocks,
		ToolCalls: m.live.calls,
	})
	m.live = liveSegment{}
}

// handleToolStarted appends a new running tool card to the live segment. When the
// existing cards are all terminal, the new card begins a fresh batch with no
// narration between (design §5 back-to-back) — the prior segment is committed
// first so each batch becomes its own (possibly bare) assistant row.
func (m *Screen) handleToolStarted(ev event.ToolCallStarted) {
	if len(m.live.calls) > 0 && m.allCallsTerminal() {
		m.commitLive()
	}
	m.live.calls = append(m.live.calls, ToolCallView{
		CallID:   ev.CallID,
		ToolName: ev.ToolName,
		Summary:  ev.Summary,
		Status:   ToolRunning,
	})
	m.refreshHistory()
}

// allCallsTerminal reports whether every call in the live segment has reached a
// terminal status (anything other than ToolRunning). An empty slice is vacuously
// terminal, but callers guard on len(calls) > 0 before relying on it.
func (m *Screen) allCallsTerminal() bool {
	for i := range m.live.calls {
		if m.live.calls[i].Status == ToolRunning {
			return false
		}
	}
	return true
}

// handleToolCompleted flips the matching tool card (by CallID) to its terminal
// status and attaches the result preview. It searches the live segment first,
// then falls back to the most recent committed RoleAssistant row (defensive: the
// batch normally completes while its cards are still live). An unknown CallID is
// a no-op — no panic.
func (m *Screen) handleToolCompleted(ev event.ToolCallCompleted) {
	status := ToolOK
	if ev.IsError {
		status = ToolError
	}
	result := splitLines(ev.ResultPreview)

	for i := range m.live.calls {
		if m.live.calls[i].CallID == ev.CallID {
			m.live.calls[i].Status = status
			m.live.calls[i].Result = result
			m.refreshHistory()
			return
		}
	}
	// Fallback: scan the most recent committed RoleAssistant segment's children.
	for mi := len(m.messages) - 1; mi >= 0; mi-- {
		if m.messages[mi].Role != RoleAssistant {
			continue
		}
		for ci := range m.messages[mi].ToolCalls {
			if m.messages[mi].ToolCalls[ci].CallID == ev.CallID {
				m.messages[mi].ToolCalls[ci].Status = status
				m.messages[mi].ToolCalls[ci].Result = result
				m.refreshHistory()
				return
			}
		}
		break // only the most recent assistant segment is a candidate
	}
	// Unknown CallID: no-op.
}

// handleTurnDone commits the final assistant segment. The live segment is
// authoritative: if it carries content it is committed (its streamed text + any
// cards). Otherwise — and ONLY otherwise — ev.Message is the fallback for a
// non-streamed message. Never both, so the streamed final text is never
// duplicated by the message copy (design §6). A nil Message with an empty live
// produces no final segment.
func (m *Screen) handleTurnDone(ev event.TurnDone) {
	switch {
	case m.liveNonEmpty():
		m.commitLive()
	case ev.Message != nil && len(ev.Message.Blocks) > 0:
		m.messages = append(m.messages, DisplayMessage{Role: RoleAssistant, Blocks: ev.Message.Blocks})
	}
	m.live = liveSegment{}
	m.refreshHistory()
}

// handleTurnInterrupted commits whatever tool work + partial text exists so it
// stays visible, marking any still-running card as cancelled first, then appends
// the RoleInterrupted tombstone. This fixes the prior flush that dropped the live
// segment's tool cards on interrupt.
func (m *Screen) handleTurnInterrupted() {
	for i := range m.live.calls {
		if m.live.calls[i].Status == ToolRunning {
			m.live.calls[i].Status = ToolCancelled
		}
	}
	m.commitLive()
	m.messages = append(m.messages, DisplayMessage{Role: RoleInterrupted, Blocks: nil})
	m.live = liveSegment{}
	m.refreshHistory()
}

// splitLines splits a tool-result preview into display lines on "\n". An empty
// preview yields nil (no result lines; the renderer shows "(no output)"); a
// non-empty preview always yields at least one line. A trailing newline produces
// a trailing empty line, preserved as-is (the runner caps/marks the preview).
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// finishTurnAdvanceQueue closes the active reader, returns the model to Idle, and
// peeks the queue: if a queued input starts successfully its head is removed and
// the new turn's first readNext is returned; otherwise the head stays queued
// (startTurn already showed a RoleError and stayed Idle) and nil is returned. It
// is shared by the EOF and error stream arms.
func (m *Screen) finishTurnAdvanceQueue() tea.Cmd {
	if m.reader != nil {
		_ = m.reader.Close() // best-effort; idempotent closer, nothing actionable at the UI
	}
	m.reader = nil
	m.status = StatusIdle

	if len(m.queue) > 0 {
		head := m.queue[0]
		cmd, ok := m.startTurn(head.Blocks)
		if ok {
			m.queue = m.queue[1:] // remove the head; its RoleUser row already exists
		}
		m.refreshHistory()
		return cmd
	}
	m.refreshHistory()
	return nil
}

// handleInterruptResult applies the outcome of an Interrupt call. On error the
// turn may still be live, so the model returns to Running and surfaces a RoleError;
// on success it stays Interrupting — the loop's TurnInterrupted terminal event (or
// the in-flight stream's pending EOF when cancelled==false) returns it to Idle.
func (m *Screen) handleInterruptResult(msg interruptResultMsg) tea.Cmd {
	if msg.err != nil {
		m.appendError(msg.err)
		m.status = StatusRunning
		m.refreshHistory()
	}
	return nil
}

// handleReopenResult applies a /clear reopen outcome (the model is Resetting). On
// error the old agent is kept and the model returns to Idle with a RoleError. On
// success the fresh agent is swapped in, all display state is cleared, the model
// returns to Idle, and the old agent is closed best-effort via closeAgent.
func (m *Screen) handleReopenResult(msg reopenResultMsg) tea.Cmd {
	if msg.err != nil {
		m.appendError(msg.err)
		m.status = StatusIdle
		m.refreshHistory()
		return nil
	}
	old := m.agent
	m.agent = msg.agent
	m.messages = nil
	m.live = liveSegment{}
	m.queue = nil
	m.history.Clear()
	m.status = StatusIdle
	m.refreshHistory()
	return closeAgent(old)
}

// View renders an empty string until the first WindowSizeMsg (avoids a 0×0 first
// frame), then vertically joins history, status line, an optional slash-complete
// panel, and the input box.
func (m Screen) View() string {
	if !m.ready {
		return ""
	}
	rows := []string{m.history.View()}
	if status := RenderStatusLine(m.status); status != "" {
		// Skip the Idle empty status: JoinVertical would otherwise count it as
		// a blank row and the composite would exceed the height reservation.
		rows = append(rows, status)
	}
	if m.slashComplete != nil {
		rows = append(rows, m.slashComplete.View())
	}
	rows = append(rows, m.input.View())
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// handleKey routes a key press to the input editor, slash-complete panel, or a
// turn-control action. It mutates the addressable receiver and returns the value.
// Ctrl+C quits from any state; Esc interrupts only while Running; Tab/Up/Down
// drive the slash panel when visible; Enter submits, queues, or runs a slash
// command; any other key forwards to the input editor and rebuilds the panel.
func (m *Screen) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return *m, tea.Sequence(closeAgent(m.agent), tea.Quit)
	case "esc":
		if m.status == StatusRunning {
			return *m, m.interruptRunning()
		}
		return *m, nil
	case "tab":
		if m.slashComplete != nil {
			m.input.SetValue(m.slashComplete.Selected().Name)
			m.slashComplete = nil
			m.resizeHistory() // panel cleared: re-budget the viewport height
			return *m, nil
		}
	case "up":
		if m.slashComplete != nil {
			m.slashComplete.Up()
			return *m, nil
		}
	case "down":
		if m.slashComplete != nil {
			m.slashComplete.Down()
			return *m, nil
		}
	case "enter":
		return *m, m.handleEnter()
	case "pgup", "pgdown", "ctrl+u", "ctrl+d":
		// Scroll the transcript viewport instead of editing the input. This
		// exercises the viewport's "stick to bottom only if at bottom" logic.
		cmd := m.history.Update(msg)
		return *m, cmd
	}
	return *m, m.forwardToInput(msg)
}

// interruptRunning begins an interrupt: it flips to Interrupting, drops the
// pending queue, and removes exactly the queued-but-unsent rows from the
// transcript (by DisplayIndex) before returning the bounded Interrupt command.
func (m *Screen) interruptRunning() tea.Cmd {
	m.status = StatusInterrupting

	drop := make(map[int]bool, len(m.queue))
	for _, q := range m.queue {
		drop[q.DisplayIndex] = true
	}
	kept := make([]DisplayMessage, 0, len(m.messages))
	for i, row := range m.messages {
		if drop[i] {
			continue
		}
		kept = append(kept, row)
	}
	m.messages = kept
	m.queue = nil
	m.refreshHistory()
	return interruptTurn(m.appCtx, m.agent)
}

// handleEnter resolves an Enter press. A visible slash-complete selection or a
// leading-slash input routes through dispatchSlash; only an actual run resets
// the input (and hides the panel). Otherwise it is a plain submit/queue.
func (m *Screen) handleEnter() tea.Cmd {
	if m.slashComplete != nil {
		name := m.slashComplete.Selected().Name
		cmd, ran := m.dispatchSlash(name)
		if ran {
			m.input.Reset()
			m.slashComplete = nil
			m.resizeHistory() // panel cleared: re-budget the viewport height
		}
		return cmd
	}

	if strings.TrimSpace(m.input.Value()) == "" {
		return nil
	}

	if strings.HasPrefix(m.input.Value(), "/") {
		name := firstToken(m.input.Value())
		if isSlashCommand(name) {
			cmd, ran := m.dispatchSlash(name)
			if ran {
				m.input.Reset()
			}
			return cmd
		}
		// Unknown command: fall through to a plain-text submit.
	}

	return m.submit()
}

// dispatchSlash runs a known slash command, returning whether it actually ran.
// A no-op (e.g. /clear while busy) returns ran=false so the caller keeps the
// input and panel intact.
func (m *Screen) dispatchSlash(name string) (cmd tea.Cmd, ran bool) {
	switch name {
	case "/help":
		m.messages = append(m.messages, DisplayMessage{
			Role:   RoleSystem,
			Blocks: []content.Block{&content.TextBlock{Text: helpText()}},
		})
		m.refreshHistory()
		return nil, true
	case "/clear":
		if m.status == StatusIdle {
			m.status = StatusResetting
			return reopenAgent(m.appCtx, m.openAgent), true
		}
		return nil, false
	default:
		return nil, false
	}
}

// submit builds blocks from the input and either starts a turn (Idle), queues
// it (Running), or no-ops (Interrupting/Resetting). A buildBlocks error and a
// failed start both keep the input intact; only a successful start or queue
// appends the RoleUser row and resets the input.
func (m *Screen) submit() tea.Cmd {
	blocks, err := buildBlocks(m.input.Value(), m.agent.AcceptsImages())
	if err != nil {
		m.appendError(err) // keep input intact; no turn
		return nil
	}

	switch m.status {
	case StatusIdle:
		cmd, ok := m.startTurn(blocks)
		if ok {
			m.messages = append(m.messages, DisplayMessage{Role: RoleUser, Blocks: blocks})
			m.input.Reset()
		}
		// On !ok startTurn already appended a RoleError and stayed Idle: keep input.
		m.refreshHistory()
		return cmd
	case StatusRunning:
		m.messages = append(m.messages, DisplayMessage{Role: RoleUser, Blocks: blocks})
		m.queue = append(m.queue, queuedInput{Blocks: blocks, DisplayIndex: len(m.messages) - 1})
		m.input.Reset()
		m.refreshHistory()
		return nil
	default: // Interrupting / Resetting: no-op, keep input intact.
		return nil
	}
}

// forwardToInput sends msg to the input editor and rebuilds the slash-complete
// panel from the new value: a leading-slash word (no whitespace) rebuilds it
// from the prefix (nil if no command matches); anything else hides it.
func (m *Screen) forwardToInput(msg tea.KeyMsg) tea.Cmd {
	cmd := m.input.Update(msg)
	v := m.input.Value()
	if strings.HasPrefix(v, "/") && !strings.ContainsAny(v, " \t\n") {
		m.slashComplete = components.NewSlashComplete(firstToken(v))
	} else {
		m.slashComplete = nil
	}
	m.resizeHistory() // panel toggled: re-budget the viewport height
	return cmd
}

// helpText builds the /help listing from the canonical command table.
func helpText() string {
	var b strings.Builder
	b.WriteString("commands:")
	for _, c := range components.SlashCommands {
		b.WriteString("\n  " + c.Name + " — " + c.Desc)
	}
	return b.String()
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

// startTurn begins a turn from blocks. agent.StreamBlocks may fail before a reader
// exists (TurnBusyError, loop exited, ctx done); on error it shows a RoleError and
// stays Idle — never Running without a reader, never readNext(nil).
func (m *Screen) startTurn(blocks []content.Block) (tea.Cmd, bool) {
	r, err := m.agent.StreamBlocks(m.appCtx, blocks)
	if err != nil {
		m.appendError(err)
		m.status, m.reader = StatusIdle, nil
		return nil, false
	}
	m.reader, m.status = r, StatusRunning
	return readNext(r), true
}

// appendError appends a RoleError row carrying err's text and refreshes history.
func (m *Screen) appendError(err error) {
	m.messages = append(m.messages, DisplayMessage{
		Role:   RoleError,
		Blocks: []content.Block{&content.TextBlock{Text: err.Error()}},
	})
	m.refreshHistory()
}

// refreshHistory re-renders the transcript from current state and feeds it to the
// history viewport. Call it after any change to messages, the live segment, queue,
// or width.
func (m *Screen) refreshHistory() {
	queued := make(map[int]bool, len(m.queue))
	for _, q := range m.queue {
		queued[q.DisplayIndex] = true
	}
	rendered := renderMessages(m.messages, m.live, queued, m.expandTools, m.contentWidth())
	m.history.SetContent(rendered)
}

// contentWidth is the column budget for rendered transcript text.
func (m Screen) contentWidth() int {
	return max(0, m.width)
}

// panelHeight is the rendered height of the slash-complete panel, or 0 when the
// panel is hidden (nil). It is subtracted from the viewport height so the panel
// can never push the input box off-screen.
func (m Screen) panelHeight() int {
	if m.slashComplete == nil {
		return 0
	}
	return lipgloss.Height(m.slashComplete.View())
}

// historyHeight is the viewport height: total height minus the status/input
// reservation and the current slash-complete panel height, floored at zero.
func (m Screen) historyHeight() int {
	return max(0, m.height-reservedLines-m.panelHeight())
}

// resizeHistory re-sizes the history viewport to the current width and computed
// height. Call it after any change that affects the height budget: the window
// size, or any toggle of the slash-complete panel.
func (m *Screen) resizeHistory() {
	m.history.Resize(m.width, m.historyHeight())
}
