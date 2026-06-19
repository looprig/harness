package event_test

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// newID mints a UUID or fails the test.
func newID(t *testing.T) uuid.UUID {
	t.Helper()
	u, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return u
}

// TestToolEventsSatisfyEvent asserts each new tool event satisfies the sealed
// Event interface and round-trips its CallID where specified.
func TestToolEventsSatisfyEvent(t *testing.T) {
	t.Parallel()
	callID := newID(t)
	tests := []struct {
		name       string
		ev         event.Event
		wantCallID uuid.UUID
		getCallID  func(event.Event) uuid.UUID
	}{
		{
			name:       "PermissionRequested",
			ev:         event.PermissionRequested{CallID: callID, Request: tool.BashRequest{Command: "rm -rf /"}},
			wantCallID: callID,
			getCallID:  func(e event.Event) uuid.UUID { return e.(event.PermissionRequested).CallID },
		},
		{
			name:       "UserInputRequested",
			ev:         event.UserInputRequested{CallID: callID, Question: "proceed?", Choices: []string{"yes", "no"}},
			wantCallID: callID,
			getCallID:  func(e event.Event) uuid.UUID { return e.(event.UserInputRequested).CallID },
		},
		{
			name:       "ToolCallStarted",
			ev:         event.ToolCallStarted{CallID: callID, ToolName: "Bash", Summary: "run a command"},
			wantCallID: callID,
			getCallID:  func(e event.Event) uuid.UUID { return e.(event.ToolCallStarted).CallID },
		},
		{
			name:       "ToolCallCompleted",
			ev:         event.ToolCallCompleted{CallID: callID, IsError: true, ResultPreview: "boom"},
			wantCallID: callID,
			getCallID:  func(e event.Event) uuid.UUID { return e.(event.ToolCallCompleted).CallID },
		},
		{
			name:       "PermissionRequested zero CallID is boundary",
			ev:         event.PermissionRequested{Request: tool.UnknownRequest{Tool: "X"}},
			wantCallID: uuid.UUID{},
			getCallID:  func(e event.Event) uuid.UUID { return e.(event.PermissionRequested).CallID },
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.getCallID(tt.ev); got != tt.wantCallID {
				t.Errorf("CallID = %v, want %v", got, tt.wantCallID)
			}
		})
	}
}

// secretMarkers are the sensitive substrings that must NEVER survive a sink
// projection. Each test stuffs a known marker into the field the redaction table
// says to drop, then asserts the marker is absent from the projection's encoding.
const (
	markerDescription = "SECRET_BASH_COMMAND_rm_-rf"
	markerQuestion    = "SECRET_QUESTION_text"
	markerChoice      = "SECRET_CHOICE_value"
	markerResult      = "SECRET_RESULT_preview"
	markerToolArgs    = "SECRET_TOOL_ARG_token"
)

// encode renders an event to a string that includes every reachable field, so a
// test can assert a secret substring is absent. PermissionRequested is special:
// its Request is an interface, so we surface ToolName + Description explicitly.
func encode(t *testing.T, ev event.Event) string {
	t.Helper()
	switch e := ev.(type) {
	case event.PermissionRequested:
		return "callid=" + e.CallID.String() + " tool=" + e.Request.ToolName() + " desc=" + e.Request.Description()
	case event.UserInputRequested:
		return "callid=" + e.CallID.String() + " q=" + e.Question + " choices=" + strings.Join(e.Choices, ",")
	case event.UserInputRequestedSink:
		return "callid=" + e.CallID.String() + " count=" + strconv.Itoa(e.ChoiceCount)
	case event.ToolCallStarted:
		return "callid=" + e.CallID.String() + " tool=" + e.ToolName + " summary=" + e.Summary
	case event.ToolCallCompleted:
		return "callid=" + e.CallID.String() + " err=" + boolStr(e.IsError) + " preview=" + e.ResultPreview
	case event.TokenDelta:
		return "tokendelta " + chunkString(e.Chunk)
	case event.TurnDone:
		return "turndone " + messageString(t, e.Message)
	default:
		b, err := json.Marshal(struct {
			Type string `json:"type"`
		}{Type: "unknown"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func chunkString(c content.Chunk) string {
	switch ch := c.(type) {
	case *content.TextChunk:
		return "text=" + ch.Text
	case *content.ThinkingChunk:
		return "thinking=" + ch.Thinking
	case *content.ToolUseChunk:
		return "tooluse name=" + ch.Name + " input=" + ch.InputJSON
	case nil:
		return "nil"
	default:
		return "other"
	}
}

func messageString(t *testing.T, m *content.AIMessage) string {
	t.Helper()
	if m == nil {
		return "nil"
	}
	var sb strings.Builder
	for _, b := range m.Blocks {
		switch blk := b.(type) {
		case *content.TextBlock:
			sb.WriteString("text=" + blk.Text + " ")
		case *content.ThinkingBlock:
			sb.WriteString("thinking=" + blk.Thinking + " ")
		case *content.ToolUseBlock:
			sb.WriteString("tooluse name=" + blk.Name + " input=" + string(blk.Input) + " ")
		}
	}
	return sb.String()
}

// TestSinkProjectionDropsSecrets is the security core: for every Redactable
// event, the projection drops EXACTLY the sensitive field(s) per the redaction
// table while the original retains them. The marker substring proves absence.
func TestSinkProjectionDropsSecrets(t *testing.T) {
	t.Parallel()
	callID := newID(t)

	tests := []struct {
		name         string
		original     event.Redactable
		secretMarker string // must be present in original, absent from projection
		// keepPresent is a non-secret substring that MUST survive into the projection
		keepPresent string
	}{
		{
			name:         "PermissionRequested drops Description",
			original:     event.PermissionRequested{CallID: callID, Request: tool.BashRequest{Command: markerDescription}},
			secretMarker: markerDescription,
			keepPresent:  "tool=Bash",
		},
		{
			name:         "UserInputRequested drops Question and Choices",
			original:     event.UserInputRequested{CallID: callID, Question: markerQuestion, Choices: []string{markerChoice, "other"}},
			secretMarker: markerQuestion,
			keepPresent:  "count=2",
		},
		{
			name:         "UserInputRequested drops choice strings keeps count",
			original:     event.UserInputRequested{CallID: callID, Question: "q", Choices: []string{markerChoice}},
			secretMarker: markerChoice,
			keepPresent:  "count=1",
		},
		{
			name:         "ToolCallCompleted drops ResultPreview",
			original:     event.ToolCallCompleted{CallID: callID, IsError: true, ResultPreview: markerResult},
			secretMarker: markerResult,
			keepPresent:  "err=true",
		},
		{
			name:         "TokenDelta ToolUseChunk drops InputJSON",
			original:     event.TokenDelta{TurnIndex: 1, Chunk: &content.ToolUseChunk{Index: 0, Name: "Bash", InputJSON: `{"cmd":"` + markerToolArgs + `"}`}},
			secretMarker: markerToolArgs,
			keepPresent:  "name=Bash",
		},
		{
			name: "TurnDone redacts ToolUseBlock.Input",
			original: event.TurnDone{TurnIndex: 1, Message: &content.AIMessage{Message: content.Message{
				Role: content.RoleAssistant,
				Blocks: []content.Block{
					&content.TextBlock{Text: "calling tool"},
					&content.ToolUseBlock{ID: "1", Name: "Bash", Input: json.RawMessage(`{"cmd":"` + markerToolArgs + `"}`)},
				},
			}}},
			secretMarker: markerToolArgs,
			keepPresent:  "text=calling tool",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			origStr := encode(t, tt.original)
			if !strings.Contains(origStr, tt.secretMarker) {
				t.Fatalf("test setup bug: original %q does not contain marker %q", origStr, tt.secretMarker)
			}

			projected := tt.original.SinkProjection()
			projStr := encode(t, projected)

			if strings.Contains(projStr, tt.secretMarker) {
				t.Errorf("SECURITY: projection %q leaks secret marker %q", projStr, tt.secretMarker)
			}
			if tt.keepPresent != "" && !strings.Contains(projStr, tt.keepPresent) {
				t.Errorf("projection %q dropped required non-secret %q", projStr, tt.keepPresent)
			}
		})
	}
}

// TestSinkProjectionPreservesCallID asserts redaction never loses the CallID
// correlation id (the loop also reads it from the original, but the projection
// must keep it so the sink event self-identifies).
func TestSinkProjectionPreservesCallID(t *testing.T) {
	t.Parallel()
	callID := newID(t)
	tests := []struct {
		name      string
		original  event.Redactable
		getCallID func(event.Event) uuid.UUID
	}{
		{
			name:      "PermissionRequested",
			original:  event.PermissionRequested{CallID: callID, Request: tool.BashRequest{Command: "x"}},
			getCallID: func(e event.Event) uuid.UUID { return e.(event.PermissionRequested).CallID },
		},
		{
			name:      "UserInputRequested",
			original:  event.UserInputRequested{CallID: callID, Question: "q", Choices: []string{"a"}},
			getCallID: func(e event.Event) uuid.UUID { return e.(event.UserInputRequestedSink).CallID },
		},
		{
			name:      "ToolCallCompleted",
			original:  event.ToolCallCompleted{CallID: callID, ResultPreview: "x"},
			getCallID: func(e event.Event) uuid.UUID { return e.(event.ToolCallCompleted).CallID },
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.getCallID(tt.original.SinkProjection()); got != callID {
				t.Errorf("projection CallID = %v, want %v", got, callID)
			}
		})
	}
}

// TestNonSecretTokenDeltaUnchanged asserts a TextChunk/ThinkingChunk TokenDelta
// passes through SinkProjection unchanged (model output, not a secret).
func TestNonSecretTokenDeltaUnchanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		chunk content.Chunk
		want  string
	}{
		{"text chunk preserved", &content.TextChunk{Text: "hello"}, "text=hello"},
		{"thinking chunk preserved", &content.ThinkingChunk{Thinking: "reasoning"}, "thinking=reasoning"},
		{"nil chunk preserved", nil, "nil"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			td := event.TokenDelta{TurnIndex: 3, Chunk: tt.chunk}
			got := encode(t, td.SinkProjection())
			if !strings.Contains(got, tt.want) {
				t.Errorf("projection %q does not contain %q", got, tt.want)
			}
		})
	}
}

// TestTurnDoneTextThinkingUnchanged asserts TurnDone keeps text/thinking blocks
// verbatim (only ToolUseBlock.Input is redacted) and tolerates a nil message.
func TestTurnDoneTextThinkingUnchanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		msg  *content.AIMessage
		want string // substring that must survive
	}{
		{
			name: "text and thinking preserved",
			msg: &content.AIMessage{Message: content.Message{Blocks: []content.Block{
				&content.TextBlock{Text: "answer"},
				&content.ThinkingBlock{Thinking: "thought"},
			}}},
			want: "text=answer",
		},
		{
			name: "nil message tolerated",
			msg:  nil,
			want: "nil",
		},
		{
			name: "empty blocks tolerated",
			msg:  &content.AIMessage{},
			want: "turndone",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			td := event.TurnDone{TurnIndex: 1, Message: tt.msg}
			got := encode(t, td.SinkProjection())
			if !strings.Contains(got, tt.want) {
				t.Errorf("projection %q does not contain %q", got, tt.want)
			}
		})
	}
}

// TestRedactableImplementations pins WHICH events implement Redactable. Events
// without sensitive payload MUST NOT (the loop only projects those that do). The
// two lists must between them enumerate EVERY known event: the final check fails
// if a new event lands in neither list, forcing its author to make a deliberate,
// recorded sink-safety decision (CLAUDE.md: log events, not secrets).
func TestRedactableImplementations(t *testing.T) {
	t.Parallel()
	shouldRedact := []event.Event{
		event.PermissionRequested{Request: tool.UnknownRequest{}},
		event.UserInputRequested{},
		event.ToolCallCompleted{},
		event.TokenDelta{},
		event.TurnDone{},
	}
	for _, e := range shouldRedact {
		if _, ok := e.(event.Redactable); !ok {
			t.Errorf("%T must implement Redactable", e)
		}
	}
	// shouldNotRedact enumerates the full set of events the loop forwards to sinks
	// VERBATIM. Each is sink-safe BY CONSTRUCTION today:
	//   - ToolCallStarted: Summary is pre-redacted at construction.
	//   - UserInputRequestedSink: the already-redacted sink shape (no Q/choices).
	//   - TurnStarted/SessionStarted/TurnInterrupted: carry only indices/IDs.
	//   - TurnFailed: Err is a typed, secret-free cause this package constructs;
	//     wrapping a provider/LLM error string into it later MUST redact (see the
	//     SECURITY note on TurnFailed in turn.go).
	//   - LoopIdle/SessionActive/SessionIdle/SessionStopped: carry only Header ids.
	//   - StepDone/TurnFoldedInto/InputCancelled carry user/assistant message
	//     content. The loop-machine spec ACCEPTS that leakage risk for now and
	//     defers their redaction to the journal/redaction follow-on
	//     (TODO(Open Items B)); they MUST NOT implement Redactable here, so a new
	//     projection is not silently added before the follow-on owns it.
	//   - InputQueued/TurnRejected carry only an InputID (and TurnRejected a typed
	//     RejectReason enum) — no message content or secret payload — so they are
	//     sink-safe verbatim.
	shouldNotRedact := []event.Event{
		event.ToolCallStarted{},
		event.UserInputRequestedSink{},
		event.TurnStarted{},
		event.SessionStarted{},
		event.TurnInterrupted{},
		event.TurnFailed{},
		event.LoopIdle{},
		event.SessionActive{},
		event.SessionIdle{},
		event.SessionStopped{},
		event.StepDone{},
		event.TurnFoldedInto{},
		event.InputCancelled{},
		event.InputQueued{},
		event.TurnRejected{},
	}
	for _, e := range shouldNotRedact {
		if _, ok := e.(event.Redactable); ok {
			t.Errorf("%T must NOT implement Redactable (no sensitive payload)", e)
		}
	}

	// allKnownEvents is the complete sealed event set. A new event MUST be added
	// to exactly one classification list above; this assertion fails if any known
	// event is unclassified, so sink-safety is an enumerated, deliberate decision.
	allKnownEvents := []event.Event{
		event.PermissionRequested{Request: tool.UnknownRequest{}},
		event.UserInputRequested{},
		event.UserInputRequestedSink{},
		event.ToolCallStarted{},
		event.ToolCallCompleted{},
		event.TokenDelta{},
		event.TurnStarted{},
		event.StepDone{},
		event.TurnFoldedInto{},
		event.InputCancelled{},
		event.InputQueued{},
		event.TurnRejected{},
		event.TurnDone{},
		event.TurnFailed{},
		event.TurnInterrupted{},
		event.SessionStarted{},
		event.SessionActive{},
		event.SessionIdle{},
		event.SessionStopped{},
		event.LoopIdle{},
	}
	classified := make(map[string]bool, len(shouldRedact)+len(shouldNotRedact))
	for _, e := range shouldRedact {
		classified[typeName(e)] = true
	}
	for _, e := range shouldNotRedact {
		classified[typeName(e)] = true
	}
	for _, e := range allKnownEvents {
		if !classified[typeName(e)] {
			t.Errorf("%T is unclassified: add it to shouldRedact or shouldNotRedact in TestRedactableImplementations (and decide its sink-safety)", e)
		}
	}
}

// typeName returns the concrete type name of an event for set membership.
func typeName(e event.Event) string { return fmt.Sprintf("%T", e) }

// TestTurnDoneProjectionDoesNotMutateOriginal asserts the projection is a copy:
// redacting ToolUseBlock.Input must not corrupt the in-memory message the stream
// and history still reference.
func TestTurnDoneProjectionDoesNotMutateOriginal(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"cmd":"secret"}`)
	original := event.TurnDone{TurnIndex: 1, Message: &content.AIMessage{Message: content.Message{
		Blocks: []content.Block{&content.ToolUseBlock{ID: "1", Name: "Bash", Input: raw}},
	}}}
	_ = original.SinkProjection()
	blk := original.Message.Blocks[0].(*content.ToolUseBlock)
	if string(blk.Input) != `{"cmd":"secret"}` {
		t.Errorf("SinkProjection mutated original ToolUseBlock.Input = %q", string(blk.Input))
	}
}
