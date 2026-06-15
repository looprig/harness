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

// displayID is a stable, monotonically assigned identifier for a committed
// transcript entry. It is allocated once when a live segment is committed and
// never reused, so a renderer can key on it across re-renders. The zero value is
// never a valid assigned ID — the first commit allocates 1.
type displayID uint64

// entryKind discriminates the source/kind of a committed transcript entry.
// kindTool is one resolved tool call (terminal state); kindError carries a turn
// failure message; kindInterrupted is the content-less tombstone for an
// interrupted turn.
type entryKind uint8

const (
	kindUser entryKind = iota
	kindAssistant
	kindTool
	kindPromptRecord
	kindError
	kindInterrupted
	// kindSystem is a session/notice line (e.g. the startup "session ready" row or
	// the /help listing). It carries a single TextBlock and renders faint.
	kindSystem
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
	// Prompt carries the FULL prompt context for a kindPromptRecord entry; it is
	// nil for every other kind. Kept as a pointer so non-prompt entries pay no
	// per-entry cost and a nil here is an unambiguous "not a prompt record".
	Prompt *promptContext
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
// *content.TextChunk → live.Text and *content.ThinkingChunk → live.Thinking;
// TurnDone commits a non-empty live segment. ToolCallStarted/ToolCallCompleted
// drive the per-call card state machine. PermissionRequested/UserInputRequested
// are prompt-open boundaries: each commits any pending prose, then commits the
// FULL prompt context as a kindPromptRecord entry (the live segment is NOT reset —
// the turn continues while the gate is pending). TurnInterrupted/TurnFailed are
// the terminals. It returns ONLY the next transcriptModel — no uiAction; prompt
// clearing on terminals and active-surface control are the interactionModel's job,
// not the transcript's.
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

// CommitSystem appends a session/notice line (e.g. "session ready" or the /help
// listing) as one kindSystem entry with a fresh stable ID and returns the next
// model. It does NOT touch the live segment — a system notice is out-of-band from
// the assistant's in-progress output.
func (m transcriptModel) CommitSystem(text string) transcriptModel {
	m.nextID++
	m.committed = append(m.committed, entry{
		ID:     m.nextID,
		Kind:   kindSystem,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	})
	return m
}

// CommitError appends a faint, non-fatal error line as one kindError entry with a
// fresh stable ID and returns the next model. It is the out-of-band error path —
// distinct from a turn failure's terminal kindError (turnFailed) — used by Screen
// for submit/dispatch/reopen failures that must be surfaced without ending a turn.
// A nil err commits an empty message (the entry still marks the failure).
func (m transcriptModel) CommitError(err error) transcriptModel {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	m.nextID++
	m.committed = append(m.committed, entry{
		ID:     m.nextID,
		Kind:   kindError,
		Blocks: []content.Block{&content.TextBlock{Text: msg}},
	})
	return m
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

// commitLive is the TurnDone path: it commits any pending live prose/thinking as
// one kindAssistant entry (see commitProse), then commits any leftover live.Calls
// in their CURRENT status (TurnDone is a normal completion, NOT a cancellation —
// see flushCalls with the identity transform), and finally resets live. In a
// well-formed stream every call resolves via toolCompleted before TurnDone, so
// live.Calls is empty here; flushing rather than dropping guarantees a stray
// unresolved call is never silently lost from the transcript.
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
// no-op when there is no pending prose. Committing prose before a tool card (on
// tool start/complete and on the terminals) is what preserves append-only reading
// order: prose1 → tool card → prose2.
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

// toolStarted records a freshly started tool call. Any pending live prose/thinking
// is committed FIRST so the assistant narration that precedes the call lands ahead
// of the tool card in append-only scrollback order; then a running ToolCallView is
// appended to live.Calls to await its completion.
func (m *transcriptModel) toolStarted(ev event.ToolCallStarted) {
	m.commitProse()
	m.live.Calls = append(m.live.Calls, ToolCallView{
		CallID:   ev.CallID,
		ToolName: ev.ToolName,
		Summary:  ev.Summary,
		Status:   ToolRunning,
	})
}

// toolCompleted resolves the matching live call (by CallID) into exactly one
// committed kindTool entry at terminal state, then removes it from live.Calls
// (commit-once: never both live and committed, never double-committed). Pending
// prose is committed first so any narration interleaved before this completion
// keeps its order. An unknown CallID is a no-op — no panic, no commit.
func (m *transcriptModel) toolCompleted(ev event.ToolCallCompleted) {
	idx := -1
	for i := range m.live.Calls {
		if m.live.Calls[i].CallID == ev.CallID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return // unknown CallID: no-op
	}
	m.commitProse()
	call := m.live.Calls[idx]
	call.Status = ToolOK
	if ev.IsError {
		call.Status = ToolError
	}
	call.Result = splitLines(ev.ResultPreview)
	m.commitCall(call)
	m.live.Calls = append(m.live.Calls[:idx], m.live.Calls[idx+1:]...)
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
// stays visible, appends a kindError entry carrying the failure message (a nil Err
// yields an empty message — the entry still marks the failure), and resets live.
func (m *transcriptModel) turnFailed(ev event.TurnFailed) {
	m.commitProse()
	msg := ""
	if ev.Err != nil {
		msg = ev.Err.Error()
	}
	m.nextID++
	m.committed = append(m.committed, entry{
		ID:     m.nextID,
		Kind:   kindError,
		Blocks: []content.Block{&content.TextBlock{Text: msg}},
	})
	m.live = liveSeg{}
}
