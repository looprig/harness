// Package content defines the unified content vocabulary shared across all
// internal packages. It provides a discriminated-union Block type and all
// concrete block subtypes.
package content

import "encoding/json"

type BlockType string

const (
	TypeText       BlockType = "text"
	TypeImage      BlockType = "image"
	TypeAudio      BlockType = "audio"
	TypeDocument   BlockType = "document"
	TypeThinking   BlockType = "thinking"
	TypeToolUse    BlockType = "tool_use"
	TypeToolResult BlockType = "tool_result"
)

// Block is a discriminated union: exactly one pointer field must be non-nil;
// Type identifies which one. Callers must not set more than one payload field.
type Block struct {
	Type       BlockType
	Text       *TextBlock
	Image      *ImageBlock
	Audio      *AudioBlock
	Document   *DocumentBlock
	Thinking   *ThinkingBlock
	ToolUse    *ToolUseBlock
	ToolResult *ToolResultBlock
}

type TextBlock struct {
	Text string
}

// ImageSource is a sum type for the origin of image data.
// Set exactly one of URL (remote) or Data (inline bytes).
type ImageSource struct {
	URL  string
	Data []byte
}

type ImageBlock struct {
	MediaType string
	Source    ImageSource
}

type AudioBlock struct {
	MediaType string
	Data      []byte
}

// DocumentBlock carries document data. Either Data (binary) or Text (extracted
// text) may be populated depending on how the document was provided.
type DocumentBlock struct {
	MediaType string
	Name      string
	Data      []byte
	Text      string
}

// ThinkingBlock carries model reasoning text.
// Signature is empty during streaming and non-empty only on a complete block.
type ThinkingBlock struct {
	Thinking  string
	Signature string
}

type ToolUseBlock struct {
	ID    string
	Name  string
	Input json.RawMessage
}

type ToolResultBlock struct {
	ToolUseID string
	Content   []*Block
	IsError   bool
}
