package transcript

import (
	"context"
	"errors"
	"io"
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
	// key that pairs a tool result (and, in a later task, a gate or child loop) back
	// to its call.
	toolByUseID map[string]*ToolCall
	// toolByExecID indexes ToolCalls by tool-execution id for gate correlation; a
	// later task populates it.
	toolByExecID map[uuid.UUID]*ToolCall
}

func newBuilder(prompts SystemPromptResolver) *builder {
	return &builder{
		prompts:      prompts,
		session:      &Session{},
		loops:        make(map[uuid.UUID]*Loop),
		openTurns:    make(map[uuid.UUID]*Turn),
		toolByUseID:  make(map[string]*ToolCall),
		toolByExecID: make(map[uuid.UUID]*ToolCall),
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
	default:
		// Gates, subagent nesting, non-Done outcomes, and notices land in later
		// tasks as additional cases.
	}
}

// foldCommand dispatches a user command. Gate-resolving commands are handled in a
// later task; for now every command no-ops.
func (b *builder) foldCommand(command.Command) {
	// Approve/Deny/ProvideUserInput gate resolution lands in a later task.
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

// onLoopStarted registers a new loop; the primary (no parent tool-use id) becomes
// the session Root. Attaching a spawned loop as a child of its parent tool call is
// a later task. SystemPrompt resolution via b.prompts is also a later task.
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
	}
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
	turn := b.openTurns[e.Header.LoopID]
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
	turn.Steps = append(turn.Steps, step)
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
