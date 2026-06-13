package loop

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
)

// runTurn streams one LLM turn. Returns updated history and the terminal event.
// History only ever advances by complete user/assistant pairs: a successful turn
// appends both messages; any failure or cancellation rolls the user message back
// out. This keeps the thread free of trailing or doubled user messages that
// strict providers (alternating-role APIs) reject on the next turn. The caller
// still holds the original input and the TurnFailed.Err cause, and retries by
// re-invoking with the same input.
func runTurn(
	ctx context.Context,
	input []*content.Block,
	turnIndex TurnIndex,
	msgs content.AgenticMessages,
	cfg Config,
	client llm.LLM,
	emit func(Event),
) (content.AgenticMessages, Event) {
	userMsg := &content.UserMessage{
		Message: content.Message{Role: content.RoleUser, Blocks: input},
	}
	msgs = append(msgs, userMsg)
	emit(TurnStarted{TurnIndex: turnIndex})

	req := llm.Request{Model: cfg.Model, Messages: msgs}
	sr, err := client.Stream(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return msgs[:len(msgs)-1], TurnInterrupted{TurnIndex: turnIndex}
		}
		return msgs[:len(msgs)-1], TurnFailed{TurnIndex: turnIndex, Err: err}
	}
	defer sr.Close()

	var textBuf, thinkBuf strings.Builder
	for {
		chunk, err := sr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return msgs[:len(msgs)-1], TurnInterrupted{TurnIndex: turnIndex}
			}
			return msgs[:len(msgs)-1], TurnFailed{TurnIndex: turnIndex, Err: err}
		}
		emit(TokenDelta{TurnIndex: turnIndex, Chunk: chunk})
		switch chunk.Type {
		case content.ChunkTypeText:
			if chunk.Text != nil {
				textBuf.WriteString(chunk.Text.Text)
			}
		case content.ChunkTypeThinking:
			if chunk.Thinking != nil {
				thinkBuf.WriteString(chunk.Thinking.Thinking)
			}
		}
	}

	var blocks []*content.Block
	if thinkBuf.Len() > 0 {
		blocks = append(blocks, &content.Block{
			Type:     content.TypeThinking,
			Thinking: &content.ThinkingBlock{Thinking: thinkBuf.String()},
		})
	}
	if textBuf.Len() > 0 {
		blocks = append(blocks, &content.Block{
			Type: content.TypeText,
			Text: &content.TextBlock{Text: textBuf.String()},
		})
	}
	if len(blocks) == 0 {
		// Provider sent a successful stream with no content — treat as a failure
		// and roll the user message back out, so callers are left with neither an
		// empty assistant message nor a dangling user message in history.
		return msgs[:len(msgs)-1], TurnFailed{TurnIndex: turnIndex, Err: &EmptyResponseError{}}
	}
	aiMsg := &content.AIMessage{
		Message: content.Message{Role: content.RoleAssistant, Blocks: blocks},
	}
	return append(msgs, aiMsg), TurnDone{TurnIndex: turnIndex, Message: aiMsg}
}
