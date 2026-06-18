package streamaccumulator_test

import (
	"reflect"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/content/streamaccumulator"
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
