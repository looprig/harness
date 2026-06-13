package content_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
)

func TestChunkTypeConstants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		got      content.ChunkType
		expected content.ChunkType
	}{
		{
			name:     "text delta constant value",
			got:      content.ChunkTypeText,
			expected: "text_delta",
		},
		{
			name:     "thinking delta constant value",
			got:      content.ChunkTypeThinking,
			expected: "thinking_delta",
		},
		{
			name:     "constants are distinct",
			got:      content.ChunkTypeText,
			expected: content.ChunkTypeThinking,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Last case intentionally tests inequality — skip equality check.
			if i == 2 {
				if tt.got == tt.expected {
					t.Errorf("ChunkTypeText and ChunkTypeThinking must be distinct, both equal %q", tt.got)
				}
				return
			}
			if tt.got != tt.expected {
				t.Errorf("got %q, want %q", tt.got, tt.expected)
			}
		})
	}
}

func TestChunk(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		chunk            content.Chunk
		wantType         content.ChunkType
		wantTextNil      bool
		wantThinkingNil  bool
		wantTextContent  string
		wantThinkContent string
	}{
		{
			name:            "zero value has nil payload fields",
			chunk:           content.Chunk{},
			wantType:        "",
			wantTextNil:     true,
			wantThinkingNil: true,
		},
		{
			name: "text chunk carries text payload and nil thinking",
			chunk: content.Chunk{
				Type: content.ChunkTypeText,
				Text: &content.TextChunk{Text: "hello"},
			},
			wantType:        content.ChunkTypeText,
			wantTextNil:     false,
			wantThinkingNil: true,
			wantTextContent: "hello",
		},
		{
			name: "thinking chunk carries thinking payload and nil text",
			chunk: content.Chunk{
				Type:     content.ChunkTypeThinking,
				Thinking: &content.ThinkingChunk{Thinking: "reasoning"},
			},
			wantType:         content.ChunkTypeThinking,
			wantTextNil:      true,
			wantThinkingNil:  false,
			wantThinkContent: "reasoning",
		},
		{
			name: "text chunk with empty string is a valid delta",
			chunk: content.Chunk{
				Type: content.ChunkTypeText,
				Text: &content.TextChunk{Text: ""},
			},
			wantType:        content.ChunkTypeText,
			wantTextNil:     false,
			wantThinkingNil: true,
			wantTextContent: "",
		},
		{
			name: "thinking chunk with empty string is a valid delta",
			chunk: content.Chunk{
				Type:     content.ChunkTypeThinking,
				Thinking: &content.ThinkingChunk{Thinking: ""},
			},
			wantType:         content.ChunkTypeThinking,
			wantTextNil:      true,
			wantThinkingNil:  false,
			wantThinkContent: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.chunk.Type != tt.wantType {
				t.Errorf("Type: got %q, want %q", tt.chunk.Type, tt.wantType)
			}

			if (tt.chunk.Text == nil) != tt.wantTextNil {
				t.Errorf("Text nil: got %v, want %v", tt.chunk.Text == nil, tt.wantTextNil)
			}

			if (tt.chunk.Thinking == nil) != tt.wantThinkingNil {
				t.Errorf("Thinking nil: got %v, want %v", tt.chunk.Thinking == nil, tt.wantThinkingNil)
			}

			if !tt.wantTextNil && tt.chunk.Text.Text != tt.wantTextContent {
				t.Errorf("TextChunk.Text: got %q, want %q", tt.chunk.Text.Text, tt.wantTextContent)
			}

			if !tt.wantThinkingNil && tt.chunk.Thinking.Thinking != tt.wantThinkContent {
				t.Errorf("ThinkingChunk.Thinking: got %q, want %q", tt.chunk.Thinking.Thinking, tt.wantThinkContent)
			}
		})
	}
}
