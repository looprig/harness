package tui

import (
	"strings"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
)

// splitLines splits a tool-result preview into display lines on "\n". An empty
// preview yields nil (no result lines; the renderer shows "(no output)"); a
// non-empty preview always yields at least one line. A trailing newline produces a
// trailing empty line, preserved as-is (the runner caps/marks the preview).
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// splitStepGroup splits a StepDone.Messages group into its single AIMessage and a
// ToolUseID→ToolResultMessage index of the tool results that follow it. The step
// shape is one AIMessage followed by zero or more ToolResultMessages (loop-machine
// design §Step); a missing AIMessage yields nil so the caller commits no assistant
// entry. UserMessages (a folded tool-continuation input) and any other message types
// are ignored — the transcript commits those from their own TurnStarted/TurnFoldedInto
// events, not from a StepDone group.
func splitStepGroup(msgs content.AgenticMessages) (*content.AIMessage, map[string]*content.ToolResultMessage) {
	var ai *content.AIMessage
	results := make(map[string]*content.ToolResultMessage)
	for _, msg := range msgs {
		switch v := msg.(type) {
		case *content.AIMessage:
			if ai == nil {
				ai = v
			}
		case *content.ToolResultMessage:
			results[v.ToolUseID] = v
		}
	}
	return ai, results
}

// toolUsesOf returns the AIMessage's tool-use blocks in block order — the executable
// children of the assistant message. A nil message yields nil.
func toolUsesOf(ai *content.AIMessage) []content.ToolUseBlock {
	if ai == nil {
		return nil
	}
	var out []content.ToolUseBlock
	for _, b := range ai.Blocks {
		if tu, ok := b.(*content.ToolUseBlock); ok {
			out = append(out, *tu)
		}
	}
	return out
}

// textOnly concatenates ONLY the narration (TextBlocks) of an assistant message,
// joined by "\n". Thinking blocks (rendered separately as the dim reasoning block)
// and tool-use blocks (rendered as their own tool cards) are excluded, so the
// committed assistant entry's Blocks carry exactly the markdown narration. An
// all-thinking/all-tool message yields "" (no narration entry).
func textOnly(blocks []content.Block) string {
	var parts []string
	for _, b := range blocks {
		if tb, ok := b.(*content.TextBlock); ok {
			parts = append(parts, tb.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// toolResultText flattens a ToolResultMessage's TextBlocks into one display string.
// The loop builds a ToolResultMessage carrying a single flattened TextBlock, so this
// concatenates every TextBlock; non-text blocks are skipped (they have no display
// form here — the live card's redacted preview is the display path for those).
func toolResultText(r *content.ToolResultMessage) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range r.Blocks {
		if tb, ok := blk.(*content.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

// displayID is a stable, monotonically assigned identifier for a committed
// transcript entry. It is allocated once when a live segment is committed and
// never reused, so a renderer can key on it across re-renders. The zero value is
// never a valid assigned ID — the first commit allocates 1.
type displayID uint64

// entryKind discriminates the source/kind of a committed transcript entry.
// kindTool is one resolved tool call (terminal state); kindNotice carries a leveled
// notification (info/warn/error) — including a turn-failure message at error level;
// kindInterrupted is the content-less tombstone for an interrupted turn.
type entryKind uint8

const (
	kindUser entryKind = iota
	kindAssistant
	kindTool
	kindPromptRecord
	kindInterrupted
	// kindNotice is a leveled, out-of-band notification line (the startup banner, the
	// /help listing, a non-fatal error, a turn failure). It carries a single TextBlock
	// and a noticeLevel; renderEntry renders it with the shared "▌ " accent bar colored
	// per level (see noticeLevel and styles.NoticeStyle).
	kindNotice
)

// noticeLevel grades a kindNotice's severity, selecting its accent-bar color. The
// three levels share the SAME "▌ " wrapper (the user-message accent bar) and differ
// only in color: info is the neutral user-message gray, warn is yellow, error is red.
// It is an explicit enum (not a bool/string) so the renderer and styles map levels to
// colors exhaustively. The zero value is noticeInfo.
type noticeLevel uint8

const (
	noticeInfo noticeLevel = iota
	noticeWarn
	noticeError
)

// promptContext is the FULL prompt payload a kindPromptRecord entry commits to
// scrollback when a gate opens: the copyable command/diff (permission) or the
// question + every choice (user input). It is the append-only SCROLLBACK record —
// distinct from the interaction layer's compact bottom-box control (prompt in
// prompt.go), which carries selection state and is redrawn every frame. Kind
// reuses the prompt.go promptKind so the renderer dispatches the same way. A
// permission record uses ToolName/Description; a user-input record uses
// Question/Choices.
type promptContext struct {
	Kind        promptKind
	ToolName    string   // promptPermission: approval header ("Approve <ToolName>?")
	Description string   // promptPermission: copyable body (command / diff / url)
	Question    string   // promptUserInput: the AskUser question
	Choices     []string // promptUserInput: every offered choice, in order
}

// entry is one committed (finalized) row of the transcript. It stores the minimal
// data needed to render the row later: its stable ID, its kind, and the content
// blocks captured when the live segment was committed. Calls holds the tool-call
// children of an assistant segment; it is left unpopulated in this task (tool-call
// reconstruction lands in a later task).
type entry struct {
	ID     displayID
	Kind   entryKind
	Blocks []content.Block
	Calls  []ToolCallView
	// Level grades a kindNotice entry's severity (info/warn/error), selecting its
	// accent-bar color. It is meaningful ONLY for kindNotice; every other kind leaves
	// it at the zero value (noticeInfo) and the renderer ignores it.
	Level noticeLevel
	// Prompt carries the FULL prompt context for a kindPromptRecord entry; it is
	// nil for every other kind. Kept as a pointer so non-prompt entries pay no
	// per-entry cost and a nil here is an unambiguous "not a prompt record".
	Prompt *promptContext
	// doneHeadline marks a kindAssistant entry that is the committed form of an
	// empty-text tool step (design §3 rule 4): it carries no prose blocks and renders
	// a bold "● Done" headline above its separately-committed tool cards. Set ONLY by
	// commitStepAssistant on the clean StepDone path; the interrupt/failure partial
	// path never sets it (an interrupted step is not "done").
	doneHeadline bool
}

// liveSeg is the in-progress assistant segment for the current turn: the streamed
// reasoning (Thinking) and narration (Text) plus the tool calls reconstructed from
// the event stream. It is committed to an entry when the turn ends. Calls stays
// empty until the event-reconstruction state machine populates it (a later task).
// active marks that a turn is in progress. It is the transcriptModel's own segment
// type — the single in-progress segment the scrollback-first path renders.
type liveSeg struct {
	Thinking string
	Text     string
	Calls    []ToolCallView
	active   bool
}

// empty reports whether the live segment carries no committable content — no
// streamed reasoning, no streamed narration, and no reconstructed tool call.
// active is intentionally not consulted: an active-but-content-less segment is
// still empty and must not commit.
func (s liveSeg) empty() bool {
	return s.Thinking == "" && s.Text == "" && len(s.Calls) == 0
}

// transcriptModel is the pure, side-effect-free reducer over a turn's event
// stream. committed holds finalized entries in display order; live is the
// in-progress segment for the current turn; nextID is the next stable ID to
// allocate. It is applied by value: ApplyEvent returns the next model.
type transcriptModel struct {
	committed []entry
	live      liveSeg
	nextID    displayID
}

// ApplyEvent folds one turn-stream event into the model and returns the next
// model. TurnStarted begins/keeps a live assistant segment; TokenDelta routes
// *content.TextChunk → live.Text and *content.ThinkingChunk → live.Thinking as a
// PROVISIONAL live render; ToolCallStarted/ToolCallCompleted drive the live tool
// cards (in the live tail only — they are not committed to scrollback here).
//
// StepDone is the authoritative commit point and the self-heal anchor: it SNAPS the
// transcript to the loop's finalized StepDone.Messages (the step's AIMessage + its
// ToolResultMessages), committing that group as separate entries and discarding the
// provisional live segment — so a dropped/partial TokenDelta never survives past the
// step boundary, and the displayed transcript equals the committed transcript by
// construction. A multi-step turn therefore renders as multiple separate assistant +
// tool entries, never one merged entry.
//
// TurnDone is a lifecycle terminal: every completed step already committed via its
// StepDone, so it only flushes any leftover provisional live (defensive) and resets.
// PermissionRequested/UserInputRequested are prompt-open boundaries: each commits any
// pending prose, then commits the FULL prompt context as a kindPromptRecord entry
// (the live segment is NOT reset — the turn continues while the gate is pending).
// TurnInterrupted/TurnFailed are the abnormal terminals: the in-flight INCOMPLETE step
// never emitted a StepDone, so its provisional live is committed (partial work stays
// visible) before the tombstone/error. It returns ONLY the next transcriptModel — no
// uiAction; prompt clearing on terminals and active-surface control are the
// interactionModel's job, not the transcript's.
func (m transcriptModel) ApplyEvent(ev event.Event) transcriptModel {
	switch ev := ev.(type) {
	case event.TurnStarted:
		m.live.active = true
	case event.TokenDelta:
		m.applyChunk(ev.Chunk)
	case event.ToolCallStarted:
		m.toolStarted(ev)
	case event.ToolCallCompleted:
		m.toolCompleted(ev)
	case event.StepDone:
		m.stepDone(ev)
	case event.PermissionRequested:
		m.permissionRequested(ev)
	case event.UserInputRequested:
		m.userInputRequested(ev)
	case event.TurnDone:
		m.commitLive()
	case event.TurnInterrupted:
		m.turnInterrupted()
	case event.TurnFailed:
		m.turnFailed(ev)
	}
	return m
}

// CommitUser appends the user's submitted message as one kindUser entry with a
// fresh stable ID and returns the next model. Blocks are the same []content.Block
// the submit path builds (buildBlocks) and queues — text plus any @attachments.
// It does NOT touch the live segment: a message submitted mid-turn (queued while
// Running) must land in scrollback without truncating the in-progress assistant
// output. An empty Blocks slice still commits one entry — emptiness is rejected
// upstream at the input boundary, not here.
func (m transcriptModel) CommitUser(blocks []content.Block) transcriptModel {
	m.nextID++
	m.committed = append(m.committed, entry{ID: m.nextID, Kind: kindUser, Blocks: blocks})
	return m
}

// CommitNotice appends a leveled, out-of-band notification as one kindNotice entry
// carrying level + text with a fresh stable ID, and returns the next model. It is the
// single notice-commit primitive — the startup banner and /help (info), and the
// error paths (error) all route through it. It does NOT touch the live segment: a
// notice is out-of-band from the assistant's in-progress output. An empty text still
// commits one entry (the bar marks the event).
func (m transcriptModel) CommitNotice(level noticeLevel, text string) transcriptModel {
	m.nextID++
	m.committed = append(m.committed, entry{
		ID:     m.nextID,
		Kind:   kindNotice,
		Level:  level,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	})
	return m
}

// CommitSystem appends an info-level notice (e.g. the /help listing). It is a thin
// wrapper over CommitNotice(noticeInfo, …) — a system notice IS an info notice.
func (m transcriptModel) CommitSystem(text string) transcriptModel {
	return m.CommitNotice(noticeInfo, text)
}

// CommitError appends an error-level notice for a non-fatal failure. It is the
// out-of-band error path — distinct from a turn failure's terminal notice
// (turnFailed) — used by Screen for submit/dispatch/reopen failures that must be
// surfaced without ending a turn. A nil err commits an empty message (the entry
// still marks the failure). It is a thin wrapper over CommitNotice(noticeError, …).
func (m transcriptModel) CommitError(err error) transcriptModel {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return m.CommitNotice(noticeError, msg)
}

// permissionRequested is the permission-gate prompt-open boundary: it commits any
// pending live prose FIRST (so the narration that precedes the gate lands ahead of
// the prompt record in append-only order), then commits the FULL permission
// context (ToolName + Description, read off the sealed request) as a
// kindPromptRecord entry. A nil Request yields an empty-context record rather than
// a panic (fail-visible). live is intentionally NOT reset — the turn continues
// while the gate is pending.
func (m *transcriptModel) permissionRequested(ev event.PermissionRequested) {
	m.commitProse()
	ctx := promptContext{Kind: promptPermission}
	if ev.Request != nil {
		ctx.ToolName = ev.Request.ToolName()
		ctx.Description = ev.Request.Description()
	}
	m.commitPrompt(ctx)
}

// userInputRequested is the AskUser prompt-open boundary: it commits any pending
// live prose FIRST, then commits the FULL user-input context (Question + ALL
// Choices) as a kindPromptRecord entry. Choices are copied so a later mutation of
// the event's slice cannot reach the committed record. live is NOT reset.
func (m *transcriptModel) userInputRequested(ev event.UserInputRequested) {
	m.commitProse()
	ctx := promptContext{Kind: promptUserInput, Question: ev.Question}
	if len(ev.Choices) > 0 {
		ctx.Choices = append([]string(nil), ev.Choices...)
	}
	m.commitPrompt(ctx)
}

// commitPrompt appends one kindPromptRecord entry carrying ctx with a fresh stable
// ID. It is the shared tail of the two prompt-open boundaries.
func (m *transcriptModel) commitPrompt(ctx promptContext) {
	m.nextID++
	m.committed = append(m.committed, entry{ID: m.nextID, Kind: kindPromptRecord, Prompt: &ctx})
}

// applyChunk routes one streamed chunk into the live segment: text accumulates
// into live.Text, thinking into live.Thinking. Any other chunk variant (e.g. a
// tool-use chunk) is skipped — tool-call reconstruction is a later task.
func (m *transcriptModel) applyChunk(c content.Chunk) {
	switch chunk := c.(type) {
	case *content.TextChunk:
		m.live.Text += chunk.Text
	case *content.ThinkingChunk:
		m.live.Thinking += chunk.Thinking
	}
}

// commitLive is the TurnDone lifecycle path. In a well-formed stream every step
// already committed via its StepDone (which resets the live segment), so live is
// empty here and this is a pure reset. It is DEFENSIVE: should a turn somehow end with
// uncommitted provisional live (no StepDone for an in-flight step), it flushes that
// prose as one kindAssistant entry and any leftover live.Calls in their CURRENT status
// (TurnDone is a normal completion, NOT a cancellation — flushCalls with the identity
// transform), so a stray segment is never silently lost. It finally resets live.
func (m *transcriptModel) commitLive() {
	m.commitProse()
	m.flushCalls(func(c ToolCallView) ToolCallView { return c })
	m.live = liveSeg{}
}

// flushCalls commits every live call as its own kindTool entry, in order, after
// applying transform to each (so a terminal can rewrite status — e.g. running →
// cancelled — while a normal completion leaves it untouched). It is the shared
// drain used by both commitLive (identity transform: preserve status) and
// turnInterrupted (cancel running calls). It does NOT reset live.Calls; callers
// reset the whole live segment afterward.
func (m *transcriptModel) flushCalls(transform func(ToolCallView) ToolCallView) {
	for i := range m.live.Calls {
		m.commitCall(transform(m.live.Calls[i]))
	}
}

// commitProse appends the live segment's pending reasoning/narration to committed
// as one kindAssistant entry (leading ThinkingBlock, then TextBlock; empty blocks
// omitted), allocates its stable ID, and clears ONLY the prose fields — live.Calls
// and active are left intact so a running batch survives the prose commit. It is a
// no-op when there is no pending prose. It is the PROVISIONAL-prose path used at the
// prompt-open boundaries and the abnormal terminals (TurnInterrupted/TurnFailed) to
// flush an in-flight step's narration before its tombstone/error; the normal,
// finalized prose path is stepDone → commitStepAssistant (which renders the AIMessage,
// not the accumulated provisional text).
func (m *transcriptModel) commitProse() {
	if m.live.Thinking == "" && m.live.Text == "" {
		return
	}
	var blocks []content.Block
	if m.live.Thinking != "" {
		blocks = append(blocks, &content.ThinkingBlock{Thinking: m.live.Thinking})
	}
	if m.live.Text != "" {
		blocks = append(blocks, &content.TextBlock{Text: m.live.Text})
	}
	m.nextID++
	m.committed = append(m.committed, entry{ID: m.nextID, Kind: kindAssistant, Blocks: blocks})
	m.live.Thinking, m.live.Text = "", ""
}

// toolStarted records a freshly started tool call as a running card in live.Calls.
// The card lives in the live tail (the in-progress assistant segment) and is NOT
// committed to scrollback here: a step's tool cards are committed as a group when its
// StepDone snaps the finalized step in (or, defensively, when a turn ends with an
// incomplete in-flight step). It carries the event's redacted Summary so the live and
// committed cards show the same one-line, secret-free header.
func (m *transcriptModel) toolStarted(ev event.ToolCallStarted) {
	m.live.Calls = append(m.live.Calls, ToolCallView{
		CallID:   ev.CallID,
		ToolName: ev.ToolName,
		Summary:  ev.Summary,
		Status:   ToolRunning,
	})
}

// toolCompleted resolves the matching live call (by CallID) IN PLACE — setting its
// terminal status and its capped, redacted ResultPreview — so the live tail shows the
// completed card. It does NOT commit the card or remove it from live.Calls: the card
// is committed only at the step boundary (StepDone) or, defensively, at the turn
// terminal. Keeping the resolved live card lets StepDone reuse its redacted
// Summary/preview when it commits the finalized group (the stored ToolResultMessage
// carries the raw, uncapped result; the resolved live card carries the display-safe
// one). An unknown CallID is a no-op — no panic.
func (m *transcriptModel) toolCompleted(ev event.ToolCallCompleted) {
	for i := range m.live.Calls {
		if m.live.Calls[i].CallID != ev.CallID {
			continue
		}
		m.live.Calls[i].Status = ToolOK
		if ev.IsError {
			m.live.Calls[i].Status = ToolError
		}
		m.live.Calls[i].Result = splitLines(ev.ResultPreview)
		return
	}
	// unknown CallID: no-op
}

// stepDone is the StepDone commit point: it SNAPS the transcript to the loop's
// finalized step group. It commits the step's AIMessage prose (thinking + narration)
// as one kindAssistant entry, then each of the AIMessage's ToolUseBlocks as its own
// kindTool entry — preferring the matching resolved LIVE card (its redacted Summary +
// capped preview) and falling back to the stored block + ToolResultMessage when no
// live card streamed (e.g. a dropped ToolCallStarted, or a subagent-loop step the TUI
// only sees finalized). Committing prose first then tools mirrors the AIMessage block
// order; a multi-step turn therefore renders as separate per-step groups, never
// merged. After committing, the provisional live segment is reset (active preserved):
// the dropped/partial TokenDeltas of this step vanish — the self-heal.
func (m *transcriptModel) stepDone(ev event.StepDone) {
	ai, results := splitStepGroup(ev.Messages)
	m.commitStepAssistant(ai)
	uses := toolUsesOf(ai)
	for i := range uses {
		m.commitCall(m.stepToolCard(uses[i], results, i))
	}
	// SNAP: drop the provisional live for this step; active stays so the turn's next
	// step (or its terminal) is still seen as in-progress.
	active := m.live.active
	m.live = liveSeg{active: active}
}

// commitStepAssistant commits the AIMessage's prose (leading ThinkingBlock, then
// TextBlock) as one kindAssistant entry. A nil AIMessage commits nothing. A
// tool-use-only message (no thinking, no text) still commits one bare kindAssistant
// entry so the step's assistant bullet renders ahead of its tool cards (the renderer
// shows a bare bullet for a card-only segment); a fully empty message commits nothing.
func (m *transcriptModel) commitStepAssistant(ai *content.AIMessage) {
	if ai == nil {
		return
	}
	var blocks []content.Block
	if th := thinkingText(ai.Blocks); th != "" {
		blocks = append(blocks, &content.ThinkingBlock{Thinking: th})
	}
	if tx := textOnly(ai.Blocks); tx != "" {
		blocks = append(blocks, &content.TextBlock{Text: tx})
	}
	if len(blocks) == 0 && len(toolUsesOf(ai)) == 0 {
		return // nothing to show for this assistant message
	}
	// An empty-text tool step (no prose blocks, but tool uses present) commits a
	// doneHeadline entry so the renderer shows a bold "● Done" headline above the
	// step's separately-committed tool cards (design §3 rule 4), not a bare bullet.
	m.nextID++
	m.committed = append(m.committed, entry{
		ID:           m.nextID,
		Kind:         kindAssistant,
		Blocks:       blocks,
		doneHeadline: len(blocks) == 0 && len(toolUsesOf(ai)) > 0,
	})
}

// stepToolCard builds the committed ToolCallView for the index-th tool-use block of a
// step. It prefers the resolved live card at the same position (carrying the redacted
// Summary and capped preview already shown live); when there is none it falls back to
// the stored block's tool name and the matching ToolResultMessage text (correlated by
// ToolUseID). The fallback shows no summary (the redacted summary is not carried in
// the stored message); its ✓/✗ status comes from ToolResultMessage.IsError, which the
// stored message now preserves (an error result commits a ✗ card even on the fallback
// path, no longer relying on the live card's preview).
func (m *transcriptModel) stepToolCard(use content.ToolUseBlock, results map[string]*content.ToolResultMessage, idx int) ToolCallView {
	if idx < len(m.live.Calls) {
		live := m.live.Calls[idx]
		if live.ToolName == use.Name {
			if live.Status == ToolRunning {
				live.Status = ToolOK // the step finalized: a still-"running" live card resolves OK
			}
			return live
		}
	}
	card := ToolCallView{ToolName: use.Name, Status: ToolOK}
	if r, ok := results[use.ID]; ok {
		card.Result = splitLines(toolResultText(r))
		if r.IsError {
			card.Status = ToolError
		}
	}
	return card
}

// commitCall appends one resolved tool call as its own kindTool entry with a fresh
// stable ID. The single-element Calls slice carries the terminal ToolCallView so
// the renderer can reuse the existing tool-card rendering.
func (m *transcriptModel) commitCall(call ToolCallView) {
	m.nextID++
	m.committed = append(m.committed, entry{
		ID:    m.nextID,
		Kind:  kindTool,
		Calls: []ToolCallView{call},
	})
}

// turnInterrupted is the cancellation terminal: it commits pending prose, marks
// every still-running live call cancelled and commits each as its own kindTool
// entry (so completed/cancelled tool work stays visible), appends the
// content-less kindInterrupted tombstone, and resets live.
func (m *transcriptModel) turnInterrupted() {
	m.commitProse()
	m.flushCalls(func(c ToolCallView) ToolCallView {
		if c.Status == ToolRunning {
			c.Status = ToolCancelled
		}
		return c
	})
	m.nextID++
	m.committed = append(m.committed, entry{ID: m.nextID, Kind: kindInterrupted})
	m.live = liveSeg{}
}

// turnFailed is the failure terminal: it commits pending prose so partial work
// stays visible, appends an error-level kindNotice carrying the failure message (a
// nil Err yields an empty message — the entry still marks the failure), and resets
// live. The error-notice commit reuses the same noticeError path as CommitError.
func (m *transcriptModel) turnFailed(ev event.TurnFailed) {
	m.commitProse()
	msg := ""
	if ev.Err != nil {
		msg = ev.Err.Error()
	}
	m.nextID++
	m.committed = append(m.committed, entry{
		ID:     m.nextID,
		Kind:   kindNotice,
		Level:  noticeError,
		Blocks: []content.Block{&content.TextBlock{Text: msg}},
	})
	m.live = liveSeg{}
}
