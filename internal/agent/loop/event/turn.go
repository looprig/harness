package event

import (
	"encoding/json"

	"github.com/inventivepotter/urvi/internal/content"
)

// TurnStarted is the first event written to StartTurn.Events.
type TurnStarted struct{ TurnIndex TurnIndex }

// TokenDelta is emitted for each streaming chunk from the LLM.
type TokenDelta struct {
	TurnIndex TurnIndex
	Chunk     content.Chunk
}

// TurnDone is the terminal success event. Message is the complete AI response.
type TurnDone struct {
	TurnIndex TurnIndex
	Message   *content.AIMessage
}

// TurnFailed is the terminal event for non-cancellation LLM/provider errors.
// On failure the user message is rolled back from history. Err carries the
// typed cause; callers may errors.As it to inspect and retry.
//
// SECURITY: TurnFailed is intentionally NOT Redactable, so Err is forwarded to
// sinks UN-redacted — an audit log legitimately needs the failure cause. This is
// safe ONLY while every cause is a typed, secret-free error constructed in this
// package (EmptyResponseError, TurnPanicError). A future change that wraps a
// provider/LLM error string into TurnFailed.Err MUST redact it (or make
// TurnFailed implement Redactable), because such strings can carry request
// bodies, headers, or tokens.
type TurnFailed struct {
	TurnIndex TurnIndex
	Err       error
}

// TurnInterrupted is the terminal event when the turn context is cancelled.
// The user message for the cancelled turn is rolled back from history.
type TurnInterrupted struct{ TurnIndex TurnIndex }

func (TurnStarted) isEvent()     {}
func (TokenDelta) isEvent()      {}
func (TurnDone) isEvent()        {}
func (TurnFailed) isEvent()      {}
func (TurnInterrupted) isEvent() {}

// SinkProjection redacts a tool-call delta before it reaches a sink. A
// *content.ToolUseChunk carries partial argument JSON (InputJSON) — the same
// secret ToolCallStarted.Summary redacts — so the projection keeps Index/ID/Name
// and drops InputJSON. A TextChunk/ThinkingChunk TokenDelta is model output, not
// a secret, so it is returned unchanged.
func (e TokenDelta) SinkProjection() Event {
	tu, ok := e.Chunk.(*content.ToolUseChunk)
	if !ok {
		return e
	}
	return TokenDelta{
		TurnIndex: e.TurnIndex,
		Chunk: &content.ToolUseChunk{
			Index: tu.Index,
			ID:    tu.ID,
			Name:  tu.Name,
			// InputJSON deliberately dropped: raw tool arguments never reach a sink.
		},
	}
}

// SinkProjection redacts a completed message before it reaches a sink. Every
// ToolUseBlock.Input is replaced with `{}` so raw tool arguments (a WriteFile
// body, a Bash command with an inline token, Fetch headers) never log. Text and
// thinking blocks are model output and pass through unchanged. The projection is
// a deep-enough copy that the original message — still referenced by the stream
// and conversation history — is never mutated.
func (e TurnDone) SinkProjection() Event {
	if e.Message == nil {
		return e
	}
	blocks := make([]content.Block, len(e.Message.Blocks))
	for i, b := range e.Message.Blocks {
		if tu, ok := b.(*content.ToolUseBlock); ok {
			blocks[i] = &content.ToolUseBlock{
				ID:   tu.ID,
				Name: tu.Name,
				// Fresh allocation per block: no two projected events may share a
				// backing array a sink could mutate. An empty JSON object carries
				// no argument bytes.
				Input: json.RawMessage(`{}`),
			}
			continue
		}
		blocks[i] = b
	}
	return TurnDone{
		TurnIndex: e.TurnIndex,
		Message: &content.AIMessage{Message: content.Message{
			Role:   e.Message.Role,
			Blocks: blocks,
		}},
	}
}
