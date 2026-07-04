// Package content defines the unified content vocabulary shared across all
// internal packages. Block is a sealed interface; the concrete payload type is
// the discriminator. Only this package can add variants (unexported marker).
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

// Block is the sealed interface over all content block payloads. The concrete
// type is the discriminator; there is no Type field and no nil-able payload
// pointers. BlockType is retained only as the wire tag for the JSON codec
// (block_json.go, added in a later task), not as a field on any in-memory value.
type Block interface{ isBlock() }

func (*TextBlock) isBlock()       {}
func (*ImageBlock) isBlock()      {}
func (*AudioBlock) isBlock()      {}
func (*DocumentBlock) isBlock()   {}
func (*ThinkingBlock) isBlock()   {}
func (*ToolUseBlock) isBlock()    {}
func (*ToolResultBlock) isBlock() {}

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
	MediaType MediaType
	Source    ImageSource
}

type AudioBlock struct {
	MediaType MediaType
	Data      []byte
}

// DocumentBlock carries document data. Either Data (binary) or Text (extracted
// text) may be populated depending on how the document was provided.
type DocumentBlock struct {
	MediaType MediaType
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

// ToolResultBlock nests its own []Block, so it implements json.Marshaler /
// json.Unmarshaler in block_json.go (a later task). Do not add a Type field.
type ToolResultBlock struct {
	ToolUseID string
	Content   []Block
	IsError   bool
}
