// internal/llm/openaiapi/encode.go
package openaiapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
)

// BuildChatRequest converts a provider-neutral llm.Request into a ChatRequest
// struct. Exported so provider packages can embed or extend the result before
// marshaling (e.g. chutes adds e2e_response_pk as a typed field).
func BuildChatRequest(req llm.Request, stream bool) (ChatRequest, error) {
	cr := ChatRequest{
		Model:           req.Model.Model,
		Temperature:     req.Model.Temperature,
		TopP:            req.Model.TopP,
		MaxTokens:       req.Model.MaxTokens,
		Stop:            req.Model.Stop,
		Stream:          stream,
		ReasoningEffort: string(req.Model.ReasoningEffort),
	}

	if req.Model.System != "" {
		cr.Messages = append(cr.Messages, chatMessage{
			Role:    "system",
			Content: req.Model.System,
		})
	}

	for _, conv := range req.Messages {
		msgs, err := encodeConversation(conv)
		if err != nil {
			return ChatRequest{}, err
		}
		cr.Messages = append(cr.Messages, msgs...)
	}

	for _, t := range req.Tools {
		cr.Tools = append(cr.Tools, chatTool{
			Type: "function",
			Function: chatFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Schema,
			},
		})
	}

	return cr, nil
}

// EncodeRequest converts a provider-neutral llm.Request to an OpenAI chat
// completions JSON body. stream=true adds "stream":true to the body.
// ModelSpec.System is prepended as a system message if non-empty.
func EncodeRequest(req llm.Request, stream bool) ([]byte, error) {
	cr, err := BuildChatRequest(req, stream)
	if err != nil {
		return nil, err
	}
	return json.Marshal(cr)
}

// encodeConversation dispatches a content.Conversation to the appropriate
// chatMessage encoder.
func encodeConversation(conv content.Conversation) ([]chatMessage, error) {
	switch m := conv.(type) {
	case *content.SystemMessage:
		return []chatMessage{{
			Role:    "system",
			Content: textContent(m.Blocks),
		}}, nil

	case *content.UserMessage:
		return []chatMessage{{
			Role:    "user",
			Content: encodeContentParts(m.Blocks),
		}}, nil

	case *content.AIMessage:
		msg, err := encodeAIMessage(m)
		if err != nil {
			return nil, err
		}
		return []chatMessage{msg}, nil

	case *content.ToolMessage:
		return []chatMessage{{
			Role:       "tool",
			Content:    textContent(m.Blocks),
			ToolCallID: m.ToolUseID,
		}}, nil

	default:
		return nil, fmt.Errorf("openaiapi: unknown conversation type %T", conv)
	}
}

// textContent concatenates all text blocks into a single string.
func textContent(blocks []content.Block) string {
	var out string
	for _, b := range blocks {
		if t, ok := b.(*content.TextBlock); ok {
			out += t.Text
		}
	}
	return out
}

// encodeContentParts returns a plain string when all blocks are text,
// or a []chatContentPart slice when non-text blocks are present.
func encodeContentParts(blocks []content.Block) interface{} {
	allText := true
	for _, b := range blocks {
		if _, ok := b.(*content.TextBlock); !ok {
			allText = false
			break
		}
	}
	if allText {
		return textContent(blocks)
	}

	parts := make([]chatContentPart, 0, len(blocks))
	for _, b := range blocks {
		switch b := b.(type) {
		case *content.TextBlock:
			parts = append(parts, chatContentPart{Type: "text", Text: b.Text})
		case *content.ImageBlock:
			parts = append(parts, chatContentPart{Type: "image_url", ImageURL: &imageURLPart{URL: imageURL(b)}})
		}
	}
	return parts
}

// imageURL builds the URL string for an ImageBlock. URL takes precedence over
// Data. If Data is set, a data URI is returned.
func imageURL(img *content.ImageBlock) string {
	if img.Source.URL != "" {
		return img.Source.URL
	}
	encoded := base64.StdEncoding.EncodeToString(img.Source.Data)
	return "data:" + string(img.MediaType) + ";base64," + encoded
}

// encodeAIMessage builds a chatMessage from an AIMessage, handling text,
// tool calls, and ignoring ThinkingBlock.
func encodeAIMessage(m *content.AIMessage) (chatMessage, error) {
	var text string
	var calls []toolCall

	for _, b := range m.Blocks {
		switch b := b.(type) {
		case *content.TextBlock:
			text += b.Text
		case *content.ToolUseBlock:
			calls = append(calls, toolCall{
				ID:       b.ID,
				Type:     "function",
				Function: toolCallFunction{Name: b.Name, Arguments: b.Input},
			})
		case *content.ThinkingBlock:
			// Deliberately ignored: thinking is not part of the OpenAI wire format.
		}
	}

	return chatMessage{
		Role:      "assistant",
		Content:   text,
		ToolCalls: calls,
	}, nil
}
