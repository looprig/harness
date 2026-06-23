package tui

import (
	"strings"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/uuid"
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
	// kindSubagent is the COLLAPSED-but-present activity line for a SUBAGENT loop's
	// StepDone (design §6d Option B): one compact "▸ <agent>: <verb>" row attributing
	// the step to the agent driving that loop (the agent name learned from the loop's
	// LoopStarted, or the loopID short form when unknown/empty). It deliberately does
	// NOT fold the subagent's narration/tool cards into the root transcript — that would
	// interleave concurrent subagent streams (Option A, deferred). It carries Agent +
	// Verb; renderEntry renders it via renderSubagentLine.
	kindSubagent
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

// promptContext is the FULL AskUser payload a kindPromptRecord entry commits to
// scrollback when a user-input gate opens: the question + every choice. It is the
// append-only SCROLLBACK record — distinct from the interaction layer's compact
// bottom-box control (prompt in prompt.go), which carries selection state and is
// redrawn every frame. Permission gates do NOT use this: they surface as the
// "Approved …"/"Denied …" verb on their committed tool card, never as a record.
type promptContext struct {
	Question string   // the AskUser question
	Choices  []string // every offered choice, in order
	// Agent attributes the prompt to the SUBAGENT that opened it (design §6d Option B:
	// child gates surface as attributed records). It is the resolved label — the
	// subagent's agent name, or its loopID short form when the name is unknown/empty.
	// It is EMPTY for a PRIMARY-loop AskUser (the orchestrator's own question is not
	// agent-labeled); renderUserInputRecord prepends "<agent>: " only when it is set.
	Agent string
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
	// headline is the bold bullet text of a kindAssistant entry that has NO narration
	// (empty TextBlock) — currently "Multiple actions", the umbrella for an empty-text
	// step that ran more than one tool call. Empty for every other assistant entry
	// (narration entries render their text; a single-tool empty-text step promotes its
	// one card to the bullet instead — see promoted). Renders as "● <headline>".
	headline string
	// promoted marks a kindTool entry whose single card is rendered AS the assistant
	// bullet ("● <verb >ToolName(args)" + result) rather than an indented "⎿ …" card —
	// the committed form of an empty-text step that ran exactly one tool call. Set ONLY
	// by stepDone for that case; every other kindTool entry renders as a normal card.
	promoted bool
	// Agent is the attribution label of a kindSubagent line — the agent name driving the
	// producing subagent loop, or that loop's id short form when the name is unknown/
	// empty. Meaningful ONLY for kindSubagent; empty for every other kind.
	Agent string
	// Verb is the activity word of a kindSubagent line ("done" for a committed StepDone).
	// Meaningful ONLY for kindSubagent; empty for every other kind.
	Verb string
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
	// gateDecisions records, by gate ToolExecutionID, how each PERMISSION gate of the current
	// step was resolved: gatePending on PermissionRequested, then gateApproved/
	// gateDenied once Screen calls ResolveGate from the user's keypress. toolStarted
	// bakes the decision into its live card, so the committed card reads "Approved …" /
	// "Denied …". It is reset (to nil) at each StepDone with the rest of the segment.
	gateDecisions map[uuid.UUID]gateDecision
}

// empty reports whether the live segment carries no committable content — no
// streamed reasoning, no streamed narration, and no reconstructed tool call.
// active is intentionally not consulted: an active-but-content-less segment is
// still empty and must not commit.
func (s liveSeg) empty() bool {
	return s.Thinking == "" && s.Text == "" && len(s.Calls) == 0
}

// queuedInput is a transient affordance for one submitted-but-not-yet-committed
// user message: its submit correlation id, the blocks the TUI remembers from the
// submit (InputQueued carries no Message, so the affordance text comes from here),
// and a shown flag the loop's InputQueued event flips on. It is NOT a committed
// transcript entry — it is a pending hint rendered below the live tail until the
// authoritative TurnStarted/TurnFoldedInto commits the real user row (or
// InputCancelled/TurnRejected drops it).
type queuedInput struct {
	inputID uuid.UUID
	blocks  []content.Block
	shown   bool
}

// transcriptModel is the pure, side-effect-free reducer over a turn's event
// stream. committed holds finalized entries in display order; live is the
// in-progress segment for the current turn; queued holds the pending
// queued-input affordances (ordered by submit); nextID is the next stable ID to
// allocate. It is applied by value: ApplyEvent returns the next model.
type transcriptModel struct {
	committed []entry
	live      liveSeg
	queued    []queuedInput
	nextID    displayID
	// primaryLoopID is the loop whose GENUINE user turns become committed kindUser
	// rows. A turn-start event commits a user row only when its Header.LoopID equals
	// this id (in addition to Cause.LoopID being zero and a Message present): a
	// SUBAGENT loop's own initial task also arrives as an untriggered TurnStarted
	// (Cause.LoopID == 0) carrying a Message, and the DefaultEventFilter delivers
	// it (Enduring from every loop), so without this scoping it would bogusly commit as
	// a human user row. A subagent loop's own turns surface ONLY collapsed via StepDone,
	// attributed by LoopID (§5/§6) — never as a human user row. It is wired from
	// Agent.PrimaryLoopID() at construction (see screen.go New / handleReopenResult);
	// the zero value matches a zero-LoopID primary turn (the single-loop default).
	primaryLoopID uuid.UUID
	// loopAgents maps a loop id to the agent name driving it, learned from each loop's
	// LoopStarted.AgentName (P1) as those Enduring/all-loops events arrive on the TUI's
	// lifetime subscription. It is the source of a subagent's attribution label (the
	// "▸ <agent>: done" collapsed StepDone line and an attributed AskUser record). A
	// loop absent from the map (LoopStarted not yet seen) or mapped to an empty name (a
	// legacy/no-name loop) falls back to the loopID short form — see agentLabel. It is
	// cloned on write so the by-value reducer never aliases a prior model's map.
	loopAgents map[uuid.UUID]identity.AgentName
	// loopParent maps a SUBAGENT child loop id to its spawn key — the parent loop/turn/
	// step coordinates plus the durable provider tool-use id of the Subagent call that
	// spawned it — recorded from a child LoopStarted whose ParentToolUseID is non-empty
	// (design §3). A child loop NOT in this map (empty/absent ParentToolUseID, e.g. a
	// non-tool spawn) keeps the legacy collapsed "▸ <agent>: done" fallback line. It is
	// cloned on write (value-copy contract).
	loopParent map[uuid.UUID]spawnKey
	// subagentAccum holds, per spawn key, the detached accumulator a child's ENDURING
	// events build up (design §1/§3): agent, task, nested children, step count, terminal
	// status. It is DETACHED (not state on a card) because the child's events precede the
	// parent's StepDone (the parent card does not exist yet) and a cold restore rebuilds
	// it from scratch. The orchestrator's StepDone reconciles each Subagent block against
	// it by spawn key. It is cloned on write (value-copy contract).
	subagentAccum map[spawnKey]*subagentAccumulator
}

// spawnKey identifies one Subagent tool call's spawn: the parent loop/turn/step
// coordinates (where the Subagent block lives, read from the orchestrator's StepDone
// header) plus the durable provider tool-use id of that block. It is built identically
// on the two sides that must meet — the child LoopStarted (Cause.* + ParentToolUseID)
// and the orchestrator StepDone (its own LoopID/TurnID/StepID + block.ID) — so the
// accumulator a child fills is the one the parent card looks up.
type spawnKey struct {
	parentLoopID uuid.UUID
	parentTurnID uuid.UUID
	parentStepID uuid.UUID
	toolUseID    string
}

// subagentAccumulator is the detached, per-spawn build-up of a subagent's nested card,
// assembled PURELY from the child's ENDURING events (LoopStarted→agent, first
// TurnStarted→task, each StepDone→a child card + steps++, the terminal→status). The
// orchestrator's StepDone copies these onto the committed Subagent ToolCallView (design
// §3). It is a pointer in the map so an in-place field bump (steps++, append a child)
// is visible without re-storing — but the MAP is cloned on write so a by-value reducer
// never aliases a prior model's map; a freshly-cloned map's pointers are themselves
// freshly allocated when first created here (see ensureAccum).
type subagentAccumulator struct {
	agent    string         // the child LoopStarted.AgentName
	task     string         // the child's first TurnStarted.Message, truncated
	children []ToolCallView // nested tool cards from the child's StepDone groups
	steps    int            // count of the child's StepDone events
	status   subStatus      // the child terminal state (running until a terminal arrives)
	nested   int            // depth-2 collapsed counter (Task 7; zero here)
}

// ApplyEvent folds one turn-stream event into the model and returns the next
// model. LoopStarted records the producing loop's agent name (LoopID→AgentName) for
// later subagent attribution; it commits nothing. TurnStarted begins/keeps a live
// assistant segment AND — for GENUINE user
// input only (Header.Cause.LoopID == 0; a subagent hand-back carries a
// non-zero one and commits NO user row) — commits the authoritative user row from
// its Message and drops the matching queued affordance. TurnFoldedInto does the
// same user-row commit for a folded tool-continuation input. InputQueued reveals
// the queued affordance for its InputID; InputCancelled drops it (no row);
// TurnRejected drops it and commits an error notice (a rejected message must not
// silently vanish). TokenDelta routes
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
// PermissionRequested only REMEMBERS the gate (by ToolExecutionID) so the call's committed card
// can read "Approved …" / "Denied …" once Screen reports the keypress via ResolveGate;
// it commits nothing (the permission shows on the tool card, not a separate record).
// UserInputRequested (AskUser is not a tool) commits ONLY the prompt record. Neither
// commits pending prose — the provisional live prose stays live and commits exactly
// once via StepDone (no duplicate) — and neither resets the live segment, so the turn
// continues while the gate is pending.
// TurnInterrupted/TurnFailed are the abnormal terminals: the in-flight INCOMPLETE step
// never emitted a StepDone, so its provisional live is committed (partial work stays
// visible) before the tombstone/error. It returns ONLY the next transcriptModel — no
// uiAction; prompt clearing on terminals and active-surface control are the
// interactionModel's job, not the transcript's.
func (m transcriptModel) ApplyEvent(ev event.Event) transcriptModel {
	switch ev := ev.(type) {
	case event.LoopStarted:
		m.recordLoopAgent(ev.LoopID, ev.AgentName)
		m.loopSpawned(ev)
	case event.TurnStarted:
		m.live.active = true
		m.subagentTask(ev.LoopID, ev.Message)
		m.startTurnUser(ev.LoopID, ev.Cause.LoopID, ev.Cause.CommandID, ev.Message)
	case event.TurnFoldedInto:
		m.startTurnUser(ev.LoopID, ev.Cause.LoopID, ev.Cause.CommandID, ev.Message)
	case event.InputQueued:
		m.markQueued(ev.Cause.CommandID)
	case event.InputCancelled:
		m.dropQueued(ev.Cause.CommandID)
	case event.TurnRejected:
		m.rejectInput(ev.Cause.CommandID, ev.Reason)
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
		if m.subagentTerminal(ev.LoopID, subDone) {
			break // a child terminal: record status, never touch the primary live segment
		}
		m.commitLive()
	case event.TurnInterrupted:
		if m.subagentTerminal(ev.LoopID, subInterrupted) {
			break
		}
		m.turnInterrupted()
	case event.TurnFailed:
		if m.subagentTerminal(ev.LoopID, subFailed) {
			break
		}
		m.turnFailed(ev)
	}
	return m
}

// recordLoopAgent records the agent name driving loopID (the LoopStarted boundary), so
// a later subagent StepDone/AskUser from that loop can be attributed by name. The map
// is cloned on write (value-copy contract) so the by-value reducer never aliases a
// prior model's map. A re-seen loop id overwrites — the name is immutable, so this is
// idempotent in practice.
func (m *transcriptModel) recordLoopAgent(loopID uuid.UUID, name identity.AgentName) {
	next := make(map[uuid.UUID]identity.AgentName, len(m.loopAgents)+1)
	for k, v := range m.loopAgents {
		next[k] = v
	}
	next[loopID] = name
	m.loopAgents = next
}

// agentLabel resolves the attribution label for loopID: the agent name learned from
// its LoopStarted when known and non-empty, else the loopID short form (loopShortForm)
// — never an empty string, so a label is always shown and a missing/legacy name never
// renders a dangling "▸ :". It is the single resolver shared by the subagent StepDone
// line and the attributed AskUser record.
func (m transcriptModel) agentLabel(loopID uuid.UUID) string {
	if name, ok := m.loopAgents[loopID]; ok && name != "" {
		return string(name)
	}
	return loopShortForm(loopID)
}

// loopShortForm is the loopID fallback label: the first hyphen-delimited group of the
// canonical uuid string (its 8 leading hex chars), a compact stable id for a loop with
// no known/empty agent name. The zero loop id yields "00000000" (still a non-empty,
// unambiguous label).
func loopShortForm(loopID uuid.UUID) string {
	s := loopID.String()
	if i := strings.IndexByte(s, '-'); i >= 0 {
		return s[:i]
	}
	return s
}

// CommitUser appends the user's submitted message as one kindUser entry with a
// fresh stable ID and returns the next model. Its authoritative caller is
// startTurnUser, which passes the loop's event Message.Blocks (the stored user
// message) — NOT the submit-built blocks, which now only feed the queued affordance.
// It does NOT touch the live segment: a message folded mid-turn must land in
// scrollback without truncating the in-progress assistant output. An empty Blocks
// slice still commits one entry — emptiness is rejected upstream at the input
// boundary, not here.
func (m transcriptModel) CommitUser(blocks []content.Block) transcriptModel {
	m.nextID++
	m.committed = append(m.committed, entry{ID: m.nextID, Kind: kindUser, Blocks: blocks})
	return m
}

// CommitUserText commits a plain-text user row (no attachments). It is for the
// submit-FAILED path: when buildBlocks rejects a message (e.g. an image on a text-only
// model) the message is shown in scrollback as the user's row — even though it was
// never sent to the model — so the user sees what they asked alongside the error.
func (m transcriptModel) CommitUserText(text string) transcriptModel {
	return m.CommitUser([]content.Block{&content.TextBlock{Text: text}})
}

// RecordSubmit registers a fire-and-forget submit by its correlation id so the
// queued affordance can show the remembered blocks once the loop's InputQueued
// event arrives. The remembered blocks are DISPLAY-ONLY and assumed immutable after
// submit (the committed row comes from the event's authoritative Message, not these);
// callers must not mutate the slice they pass. It is called by the Screen on a
// successful submitResultMsg. If an
// entry for inputID already exists (the InputQueued event raced ahead of the
// submit result), this FILLS its blocks rather than appending a duplicate — so a
// shown-but-blockless placeholder gets its text. Otherwise it appends a fresh
// queuedInput (shown=false) at the submit-order tail. It returns the next model.
//
// Value-copy contract: a fresh queued slice is built (never an in-place mutation
// of a shared backing array) so the by-value reducer never aliases a prior model's
// queue — mirroring the interaction model's cloneHead rationale.
func (m transcriptModel) RecordSubmit(inputID uuid.UUID, blocks []content.Block) transcriptModel {
	next := append([]queuedInput(nil), m.queued...)
	for i := range next {
		if next[i].inputID == inputID {
			next[i].blocks = blocks
			m.queued = next
			return m
		}
	}
	m.queued = append(next, queuedInput{inputID: inputID, blocks: blocks})
	return m
}

// QueuedInputs returns, in submit order, the blocks of every queued affordance
// that is ready to render (shown by an InputQueued event AND carrying remembered
// blocks). A still-blockless placeholder (InputQueued arrived before RecordSubmit
// filled the blocks) is skipped until its blocks land. The returned slice is a
// fresh copy, so a caller cannot reach the model's internal queue.
func (m transcriptModel) QueuedInputs() [][]content.Block {
	var out [][]content.Block
	for _, q := range m.queued {
		if q.shown && q.blocks != nil {
			out = append(out, q.blocks)
		}
	}
	return out
}

// startTurnUser commits the authoritative user row for a turn-start event
// (TurnStarted/TurnFoldedInto) and drops the matching queued affordance. It
// commits a kindUser row ONLY for a GENUINE PRIMARY-loop user turn — ALL THREE must
// hold: loopID == m.primaryLoopID (a SUBAGENT loop's OWN initial task also arrives
// as an untriggered TurnStarted carrying a Message — Cause.LoopID == 0,
// LoopID == the subagent loop — and the DefaultEventFilter delivers it from every
// loop, so without this scoping it would bogusly commit as a human user row);
// triggeredBy is the zero loop id (a SubagentResult hand-back FOLDS into the PRIMARY
// loop, so LoopID == primary but Cause.LoopID != 0 — that is a hand-back, not a
// human turn); and a Message is present. The row is committed from the event's
// authoritative blocks, never from remembered submit state, which sidesteps the
// submit↔event arrival race. The queued affordance for this InputID is always
// dropped (the real row, if any, supersedes it).
func (m *transcriptModel) startTurnUser(loopID, triggeredBy, inputID uuid.UUID, msg *content.UserMessage) {
	if loopID == m.primaryLoopID && triggeredBy.IsZero() && msg != nil {
		*m = m.CommitUser(msg.Blocks)
	}
	m.dropQueued(inputID)
}

// markQueued reveals the queued affordance for inputID (InputQueued boundary). If
// no entry exists yet (InputQueued raced ahead of RecordSubmit) it creates a
// shown-but-blockless placeholder so the affordance appears the instant the
// remembered blocks land via RecordSubmit; until then QueuedInputs skips it. It
// rebuilds the slice rather than mutating a shared backing array (value-copy
// contract).
func (m *transcriptModel) markQueued(inputID uuid.UUID) {
	next := append([]queuedInput(nil), m.queued...)
	for i := range next {
		if next[i].inputID == inputID {
			next[i].shown = true
			m.queued = next
			return
		}
	}
	m.queued = append(next, queuedInput{inputID: inputID, shown: true})
}

// dropQueued removes the queued affordance for inputID, if present. It rebuilds the
// slice (value-copy contract) so the reducer never mutates a prior model's queue.
// An unknown inputID is a no-op.
func (m *transcriptModel) dropQueued(inputID uuid.UUID) {
	if len(m.queued) == 0 {
		return
	}
	next := make([]queuedInput, 0, len(m.queued))
	for _, q := range m.queued {
		if q.inputID != inputID {
			next = append(next, q)
		}
	}
	m.queued = next
}

// rejectInput is the TurnRejected boundary: a submitted message the loop refused
// must not silently vanish. It drops the queued affordance for inputID and commits
// an error-level notice naming the reason, so the user sees the rejection.
func (m *transcriptModel) rejectInput(inputID uuid.UUID, reason event.RejectReason) {
	m.dropQueued(inputID)
	*m = m.CommitNotice(noticeError, "input rejected: "+rejectReasonText(reason))
}

// rejectReasonText maps a RejectReason to a short user-facing phrase. The
// zero-value sentinel (RejectUnspecified) and any unknown value degrade to a
// neutral "refused" rather than printing a raw enum number.
func rejectReasonText(reason event.RejectReason) string {
	switch reason {
	case event.RejectQueueFull:
		return "queue full"
	case event.RejectShuttingDown:
		return "shutting down"
	case event.RejectInternal:
		return "internal error"
	default:
		return "refused"
	}
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

// permissionRequested is the permission-gate boundary: it REMEMBERS the gate by its
// ToolExecutionID (decision gatePending) so the call's committed card can read "Approved …" /
// "Denied …" once Screen reports the user's keypress via ResolveGate. It commits
// NOTHING — the permission shows on the tool card itself (the verb + the ✓/✗ glyph),
// not as a separate record — and it does NOT commit pending live prose (the
// provisional narration stays live and commits once at StepDone). live is NOT reset:
// the turn continues while the gate is pending. The map is cloned on write so the
// value-copy reducer never aliases a prior model's gate map.
func (m *transcriptModel) permissionRequested(ev event.PermissionRequested) {
	g := cloneGates(m.live.gateDecisions)
	g[ev.ToolExecutionID] = gatePending
	m.live.gateDecisions = g
}

// ResolveGate records the user's decision for a pending permission gate (callID),
// the source the loop never emits as an event — Screen calls it from the approve/deny
// keypress. An unknown callID (no matching pending gate) is a no-op. The map is cloned
// on write (value-copy contract). It returns the next model.
func (m transcriptModel) ResolveGate(callID uuid.UUID, decision gateDecision) transcriptModel {
	if _, ok := m.live.gateDecisions[callID]; !ok {
		return m
	}
	g := cloneGates(m.live.gateDecisions)
	g[callID] = decision
	m.live.gateDecisions = g
	return m
}

// cloneGates returns a fresh copy of a gate-decision map (nil-safe), so a by-value
// reducer mutation never writes through a map a prior model still holds — the map
// analogue of the slice value-copy contract used elsewhere in this model.
func cloneGates(g map[uuid.UUID]gateDecision) map[uuid.UUID]gateDecision {
	next := make(map[uuid.UUID]gateDecision, len(g)+1)
	for k, v := range g {
		next[k] = v
	}
	return next
}

// userInputRequested is the AskUser prompt-open boundary: it commits the FULL
// user-input context (Question + ALL Choices) as a kindPromptRecord entry. Choices
// are copied so a later mutation of the event's slice cannot reach the committed
// record. A SUBAGENT loop's AskUser (LoopID != primaryLoopID) is attributed: ctx.Agent
// is set to the loop's label (agentLabel) so the record reads "<agent>: <question>"; a
// PRIMARY-loop AskUser leaves Agent empty (the orchestrator's own question is not
// agent-labeled). It does NOT commit pending live prose: the provisional narration
// stays in the live segment and is committed exactly once by the step's StepDone
// (committing it here would duplicate it in append-only scrollback). live is NOT reset
// — the turn continues while the gate is pending.
func (m *transcriptModel) userInputRequested(ev event.UserInputRequested) {
	ctx := promptContext{Question: ev.Question}
	if len(ev.Choices) > 0 {
		ctx.Choices = append([]string(nil), ev.Choices...)
	}
	if ev.LoopID != m.primaryLoopID {
		ctx.Agent = m.agentLabel(ev.LoopID)
	}
	m.commitPrompt(ctx)
}

// commitPrompt appends one kindPromptRecord entry carrying ctx with a fresh stable
// ID. It is the AskUser prompt-open boundary's commit (permission gates surface as the
// verb on their tool card, not as a committed record).
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
		m.commitCall(transform(m.live.Calls[i]), false)
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
		ToolExecutionID: ev.ToolExecutionID,
		ToolName:        ev.ToolName,
		Summary:         ev.Summary,
		Status:          ToolRunning,
		// Bake in the permission decision (if this call prompted): permission resolves
		// BEFORE ToolCallStarted, so the gate is already gateApproved/gateDenied here
		// (gateNone for an ungated/pre-approved call). The card carries it through to
		// the committed entry, so it reads "Approved …" / "Denied …".
		Decision: m.live.gateDecisions[ev.ToolExecutionID],
	})
}

// toolCompleted resolves the matching live call (by ToolExecutionID) IN PLACE — setting its
// terminal status and its capped, redacted ResultPreview — so the live tail shows the
// completed card. It does NOT commit the card or remove it from live.Calls: the card
// is committed only at the step boundary (StepDone) or, defensively, at the turn
// terminal. Keeping the resolved live card lets StepDone reuse its redacted
// Summary/preview when it commits the finalized group (the stored ToolResultMessage
// carries the raw, uncapped result; the resolved live card carries the display-safe
// one). An unknown ToolExecutionID is a no-op — no panic.
func (m *transcriptModel) toolCompleted(ev event.ToolCallCompleted) {
	for i := range m.live.Calls {
		if m.live.Calls[i].ToolExecutionID != ev.ToolExecutionID {
			continue
		}
		m.live.Calls[i].Status = ToolOK
		if ev.IsError {
			m.live.Calls[i].Status = ToolError
		}
		m.live.Calls[i].Result = splitLines(ev.ResultPreview)
		return
	}
	// unknown ToolExecutionID: no-op
}

// stepDone is the StepDone commit point. A SUBAGENT loop's step (LoopID !=
// primaryLoopID) is committed COLLAPSED as a single attributed "▸ <agent>: done" line
// (commitSubagentLine) and returns early — its narration/tool group is deliberately
// NOT folded into the root transcript (design §6d Option B). For the PRIMARY loop it
// SNAPS the transcript to the loop's finalized step group: it builds each tool-use
// block's card (reusing the resolved
// LIVE card — with its redacted Summary, capped preview, and permission Decision — or
// falling back to the stored block + ToolResultMessage when no live card streamed),
// commits the step's AIMessage prose / headline as one kindAssistant entry, then
// commits each card as its own kindTool entry. An empty-text step that ran exactly ONE
// tool promotes that single card to the assistant bullet (promoted=true, no umbrella
// entry); an empty-text step with MORE than one tool gets a "Multiple actions"
// umbrella headline above its cards. A multi-step turn renders as separate per-step
// groups, never merged. After committing, the provisional live segment is reset
// (active preserved): the dropped/partial TokenDeltas of this step vanish — the
// self-heal — and the step's gate decisions are cleared.
func (m *transcriptModel) stepDone(ev event.StepDone) {
	// A SUBAGENT loop's step routes by whether it has a recorded spawn parent:
	//   - WITH a loopParent entry (a tool-spawned subagent): its tool group accumulates
	//     under the parent Subagent card (subagentStep) — no collapsed line; the card is
	//     committed later when the orchestrator's StepDone reconciles it (design §1/§3).
	//   - WITHOUT one (empty/absent ParentToolUseID, e.g. a non-tool spawn): the LEGACY
	//     collapsed "▸ <agent>: done" line, retained as the fallback (design §6d Option B).
	// Either way a subagent step returns early; the PRIMARY loop falls through to commit.
	if ev.LoopID != m.primaryLoopID {
		if _, ok := m.loopParent[ev.LoopID]; ok {
			m.subagentStep(ev)
			return
		}
		m.commitSubagentLine(ev.LoopID, subagentVerbDone)
		return
	}
	ai, results := splitStepGroup(ev.Messages)
	uses := toolUsesOf(ai)
	cards := make([]ToolCallView, len(uses))
	for i := range uses {
		cards[i] = m.stepToolCard(uses[i], results, i)
		cards[i] = m.reconcileSubagent(ev, uses[i], results, cards[i])
	}
	m.commitStepAssistant(ai, len(cards))
	promotedSingle := ai != nil && textOnly(ai.Blocks) == "" && len(cards) == 1
	for i := range cards {
		m.commitCall(cards[i], promotedSingle)
	}
	// SNAP: drop the provisional live for this step; active stays so the turn's next
	// step (or its terminal) is still seen as in-progress.
	active := m.live.active
	m.live = liveSeg{active: active}
}

// subagentVerbDone is the activity word of a committed subagent StepDone line: a
// finalized step reads "done".
const subagentVerbDone = "done"

// commitSubagentLine appends ONE collapsed kindSubagent entry attributing a subagent
// loop's step to its agent (agentLabel: the agent name, or the loopID short form when
// unknown/empty) with the given verb. It does NOT touch the primary live segment — a
// subagent's step is out-of-band from the orchestrator's in-progress narration.
func (m *transcriptModel) commitSubagentLine(loopID uuid.UUID, verb string) {
	m.nextID++
	m.committed = append(m.committed, entry{
		ID:    m.nextID,
		Kind:  kindSubagent,
		Agent: m.agentLabel(loopID),
		Verb:  verb,
	})
}

// loopSpawned records a TOOL-spawned subagent's spawn relationship at its child
// LoopStarted: a non-empty ParentToolUseID means the loop was spawned by a Subagent
// call, so it maps child loopID → spawnKey{Cause.LoopID,Cause.TurnID,Cause.StepID,
// ParentToolUseID} (loopParent) and seeds the detached accumulator with the agent name
// (subagentAccum). An empty ParentToolUseID (primary/non-tool spawn) is left alone —
// that loop keeps the legacy collapsed "▸ <agent>: done" fallback. Both maps are cloned
// on write (value-copy contract).
func (m *transcriptModel) loopSpawned(ev event.LoopStarted) {
	if ev.ParentToolUseID == "" {
		return
	}
	key := spawnKey{ev.Cause.LoopID, ev.Cause.TurnID, ev.Cause.StepID, ev.ParentToolUseID}
	m.loopParent = cloneLoopParent(m.loopParent)
	m.loopParent[ev.LoopID] = key
	acc := m.ensureAccum(key)
	acc.agent = string(ev.AgentName)
}

// subagentTask sets a child accumulator's task from the child's FIRST TurnStarted
// message (truncated to one line), if loopID is a recorded subagent child. Only the
// first message wins: a later TurnStarted for the same loop (a folded continuation)
// leaves the task as the original spawn task. A non-child loop, a missing accumulator,
// or an absent/empty message is a no-op.
func (m *transcriptModel) subagentTask(loopID uuid.UUID, msg *content.UserMessage) {
	key, ok := m.loopParent[loopID]
	if !ok || msg == nil {
		return
	}
	acc, ok := m.subagentAccum[key]
	if !ok || acc.task != "" {
		return
	}
	text := subagentTruncate(textOnly(msg.Blocks))
	if text == "" {
		return
	}
	acc = m.ensureAccum(key)
	acc.task = text
}

// subagentStep folds one child StepDone into its accumulator (design §1): it splits the
// finalized group and builds each tool-use block's card via the PURE storedStepToolCard
// (NEVER stepToolCard — a child must not consult m.live.Calls), appends them as nested
// children, and increments the child's step count. It does NOT commit anything — the
// cards live on the accumulator until the orchestrator's StepDone reconciles them onto
// the parent Subagent card. loopID is known to be a recorded child (the caller checked
// loopParent).
func (m *transcriptModel) subagentStep(ev event.StepDone) {
	key := m.loopParent[ev.LoopID]
	ai, results := splitStepGroup(ev.Messages)
	uses := toolUsesOf(ai)
	acc := m.ensureAccum(key)
	for i := range uses {
		acc.children = append(acc.children, storedStepToolCard(uses[i], results))
	}
	acc.steps++
}

// subagentTerminal records a child loop's terminal status on its accumulator and
// reports whether loopID was a recorded subagent child (so ApplyEvent skips the
// primary-loop terminal handling for it — a child terminal must never flush the
// orchestrator's live segment). A non-child loop returns false (the normal terminal
// path runs).
func (m *transcriptModel) subagentTerminal(loopID uuid.UUID, status subStatus) bool {
	key, ok := m.loopParent[loopID]
	if !ok {
		return false
	}
	acc := m.ensureAccum(key)
	acc.status = status
	return true
}

// reconcileSubagent attaches a child accumulator onto the orchestrator's committed
// Subagent card (design §3). For a Subagent tool-use block it computes the spawn key
// from the orchestrator's OWN coordinates (ev.LoopID/TurnID/StepID) + block.ID and
// looks up subagentAccum: on a hit it copies the agent/task/children/steps/status and
// sets the done summary from the block's paired ToolResultMessage text (truncated),
// then SUPPRESSES the card's normal result body so the hand-back text shows ONLY in the
// done child, not doubled (Task 7 renders the suppression; here Agent being set is the
// marker). On a miss (spawn failed before any child LoopStarted) the card is returned
// unchanged — it renders normally with its error result text. A non-Subagent block is
// returned unchanged.
func (m *transcriptModel) reconcileSubagent(ev event.StepDone, use content.ToolUseBlock, results map[string]*content.ToolResultMessage, card ToolCallView) ToolCallView {
	if use.Name != subagentToolName {
		return card
	}
	key := spawnKey{ev.LoopID, ev.TurnID, ev.StepID, use.ID}
	acc, ok := m.subagentAccum[key]
	if !ok {
		return card // spawn failed before any child loop: render the error result normally
	}
	card.Agent = acc.agent
	card.Task = acc.task
	card.Children = acc.children
	card.Steps = acc.steps
	card.SubStatus = acc.status
	card.Nested = acc.nested
	// The done summary is the hand-back text; suppress the card's own result body so it
	// is not also shown as the normal result preview (design §4: no doubling).
	card.Result = splitLines(subagentTruncate(toolResultText(results[use.ID])))
	return card
}

// subagentToolName is the tool name the orchestrator's StepDone matches to promote a
// tool-use block to a nested Subagent card.
const subagentToolName = "Subagent"

// ensureAccum returns the accumulator for key, creating it (and cloning the map on
// write) when absent so a child event can fill it before the parent card exists. The
// returned pointer is owned by the (possibly freshly cloned) map, so in-place mutation
// of its fields is the intended write path — the MAP clone, not a per-accumulator
// clone, is what upholds the value-copy reducer contract (a child's events are folded
// in document order into the one accumulator the parent later reads).
func (m *transcriptModel) ensureAccum(key spawnKey) *subagentAccumulator {
	if acc, ok := m.subagentAccum[key]; ok {
		return acc
	}
	next := make(map[spawnKey]*subagentAccumulator, len(m.subagentAccum)+1)
	for k, v := range m.subagentAccum {
		next[k] = v
	}
	acc := &subagentAccumulator{}
	next[key] = acc
	m.subagentAccum = next
	return acc
}

// cloneLoopParent returns a fresh copy of the child→spawnKey map (nil-safe), so a
// by-value reducer mutation never writes through a map a prior model still holds — the
// map analogue of the slice value-copy contract used throughout this model.
func cloneLoopParent(p map[uuid.UUID]spawnKey) map[uuid.UUID]spawnKey {
	next := make(map[uuid.UUID]spawnKey, len(p)+1)
	for k, v := range p {
		next[k] = v
	}
	return next
}

// subagentTruncate normalizes a task/summary string to a single transcript line:
// newlines→spaces, trim, then cap at subagentLineCap display runes with an ellipsis
// (truncate). The full text remains in the durable journal. An empty/whitespace string
// yields "".
func subagentTruncate(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	return truncate(s, subagentLineCap)
}

// subagentLineCap is the ~80-col cap for a subagent task/summary line (design §Definitions).
const subagentLineCap = 80

// commitStepAssistant commits the AIMessage's prose / headline as one kindAssistant
// entry. A nil AIMessage commits nothing. It commits the thinking rail (if any) and
// the narration (if any); when there is NO narration it sets a bullet headline only
// for an empty-text step that ran MORE than one tool ("Multiple actions") — a
// single-tool empty-text step commits no umbrella here (its one card is promoted to
// the bullet by stepDone). So a thinking-only message renders just the rail, a
// single-tool empty-text step with no thinking commits nothing here at all, and a
// multi-tool empty-text step gets the "● Multiple actions" umbrella.
func (m *transcriptModel) commitStepAssistant(ai *content.AIMessage, cardCount int) {
	if ai == nil {
		return
	}
	var blocks []content.Block
	if th := thinkingText(ai.Blocks); th != "" {
		blocks = append(blocks, &content.ThinkingBlock{Thinking: th})
	}
	text := textOnly(ai.Blocks)
	if text != "" {
		blocks = append(blocks, &content.TextBlock{Text: text})
	}
	headline := ""
	if text == "" && cardCount > 1 {
		headline = multipleActionsHeadline
	}
	if len(blocks) == 0 && headline == "" {
		return // nothing to show here (a single-tool empty-text step, or a fully empty message)
	}
	m.nextID++
	m.committed = append(m.committed, entry{
		ID:       m.nextID,
		Kind:     kindAssistant,
		Blocks:   blocks,
		headline: headline,
	})
}

// stepToolCard builds the committed ToolCallView for the index-th tool-use block of a
// PRIMARY-loop step. It prefers the resolved live card at the same position (carrying
// the redacted Summary and capped preview already shown live); when there is none it
// falls back to storedStepToolCard (the stored block + matching ToolResultMessage). The
// live-preference branch is correct ONLY for the primary loop's own step — a child's
// nested cards must NOT consult m.live.Calls (it would steal a same-index parent live
// card in a mixed batch), so child reconstruction uses storedStepToolCard exclusively
// (design §3a).
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
	return storedStepToolCard(use, results)
}

// storedStepToolCard builds a ToolCallView PURELY from the stored tool-use block and
// its paired ToolResultMessage (correlated by use.ID), with NO m.live.Calls access. It
// is the durable, position-independent card builder: the shared fallback of
// stepToolCard (the primary loop) AND the EXCLUSIVE builder for a subagent's nested
// children (where a same-index parent live card must never leak in — design §3a). The
// card shows no summary (the redacted summary is not carried in the stored message);
// its ✓/✗ status comes from ToolResultMessage.IsError, which the stored message
// preserves (an error result commits a ✗ card on this path too). It is a free function
// — it depends on nothing but its arguments, which is the property the design relies on.
func storedStepToolCard(use content.ToolUseBlock, results map[string]*content.ToolResultMessage) ToolCallView {
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
// stable ID. The single-element Calls slice carries the terminal ToolCallView so the
// renderer can reuse the existing tool-card rendering. promoted marks the lone card of
// an empty-text single-tool step: it renders AS the assistant bullet ("● <verb >
// ToolName(args)" + result) instead of an indented "⎿ …" card (renderPromotedTool).
func (m *transcriptModel) commitCall(call ToolCallView, promoted bool) {
	m.nextID++
	m.committed = append(m.committed, entry{
		ID:       m.nextID,
		Kind:     kindTool,
		Calls:    []ToolCallView{call},
		promoted: promoted,
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
