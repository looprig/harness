package tui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/tui/components"
)

// Screen is the Elm model for the chat TUI. In scrollback-first mode it is a thin
// router over three pure helpers: transcript reconstructs the turn from the event
// stream and tracks committed entries + the live segment; scrollback prints each
// committed entry to native terminal scrollback exactly once; interaction owns the
// bottom surface (composer, slash panel, and the FIFO of pending permission/AskUser
// prompts). Screen holds only the agent wiring, the turn status, the ONE
// session-lifetime event subscription, the terminal dimensions, and the ctrl+t
// expand flag. There is no transcript viewport — the terminal owns history.
//
// Event transport: a SINGLE whole-session subscription (sub), established once at
// startup and read continuously by subNext, is the sole event source — it spans
// every turn and loop and never EOFs per turn. Submissions are fire-and-forget
// (submitCmd → agent.Submit); the LOOP owns queueing, so Screen keeps no queue.
// User rows are EVENT-DRIVEN: the authoritative user row commits from the loop's
// TurnStarted/TurnFoldedInto Message (genuine input only — Cause.LoopID == 0),
// never optimistically at submit; a successful submit only records the queued
// affordance (RecordSubmit), shown once InputQueued arrives. The loop's
// TurnStarted/TurnDone/TurnFailed/TurnInterrupted terminals on the subscription
// drive the turn status (for the PRIMARY loop only).
type Screen struct {
	agent     Agent
	openAgent OpenAgent       // builds a replacement agent on /clear
	appCtx    context.Context // long-lived; cancelled on quit
	banner    AgentBanner     // agent name + description, shown as the startup info notice

	transcript  transcriptModel
	scrollback  scrollbackModel
	interaction interactionModel

	status Status      // Idle | Running | Interrupting | Resetting
	sub    EventStream // the session-lifetime event subscription; nil until subscribed

	// expand drives the ctrl+t fold for the THINKING block only. It defaults to TRUE
	// (full "│ " thinking body) because native scrollback is append-only: an entry prints
	// once and can never be retroactively re-rendered, so a toggle cannot expand thinking
	// already committed to history; showing it full by default avoids permanently
	// truncating that history. ctrl+t flips it to the compact "thinking · N lines" summary
	// for the live tail + future commits. Tool RESULT output is NOT governed by this flag —
	// it is hard-capped to previewLineCap lines always (see render.go), so a huge result
	// can't fill the live tail or strand a commit-time scrollback gap.
	expand        bool
	width, height int
	ready         bool

	// anim holds the LIVE-surface animation state (blink phase + spinner frame +
	// ticking guard). It is advanced once per blinkTick while Running, threaded into
	// renderLiveTail (its blink phase pulses the live assistant dot) AND the status line
	// (its frame counter flows the status-line gradient — label and dot), and reset to its
	// zero value when the turn ends. The committed scrollback path never consults it. See
	// animState and the blinkMsg handler.
	anim animState

	// tip is the rotating educational hint shown faint below the status line. It is
	// seeded at construction and refreshed (nextTip) on every turn terminal, so a fresh
	// hint shows after each turn.
	tip string
}

// AgentBanner is the agent metadata shown as the startup info notice — its Name and
// Description, threaded in at construction from the composition root (cmd/swe) so the
// Agent interface need not expose them. The zero value renders a name-less banner;
// bannerText degrades gracefully when either field is empty.
type AgentBanner struct {
	Name        string
	Description string

	// Greeting is the OPTIONAL, UI-only startup greeting (§5a): a deterministic, already-
	// built capability description (composed by the composition root from the agent
	// registry — never the model). When non-empty it is committed as a SECOND opening
	// transcript notice, after the banner, by the systemReady handler. It is purely a
	// rendered opening entry — NOT a turn, NOT a command, never in the model's context —
	// so the primary loop's history stays empty until the first real user message. Empty
	// (the default-off case) → no greeting entry, behavior identical to today.
	Greeting string
}

// bannerText renders the startup banner line from the agent metadata: "<Name> —
// <Description>" when both are present, just the Name when the description is empty,
// just the Description when the name is empty, and a neutral fallback when both are
// empty (the notice still marks the session start). It degrades rather than emitting
// a dangling separator.
func (b AgentBanner) bannerText() string {
	name, desc := strings.TrimSpace(b.Name), strings.TrimSpace(b.Description)
	switch {
	case name != "" && desc != "":
		return name + " — " + desc
	case name != "":
		return name
	case desc != "":
		return desc
	default:
		return "session ready"
	}
}

// New constructs an idle Screen driving agent, with open as the /clear thunk and
// banner the agent name/description shown as the startup info notice. The expand flag
// starts TRUE so the thinking block renders in full from the start — see the field
// comment for why append-only scrollback forces expanded-by-default (tool output is
// hard-capped independently of this flag).
func New(ctx context.Context, agent Agent, open OpenAgent, banner AgentBanner) Screen {
	return Screen{
		agent:       agent,
		openAgent:   open,
		appCtx:      ctx,
		banner:      banner,
		status:      StatusIdle,
		transcript:  transcriptModel{primaryLoopID: agent.PrimaryLoopID()},
		scrollback:  newScrollbackModel(0),
		interaction: newInteractionModel(),
		expand:      true,
		tip:         nextTip(""),
	}
}

// Init focuses the composer (starting the cursor blink), emits the initial
// startup-banner entry, schedules the cold-restore repaint, AND attaches the
// session-lifetime event subscription. The subscription is the single LIVE event
// source for the whole session; subscribeCmd's subscribedMsg installs it and starts
// the continuous reader. restoreBacklogCmd folds a RESTORED session's historical
// Enduring backlog OFF the update loop and repaints it once (restoredMsg) before any
// live event drives the transcript — a new session's empty backlog makes it a no-op.
// Both run as independent background commands: for a cold restore the loop comes up
// idle, so no live event arrives until a user Submit, and there is no backlog/live
// overlap to dedup (the live-attach overlap case is deferred).
func (m Screen) Init() tea.Cmd {
	return tea.Batch(
		m.interaction.input.Focus(),
		func() tea.Msg { return systemReadyMsg{} },
		restoreBacklogCmd(m.appCtx, m.agent, m.agent.PrimaryLoopID()),
		subscribeCmd(m.agent),
	)
}

// Agent returns the live agent. cmd/swe uses this for a bounded backstop Close
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
	case restoredMsg:
		return m, m.handleRestored(msg)
	case subscribedMsg:
		return m, m.handleSubscribed(msg)
	case subClosedMsg:
		return m, m.handleSubClosed(msg)
	case submitResultMsg:
		return m, m.handleSubmitResult(msg)
	case interruptResultMsg:
		return m, m.handleInterruptResult(msg)
	case reopenResultMsg:
		return m, m.handleReopenResult(msg)
	case promptResultMsg:
		return m, m.handlePromptResult(msg)
	case exportResultMsg:
		return m, m.handleExportResult(msg)
	case systemReadyMsg:
		return m, m.commitStartup()
	case blinkMsg:
		return m, m.handleBlink()
	}
	return m, nil
}

// commitStartup commits the opening transcript entries on the systemReady boundary: the
// startup banner (always) followed by the OPTIONAL greeting (only when banner.Greeting is
// non-empty/non-blank). Both are committed via the plain info-notice path — they are
// rendered opening entries, NOT turns or commands: this never calls Submit, never drives
// a loop, and never enters the model's context. It flushes the new entries to scrollback
// and returns the print command (non-nil because at least the banner is always committed).
func (m *Screen) commitStartup() tea.Cmd {
	m.transcript = m.transcript.CommitNotice(noticeInfo, m.banner.bannerText())
	if greeting := strings.TrimSpace(m.banner.Greeting); greeting != "" {
		m.transcript = m.transcript.CommitNotice(noticeInfo, greeting)
	}
	return m.flush()
}

// handleBlink advances the live-surface animation by one frame and, ONLY while the
// turn is still Running, reschedules the next tick — so View re-renders the live tail
// with the new blink/spinner phase. It is a PURE active-surface re-render: it never
// calls flush/printToScrollback/subNext, so a tick can never write to scrollback.
// At any non-Running status it stops the loop (returns nil — no reschedule) and resets
// the animation state so the next live render is clean and a fresh turn starts a new
// tick loop. Reset clears the ticking guard, letting the next Running turn start one.
func (m *Screen) handleBlink() tea.Cmd {
	if m.status != StatusRunning {
		m.anim = m.anim.reset()
		return nil
	}
	m.anim = m.anim.advance()
	return blinkTick()
}

// handleEvent routes one subscription event through BOTH reducers — the transcript
// (which reconstructs the live segment and commits user/tool/prompt/terminal
// entries) and the interaction model (which enqueues prompts and clears its queue
// on terminal events) — derives the turn STATUS from the primary loop's
// turn-lifecycle events, flushes any newly committed entries to scrollback, and
// re-arms the continuous reader. Reading continues unconditionally: the loop is
// blocked on the permission GATE, not the stream, so the user's keypress
// (approve/deny/answer) is what releases it. A prompt event dispatches nothing
// here; the trio call happens later when the user resolves it.
func (m *Screen) handleEvent(ev event.Event) tea.Cmd {
	m.transcript = m.transcript.ApplyEvent(ev)
	m.interaction = m.interaction.ApplyEvent(ev)
	statusCmd := m.applyTurnStatus(ev)
	// Re-arm only while a live subscription is installed: during the /clear window
	// m.sub is transiently nil, and the fresh subscription's reader is (re)started
	// by handleSubscribed, not here. subNext is also nil-guarded as a backstop. A
	// stale buffered event from the OLD sub that lands just after re-subscribe is
	// harmless: this re-arms a second reader on the new channel, but channel receive
	// is single-consumer, so no event is duplicated and the spare reader dies on its
	// next receive/close.
	var rearm tea.Cmd
	if m.sub != nil {
		rearm = subNext(m.sub)
	}
	return tea.Batch(rearm, statusCmd, m.flush())
}

// applyTurnStatus derives the turn status from a turn-lifecycle event for the
// PRIMARY loop ONLY (events carry Header.LoopID; a subagent loop's turn events must
// NOT flip the primary status — its output surfaces via Enduring StepDone, rendered
// collapsed-but-present). On the primary loop's TurnStarted it goes Running and
// starts the live-surface blink; on any of its terminals (TurnDone/TurnFailed/
// TurnInterrupted) it returns to Idle. Interrupting/Resetting are owned by their own
// handlers and are NOT set here — but a TurnInterrupted terminal does land Idle from
// Interrupting, completing the interrupt. Non-turn events and subagent turn events
// return nil and leave the status untouched.
func (m *Screen) applyTurnStatus(ev event.Event) tea.Cmd {
	if ev.EventHeader().LoopID != m.agent.PrimaryLoopID() {
		return nil
	}
	switch ev.(type) {
	case event.TurnStarted:
		m.status = StatusRunning
		return m.startBlink()
	case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
		m.status = StatusIdle
		m.tip = nextTip(m.tip) // rotate the hint after each completed turn
		return nil
	default:
		return nil
	}
}

// handleSubscribed installs the session-lifetime subscription and starts the
// continuous reader. On a non-nil err the TUI cannot observe the session at all —
// it commits a fatal error entry (the user sees the failure rather than a silently
// dead surface). On success it stores the stream and kicks off subNext, the single
// reader that drives every subsequent event.
func (m *Screen) handleSubscribed(msg subscribedMsg) tea.Cmd {
	if msg.err != nil {
		m.transcript = m.transcript.CommitError(msg.err)
		return m.flush()
	}
	m.sub = msg.sub
	return subNext(m.sub)
}

// handleSubClosed reacts to the continuous reader observing a closed channel. A nil
// err is an intentional Close (a /clear swap or quit teardown) — nothing to surface.
// A non-nil err is a hub-forced loss (egress overflow); it commits an error entry so
// the user learns the live stream was dropped rather than silently stalling. Either
// way the reader is not re-armed (the channel is closed); a /clear re-subscribe is
// the path back to a live stream.
func (m *Screen) handleSubClosed(msg subClosedMsg) tea.Cmd {
	if msg.err == nil {
		return nil
	}
	m.transcript = m.transcript.CommitError(msg.err)
	return m.flush()
}

// handleSubmitResult surfaces a fire-and-forget Submit outcome. On success it
// records the submit (RecordSubmit) under the loop-assigned InputID so the queued
// affordance can show the remembered blocks once the loop's InputQueued event
// arrives; the authoritative user row is committed later from the loop's
// TurnStarted/TurnFoldedInto Message, NOT here. A non-nil err commits a faint,
// NON-FATAL error entry noting the send failed (no user row was ever committed, so
// nothing is left dangling). It never panics and never hangs.
func (m *Screen) handleSubmitResult(msg submitResultMsg) tea.Cmd {
	if msg.err != nil {
		m.transcript = m.transcript.CommitError(msg.err)
		return m.flush()
	}
	m.transcript = m.transcript.RecordSubmit(msg.inputID, msg.blocks)
	return nil
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

// handleInterruptResult applies the outcome of an Interrupt call. On error the turn
// may still be live, so the model returns to Running and commits a faint error
// entry; on success it stays Interrupting — the loop's TurnInterrupted terminal on
// the subscription (applyTurnStatus, primary loop) returns it to Idle.
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
// returns to Idle, the OLD subscription is closed and a NEW one is established
// against the fresh agent, and the old agent is closed best-effort. Already-printed
// scrollback stays in the terminal (native history is append-only); the print-once
// engine is reset so a fresh session starts a clean transcript model.
//
// Ordering matters: the agent is swapped to msg.agent BEFORE subscribeCmd is built
// so the re-subscribe reads the NEW agent (subscribeCmd reads m.agent). The old
// subscription is closed best-effort first so the old agent's hub does not leak it;
// m.sub is cleared so a late subClosedMsg from the old stream (nil err — an
// intentional Close) is a harmless no-op.
func (m *Screen) handleReopenResult(msg reopenResultMsg) tea.Cmd {
	if msg.err != nil {
		m.transcript = m.transcript.CommitError(msg.err)
		m.status = StatusIdle
		return m.flush()
	}
	if m.sub != nil {
		_ = m.sub.Close() // best-effort; idempotent, nothing actionable at the UI
	}
	m.sub = nil
	old := m.agent
	m.agent = msg.agent
	// Read the NEW agent's primary loop id (the swap above happened first) so the
	// fresh transcript scopes its committed user rows to the replacement loop.
	m.transcript = transcriptModel{primaryLoopID: m.agent.PrimaryLoopID()}
	m.scrollback = newScrollbackModel(m.width)
	m.interaction = m.interaction.ClearPrompts()
	m.status = StatusIdle
	return tea.Batch(closeAgent(old), subscribeCmd(m.agent))
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

// startBlink starts the live-surface animation tick loop iff one is not already
// running: it sets the ticking guard and returns the first blinkTick. If a tick is
// already in flight (anim.ticking) — e.g. a fresh TurnStarted arrives before the
// prior loop has observed Idle and reset — it returns nil so no second, parallel
// loop is spawned. The single in-flight tick keeps ticking (it observes Running), so
// the animation continues seamlessly across back-to-back turns.
func (m *Screen) startBlink() tea.Cmd {
	if m.anim.ticking {
		return nil
	}
	m.anim.ticking = true
	return blinkTick()
}

// View renders an empty string until the first WindowSizeMsg (avoids a 0×0 first
// frame), then composes the active surface via surfaceView: the capped live tail
// (rendered from the live segment), the separator rule, the bottom box (composer /
// prompt control / answer field by interaction mode), the slash panel when visible,
// and one status line. Committed entries are NOT re-rendered here — they live in
// native scrollback. The View leaves AltScreen false and MouseMode none (the v2 zero
// values), the scrollback-first configuration.
//
// KeyboardEnhancements.ReportAllKeysAsEscapeCodes is requested so the composer's
// Shift+Enter binding works (see tui/components/input.go). v2's DEFAULT Kitty request
// is just flag 1 ("disambiguate escape codes"), under which the Kitty spec keeps
// Enter/Tab/Backspace as their legacy bytes — so a modified Enter (Shift+Enter) is NOT
// reported distinctly and arrives as a plain Enter (submit). Flag 8 ("report all keys
// as escape codes") is the one that makes the terminal report a modified Enter as
// CSI 13;2u, which v2 decodes to KeyEnter+ModShift. This ONLY helps on terminals that
// implement the Kitty keyboard protocol (kitty, Ghostty, WezTerm, foot, Alacritty,
// recent iTerm2 with the option enabled). On terminals WITHOUT it (Apple Terminal,
// many VS Code setups) Shift+Enter is indistinguishable from Enter no matter what we
// request — those rely on the Ctrl+J fallback (input.go). The request is inert on
// non-supporting terminals (they ignore the CSI), and it does NOT enable alt-screen,
// mouse capture, or focus reporting, so the scrollback-first invariant is preserved.
func (m Screen) View() tea.View {
	if !m.ready {
		return tea.NewView("")
	}
	v := tea.NewView(surfaceView(surfaceInputs{
		Interaction: m.interaction,
		LiveTail:    m.renderLiveTail(),
		Queued:      m.renderQueued(),
		Status:      m.status,
		StatusState: m.statusInputs(),
		Phase:       m.anim.frame,
		Tip:         m.tip,
		Width:       m.width,
		Height:      m.height,
	}))
	v.KeyboardEnhancements.ReportAllKeysAsEscapeCodes = true
	return v
}

// renderLiveTail renders the in-progress assistant segment (streamed thinking,
// narration, any still-running tool cards, and any in-flight nested Subagent cards) to
// its display lines. It is empty when there is no live content AND no pending subagent
// card, so the surface omits the tail region entirely.
func (m Screen) renderLiveTail() string {
	live := m.transcript.live
	pending := m.transcript.pendingSubagentCards()
	if live.empty() && len(pending) == 0 {
		return ""
	}
	// Suppress the orchestrator's raw running Subagent tool card — its activity is shown
	// by the nested pending card (renderSubagentCard) instead, so it must not be doubled.
	calls := nonSubagentCalls(live.Calls)
	return renderLiveAssistant(live.Thinking, live.Text, calls, pending, m.expand, m.width, m.anim)
}

// renderQueued renders the transcript's pending queued-input affordances (the
// submitted-but-not-yet-running user messages) to their dim display lines, shown
// below the live tail. It is empty when nothing is queued, so the surface omits the
// region entirely.
func (m Screen) renderQueued() string {
	return renderQueued(m.transcript.QueuedInputs(), m.width)
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
		// Close the session subscription best-effort so it does not leak past quit.
		// Close is a synchronous, idempotent in-process teardown (it just closes the
		// egress channel under a lock), so it is safe to call inline here; the agent
		// close is the bounded async cmd that may block.
		if m.sub != nil {
			_ = m.sub.Close()
			m.sub = nil
		}
		return *m, tea.Sequence(closeAgent(m.agent), tea.Quit)
	case "ctrl+t":
		m.expand = !m.expand // pure display state; default expanded, so this collapses first
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
		// Record the decision so the call's committed card reads "Approved …". The loop
		// emits no decision event, so the keypress is the only source (the gate was
		// remembered by the transcript on PermissionRequested).
		m.transcript = m.transcript.ResolveGate(a.ToolExecutionID, gateApproved)
		return approveCmd(m.appCtx, m.agent, a.LoopID, a.ToolExecutionID, a.Scope)
	case uiDeny:
		m.transcript = m.transcript.ResolveGate(a.ToolExecutionID, gateDenied)
		return denyCmd(m.appCtx, m.agent, a.LoopID, a.ToolExecutionID)
	case uiAnswer:
		return provideAnswerCmd(m.appCtx, m.agent, a.LoopID, a.ToolExecutionID, a.Text)
	case uiInterrupt:
		return m.interruptRunning()
	case uiExport:
		// /export is funneled through runSlash so all status-gated slash dispatch lives
		// in one place; runSlash allows it in ANY status (snapshot semantics, D1).
		return m.runSlash(components.CmdExport)
	default: // uiNoop
		return nil
	}
}

// submit builds blocks from the composed text and sends them fire-and-forget. The
// LOOP owns queueing now (a submission while Running is queued by the loop, not by
// Screen), so there is no status branching and no Screen-side queue. It does NOT
// commit a user row optimistically: the authoritative user row is committed from
// the loop's TurnStarted/TurnFoldedInto Message (event-driven), so submit only
// fires submitCmd. A buildBlocks error commits a faint error entry and sends
// nothing. submitCmd's submitResultMsg carries the InputID + blocks so a successful
// send records the submit (so the queued affordance can show on the later
// InputQueued); a send failure surfaces a faint error notice.
func (m *Screen) submit(text string) tea.Cmd {
	blocks, err := buildBlocks(text, m.agent.AcceptsImages())
	if err != nil {
		// A build error (e.g. an unsupported image on a text-only model) commits the
		// message as a plain user row FIRST, then the error beneath it — the message is
		// preserved in scrollback rather than lost or stuffed back into the editor, and
		// composeEnter already left the input box empty. The message was NOT sent to the
		// model (the build failed); it is displayed so the user sees what they asked.
		m.transcript = m.transcript.CommitUserText(text)
		m.transcript = m.transcript.CommitError(err)
		return m.flush()
	}
	return submitCmd(m.appCtx, m.agent, blocks)
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
	case components.CmdExport:
		// Allowed in ANY status: export is a journal SNAPSHOT (D1), so a mid-turn
		// /export captures records committed so far without blocking the live turn —
		// unlike /clear, which is gated to Idle. The async cmd does reconstruct →
		// render → atomic write off the update loop and reports an exportResultMsg.
		return exportCmd(m.appCtx, m.agent)
	default:
		return nil
	}
}

// interruptRunning begins an interrupt only while Running: it flips to Interrupting
// and returns the bounded Interrupt command. The loop owns queueing, so there is no
// Screen-side queue to drop — the loop returns any queued inputs as InputCancelled
// events on the subscription (harmless to the transcript today). From any other
// status it is a no-op. It is the home for both the Esc-in-compose path and the
// uiInterrupt action raised from a choice/answer prompt.
func (m *Screen) interruptRunning() tea.Cmd {
	if m.status != StatusRunning {
		return nil
	}
	m.status = StatusInterrupting
	return interruptTurn(m.appCtx, m.agent)
}
