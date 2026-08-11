package loopruntime

import (
	"fmt"
	"strings"

	"github.com/looprig/core/content"
)

// compactionToolResultRunes is the v1 model-facing cap for one tool result.
// Keep it private so a future projection revision can change the budget without
// accidentally becoming a live-history or event-wire contract.
const compactionToolResultRunes = 2000

const (
	compactionToolResultTruncatedMarker = "[tool result truncated for compaction: omitted %d runes]"
	compactionThinkingPlaceholder       = "[thinking omitted for compaction]"
	compactionImagePlaceholder          = "[image omitted for compaction]"
	compactionAudioPlaceholder          = "[audio omitted for compaction]"
	compactionDocumentPlaceholder       = "[document omitted for compaction]"
)

const compactionProjectionMaxDepth = 128

// projectCompactionTranscript builds a fresh, text-oriented view for a
// summarization request. It never mutates or stores the source conversation;
// callers must keep the returned projection at the compaction boundary only.
func projectCompactionTranscript(messages content.AgenticMessages) (content.AgenticMessages, error) {
	projected := make(content.AgenticMessages, len(messages))
	for index, message := range messages {
		value, err := projectCompactionMessage(message)
		if err != nil {
			return nil, err
		}
		projected[index] = value
	}
	return projected, nil
}

type compactionProjectionError struct{ field string }

func (e *compactionProjectionError) Error() string {
	return "loopruntime: invalid compaction projection " + e.field
}

func projectCompactionMessage(message content.Conversation) (content.Conversation, error) {
	if message == nil {
		return nil, &compactionProjectionError{field: "message"}
	}
	switch typed := message.(type) {
	case *content.UserMessage:
		if typed == nil || typed.Role != content.RoleUser {
			return nil, &compactionProjectionError{field: "message"}
		}
		blocks, err := projectCompactionBlocks(typed.Blocks, 0)
		if err != nil {
			return nil, err
		}
		return &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: blocks}}, nil
	case *content.AIMessage:
		if typed == nil || typed.Role != content.RoleAssistant {
			return nil, &compactionProjectionError{field: "message"}
		}
		blocks, err := projectCompactionBlocks(typed.Blocks, 0)
		if err != nil {
			return nil, err
		}
		var usage *content.Usage
		if typed.Usage != nil {
			cloned := *typed.Usage
			usage = &cloned
		}
		return &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}, Usage: usage}, nil
	case *content.SystemMessage:
		if typed == nil || typed.Role != content.RoleSystem {
			return nil, &compactionProjectionError{field: "message"}
		}
		blocks, err := projectCompactionBlocks(typed.Blocks, 0)
		if err != nil {
			return nil, err
		}
		return &content.SystemMessage{Message: content.Message{Role: content.RoleSystem, Blocks: blocks}}, nil
	case *content.ToolResultMessage:
		if typed == nil || typed.Role != content.RoleTool {
			return nil, &compactionProjectionError{field: "message"}
		}
		if len(typed.Blocks) == 0 {
			var blocks []content.Block
			if typed.Blocks != nil {
				blocks = make([]content.Block, 0)
			}
			return &content.ToolResultMessage{
				Message: content.Message{Role: content.RoleTool, Blocks: blocks}, ToolUseID: typed.ToolUseID, IsError: typed.IsError,
			}, nil
		}
		body, err := projectCompactionToolResultBody(typed.Blocks, 0)
		if err != nil {
			return nil, err
		}
		return &content.ToolResultMessage{
			Message:   content.Message{Role: content.RoleTool, Blocks: []content.Block{&content.TextBlock{Text: capCompactionToolResult(body)}}},
			ToolUseID: typed.ToolUseID,
			IsError:   typed.IsError,
		}, nil
	default:
		return nil, &compactionProjectionError{field: "message"}
	}
}

func projectCompactionBlocks(blocks []content.Block, depth int) ([]content.Block, error) {
	if depth > compactionProjectionMaxDepth {
		return nil, &compactionProjectionError{field: "depth"}
	}
	if blocks == nil {
		return nil, nil
	}
	projected := make([]content.Block, len(blocks))
	for index, block := range blocks {
		text, err := projectCompactionBlock(block, depth)
		if err != nil {
			return nil, err
		}
		projected[index] = &content.TextBlock{Text: text}
	}
	return projected, nil
}

func projectCompactionToolResultBody(blocks []content.Block, depth int) (string, error) {
	if depth > compactionProjectionMaxDepth {
		return "", &compactionProjectionError{field: "depth"}
	}
	var body strings.Builder
	for _, block := range blocks {
		text, err := projectCompactionBlock(block, depth)
		if err != nil {
			return "", err
		}
		body.WriteString(text)
	}
	return body.String(), nil
}

func projectCompactionBlock(block content.Block, depth int) (string, error) {
	if depth > compactionProjectionMaxDepth || block == nil {
		return "", &compactionProjectionError{field: "block"}
	}
	switch typed := block.(type) {
	case *content.TextBlock:
		if typed == nil {
			return "", &compactionProjectionError{field: "block"}
		}
		return typed.Text, nil
	case *content.ToolUseBlock:
		if typed == nil {
			return "", &compactionProjectionError{field: "block"}
		}
		return "[called tool: " + typed.Name + "]", nil
	case *content.ThinkingBlock:
		if typed == nil {
			return "", &compactionProjectionError{field: "block"}
		}
		return compactionThinkingPlaceholder, nil
	case *content.ImageBlock:
		if typed == nil {
			return "", &compactionProjectionError{field: "block"}
		}
		return compactionImagePlaceholder, nil
	case *content.AudioBlock:
		if typed == nil {
			return "", &compactionProjectionError{field: "block"}
		}
		return compactionAudioPlaceholder, nil
	case *content.DocumentBlock:
		if typed == nil {
			return "", &compactionProjectionError{field: "block"}
		}
		return compactionDocumentPlaceholder, nil
	case *content.ToolResultBlock:
		if typed == nil {
			return "", &compactionProjectionError{field: "block"}
		}
		body, err := projectCompactionToolResultBody(typed.Content, depth+1)
		if err != nil {
			return "", err
		}
		return capCompactionToolResult(body), nil
	default:
		return "", &compactionProjectionError{field: "block"}
	}
}

func capCompactionToolResult(value string) string {
	runes := []rune(value)
	if len(runes) <= compactionToolResultRunes {
		return value
	}
	marker := fmt.Sprintf(compactionToolResultTruncatedMarker, len(runes))
	for iteration := 0; iteration < 8; iteration++ {
		available := compactionToolResultRunes - len([]rune(marker))
		if available <= 0 {
			return string([]rune(marker)[:compactionToolResultRunes])
		}
		headCount := available / 2
		tailCount := available - headCount
		omitted := len(runes) - headCount - tailCount
		updated := fmt.Sprintf(compactionToolResultTruncatedMarker, omitted)
		if updated == marker {
			return string(runes[:headCount]) + marker + string(runes[len(runes)-tailCount:])
		}
		marker = updated
	}
	available := compactionToolResultRunes - len([]rune(marker))
	if available <= 0 {
		return string([]rune(marker)[:compactionToolResultRunes])
	}
	headCount := available / 2
	tailCount := available - headCount
	return string(runes[:headCount]) + marker + string(runes[len(runes)-tailCount:])
}
