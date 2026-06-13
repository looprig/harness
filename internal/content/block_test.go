package content_test

import (
	"encoding/json"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
)

// TestBlockTypeConstants verifies all required BlockType constants exist and have expected string values.
func TestBlockTypeConstants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  content.BlockType
		want content.BlockType
	}{
		{name: "TypeText", got: content.TypeText, want: "text"},
		{name: "TypeImage", got: content.TypeImage, want: "image"},
		{name: "TypeAudio", got: content.TypeAudio, want: "audio"},
		{name: "TypeDocument", got: content.TypeDocument, want: "document"},
		{name: "TypeThinking", got: content.TypeThinking, want: "thinking"},
		{name: "TypeToolUse", got: content.TypeToolUse, want: "tool_use"},
		{name: "TypeToolResult", got: content.TypeToolResult, want: "tool_result"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Errorf("BlockType %s = %q, want %q", tt.name, tt.got, tt.want)
			}
		})
	}
}

// TestBlockDiscriminatedUnion verifies that Block is a discriminated union:
// Type identifies which pointer field is set, and only one is non-nil per valid block.
func TestBlockDiscriminatedUnion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		block       content.Block
		wantType    content.BlockType
		wantNonNil  string // which field should be non-nil
		wantAllNil  bool   // true when all payload fields are nil (zero-value block)
	}{
		{
			name: "text block has Type and non-nil Text field",
			block: content.Block{
				Type: content.TypeText,
				Text: &content.TextBlock{Text: "hello"},
			},
			wantType:   content.TypeText,
			wantNonNil: "Text",
		},
		{
			name: "image block has Type and non-nil Image field",
			block: content.Block{
				Type:  content.TypeImage,
				Image: &content.ImageBlock{MediaType: "image/png", Source: content.ImageSource{URL: "https://example.com/img.png"}},
			},
			wantType:   content.TypeImage,
			wantNonNil: "Image",
		},
		{
			name: "audio block has Type and non-nil Audio field",
			block: content.Block{
				Type:  content.TypeAudio,
				Audio: &content.AudioBlock{MediaType: "audio/mp3", Data: []byte{0x01}},
			},
			wantType:   content.TypeAudio,
			wantNonNil: "Audio",
		},
		{
			name: "document block has Type and non-nil Document field",
			block: content.Block{
				Type:     content.TypeDocument,
				Document: &content.DocumentBlock{MediaType: "application/pdf", Name: "report.pdf", Data: []byte{0x25, 0x50, 0x44, 0x46}},
			},
			wantType:   content.TypeDocument,
			wantNonNil: "Document",
		},
		{
			name: "thinking block has Type and non-nil Thinking field",
			block: content.Block{
				Type:     content.TypeThinking,
				Thinking: &content.ThinkingBlock{Thinking: "I think...", Signature: "sig123"},
			},
			wantType:   content.TypeThinking,
			wantNonNil: "Thinking",
		},
		{
			name: "tool_use block has Type and non-nil ToolUse field",
			block: content.Block{
				Type:    content.TypeToolUse,
				ToolUse: &content.ToolUseBlock{ID: "tu_1", Name: "search", Input: json.RawMessage(`{"q":"go"}`)},
			},
			wantType:   content.TypeToolUse,
			wantNonNil: "ToolUse",
		},
		{
			name: "tool_result block has Type and non-nil ToolResult field",
			block: content.Block{
				Type:       content.TypeToolResult,
				ToolResult: &content.ToolResultBlock{ToolUseID: "tu_1", IsError: false},
			},
			wantType:   content.TypeToolResult,
			wantNonNil: "ToolResult",
		},
		{
			name:       "zero-value Block has empty Type and all nil fields",
			block:      content.Block{},
			wantType:   "",
			wantAllNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.block.Type != tt.wantType {
				t.Errorf("block.Type = %q, want %q", tt.block.Type, tt.wantType)
			}

			if tt.wantAllNil {
				if tt.block.Text != nil {
					t.Error("expected Text to be nil")
				}
				if tt.block.Image != nil {
					t.Error("expected Image to be nil")
				}
				if tt.block.Audio != nil {
					t.Error("expected Audio to be nil")
				}
				if tt.block.Document != nil {
					t.Error("expected Document to be nil")
				}
				if tt.block.Thinking != nil {
					t.Error("expected Thinking to be nil")
				}
				if tt.block.ToolUse != nil {
					t.Error("expected ToolUse to be nil")
				}
				if tt.block.ToolResult != nil {
					t.Error("expected ToolResult to be nil")
				}
				return
			}

			// Verify exactly one non-nil field matches wantNonNil.
			nonNilCount := 0
			fields := map[string]bool{
				"Text":       tt.block.Text != nil,
				"Image":      tt.block.Image != nil,
				"Audio":      tt.block.Audio != nil,
				"Document":   tt.block.Document != nil,
				"Thinking":   tt.block.Thinking != nil,
				"ToolUse":    tt.block.ToolUse != nil,
				"ToolResult": tt.block.ToolResult != nil,
			}
			for _, nonNil := range fields {
				if nonNil {
					nonNilCount++
				}
			}
			if nonNilCount != 1 {
				t.Errorf("expected exactly 1 non-nil field, got %d", nonNilCount)
			}
			if !fields[tt.wantNonNil] {
				t.Errorf("expected field %s to be non-nil, but it is nil", tt.wantNonNil)
			}
		})
	}
}

// TestTextBlock verifies TextBlock fields.
func TestTextBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    content.TextBlock
		wantText string
	}{
		{name: "happy path", input: content.TextBlock{Text: "hello world"}, wantText: "hello world"},
		{name: "empty text", input: content.TextBlock{Text: ""}, wantText: ""},
		{name: "multiline text", input: content.TextBlock{Text: "line1\nline2\nline3"}, wantText: "line1\nline2\nline3"},
		{name: "unicode text", input: content.TextBlock{Text: "こんにちは"}, wantText: "こんにちは"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.input.Text != tt.wantText {
				t.Errorf("TextBlock.Text = %q, want %q", tt.input.Text, tt.wantText)
			}
		})
	}
}

// TestImageSource verifies ImageSource as a sum type: URL xor Data, set exactly one.
func TestImageSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		source    content.ImageSource
		hasURL    bool
		hasData   bool
	}{
		{
			name:   "remote URL source",
			source: content.ImageSource{URL: "https://example.com/image.png"},
			hasURL: true,
		},
		{
			name:    "inline data source",
			source:  content.ImageSource{Data: []byte{0xFF, 0xD8, 0xFF}}, // JPEG magic bytes
			hasData: true,
		},
		{
			name:   "zero-value source has neither URL nor Data",
			source: content.ImageSource{},
		},
		{
			name:    "both URL and Data set (caller's responsibility, struct allows it)",
			source:  content.ImageSource{URL: "https://example.com/img.png", Data: []byte{0x01}},
			hasURL:  true,
			hasData: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.hasURL && tt.source.URL == "" {
				t.Error("expected non-empty URL")
			}
			if !tt.hasURL && tt.source.URL != "" {
				t.Errorf("expected empty URL, got %q", tt.source.URL)
			}
			if tt.hasData && len(tt.source.Data) == 0 {
				t.Error("expected non-empty Data")
			}
			if !tt.hasData && len(tt.source.Data) != 0 {
				t.Errorf("expected empty Data, got %v", tt.source.Data)
			}
		})
	}
}

// TestImageBlock verifies ImageBlock fields: MediaType and Source.
func TestImageBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		block         content.ImageBlock
		wantMediaType string
	}{
		{
			name:          "happy path with URL source",
			block:         content.ImageBlock{MediaType: "image/png", Source: content.ImageSource{URL: "https://example.com/a.png"}},
			wantMediaType: "image/png",
		},
		{
			name:          "happy path with inline data",
			block:         content.ImageBlock{MediaType: "image/jpeg", Source: content.ImageSource{Data: []byte{0xFF, 0xD8}}},
			wantMediaType: "image/jpeg",
		},
		{
			name:          "empty MediaType",
			block:         content.ImageBlock{MediaType: "", Source: content.ImageSource{URL: "https://example.com/a.png"}},
			wantMediaType: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.block.MediaType != tt.wantMediaType {
				t.Errorf("ImageBlock.MediaType = %q, want %q", tt.block.MediaType, tt.wantMediaType)
			}
		})
	}
}

// TestThinkingBlock verifies ThinkingBlock: Thinking and Signature fields.
// Signature is empty during streaming; non-empty only on complete block.
func TestThinkingBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		block         content.ThinkingBlock
		wantThinking  string
		wantSignature string
	}{
		{
			name:          "complete block with signature",
			block:         content.ThinkingBlock{Thinking: "I think therefore I am", Signature: "sig_abc123"},
			wantThinking:  "I think therefore I am",
			wantSignature: "sig_abc123",
		},
		{
			name:          "streaming block has empty signature",
			block:         content.ThinkingBlock{Thinking: "partial thought...", Signature: ""},
			wantThinking:  "partial thought...",
			wantSignature: "",
		},
		{
			name:          "empty thinking and empty signature (zero value)",
			block:         content.ThinkingBlock{},
			wantThinking:  "",
			wantSignature: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.block.Thinking != tt.wantThinking {
				t.Errorf("ThinkingBlock.Thinking = %q, want %q", tt.block.Thinking, tt.wantThinking)
			}
			if tt.block.Signature != tt.wantSignature {
				t.Errorf("ThinkingBlock.Signature = %q, want %q", tt.block.Signature, tt.wantSignature)
			}
		})
	}
}

// TestToolUseBlock verifies ToolUseBlock: ID, Name, Input fields.
func TestToolUseBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		block     content.ToolUseBlock
		wantID    string
		wantName  string
		wantInput json.RawMessage
	}{
		{
			name:      "happy path",
			block:     content.ToolUseBlock{ID: "tu_001", Name: "web_search", Input: json.RawMessage(`{"query":"golang"}`)},
			wantID:    "tu_001",
			wantName:  "web_search",
			wantInput: json.RawMessage(`{"query":"golang"}`),
		},
		{
			name:      "null input",
			block:     content.ToolUseBlock{ID: "tu_002", Name: "ping", Input: json.RawMessage(`null`)},
			wantID:    "tu_002",
			wantName:  "ping",
			wantInput: json.RawMessage(`null`),
		},
		{
			name:      "empty input",
			block:     content.ToolUseBlock{ID: "tu_003", Name: "noop", Input: json.RawMessage(`{}`)},
			wantID:    "tu_003",
			wantName:  "noop",
			wantInput: json.RawMessage(`{}`),
		},
		{
			name:      "nil input (zero value)",
			block:     content.ToolUseBlock{ID: "tu_004", Name: "zeroInput"},
			wantID:    "tu_004",
			wantName:  "zeroInput",
			wantInput: nil,
		},
		{
			name:  "zero value block",
			block: content.ToolUseBlock{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.block.ID != tt.wantID {
				t.Errorf("ToolUseBlock.ID = %q, want %q", tt.block.ID, tt.wantID)
			}
			if tt.block.Name != tt.wantName {
				t.Errorf("ToolUseBlock.Name = %q, want %q", tt.block.Name, tt.wantName)
			}
			if string(tt.block.Input) != string(tt.wantInput) {
				t.Errorf("ToolUseBlock.Input = %q, want %q", tt.block.Input, tt.wantInput)
			}
		})
	}
}

// TestToolResultBlock verifies ToolResultBlock: ToolUseID, Content, IsError fields.
func TestToolResultBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		block         content.ToolResultBlock
		wantToolUseID string
		wantIsError   bool
		wantContent   int // length of Content slice
	}{
		{
			name: "happy path with text content",
			block: content.ToolResultBlock{
				ToolUseID: "tu_001",
				Content:   []*content.Block{{Type: content.TypeText, Text: &content.TextBlock{Text: "result"}}},
				IsError:   false,
			},
			wantToolUseID: "tu_001",
			wantIsError:   false,
			wantContent:   1,
		},
		{
			name: "error result with no content",
			block: content.ToolResultBlock{
				ToolUseID: "tu_002",
				Content:   nil,
				IsError:   true,
			},
			wantToolUseID: "tu_002",
			wantIsError:   true,
			wantContent:   0,
		},
		{
			name: "result with multiple content blocks",
			block: content.ToolResultBlock{
				ToolUseID: "tu_003",
				Content: []*content.Block{
					{Type: content.TypeText, Text: &content.TextBlock{Text: "part1"}},
					{Type: content.TypeText, Text: &content.TextBlock{Text: "part2"}},
				},
				IsError: false,
			},
			wantToolUseID: "tu_003",
			wantIsError:   false,
			wantContent:   2,
		},
		{
			name:          "empty content slice (not nil)",
			block:         content.ToolResultBlock{ToolUseID: "tu_004", Content: []*content.Block{}, IsError: false},
			wantToolUseID: "tu_004",
			wantIsError:   false,
			wantContent:   0,
		},
		{
			name:  "zero-value block",
			block: content.ToolResultBlock{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.block.ToolUseID != tt.wantToolUseID {
				t.Errorf("ToolResultBlock.ToolUseID = %q, want %q", tt.block.ToolUseID, tt.wantToolUseID)
			}
			if tt.block.IsError != tt.wantIsError {
				t.Errorf("ToolResultBlock.IsError = %v, want %v", tt.block.IsError, tt.wantIsError)
			}
			if len(tt.block.Content) != tt.wantContent {
				t.Errorf("len(ToolResultBlock.Content) = %d, want %d", len(tt.block.Content), tt.wantContent)
			}
		})
	}
}

// TestAudioBlock verifies AudioBlock fields.
func TestAudioBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		block         content.AudioBlock
		wantMediaType string
		wantDataLen   int
	}{
		{
			name:          "happy path",
			block:         content.AudioBlock{MediaType: "audio/mp3", Data: []byte{0x49, 0x44, 0x33}},
			wantMediaType: "audio/mp3",
			wantDataLen:   3,
		},
		{
			name:          "empty data",
			block:         content.AudioBlock{MediaType: "audio/wav", Data: []byte{}},
			wantMediaType: "audio/wav",
			wantDataLen:   0,
		},
		{
			name:          "nil data (zero value)",
			block:         content.AudioBlock{MediaType: "audio/ogg"},
			wantMediaType: "audio/ogg",
			wantDataLen:   0,
		},
		{
			name:  "zero-value block",
			block: content.AudioBlock{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.block.MediaType != tt.wantMediaType {
				t.Errorf("AudioBlock.MediaType = %q, want %q", tt.block.MediaType, tt.wantMediaType)
			}
			if len(tt.block.Data) != tt.wantDataLen {
				t.Errorf("len(AudioBlock.Data) = %d, want %d", len(tt.block.Data), tt.wantDataLen)
			}
		})
	}
}

// FuzzToolUseBlockInput exercises ToolUseBlock.Input with arbitrary provider-supplied bytes,
// since Input is json.RawMessage that accepts any byte sequence from an external source.
func FuzzToolUseBlockInput(f *testing.F) {
	f.Add([]byte(`{"query":"go"}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		b := content.ToolUseBlock{
			ID:    "fuzz",
			Name:  "fn",
			Input: json.RawMessage(data),
		}
		_ = b.Input
	})
}

// TestDocumentBlock verifies DocumentBlock fields.
func TestDocumentBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		block         content.DocumentBlock
		wantMediaType string
		wantName      string
		wantDataLen   int
		wantText      string
	}{
		{
			name:          "happy path with binary data",
			block:         content.DocumentBlock{MediaType: "application/pdf", Name: "report.pdf", Data: []byte{0x25, 0x50, 0x44, 0x46}},
			wantMediaType: "application/pdf",
			wantName:      "report.pdf",
			wantDataLen:   4,
		},
		{
			name:          "document with text content",
			block:         content.DocumentBlock{MediaType: "text/plain", Name: "notes.txt", Text: "some text content"},
			wantMediaType: "text/plain",
			wantName:      "notes.txt",
			wantText:      "some text content",
		},
		{
			name:  "zero-value block",
			block: content.DocumentBlock{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.block.MediaType != tt.wantMediaType {
				t.Errorf("DocumentBlock.MediaType = %q, want %q", tt.block.MediaType, tt.wantMediaType)
			}
			if tt.block.Name != tt.wantName {
				t.Errorf("DocumentBlock.Name = %q, want %q", tt.block.Name, tt.wantName)
			}
			if len(tt.block.Data) != tt.wantDataLen {
				t.Errorf("len(DocumentBlock.Data) = %d, want %d", len(tt.block.Data), tt.wantDataLen)
			}
			if tt.block.Text != tt.wantText {
				t.Errorf("DocumentBlock.Text = %q, want %q", tt.block.Text, tt.wantText)
			}
		})
	}
}
