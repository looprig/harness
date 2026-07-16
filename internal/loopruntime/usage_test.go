package loopruntime

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/tool"
	stream "github.com/looprig/inference/stream"
)

func TestRunTurnAccountsAuthoritativeUsage(t *testing.T) {
	t.Parallel()
	maximum := content.TokenCount(^uint64(0))
	tests := []struct {
		name             string
		scripts          [][]content.Chunk
		results          []*stream.StreamResult
		tools            ToolSet
		wantUsage        content.Usage
		wantStepDones    int
		wantReported     []bool
		wantOverflow     bool
		wantMessageUsage content.Usage
	}{
		{
			name:             "one request attaches a defensive terminal result",
			scripts:          [][]content.Chunk{{textChunk("done")}},
			results:          []*stream.StreamResult{{Usage: &content.Usage{InputTokens: 11, OutputTokens: 7, ReasoningTokens: 3}}},
			wantUsage:        content.Usage{InputTokens: 11, OutputTokens: 7, ReasoningTokens: 3},
			wantStepDones:    1,
			wantReported:     []bool{true},
			wantMessageUsage: content.Usage{InputTokens: 11, OutputTokens: 7, ReasoningTokens: 3},
		},
		{
			name: "two requests sum once while each StepDone owns its request usage",
			scripts: [][]content.Chunk{
				{toolUseChunk(0, "call-1", "Echo", `{}`)},
				{textChunk("done")},
			},
			results: []*stream.StreamResult{
				{Usage: &content.Usage{InputTokens: 10, OutputTokens: 2}},
				{Usage: &content.Usage{InputTokens: 20, OutputTokens: 4, CacheReadTokens: 5}},
			},
			tools:            agenticToolSet([]tool.InvokableTool{&echoTool{name: "Echo", output: "ok"}}, 25, 100),
			wantUsage:        content.Usage{InputTokens: 30, OutputTokens: 6, CacheReadTokens: 5},
			wantStepDones:    2,
			wantReported:     []bool{true, true},
			wantMessageUsage: content.Usage{InputTokens: 20, OutputTokens: 4, CacheReadTokens: 5},
		},
		{
			name:             "missing terminal metadata remains unknown",
			scripts:          [][]content.Chunk{{textChunk("done")}},
			results:          []*stream.StreamResult{nil},
			wantStepDones:    1,
			wantReported:     []bool{false},
			wantMessageUsage: content.Usage{},
		},
		{
			name: "turn usage overflow fails before the overflowing step commits",
			scripts: [][]content.Chunk{
				{toolUseChunk(0, "call-1", "Echo", `{}`)},
				{textChunk("done")},
			},
			results: []*stream.StreamResult{
				{Usage: &content.Usage{InputTokens: maximum}},
				{Usage: &content.Usage{InputTokens: 1}},
			},
			tools:         agenticToolSet([]tool.InvokableTool{&echoTool{name: "Echo", output: "ok"}}, 25, 100),
			wantStepDones: 1,
			wantReported:  []bool{true},
			wantOverflow:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &scriptedLLM{scripts: tt.scripts, results: tt.results}
			cfg, state, recorder := newTurnFixture([]content.Block{&content.TextBlock{Text: "go"}}, nil, tt.tools, client, noGateReg())
			terminal := runTurn(context.Background(), cfg, state)
			steps := stepDones(recorder.events())
			if len(steps) != tt.wantStepDones {
				t.Fatalf("StepDone count = %d, want %d", len(steps), tt.wantStepDones)
			}
			for i, reported := range tt.wantReported {
				ai := steps[i].Messages[0].(*content.AIMessage)
				if (ai.Usage != nil) != reported {
					t.Errorf("StepDone[%d] usage reported = %v, want %v", i, ai.Usage != nil, reported)
				}
			}
			if tt.wantOverflow {
				failed, ok := terminal.(event.TurnFailed)
				if !ok {
					t.Fatalf("terminal = %T, want TurnFailed", terminal)
				}
				var overflow *content.UsageOverflowError
				if !errors.As(failed.Err, &overflow) {
					t.Fatalf("TurnFailed.Err = %T %v, want *UsageOverflowError", failed.Err, failed.Err)
				}
				return
			}
			done, ok := terminal.(event.TurnDone)
			if !ok {
				t.Fatalf("terminal = %T, want TurnDone", terminal)
			}
			if !reflect.DeepEqual(done.Usage, tt.wantUsage) {
				t.Errorf("TurnDone.Usage = %+v, want %+v", done.Usage, tt.wantUsage)
			}
			if tt.wantReported[len(tt.wantReported)-1] {
				if done.Message.Usage == nil || !reflect.DeepEqual(*done.Message.Usage, tt.wantMessageUsage) {
					t.Errorf("TurnDone.Message.Usage = %+v, want %+v", done.Message.Usage, tt.wantMessageUsage)
				}
				providerUsage := tt.results[len(tt.results)-1].Usage
				if done.Message.Usage == providerUsage {
					t.Error("TurnDone.Message.Usage aliases provider result")
				}
			} else if done.Message.Usage != nil {
				t.Errorf("TurnDone.Message.Usage = %+v, want nil", done.Message.Usage)
			}
		})
	}
}
