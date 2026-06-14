package tui

import (
	"context"

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

// Screen is the Elm model for the chat TUI. It owns all display state — the
// transcript, the live token accumulator, the turn status, the input queue, and
// the active stream reader — and drives a single Agent. There is no separate
// goroutine: streaming and interrupts are tea.Cmds whose results return as msgs.
type Screen struct {
	agent     Agent
	openAgent OpenAgent       // builds a replacement agent on /clear
	appCtx    context.Context // long-lived; cancelled on quit

	messages []DisplayMessage               // display history
	stream   string                         // live token accumulator (current turn)
	status   Status                         // Idle | Running | Interrupting | Resetting
	queue    []queuedInput                  // inputs submitted while Running, FIFO
	reader   *llm.StreamReader[event.Event] // active turn's stream; nil when idle

	history       components.ChatHistory
	input         components.InputBox
	slashComplete *components.SlashComplete // nil = hidden
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
		m.history.Resize(msg.Width, m.historyHeight())
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
// that pulls the next event (readNext). Unknown events are no-ops.
func (m *Screen) handleEvent(ev event.Event) tea.Cmd {
	switch ev := ev.(type) {
	case event.TurnStarted:
		// Already Running; nothing to display.
	case event.TokenDelta:
		if tc, ok := ev.Chunk.(*content.TextChunk); ok {
			m.stream += tc.Text
			m.refreshHistory()
		}
		// Any other chunk variant (e.g. *content.ThinkingChunk) is skipped.
	case event.TurnDone:
		var blocks []content.Block
		if ev.Message != nil {
			blocks = ev.Message.Blocks
		}
		m.messages = append(m.messages, DisplayMessage{Role: RoleAssistant, Blocks: blocks})
		m.stream = ""
		m.refreshHistory()
	case event.TurnFailed:
		m.appendError(ev.Err)
		m.stream = ""
		m.refreshHistory()
	case event.TurnInterrupted:
		if m.stream != "" {
			m.messages = append(m.messages, DisplayMessage{
				Role:   RoleAssistant,
				Blocks: []content.Block{&content.TextBlock{Text: m.stream}},
			})
		}
		m.messages = append(m.messages, DisplayMessage{Role: RoleInterrupted, Blocks: nil})
		m.stream = ""
		m.refreshHistory()
	}
	return readNext(m.reader)
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
	m.stream = ""
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
	rows := []string{m.history.View(), RenderStatusLine(m.status)}
	if m.slashComplete != nil {
		rows = append(rows, m.slashComplete.View())
	}
	rows = append(rows, m.input.View())
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// handleKey routes a key press. Placeholder until the key-handling dispatch lands.
func (m Screen) handleKey(_ tea.KeyMsg) (tea.Model, tea.Cmd) {
	// TODO(next dispatch): key handling (Enter/Esc/Ctrl+C/slash-complete).
	return m, nil
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
// history viewport. Call it after any change to messages, stream, queue, or width.
func (m *Screen) refreshHistory() {
	queued := make(map[int]bool, len(m.queue))
	for _, q := range m.queue {
		queued[q.DisplayIndex] = true
	}
	rendered := renderMessages(m.messages, m.stream, queued, m.contentWidth())
	m.history.SetContent(rendered)
}

// contentWidth is the column budget for rendered transcript text.
func (m Screen) contentWidth() int {
	return max(0, m.width)
}

// historyHeight is the viewport height: total height minus the status line and
// input box, floored at zero.
func (m Screen) historyHeight() int {
	return max(0, m.height-reservedLines)
}
