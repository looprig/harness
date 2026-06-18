package event

import (
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// Redactable is implemented by events whose full payload is safe for the
// per-turn stream (the TUI) but NOT for the EventSink path (observability/audit
// logs). The loop calls SinkProjection BEFORE enveloping for sinks ONLY; the
// stream always receives the un-projected event. The projection must drop every
// field that could carry a secret, file content, URL/header, a user question, a
// choice string, a result preview, or raw tool arguments (CLAUDE.md: log events,
// not secrets). Events with no sensitive payload do not implement this interface,
// so the loop forwards them to sinks unchanged.
type Redactable interface {
	Event
	// SinkProjection returns a redacted copy safe to hand an EventSink. It must
	// not mutate the receiver (the stream/history still reference the original).
	SinkProjection() Event
}

// PermissionRequested is emitted when a tool call needs interactive approval.
// The STREAM (TUI) renders Request.Description() — which can hold a Bash command,
// a file-diff preview, or a URL — so the stream gets the full Request. The SINK
// must never see that text; SinkProjection drops it (see below).
type PermissionRequested struct {
	enduring
	loopScoped
	Header
	CallID  uuid.UUID
	Request tool.PermissionRequest
}

// UserInputRequested is emitted when a tool (AskUser) needs free-form input. The
// STREAM gets the full Question and Choices for rendering; the SINK gets only the
// CallID and the count of choices (the question and choice text may be sensitive).
type UserInputRequested struct {
	enduring
	loopScoped
	Header
	CallID   uuid.UUID
	Question string
	Choices  []string
}

// UserInputRequestedSink is the redacted sink projection of UserInputRequested.
// It is the only shape an EventSink ever sees for a user-input request: the
// question text and every choice string are dropped, leaving only the CallID and
// ChoiceCount (a non-sensitive shape signal for audit). It is a distinct type so
// the absence of Question/Choices fields is enforced by the compiler, not by a
// nulled field that could be repopulated by mistake.
type UserInputRequestedSink struct {
	enduring
	loopScoped
	Header
	CallID      uuid.UUID
	ChoiceCount int
}

// ToolCallStarted is emitted when an approved tool begins executing. Summary is
// already redacted/capped at construction (never raw args), so it is safe for
// both audiences — this event does NOT implement Redactable.
type ToolCallStarted struct {
	enduring
	loopScoped
	Header
	CallID   uuid.UUID
	ToolName string
	Summary  string
}

// ToolCallCompleted is emitted when a tool finishes. ResultPreview is the capped
// tool output for the TUI and is STREAM-ONLY: tool output may hold secrets/PII,
// so SinkProjection drops it, keeping only CallID and IsError.
type ToolCallCompleted struct {
	enduring
	loopScoped
	Header
	CallID        uuid.UUID
	IsError       bool
	ResultPreview string
}

func (PermissionRequested) isEvent()    {}
func (UserInputRequested) isEvent()     {}
func (UserInputRequestedSink) isEvent() {}
func (ToolCallStarted) isEvent()        {}
func (ToolCallCompleted) isEvent()      {}

// SinkProjection drops Request.Description() — the load-bearing secret — keeping
// only the CallID and the tool name. The projected Request is an UnknownRequest
// carrying just the tool name (its Description() returns the empty Summary), so
// no descriptive text can reach a sink even through the interface. The Header is
// carried through unchanged.
func (e PermissionRequested) SinkProjection() Event {
	toolName := ""
	if e.Request != nil {
		toolName = e.Request.ToolName()
	}
	return PermissionRequested{
		Header:  e.Header,
		CallID:  e.CallID,
		Request: tool.UnknownRequest{Tool: toolName},
	}
}

// SinkProjection drops the Question text and every Choice string, keeping the
// CallID and the choice count as the non-sensitive UserInputRequestedSink shape.
// The Header is carried through unchanged.
func (e UserInputRequested) SinkProjection() Event {
	return UserInputRequestedSink{
		Header:      e.Header,
		CallID:      e.CallID,
		ChoiceCount: len(e.Choices),
	}
}

// SinkProjection drops ResultPreview (tool output may hold secrets/PII), keeping
// the CallID and the IsError flag for audit. The Header is carried through
// unchanged.
func (e ToolCallCompleted) SinkProjection() Event {
	return ToolCallCompleted{
		Header:  e.Header,
		CallID:  e.CallID,
		IsError: e.IsError,
	}
}
