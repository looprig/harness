package streamaccumulator_test

import (
	"reflect"
	"testing"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/content/streamaccumulator"
)

func TestToolUses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		chunks []*content.ToolUseChunk
		want   []content.ToolUseBlock
	}{
		{
			name:   "empty: no chunks yields nil",
			chunks: nil,
			want:   nil,
		},
		{
			name: "single index, multi-fragment InputJSON concatenates",
			chunks: []*content.ToolUseChunk{
				{Index: 0, ID: "call_1", Name: "search", InputJSON: `{"q":`},
				{Index: 0, InputJSON: `"hi"}`},
			},
			want: []content.ToolUseBlock{
				{ID: "call_1", Name: "search", Input: []byte(`{"q":"hi"}`)},
			},
		},
		{
			name: "multi-index returns ascending order regardless of arrival order",
			chunks: []*content.ToolUseChunk{
				{Index: 2, ID: "c2", Name: "two", InputJSON: `{}`},
				{Index: 0, ID: "c0", Name: "zero", InputJSON: `{}`},
				{Index: 1, ID: "c1", Name: "one", InputJSON: `{}`},
			},
			want: []content.ToolUseBlock{
				{ID: "c0", Name: "zero", Input: []byte(`{}`)},
				{ID: "c1", Name: "one", Input: []byte(`{}`)},
				{ID: "c2", Name: "two", Input: []byte(`{}`)},
			},
		},
		{
			name: "negative and huge indexes do not panic and sort ascending",
			chunks: []*content.ToolUseChunk{
				{Index: 1 << 30, ID: "big", Name: "big", InputJSON: `{}`},
				{Index: -5, ID: "neg", Name: "neg", InputJSON: `{}`},
				{Index: 0, ID: "mid", Name: "mid", InputJSON: `{}`},
			},
			want: []content.ToolUseBlock{
				{ID: "neg", Name: "neg", Input: []byte(`{}`)},
				{ID: "mid", Name: "mid", Input: []byte(`{}`)},
				{ID: "big", Name: "big", Input: []byte(`{}`)},
			},
		},
		{
			name: "ID and Name arriving on a later delta are captured",
			chunks: []*content.ToolUseChunk{
				{Index: 0, InputJSON: `{"a":`},
				{Index: 0, ID: "late_id", Name: "late_name", InputJSON: `1}`},
			},
			want: []content.ToolUseBlock{
				{ID: "late_id", Name: "late_name", Input: []byte(`{"a":1}`)},
			},
		},
		{
			name: "last non-empty ID/Name wins; empty fragment never clears a set value",
			chunks: []*content.ToolUseChunk{
				{Index: 0, ID: "first", Name: "first", InputJSON: `{`},
				{Index: 0, ID: "", Name: "", InputJSON: `"k":1`},
				{Index: 0, ID: "second", Name: "second", InputJSON: `}`},
			},
			want: []content.ToolUseBlock{
				{ID: "second", Name: "second", Input: []byte(`{"k":1}`)},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var acc streamaccumulator.ToolUses
			for _, c := range tt.chunks {
				acc.Add(c)
			}
			got := acc.Blocks()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Blocks() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestToolUsesEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		chunks    []*content.ToolUseChunk
		wantEmpty bool
	}{
		{name: "empty before any add", chunks: nil, wantEmpty: true},
		{
			name:      "not empty after one add",
			chunks:    []*content.ToolUseChunk{{Index: 0, ID: "x", Name: "x"}},
			wantEmpty: false,
		},
		{
			name:      "not empty after add at negative index",
			chunks:    []*content.ToolUseChunk{{Index: -1, ID: "x", Name: "x"}},
			wantEmpty: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var acc streamaccumulator.ToolUses
			for _, c := range tt.chunks {
				acc.Add(c)
			}
			if got := acc.Empty(); got != tt.wantEmpty {
				t.Errorf("Empty() = %v, want %v", got, tt.wantEmpty)
			}
		})
	}
}

func TestText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		chunks []*content.TextChunk
		want   *content.TextBlock
	}{
		{
			name:   "empty yields nil block",
			chunks: nil,
			want:   nil,
		},
		{
			name:   "single chunk",
			chunks: []*content.TextChunk{{Text: "hello"}},
			want:   &content.TextBlock{Text: "hello"},
		},
		{
			name: "multiple chunks fold into one block",
			chunks: []*content.TextChunk{
				{Text: "hel"},
				{Text: "lo "},
				{Text: "world"},
			},
			want: &content.TextBlock{Text: "hello world"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var acc streamaccumulator.Text
			for _, c := range tt.chunks {
				acc.Add(c)
			}
			got := acc.Block()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Block() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestTextEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		chunks    []*content.TextChunk
		wantEmpty bool
	}{
		{name: "empty before any add", chunks: nil, wantEmpty: true},
		{name: "not empty after add", chunks: []*content.TextChunk{{Text: "x"}}, wantEmpty: false},
		{
			name:      "not empty after empty-string add",
			chunks:    []*content.TextChunk{{Text: ""}},
			wantEmpty: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var acc streamaccumulator.Text
			for _, c := range tt.chunks {
				acc.Add(c)
			}
			if got := acc.Empty(); got != tt.wantEmpty {
				t.Errorf("Empty() = %v, want %v", got, tt.wantEmpty)
			}
		})
	}
}

func TestThinking(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		chunks []*content.ThinkingChunk
		want   *content.ThinkingBlock
	}{
		{
			name:   "empty yields nil block",
			chunks: nil,
			want:   nil,
		},
		{
			name:   "single chunk, signature stays empty",
			chunks: []*content.ThinkingChunk{{Thinking: "reasoning"}},
			want:   &content.ThinkingBlock{Thinking: "reasoning", Signature: ""},
		},
		{
			name: "multiple chunks fold into one block with empty signature",
			chunks: []*content.ThinkingChunk{
				{Thinking: "step "},
				{Thinking: "one "},
				{Thinking: "two"},
			},
			want: &content.ThinkingBlock{Thinking: "step one two", Signature: ""},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var acc streamaccumulator.Thinking
			for _, c := range tt.chunks {
				acc.Add(c)
			}
			got := acc.Block()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Block() = %#v, want %#v", got, tt.want)
			}
			if got != nil && got.Signature != "" {
				t.Errorf("Block().Signature = %q, want empty (conscious omission)", got.Signature)
			}
		})
	}
}

func TestThinkingEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		chunks    []*content.ThinkingChunk
		wantEmpty bool
	}{
		{name: "empty before any add", chunks: nil, wantEmpty: true},
		{name: "not empty after add", chunks: []*content.ThinkingChunk{{Thinking: "x"}}, wantEmpty: false},
		{
			name:      "not empty after empty-string add",
			chunks:    []*content.ThinkingChunk{{Thinking: ""}},
			wantEmpty: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var acc streamaccumulator.Thinking
			for _, c := range tt.chunks {
				acc.Add(c)
			}
			if got := acc.Empty(); got != tt.wantEmpty {
				t.Errorf("Empty() = %v, want %v", got, tt.wantEmpty)
			}
		})
	}
}
