package foreignloop

import (
	"log"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/core/uuid"
)

// mapper is the turn-scoped, pure translation from the normalized ForeignEvent
// union to looprig event.Events. It owns exactly one piece of correlation state: a
// per-turn map from a foreign tool_use id to the ToolExecutionID minted for it, so
// a tool_use and its later tool_result carry the same id on ToolCallStarted /
// ToolCallCompleted. The mapper sets only event BODY fields; Header coordinates
// (SessionID/LoopID/TurnID/StepID) are stamped later by the actor's publish
// chokepoint, mirroring the native loop's stampLoopHeader.
type mapper struct {
	turnIndex event.TurnIndex
	idGen     func() (uuid.UUID, error)
	toolUse   map[string]uuid.UUID // foreign tool_use id -> ToolExecutionID
}

// newMapper builds a mapper for a single foreign turn. idGen mints ToolExecutionIDs
// (injected so tests stay deterministic and the mapper depends on no global rand).
func newMapper(turnIndex event.TurnIndex, idGen func() (uuid.UUID, error)) *mapper {
	return &mapper{turnIndex: turnIndex, idGen: idGen, toolUse: make(map[string]uuid.UUID)}
}

// toEvents maps one ForeignEvent to zero or more looprig events. A nil, nil result
// is a deliberate soft-skip (no event to emit); a non-nil error is fail-secure (the
// actor logs and drops the turn rather than emitting an uncorrelated event).
func (m *mapper) toEvents(fe ForeignEvent) ([]event.Event, error) {
	switch fe.Kind {
	case ForeignTextDelta:
		return one(event.TokenDelta{TurnIndex: m.turnIndex, Chunk: &content.TextChunk{Text: fe.Text}}), nil
	case ForeignThinkingDelta:
		return one(event.TokenDelta{TurnIndex: m.turnIndex, Chunk: &content.ThinkingChunk{Thinking: fe.Text}}), nil
	case ForeignToolUse:
		return m.toolStarted(fe)
	case ForeignToolResult:
		return m.toolCompleted(fe), nil
	case ForeignStepComplete:
		return m.stepDone(fe), nil
	case ForeignTerminalOK:
		return one(event.TurnDone{TurnIndex: m.turnIndex, Message: fe.Message}), nil
	case ForeignTerminalError:
		return one(event.TurnFailed{TurnIndex: m.turnIndex, Err: &ForeignResultError{Detail: fe.ErrText}}), nil
	case ForeignInit:
		// No event: the actor confirms the minted sid matches fe.SessionID and logs
		// on mismatch — identity reconciliation is not the mapper's job.
		return nil, nil
	default:
		return nil, nil // defensive: an unknown Kind maps to nothing.
	}
}

// toolStarted mints a ToolExecutionID, records the correlation, and emits the
// started event. On idGen failure it returns the error verbatim (fail-secure):
// the mapper never fabricates an id, so the tool_result later soft-skips as orphan.
func (m *mapper) toolStarted(fe ForeignEvent) ([]event.Event, error) {
	id, err := m.idGen()
	if err != nil {
		return nil, err
	}
	m.toolUse[fe.ToolUseID] = id
	return one(event.ToolCallStarted{ToolExecutionID: id, ToolName: fe.ToolName}), nil
}

// toolCompleted correlates the result to its tool_use via the per-turn map. An
// unknown tool_use id cannot be correlated, so it is soft-skipped (never fabricate
// an id); the orphan is logged with the id quoted/escaped via %q.
func (m *mapper) toolCompleted(fe ForeignEvent) []event.Event {
	id, ok := m.toolUse[fe.ToolUseID]
	if !ok {
		log.Printf("foreignloop: orphan tool_result for tool_use id %q; soft-skipping", fe.ToolUseID)
		return nil
	}
	return one(event.ToolCallCompleted{ToolExecutionID: id, IsError: fe.IsError, ResultPreview: fe.ResultPreview})
}

// stepDone maps a completed assistant round to a live StepDone. The ACTOR decides
// whether to publish this live StepDone or supersede it with the authoritative
// transcript-derived StepDone (design doc lines 424–429); the mapper just maps. A
// nil Message means there is nothing authoritative to commit, so it soft-skips.
func (m *mapper) stepDone(fe ForeignEvent) []event.Event {
	if fe.Message == nil {
		return nil
	}
	return one(event.StepDone{Messages: content.AgenticMessages{fe.Message}})
}

// one wraps a single event in the slice the mapper API returns.
func one(e event.Event) []event.Event { return []event.Event{e} }
