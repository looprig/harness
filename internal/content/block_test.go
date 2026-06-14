package content_test

import (
	"encoding/json"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
)

// TestMediaTypeConstants verifies that all MediaType constants have the correct IANA MIME string values.
func TestMediaTypeConstants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  content.MediaType
		want content.MediaType
	}{
		// Image
		{name: "ImageJPEG", got: content.MediaTypeImageJPEG, want: "image/jpeg"},
		{name: "ImagePNG", got: content.MediaTypeImagePNG, want: "image/png"},
		{name: "ImageGIF", got: content.MediaTypeImageGIF, want: "image/gif"},
		{name: "ImageWebP", got: content.MediaTypeImageWebP, want: "image/webp"},
		{name: "ImageSVG", got: content.MediaTypeImageSVG, want: "image/svg+xml"},
		// Audio
		{name: "AudioMPEG", got: content.MediaTypeAudioMPEG, want: "audio/mpeg"},
		{name: "AudioWAV", got: content.MediaTypeAudioWAV, want: "audio/wav"},
		{name: "AudioOGG", got: content.MediaTypeAudioOGG, want: "audio/ogg"},
		{name: "AudioFLAC", got: content.MediaTypeAudioFLAC, want: "audio/flac"},
		{name: "AudioAAC", got: content.MediaTypeAudioAAC, want: "audio/aac"},
		{name: "AudioMP4", got: content.MediaTypeAudioMP4, want: "audio/mp4"},
		{name: "AudioWebM", got: content.MediaTypeAudioWebM, want: "audio/webm"},
		// Document
		{name: "DocumentPDF", got: content.MediaTypeDocumentPDF, want: "application/pdf"},
		{name: "DocumentText", got: content.MediaTypeDocumentText, want: "text/plain"},
		{name: "DocumentHTML", got: content.MediaTypeDocumentHTML, want: "text/html"},
		{name: "DocumentCSV", got: content.MediaTypeDocumentCSV, want: "text/csv"},
		{name: "DocumentMarkdown", got: content.MediaTypeDocumentMarkdown, want: "text/markdown"},
		{name: "DocumentDOCX", got: content.MediaTypeDocumentDOCX, want: "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{name: "DocumentXLSX", got: content.MediaTypeDocumentXLSX, want: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Errorf("MediaType %s = %q, want %q", tt.name, tt.got, tt.want)
			}
		})
	}
}

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

// TestBlock_InterfaceCompliance is a compile-time check that every concrete
// payload type satisfies the sealed Block interface.
// Acceptable exception to the table-driven rule: purely compile-time, no runtime path to branch.
func TestBlock_InterfaceCompliance(t *testing.T) {
	var _ content.Block = (*content.TextBlock)(nil)
	var _ content.Block = (*content.ImageBlock)(nil)
	var _ content.Block = (*content.AudioBlock)(nil)
	var _ content.Block = (*content.DocumentBlock)(nil)
	var _ content.Block = (*content.ThinkingBlock)(nil)
	var _ content.Block = (*content.ToolUseBlock)(nil)
	var _ content.Block = (*content.ToolResultBlock)(nil)
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
		name    string
		source  content.ImageSource
		hasURL  bool
		hasData bool
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
		wantMediaType content.MediaType
	}{
		{
			name:          "PNG with URL source",
			block:         content.ImageBlock{MediaType: content.MediaTypeImagePNG, Source: content.ImageSource{URL: "https://example.com/a.png"}},
			wantMediaType: content.MediaTypeImagePNG,
		},
		{
			name:          "JPEG with inline data",
			block:         content.ImageBlock{MediaType: content.MediaTypeImageJPEG, Source: content.ImageSource{Data: []byte{0xFF, 0xD8}}},
			wantMediaType: content.MediaTypeImageJPEG,
		},
		{
			name:          "WebP with URL source",
			block:         content.ImageBlock{MediaType: content.MediaTypeImageWebP, Source: content.ImageSource{URL: "https://example.com/a.webp"}},
			wantMediaType: content.MediaTypeImageWebP,
		},
		{
			name:          "SVG with inline data",
			block:         content.ImageBlock{MediaType: content.MediaTypeImageSVG, Source: content.ImageSource{Data: []byte("<svg/>")}},
			wantMediaType: content.MediaTypeImageSVG,
		},
		{
			name:          "empty MediaType (zero value)",
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
				Content:   []content.Block{&content.TextBlock{Text: "result"}},
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
				Content: []content.Block{
					&content.TextBlock{Text: "part1"},
					&content.TextBlock{Text: "part2"},
				},
				IsError: false,
			},
			wantToolUseID: "tu_003",
			wantIsError:   false,
			wantContent:   2,
		},
		{
			name:          "empty content slice (not nil)",
			block:         content.ToolResultBlock{ToolUseID: "tu_004", Content: []content.Block{}, IsError: false},
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
		wantMediaType content.MediaType
		wantDataLen   int
	}{
		{
			name:          "MP3 (audio/mpeg) with ID3 header bytes",
			block:         content.AudioBlock{MediaType: content.MediaTypeAudioMPEG, Data: []byte{0x49, 0x44, 0x33}},
			wantMediaType: content.MediaTypeAudioMPEG,
			wantDataLen:   3,
		},
		{
			name:          "WAV with empty data",
			block:         content.AudioBlock{MediaType: content.MediaTypeAudioWAV, Data: []byte{}},
			wantMediaType: content.MediaTypeAudioWAV,
			wantDataLen:   0,
		},
		{
			name:          "OGG with nil data (zero value)",
			block:         content.AudioBlock{MediaType: content.MediaTypeAudioOGG},
			wantMediaType: content.MediaTypeAudioOGG,
			wantDataLen:   0,
		},
		{
			name:          "FLAC",
			block:         content.AudioBlock{MediaType: content.MediaTypeAudioFLAC, Data: []byte{0x66, 0x4C, 0x61, 0x43}},
			wantMediaType: content.MediaTypeAudioFLAC,
			wantDataLen:   4,
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
		wantMediaType content.MediaType
		wantName      string
		wantDataLen   int
		wantText      string
	}{
		{
			name:          "PDF with binary data",
			block:         content.DocumentBlock{MediaType: content.MediaTypeDocumentPDF, Name: "report.pdf", Data: []byte{0x25, 0x50, 0x44, 0x46}},
			wantMediaType: content.MediaTypeDocumentPDF,
			wantName:      "report.pdf",
			wantDataLen:   4,
		},
		{
			name:          "plain text document",
			block:         content.DocumentBlock{MediaType: content.MediaTypeDocumentText, Name: "notes.txt", Text: "some text content"},
			wantMediaType: content.MediaTypeDocumentText,
			wantName:      "notes.txt",
			wantText:      "some text content",
		},
		{
			name:          "markdown document",
			block:         content.DocumentBlock{MediaType: content.MediaTypeDocumentMarkdown, Name: "readme.md", Text: "# Title"},
			wantMediaType: content.MediaTypeDocumentMarkdown,
			wantName:      "readme.md",
			wantText:      "# Title",
		},
		{
			name:          "DOCX binary",
			block:         content.DocumentBlock{MediaType: content.MediaTypeDocumentDOCX, Name: "doc.docx", Data: []byte{0x50, 0x4B}},
			wantMediaType: content.MediaTypeDocumentDOCX,
			wantName:      "doc.docx",
			wantDataLen:   2,
		},
		{
			name:          "CSV text",
			block:         content.DocumentBlock{MediaType: content.MediaTypeDocumentCSV, Name: "data.csv", Text: "a,b\n1,2"},
			wantMediaType: content.MediaTypeDocumentCSV,
			wantName:      "data.csv",
			wantText:      "a,b\n1,2",
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
