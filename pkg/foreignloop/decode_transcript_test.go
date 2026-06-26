package foreignloop

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/ciram-co/looprig/pkg/content"
)

func textOf(t *testing.T, b content.Block) string {
	t.Helper()
	tb, ok := b.(*content.TextBlock)
	if !ok {
		t.Fatalf("block %#v is not *TextBlock", b)
	}
	return tb.Text
}

func TestDecodeTranscriptTailHappy(t *testing.T) {
	t.Parallel()
	got, err := decodeTranscriptTail(filepath.Join("testdata", "transcript", "happy.jsonl"), 0)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("groups = %d, want 3 (%v)", len(got), got)
	}
	// group 0: the leading user prompt.
	um, ok := got[0][0].(*content.UserMessage)
	if !ok || textOf(t, um.Blocks[0]) != "hi there" {
		t.Fatalf("group0[0] = %#v, want UserMessage 'hi there'", got[0][0])
	}
	// group 1: assistant (thinking,text,tool_use) + tool_result.
	ai, ok := got[1][0].(*content.AIMessage)
	if !ok || len(ai.Blocks) != 3 {
		t.Fatalf("group1[0] = %#v, want AIMessage w/ 3 blocks", got[1][0])
	}
	if _, ok := ai.Blocks[0].(*content.ThinkingBlock); !ok {
		t.Errorf("block0 = %#v, want ThinkingBlock", ai.Blocks[0])
	}
	if ub, ok := ai.Blocks[2].(*content.ToolUseBlock); !ok || ub.ID != "toolu_9" || ub.Name != "Read" {
		t.Errorf("block2 = %#v, want ToolUseBlock toolu_9/Read", ai.Blocks[2])
	}
	tr, ok := got[1][1].(*content.ToolResultMessage)
	if !ok || tr.ToolUseID != "toolu_9" || tr.IsError {
		t.Fatalf("group1[1] = %#v, want ToolResultMessage toolu_9", got[1][1])
	}
	if textOf(t, tr.Blocks[0]) != "contents" {
		t.Errorf("tool result text = %q, want contents", textOf(t, tr.Blocks[0]))
	}
	// group 2: final assistant text. Sidechain 'subagent says hi' must be absent.
	ai2, ok := got[2][0].(*content.AIMessage)
	if !ok || textOf(t, ai2.Blocks[0]) != "Done" {
		t.Fatalf("group2[0] = %#v, want AIMessage 'Done'", got[2][0])
	}
}

func TestDecodeTranscriptTailTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		path       string
		wantGroups int
		wantErr    bool
	}{
		{name: "happy", path: filepath.Join("testdata", "transcript", "happy.jsonl"), wantGroups: 3},
		{name: "empty file", path: filepath.Join("testdata", "transcript", "empty.jsonl"), wantGroups: 0},
		{name: "truncated line skipped soft", path: filepath.Join("testdata", "transcript", "truncated.jsonl"), wantGroups: 2},
		{name: "missing file errors", path: filepath.Join("testdata", "transcript", "nope.jsonl"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeTranscriptTail(tt.path, 0)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var ue *TranscriptUnavailableError
				if !errors.As(err, &ue) {
					t.Fatalf("err = %v, want *TranscriptUnavailableError", err)
				}
				return
			}
			if len(got) != tt.wantGroups {
				t.Fatalf("groups = %d, want %d", len(got), tt.wantGroups)
			}
		})
	}
}
