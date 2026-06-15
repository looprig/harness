package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
)

// Screen is the Elm model for the chat TUI. In scrollback-first mode it is a thin
// router over three pure helpers: transcript reconstructs the turn from the event
// stream and tracks committed entries + the live segment; scrollback prints each
// committed entry to native terminal scrollback exactly once; interaction owns the
// bottom surface (composer, slash panel, and the FIFO of pending permission/AskUser
// prompts). Screen holds only the agent wiring, the turn status, the active stream
// reader, the queued-while-running submissions, the terminal dimensions, and the
// ctrl+t expand flag. There is no transcript viewport — the terminal owns history.
type Screen struct {
	agent     Agent
	openAgent OpenAgent       // builds a replacement agent on /clear
	appCtx    context.Context // long-lived; cancelled on quit

	transcript  transcriptModel
	scrollback  scrollbackModel
	interaction interactionModel

	status Status                         // Idle | Running | Interrupting | Resetting
	queue  [][]content.Block              // submissions made while Running, FIFO
	reader *llm.StreamReader[event.Event] // active turn's stream; nil when idle

	expand        bool // Ctrl+T toggle; false = collapsed thinking + tool previews
	width, height int
	ready         bool
}

// New constructs an idle Screen driving agent, with open as the /clear thunk.
func New(ctx context.Context, agent Agent, open OpenAgent) Screen {
	return Screen{
		agent:       agent,
		openAgent:   open,
		appCtx:      ctx,
		status:      StatusIdle,
		scrollback:  newScrollbackModel(0),
		interaction: newInteractionModel(),
	}
}

// Init focuses the composer (starting the cursor blink) and emits the initial
// system "ready" entry.
func (m Screen) Init() tea.Cmd {
	return tea.Batch(m.interaction.input.Focus(), func() tea.Msg { return systemReadyMsg{} })
}

// Agent returns the live agent. cmd/cli uses this for a bounded backstop Close
// of whichever agent /clear may have swapped in.
func (m Screen) Agent() Agent { return m.agent }

// Update advances the model. It is a value receiver so Screen satisfies tea.Model;
// the mutating handlers take a pointer to the addressable receiver and the updated
// value is returned.
func (m Screen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		m.scrollback.width = msg.Width
		m.interaction.input.Resize(msg.Width)
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case eventMsg:
		return m, m.handleEvent(msg.ev)
	case streamEOFMsg:
		return m, m.finishTurnAdvanceQueue()
	case streamErrMsg:
		m.transcript = m.transcript.ApplyEvent(event.TurnFailed{Err: msg.err})
		return m, m.finishTurnAdvanceQueue()
	case interruptResultMsg:
		return m, m.handleInterruptResult(msg)
	case reopenResultMsg:
		return m, m.handleReopenResult(msg)
	case promptResultMsg:
		return m, m.handlePromptResult(msg)
	case systemReadyMsg:
		m.transcript = m.transcript.CommitSystem("session ready")
		return m, m.flush()
	}
	return m, nil
}

// handleEvent routes one turn-stream event through BOTH reducers — the transcript
// (which reconstructs the live segment and commits user/tool/prompt/terminal
// entries) and the interaction model (which enqueues prompts and clears the queue
// on terminal events) — flushes any newly committed entries to scrollback, and keeps
// draining the stream. The status LABEL is derived per-frame in View from the live
// signals + active prompt, so there is no status field to refresh here. Draining
// continues unconditionally: the loop is blocked on the permission GATE, not the
// stream, so the user's keypress (approve/deny/answer) is what releases it. A prompt
// event dispatches nothing here; the trio call happens later when the user resolves it.
func (m *Screen) handleEvent(ev event.Event) tea.Cmd {
	m.transcript = m.transcript.ApplyEvent(ev)
	m.interaction = m.interaction.ApplyEvent(ev)
	return tea.Batch(readNext(m.reader), m.flush())
}

// flush renders every newly committed transcript entry to scrollback exactly once
// and returns the print command (nil when nothing is new). renderEntry is given the
// current expand flag and width so scrollback and the live tail render identically.
func (m *Screen) flush() tea.Cmd {
	var actions []printAction
	m.scrollback, actions = m.scrollback.Flush(m.transcript.committed, func(e entry) []string {
		return renderEntry(e, m.expand, m.width)
	})
	return printToScrollback(actions)
}

// finishTurnAdvanceQueue closes the active reader, returns the model to Idle, and
// starts the next queued submission if one is waiting (its user entry was already
// committed to scrollback at submit time). It is shared by the EOF and error arms.
func (m *Screen) finishTurnAdvanceQueue() tea.Cmd {
	if m.reader != nil {
		_ = m.reader.Close() // best-effort; idempotent closer, nothing actionable at the UI
	}
	m.reader = nil
	m.status = StatusIdle

	var start tea.Cmd
	if len(m.queue) > 0 {
		head := m.queue[0]
		cmd, ok := m.startTurn(head) // may commit a kindError entry on failure
		if ok {
			m.queue = m.queue[1:]
		}
		start = cmd
	}
	// Flush AFTER the restart attempt so any terminal entry committed above (the
	// turn's own error/tombstone) AND any restart-failure error entry both print.
	return tea.Batch(m.flush(), start)
}

// handleInterruptResult applies the outcome of an Interrupt call. On error the turn
// may still be live, so the model returns to Running and commits a faint error
// entry; on success it stays Interrupting — the loop's TurnInterrupted terminal (or
// the in-flight stream's pending EOF) returns it to Idle.
func (m *Screen) handleInterruptResult(msg interruptResultMsg) tea.Cmd {
	if msg.err != nil {
		m.transcript = m.transcript.CommitError(msg.err)
		m.status = StatusRunning
		return m.flush()
	}
	return nil
}

// handleReopenResult applies a /clear reopen outcome (the model is Resetting). On
// error the old agent is kept and the model returns to Idle with an error entry. On
// success the fresh agent is swapped in, all display state is reset, the model
// returns to Idle, and the old agent is closed best-effort. Already-printed
// scrollback stays in the terminal (native history is append-only); the print-once
// engine is reset so a fresh session starts a clean transcript model.
func (m *Screen) handleReopenResult(msg reopenResultMsg) tea.Cmd {
	if msg.err != nil {
		m.transcript = m.transcript.CommitError(msg.err)
		m.status = StatusIdle
		return m.flush()
	}
	old := m.agent
	m.agent = msg.agent
	m.transcript = transcriptModel{}
	m.scrollback = newScrollbackModel(m.width)
	m.interaction = m.interaction.ClearPrompts()
	m.queue = nil
	m.status = StatusIdle
	return closeAgent(old)
}

// handlePromptResult surfaces a bounded prompt-dispatch outcome. A nil err is a
// silent success (the gate released; the next events arrive on the stream). A
// non-nil err commits a faint, NON-FATAL error entry: the prompt was already
// optimistically popped, and a terminal event clears any siblings — this only adds
// a record. It never panics and never hangs.
func (m *Screen) handlePromptResult(msg promptResultMsg) tea.Cmd {
	if msg.err == nil {
		return nil
	}
	m.transcript = m.transcript.CommitError(msg.err)
	return m.flush()
}

// startTurn begins a turn from blocks. agent.StreamBlocks may fail before a reader
// exists (TurnBusyError, loop exited, ctx done); on error it commits an error entry
// and stays Idle — never Running without a reader, never readNext(nil). It returns
// the readNext cmd and whether the turn actually started.
func (m *Screen) startTurn(blocks []content.Block) (tea.Cmd, bool) {
	r, err := m.agent.StreamBlocks(m.appCtx, blocks)
	if err != nil {
		m.transcript = m.transcript.CommitError(err)
		m.status, m.reader = StatusIdle, nil
		return nil, false
	}
	m.reader, m.status = r, StatusRunning
	return readNext(r), true
}

// View renders an empty string until the first WindowSizeMsg (avoids a 0×0 first
// frame), then composes the active surface via surfaceView: the capped live tail
// (rendered from the live segment), the separator rule, the bottom box (composer /
// prompt control / answer field by interaction mode), the slash panel when visible,
// and one status line. Committed entries are NOT re-rendered here — they live in
// native scrollback. tea.NewView leaves AltScreen false and MouseMode none (the v2
// zero values), the scrollback-first configuration.
func (m Screen) View() tea.View {
	if !m.ready {
		return tea.NewView("")
	}
	return tea.NewView(surfaceView(surfaceInputs{
		Interaction: m.interaction,
		LiveTail:    m.renderLiveTail(),
		Status:      m.status,
		StatusState: m.statusInputs(),
		Width:       m.width,
		Height:      m.height,
	}))
}

// renderLiveTail renders the in-progress assistant segment (streamed thinking,
// narration, and any still-running tool cards) to its display lines. It is empty
// when there is no live content, so the surface omits the tail region entirely.
func (m Screen) renderLiveTail() string {
	live := m.transcript.live
	if live.empty() {
		return ""
	}
	return renderAssistant(live.Thinking, live.Text, live.Calls, m.expand, m.width)
}

// statusInputs snapshots the live signals the status label is derived from: which
// prompt (if any) is active, and whether the live segment is streaming narration or
// only thinking so far.
func (m Screen) statusInputs() statusInputs {
	in := statusInputs{
		streaming: m.transcript.live.Text != "",
		thinking:  m.transcript.live.Text == "" && m.transcript.live.Thinking != "",
	}
	if p := m.activePrompt(); p != nil {
		in.permissionActive = p.Kind == promptPermission
		in.userInputActive = p.Kind == promptUserInput
	}
	return in
}

// activePrompt returns the interaction model's active (head) prompt, or nil.
func (m Screen) activePrompt() *prompt {
	im := m.interaction
	return im.ActivePrompt()
}

// handleKey routes a key press. ctrl+c (quit) and ctrl+t (toggle expand) are GLOBAL
// — they fire even with a prompt open — so they are handled first. Every other key
// is delegated to the interaction model, which returns the next model, a typed
// uiAction, and the editor's cursor-blink Cmd; mapAction turns the action into the
// agent-driving command, and the blink Cmd is batched in so the cursor keeps
// blinking in compose and free-text answer modes.
func (m *Screen) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return *m, tea.Sequence(closeAgent(m.agent), tea.Quit)
	case "ctrl+t":
		m.expand = !m.expand // pure display state; works in any status / prompt mode
		return *m, nil
	case "esc":
		// With no prompt active, Esc keeps its legacy meaning — interrupt a running
		// turn (a no-op otherwise). When a prompt IS active the interaction model owns
		// Esc (deny in permission mode, interrupt in choice/answer mode), so fall through.
		if m.activePrompt() == nil {
			return *m, m.interruptRunning()
		}
	}

	var action uiAction
	var blink tea.Cmd
	m.interaction, action, blink = m.interaction.Update(msg)
	cmd := m.mapAction(action)
	return *m, tea.Batch(cmd, blink)
}

// mapAction turns the interaction model's typed uiAction into the command that
// drives the agent (or mutates Screen state). uiNoop returns nil. The prompt-gate
// actions (approve/deny/answer) and interrupt reuse the bounded commands from
// commands.go; submit/runSlash mirror the legacy compose behavior.
func (m *Screen) mapAction(a uiAction) tea.Cmd {
	switch a.Kind {
	case uiSubmit:
		return m.submit(a.Text)
	case uiRunSlash:
		return m.runSlash(a.Slash)
	case uiApprove:
		return approveCmd(m.appCtx, m.agent, a.CallID, a.Scope)
	case uiDeny:
		return denyCmd(m.appCtx, m.agent, a.CallID)
	case uiAnswer:
		return provideAnswerCmd(m.appCtx, m.agent, a.CallID, a.Text)
	case uiInterrupt:
		return m.interruptRunning()
	default: // uiNoop
		return nil
	}
}

// submit builds blocks from the composed text and either starts a turn (Idle),
// queues it (Running), or no-ops (Interrupting/Resetting). On any status that
// accepts the message the user entry is committed to scrollback first (so it lands
// in native history immediately, even when queued mid-turn). A buildBlocks error or
// a failed start commits a faint error entry and starts no turn.
func (m *Screen) submit(text string) tea.Cmd {
	blocks, err := buildBlocks(text, m.agent.AcceptsImages())
	if err != nil {
		m.transcript = m.transcript.CommitError(err)
		return m.flush()
	}

	switch m.status {
	case StatusIdle:
		m.transcript = m.transcript.CommitUser(blocks)
		cmd, _ := m.startTurn(blocks)
		return tea.Batch(m.flush(), cmd)
	case StatusRunning:
		m.transcript = m.transcript.CommitUser(blocks)
		m.queue = append(m.queue, blocks)
		return m.flush()
	default: // Interrupting / Resetting: no-op, drop the submission.
		return nil
	}
}

// runSlash executes a known slash command. /help commits the listing to scrollback;
// /clear (only while Idle) flips to Resetting and reopens the agent. An unknown or
// non-actionable command is a no-op. It mirrors the legacy dispatchSlash.
func (m *Screen) runSlash(name string) tea.Cmd {
	switch name {
	case "/help":
		m.transcript = m.transcript.CommitSystem(helpText())
		return m.flush()
	case "/clear":
		if m.status == StatusIdle {
			m.status = StatusResetting
			return reopenAgent(m.appCtx, m.openAgent)
		}
		return nil
	default:
		return nil
	}
}

// interruptRunning begins an interrupt only while Running: it flips to Interrupting,
// drops the pending queue (those submissions are abandoned; their user entries
// already printed to scrollback), and returns the bounded Interrupt command. From
// any other status it is a no-op. It is the home for both the Esc-in-compose path
// and the uiInterrupt action raised from a choice/answer prompt.
func (m *Screen) interruptRunning() tea.Cmd {
	if m.status != StatusRunning {
		return nil
	}
	m.status = StatusInterrupting
	m.queue = nil
	return interruptTurn(m.appCtx, m.agent)
}
