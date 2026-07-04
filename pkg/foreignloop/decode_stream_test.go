package foreignloop

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/looprig/core/content"
)

// drainStream reads every event the decoder emits for the named fixture, then
// returns the collected events and the decoder's terminal error.
func drainStream(t *testing.T, fixture string) ([]ForeignEvent, error) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "stream", fixture))
	if err != nil {
		t.Fatalf("open fixture %s: %v", fixture, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	ch, errFn := decodeStream(f)
	var got []ForeignEvent
	for ev := range ch {
		got = append(got, ev)
	}
	return got, errFn()
}

func kinds(evs []ForeignEvent) []ForeignKind {
	out := make([]ForeignKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func TestDecodeStreamFixtures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		fixture   string
		wantKinds []ForeignKind
		wantErr   bool
	}{
		{
			name:    "happy multi-event stream",
			fixture: "happy.jsonl",
			wantKinds: []ForeignKind{
				ForeignInit,
				ForeignTextDelta,
				ForeignTextDelta,
				ForeignThinkingDelta,
				ForeignToolUse,
				ForeignStepComplete,
				ForeignToolResult,
				ForeignTerminalOK,
			},
		},
		{
			name:      "empty reader closes cleanly",
			fixture:   "empty.jsonl",
			wantKinds: nil,
		},
		{
			name:      "unknown types ignored",
			fixture:   "unknown.jsonl",
			wantKinds: []ForeignKind{ForeignInit, ForeignTerminalOK},
		},
		{
			name:      "garbage line surfaces DecodeError but stream completes",
			fixture:   "garbage.jsonl",
			wantKinds: []ForeignKind{ForeignInit, ForeignTerminalError},
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := drainStream(t, tt.fixture)
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeStream err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var de *DecodeError
				if !errors.As(err, &de) {
					t.Fatalf("err = %v, want *DecodeError", err)
				}
			}
			gk := kinds(got)
			if len(gk) != len(tt.wantKinds) {
				t.Fatalf("kinds = %v, want %v", gk, tt.wantKinds)
			}
			for i := range gk {
				if gk[i] != tt.wantKinds[i] {
					t.Fatalf("kind[%d] = %v, want %v (full %v)", i, gk[i], tt.wantKinds[i], gk)
				}
			}
		})
	}
}

func TestDecodeStreamHappyFieldDetail(t *testing.T) {
	t.Parallel()
	got, err := drainStream(t, "happy.jsonl")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got[0].SessionID != "sess-123" {
		t.Errorf("init SessionID = %q, want sess-123", got[0].SessionID)
	}
	if got[1].Text != "Hel" || got[2].Text != "lo" {
		t.Errorf("text deltas = %q,%q want Hel,lo", got[1].Text, got[2].Text)
	}
	if got[3].Text != "hmm" {
		t.Errorf("thinking delta = %q, want hmm", got[3].Text)
	}
	if got[4].ToolUseID != "toolu_1" || got[4].ToolName != "Bash" {
		t.Errorf("tool_use = %q/%q, want toolu_1/Bash", got[4].ToolUseID, got[4].ToolName)
	}
	step := got[5]
	if step.Message == nil {
		t.Fatal("step complete Message nil")
	}
	if len(step.Message.Blocks) != 2 {
		t.Fatalf("step blocks = %d, want 2", len(step.Message.Blocks))
	}
	if tb, ok := step.Message.Blocks[0].(*content.TextBlock); !ok || tb.Text != "Hello" {
		t.Errorf("block[0] = %#v, want TextBlock{Hello}", step.Message.Blocks[0])
	}
	if ub, ok := step.Message.Blocks[1].(*content.ToolUseBlock); !ok || ub.ID != "toolu_1" {
		t.Errorf("block[1] = %#v, want ToolUseBlock{toolu_1}", step.Message.Blocks[1])
	}
	res := got[6]
	if res.ToolUseID != "toolu_1" || res.IsError || res.ResultPreview != "file1\nfile2" {
		t.Errorf("tool_result = %+v, want toolu_1/false/file1\\nfile2", res)
	}
	if got[7].Message == nil || len(got[7].Message.Blocks) != 1 {
		t.Fatalf("terminal OK message = %+v, want single text block", got[7].Message)
	}
}
