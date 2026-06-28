package transcript

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"time"

	"github.com/ciram-co/looprig/pkg/command"
	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// Reconstruct folds a journal record stream into a Session model. It reads from
// src until io.EOF, folding each Record into the growing tree; a non-EOF read
// failure aborts with a *ReconstructError (Stage "read"). Reconstruction is
// otherwise best-effort: malformed or unpaired records degrade to Warnings, never
// to an error. prompts resolves live system-prompt text (wired in a later task).
func Reconstruct(ctx context.Context, src RecordSource, prompts SystemPromptResolver) (*Session, []Warning, error) {
	b := newBuilder(prompts)
	for {
		rec, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, &ReconstructError{Stage: stageRead, Cause: err}
		}
		b.fold(rec)
	}
	b.finalize()
	return b.session, b.session.Warnings, nil
}

// builder accumulates the Session as records are folded in. It indexes the loop
// tree, the open turn per loop, and tool calls so later records (results, gates,
// child loops) can correlate back to the call that owns them.
type builder struct {
	prompts SystemPromptResolver
	session *Session
	// loops indexes every Loop by its LoopID so turn/step records attach to the
	// loop that produced them (Header.Coordinates.LoopID).
	loops map[uuid.UUID]*Loop
	// openTurns holds the currently-open (unterminated) Turn per loop, so a StepDone
	// or terminal turn event resolves to the right turn.
	openTurns map[uuid.UUID]*Turn
	// toolByUseID indexes every ToolCall by its provider tool-use id, the durable
	// key that pairs a tool result (and, in a later task, a child loop) back to its
	// call.
	toolByUseID map[string]*ToolCall
	// gatesByExecID indexes the open (not-yet-flushed) gates by their tool-execution
	// id, so a resolving user command can find the gate it decides. On replay a gate
	// and its command land BEFORE the StepDone that carries the tool, so gates cannot
	// bind on arrival; they are buffered and flushed at StepDone (see flushGates). It
	// is GLOBAL across loops because a tool-execution id is unique session-wide; each
	// loop's flush forgets only its own flushed gates (see flushGates / forgetGates),
	// never another loop's still-open ones.
	gatesByExecID map[uuid.UUID]*GateAction
	// stepGateBuf accumulates each loop's pending gates in arrival order, keyed by the
	// gate event's Header.LoopID; flushGates moves one loop's slice onto its Step.Gates
	// and deletes that key at the loop's StepDone. Keying by loop is what keeps a
	// parent-loop gate from flushing onto an interleaved child loop's step (and vice
	// versa) once subagent loops nest.
	stepGateBuf map[uuid.UUID][]*GateAction
	// childByParent buffers each spawned subagent loop until the parent StepDone that
	// carries its Subagent tool-use arrives. A child loop's entire lifecycle is
	// journaled BEFORE that parent StepDone, so the parent ToolCall does not exist when
	// the child LoopStarted lands; the child is buffered under {parent loop id, spawning
	// tool-use id} and grafted onto ToolCall.Child at the matching StepDone. Keying by
	// the parent loop id (the child's LoopStarted.Cause.LoopID) keeps tool-use ids that
	// repeat across loops from cross-attaching.
	childByParent map[childKey]*Loop
}

// childKey identifies a buffered child loop by its parent loop and the tool-use id
// of the Subagent call that spawned it. The parent loop id comes from the child
// LoopStarted's Cause.LoopID; pairing it with the tool-use id keeps a tool-use id
// reused in different loops from colliding.
type childKey struct {
	parentLoopID uuid.UUID
	toolUseID    string
}

func newBuilder(prompts SystemPromptResolver) *builder {
	return &builder{
		prompts:       prompts,
		session:       &Session{},
		loops:         make(map[uuid.UUID]*Loop),
		openTurns:     make(map[uuid.UUID]*Turn),
		toolByUseID:   make(map[string]*ToolCall),
		gatesByExecID: make(map[uuid.UUID]*GateAction),
		stepGateBuf:   make(map[uuid.UUID][]*GateAction),
		childByParent: make(map[childKey]*Loop),
	}
}

// fold dispatches one record to the event or command handler and advances the
// session's EndedAt to the record's creation time (the snapshot edge is the last
// record seen).
func (b *builder) fold(rec Record) {
	b.session.EndedAt = recordTime(rec)
	switch r := rec.(type) {
	case EventRecord:
		b.foldEvent(r.Event)
	case CommandRecord:
		b.foldCommand(r.Command)
	}
}

// recordTime returns the creation timestamp of a record, from whichever header it
// carries.
func recordTime(rec Record) time.Time {
	switch r := rec.(type) {
	case EventRecord:
		return r.Event.EventHeader().CreatedAt
	case CommandRecord:
		return r.Command.CommandHeader().CreatedAt
	default:
		return time.Time{}
	}
}

// foldEvent dispatches an enduring loop event to its handler. Unhandled event
// types no-op for now; a later task turns the default into a Warning and adds the
// gate, nesting, outcome, and notice cases.
func (b *builder) foldEvent(ev event.Event) {
	switch e := ev.(type) {
	case event.SessionStarted:
		b.onSessionStarted(e)
	case event.LoopStarted:
		b.onLoopStarted(e)
	case event.TurnStarted:
		b.onTurnStarted(e)
	case event.StepDone:
		b.onStepDone(e)
	case event.TurnDone:
		b.onTurnDone(e)
	case event.PermissionRequested:
		b.onPermissionRequested(e)
	case event.UserInputRequested:
		b.onUserInputRequested(e)
	default:
		// Non-Done outcomes and notices land in a later task as additional cases (and
		// turn the default into a Warning).
	}
}

// foldCommand resolves a buffered gate from the user command that decided it. The
// gate-resolving commands are value types (matching the journal codec, which decodes
// commands by value), so the switch is over value cases. A command that targets no
// open gate is an anomaly at the untrusted-record boundary: it degrades to a Warning,
// never a panic (fail-secure).
//
// We deliberately do NOT check Header.Agency == identity.AgencyUser here: this is a
// read-only reconstruction of an already-journaled session, and only a user action
// ever resolves a gate, so the recorded command is trusted as-is. (Agency is enforced
// at the live command boundary, not at replay.)
func (b *builder) foldCommand(cmd command.Command) {
	switch c := cmd.(type) {
	case command.ApproveToolCall:
		if g := b.resolveGate(c.GateRoute.ToolExecutionID, DecisionApproved, c.Header.CreatedAt); g != nil {
			g.Scope = c.Scope
		}
	case command.DenyToolCall:
		b.resolveGate(c.GateRoute.ToolExecutionID, DecisionDenied, c.Header.CreatedAt)
	case command.ProvideUserInput:
		if g := b.resolveGate(c.GateRoute.ToolExecutionID, DecisionAnswered, c.Header.CreatedAt); g != nil {
			g.Answer = c.Answer
		}
	}
}

// onSessionStarted seeds the session identity, config fingerprint, and start time.
func (b *builder) onSessionStarted(e event.SessionStarted) {
	b.session.SessionID = e.Header.SessionID
	b.session.StartedAt = e.Header.CreatedAt
	b.session.Config = Config{
		ModelID:           e.Config.ModelID,
		AgentKind:         e.Config.AgentKind,
		PermissionPosture: e.Config.PermissionPosture,
		SystemPromptRev:   e.Config.SystemPromptRev,
	}
}

// onLoopStarted registers a new loop. The primary (no parent tool-use id) becomes
// the session Root; a spawned subagent loop is buffered under {parent loop id,
// spawning tool-use id} for attach at the parent StepDone (the parent ToolCall does
// not exist yet — Decision 6). Either way the loop is indexed in b.loops so its later
// turn/step events route by Header.LoopID. SystemPrompt resolution via b.prompts is a
// later task.
func (b *builder) onLoopStarted(e event.LoopStarted) {
	loop := &Loop{
		LoopID:          e.Header.LoopID,
		AgentName:       string(e.Header.AgentName),
		ParentToolUseID: e.ParentToolUseID,
		StartedAt:       e.Header.CreatedAt,
	}
	b.loops[loop.LoopID] = loop
	if e.ParentToolUseID == "" {
		b.session.Root = loop
		return
	}
	key := childKey{parentLoopID: e.Header.Cause.LoopID, toolUseID: e.ParentToolUseID}
	b.childByParent[key] = loop
}

// onTurnStarted opens a turn on the owning loop and records its initial user
// message.
func (b *builder) onTurnStarted(e event.TurnStarted) {
	loop := b.loops[e.Header.LoopID]
	if loop == nil {
		return // Unknown loop; a later task surfaces this as a Warning.
	}
	turn := &Turn{
		Index:     e.TurnIndex,
		StartedAt: e.Header.CreatedAt,
		User:      userMessage(e.Message, e.Header.CreatedAt),
	}
	loop.Turns = append(loop.Turns, turn)
	b.openTurns[loop.LoopID] = turn
}

// onStepDone appends one step to the loop's open turn: the leading AIMessage
// becomes Step.AI and its ToolUseBlocks become ToolCalls, and each trailing
// ToolResultMessage is paired to its call by tool-use id.
func (b *builder) onStepDone(e event.StepDone) {
	loopID := e.Header.LoopID
	turn := b.openTurns[loopID]
	if turn == nil {
		return // No open turn; a later task surfaces this as a Warning.
	}
	step := &Step{StepID: e.Header.StepID}
	for _, msg := range e.Messages {
		switch m := msg.(type) {
		case *content.AIMessage:
			step.AI = aiMessage(m, e.Header.CreatedAt)
			step.Tools = b.toolCalls(m, e.Header.CreatedAt)
		case *content.ToolResultMessage:
			b.pairResult(m)
		}
	}
	b.attachChildren(loopID, step.Tools)
	b.flushGates(loopID, step)
	turn.Steps = append(turn.Steps, step)
}

// attachChildren grafts any buffered subagent loop onto the tool call that spawned
// it: for each tool call in this loop's just-built step, a child buffered under
// {loopID, tool-use id} becomes ToolCall.Child and is removed from the buffer. An
// unattached child stays buffered and is reported by finalize at end-of-stream.
func (b *builder) attachChildren(loopID uuid.UUID, tools []*ToolCall) {
	for _, tc := range tools {
		key := childKey{parentLoopID: loopID, toolUseID: tc.ToolUseID}
		if child, ok := b.childByParent[key]; ok {
			tc.Child = child
			delete(b.childByParent, key)
		}
	}
}

// onTurnDone closes the loop's open turn as successfully completed.
func (b *builder) onTurnDone(e event.TurnDone) {
	turn := b.openTurns[e.Header.LoopID]
	if turn == nil {
		return // No open turn; a later task surfaces this as a Warning.
	}
	turn.Outcome = OutcomeDone
	turn.EndedAt = e.Header.CreatedAt
	delete(b.openTurns, e.Header.LoopID)
}

// onPermissionRequested buffers a pending permission gate for the current step,
// capturing the requesting tool's name and redacted description from the durable
// PermissionRequest. The Request is nil-guarded: a replayed event whose request did
// not round-trip yields a gate with empty name/description, still a valid pending
// notification.
func (b *builder) onPermissionRequested(e event.PermissionRequested) {
	g := &GateAction{Kind: GateKindPermission, Decision: DecisionPending, OpenedAt: e.Header.CreatedAt}
	if e.Request != nil {
		g.ToolName = e.Request.ToolName()
		g.Description = e.Request.Description()
	}
	b.bufferGate(e.Header.LoopID, e.ToolExecutionID, g)
}

// onUserInputRequested buffers a pending ask-user gate for the current step,
// capturing the durable question and choices.
func (b *builder) onUserInputRequested(e event.UserInputRequested) {
	g := &GateAction{
		Kind:     GateKindAskUser,
		Decision: DecisionPending,
		Question: e.Question,
		Choices:  e.Choices,
		OpenedAt: e.Header.CreatedAt,
	}
	b.bufferGate(e.Header.LoopID, e.ToolExecutionID, g)
}

// bufferGate indexes a pending gate by its (session-unique) tool-execution id for the
// resolving command, and appends it to the producing loop's step buffer for the
// StepDone flush. Keying the buffer by loopID keeps an interleaved child loop's gates
// from flushing onto the parent loop's step.
func (b *builder) bufferGate(loopID, execID uuid.UUID, g *GateAction) {
	b.gatesByExecID[execID] = g
	b.stepGateBuf[loopID] = append(b.stepGateBuf[loopID], g)
}

// resolveGate records a user decision on the gate the command targets and returns it
// so the caller can attach scope or answer. An unmatched command targets no open
// gate — fail-secure: it records a Warning and returns nil rather than panicking.
func (b *builder) resolveGate(execID uuid.UUID, d Decision, at time.Time) *GateAction {
	g, ok := b.gatesByExecID[execID]
	if !ok {
		b.warn("gate command targets no open gate (tool-execution id "+execID.String()+")", at)
		return nil
	}
	g.Decision = d
	g.DecidedAt = at
	return g
}

// flushGates moves the given loop's buffered gates onto step.Gates and binds each to
// the tool call it gated. A permission gate binds to the first not-yet-bound tool
// whose Name equals the gate's ToolName, so two same-named gated calls in one step
// bind positionally; an ask-user gate (no ToolName) binds to the lone unbound AskUser
// call if present. Binding sets ToolCall.Gate and GateAction.ToolUseID to the same
// pointer; an unmatched gate stays unbound on step.Gates, still renderable from its
// own durable data. Only the named loop's buffer entry is removed and only its flushed
// gates leave the exec-id index, so an interleaved loop's still-open gates are
// untouched.
//
// (Task 5's turn-terminal-with-pending-gate edge — a buffered gate whose turn ends
// without a StepDone — will plug in by flushing the leftover buffer at the terminal.)
func (b *builder) flushGates(loopID uuid.UUID, step *Step) {
	gates := b.stepGateBuf[loopID]
	step.Gates = gates
	for _, g := range gates {
		if tc := firstUnboundNamed(step.Tools, gateToolName(g)); tc != nil {
			tc.Gate = g
			g.ToolUseID = tc.ToolUseID
		}
	}
	delete(b.stepGateBuf, loopID)
	b.forgetGates(gates)
}

// forgetGates drops the flushed gates from the global exec-id index, so a user command
// that arrives after a gate's step has closed is detected as an anomaly rather than
// re-deciding an already-rendered gate (and so the index does not grow without bound).
// The index is keyed by session-unique tool-execution ids, so deleting by gate pointer
// removes only these gates, never another loop's still-open ones.
func (b *builder) forgetGates(gates []*GateAction) {
	if len(gates) == 0 {
		return
	}
	flushed := make(map[*GateAction]struct{}, len(gates))
	for _, g := range gates {
		flushed[g] = struct{}{}
	}
	for execID, g := range b.gatesByExecID {
		if _, ok := flushed[g]; ok {
			delete(b.gatesByExecID, execID)
		}
	}
}

// finalize runs end-of-stream reconciliation: a subagent loop still buffered means its
// spawning Subagent tool-use never materialised (the parent StepDone never arrived, or
// the journal was truncated mid-prompt), so it is reported as a Warning and left out of
// the tree — a fail-secure degradation at the untrusted-record boundary, never a panic.
//
// The orphans are emitted in a deterministic order — by StartedAt, then by LoopID — so
// the same journal always yields the same Warnings (they feed byte-compared HTML
// goldens and a re-exportable audit artifact); ranging the childByParent map directly
// would randomize the order.
func (b *builder) finalize() {
	orphans := make([]*Loop, 0, len(b.childByParent))
	for key := range b.childByParent {
		orphans = append(orphans, b.childByParent[key])
	}
	sort.Slice(orphans, func(i, j int) bool {
		if !orphans[i].StartedAt.Equal(orphans[j].StartedAt) {
			return orphans[i].StartedAt.Before(orphans[j].StartedAt)
		}
		return bytes.Compare(orphans[i].LoopID[:], orphans[j].LoopID[:]) < 0
	})
	for _, child := range orphans {
		b.warn("subagent loop "+child.LoopID.String()+" references parent tool-use "+child.ParentToolUseID+" that never appeared", child.StartedAt)
	}
}

// askUserToolName is the tool name an ask-user gate binds to. The AskUser tool emits
// no PermissionRequest, so its gate carries no ToolName; binding falls back to the
// tool call's own name.
//
// This MUST stay in lockstep with the unexported askUserToolName in
// pkg/tools/askuser.go (the tool's authoritative Info().Name). transcript is
// pure-data-deps only and cannot import pkg/tools, so this is a deliberate duplicate
// of that constant, not a divergent literal.
const askUserToolName = "AskUser"

// gateToolName returns the tool name a gate binds to: the permission gate's recovered
// ToolName, or the AskUser tool name for an ask-user gate.
func gateToolName(g *GateAction) string {
	if g.Kind == GateKindAskUser {
		return askUserToolName
	}
	return g.ToolName
}

// firstUnboundNamed returns the first ToolCall in tools whose Name == name and whose
// Gate is still nil, or nil if none. Skipping already-bound calls is what makes
// same-named gates bind positionally (each claims a distinct card).
func firstUnboundNamed(tools []*ToolCall, name string) *ToolCall {
	if name == "" {
		return nil
	}
	for _, tc := range tools {
		if tc.Name == name && tc.Gate == nil {
			return tc
		}
	}
	return nil
}

// warn appends a reconstruction anomaly to the session, stamped with the time of the
// record that surfaced it. Reconstruction is best-effort: anomalies become Warnings,
// never errors (the untrusted-record boundary fails secure).
func (b *builder) warn(text string, at time.Time) {
	b.session.Warnings = append(b.session.Warnings, Warning{Text: text, At: at})
}

// toolCalls extracts the AIMessage's ToolUseBlocks into ToolCalls and registers
// each in toolByUseID so a later result, gate, or child loop can correlate to it.
func (b *builder) toolCalls(m *content.AIMessage, at time.Time) []*ToolCall {
	var tools []*ToolCall
	for _, blk := range m.Blocks {
		tu, ok := blk.(*content.ToolUseBlock)
		if !ok {
			continue
		}
		tc := &ToolCall{ToolUseID: tu.ID, Name: tu.Name, Input: tu.Input, At: at}
		b.toolByUseID[tc.ToolUseID] = tc
		tools = append(tools, tc)
	}
	return tools
}

// pairResult attaches a tool result to the call that requested it, matched by
// tool-use id.
func (b *builder) pairResult(m *content.ToolResultMessage) {
	tc, ok := b.toolByUseID[m.ToolUseID]
	if !ok {
		return // Unpaired result; a later task surfaces this as a Warning.
	}
	tc.Result = m.Blocks
	tc.IsError = m.IsError
}

// userMessage projects a UserMessage into the transcript Message view, stamped
// with the given time. It returns nil for a nil message.
func userMessage(m *content.UserMessage, at time.Time) *Message {
	if m == nil {
		return nil
	}
	return &Message{Role: m.Role, Blocks: m.Blocks, At: at}
}

// aiMessage projects an AIMessage into the transcript Message view, stamped with
// the given time. It returns nil for a nil message.
func aiMessage(m *content.AIMessage, at time.Time) *Message {
	if m == nil {
		return nil
	}
	return &Message{Role: m.Role, Blocks: m.Blocks, At: at}
}
