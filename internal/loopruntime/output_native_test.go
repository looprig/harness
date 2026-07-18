package loopruntime

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
)

func nativeTurnFixture(
	t *testing.T,
	client inference.Client,
	tools ToolSet,
) (turnConfig, turnState, *turnRecorder) {
	t.Helper()
	cfg, state, recorder := newTurnFixture(
		[]content.Block{&content.TextBlock{Text: "return structured output"}},
		nil,
		tools,
		client,
		noGateReg(),
	)
	output := testLoopOutput()
	cfg.output = &output
	cfg.model = outputModel(model.Capabilities{StructuredOutput: true})
	if len(tools.Registry) > 0 {
		cfg.model = outputModel(model.Capabilities{
			Tools: true, StructuredOutput: true, StructuredOutputWithTools: true,
		})
	}
	return cfg, state, recorder
}

func TestRunTurnNativeStructuredFinalFinishPolicyIsAtomic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		chunks     []content.Chunk
		result     *stream.StreamResult
		wantDone   bool
		wantFinish stream.FinishReason
		wantReason inference.MalformedStructuredOutputReason
	}{
		{
			name: "stop valid text succeeds and canonicalizes fragments",
			chunks: []content.Chunk{
				textChunk(" {\n\t\"answer\""), textChunk(": \"ok\" } "),
			},
			result: &stream.StreamResult{
				Usage: &content.Usage{InputTokens: 4, OutputTokens: 2}, FinishReason: stream.FinishReasonStop,
			},
			wantDone: true,
		},
		{
			name:     "authoritative unknown valid text succeeds",
			chunks:   []content.Chunk{textChunk(`{"answer":"ok"}`)},
			result:   &stream.StreamResult{FinishReason: stream.FinishReasonUnknown},
			wantDone: true,
		},
		{
			name:       "length fails before commit",
			chunks:     []content.Chunk{textChunk(`{"answer":"partial"}`)},
			result:     &stream.StreamResult{FinishReason: stream.FinishReasonLength},
			wantFinish: stream.FinishReasonLength,
		},
		{
			name:       "content filter fails before commit",
			chunks:     []content.Chunk{textChunk(`{"answer":"filtered"}`)},
			result:     &stream.StreamResult{FinishReason: stream.FinishReasonContentFilter},
			wantFinish: stream.FinishReasonContentFilter,
		},
		{
			name:       "length with no content is still a finish failure",
			result:     &stream.StreamResult{FinishReason: stream.FinishReasonLength},
			wantFinish: stream.FinishReasonLength,
		},
		{
			name:       "tool use without calls fails closed",
			chunks:     []content.Chunk{textChunk(`{"answer":"not a tool"}`)},
			result:     &stream.StreamResult{FinishReason: stream.FinishReasonToolUse},
			wantFinish: stream.FinishReasonToolUse,
		},
		{
			name:       "future finish fails closed",
			chunks:     []content.Chunk{textChunk(`{"answer":"ok"}`)},
			result:     &stream.StreamResult{FinishReason: stream.FinishReason("future")},
			wantFinish: inference.StructuredOutputFinishReasonOther,
		},
		{
			name:       "missing terminal result fails closed",
			chunks:     []content.Chunk{textChunk(`{"answer":"ok"}`)},
			wantFinish: inference.StructuredOutputFinishReasonOther,
		},
		{
			name:       "malformed JSON exposes metadata only",
			chunks:     []content.Chunk{textChunk(`{"answer":"secret`)},
			result:     &stream.StreamResult{FinishReason: stream.FinishReasonStop},
			wantReason: inference.MalformedReasonMalformedJSON,
		},
		{
			name:       "empty JSON text fails without commit",
			chunks:     []content.Chunk{textChunk("   ")},
			result:     &stream.StreamResult{FinishReason: stream.FinishReasonStop},
			wantReason: inference.MalformedReasonEmpty,
		},
		{
			name:       "empty stream stop is malformed structured output",
			result:     &stream.StreamResult{FinishReason: stream.FinishReasonStop},
			wantReason: inference.MalformedReasonEmpty,
		},
		{
			name: "duplicate members fail without commit",
			chunks: []content.Chunk{
				textChunk(`{"answer":"secret","answer":"other"}`),
			},
			result:     &stream.StreamResult{FinishReason: stream.FinishReasonStop},
			wantReason: inference.MalformedReasonMalformedJSON,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &scriptedLLM{scripts: [][]content.Chunk{tt.chunks}}
			if tt.result != nil {
				client.results = []*stream.StreamResult{tt.result}
			}
			cfg, state, recorder := nativeTurnFixture(t, client, ToolSet{})
			terminal := runTurn(context.Background(), cfg, state)

			if client.calls != 1 {
				t.Fatalf("Stream calls = %d, want exactly 1 (no hidden repair)", client.calls)
			}
			if tt.wantDone {
				done, ok := terminal.(event.TurnDone)
				if !ok {
					t.Fatalf("terminal = %T %v, want TurnDone", terminal, terminal)
				}
				if len(recorder.commits) != 1 || len(stepDones(recorder.events())) != 1 {
					t.Fatalf("commits/StepDone = %d/%d, want 1/1", len(recorder.commits), len(stepDones(recorder.events())))
				}
				wantBlocks := []content.Block{&content.TextBlock{Text: `{"answer":"ok"}`}}
				if !reflect.DeepEqual(done.Message.Blocks, wantBlocks) {
					t.Fatalf("final blocks = %#v, want canonical text %#v", done.Message.Blocks, wantBlocks)
				}
				if tt.result.Usage != nil {
					if done.Message.Usage == nil || !reflect.DeepEqual(*done.Message.Usage, *tt.result.Usage) || !reflect.DeepEqual(done.Usage, *tt.result.Usage) {
						t.Fatalf("canonical message/turn usage = %v/%+v, want %+v", done.Message.Usage, done.Usage, *tt.result.Usage)
					}
					if done.Message.Usage == tt.result.Usage {
						t.Fatal("TurnDone message usage aliases producer result")
					}
				}
				return
			}

			failed, ok := terminal.(event.TurnFailed)
			if !ok {
				t.Fatalf("terminal = %T, want TurnFailed", terminal)
			}
			if len(recorder.commits) != 0 || len(stepDones(recorder.events())) != 0 || len(recorder.committedMsgs()) != 0 {
				t.Fatalf("invalid final committed: commits=%d stepDone=%d messages=%d", len(recorder.commits), len(stepDones(recorder.events())), len(recorder.committedMsgs()))
			}
			if tt.wantFinish != "" {
				var finishErr *inference.StructuredOutputFinishError
				if !errors.As(failed.Err, &finishErr) || finishErr.Reason != tt.wantFinish {
					t.Fatalf("error = %T %v, want finish %q", failed.Err, failed.Err, tt.wantFinish)
				}
			}
			if tt.wantReason != "" {
				var malformed *inference.MalformedStructuredOutputError
				if !errors.As(failed.Err, &malformed) || malformed.ReasonCode != tt.wantReason {
					t.Fatalf("error = %T %v, want malformed %q", failed.Err, failed.Err, tt.wantReason)
				}
				if strings.Contains(failed.Err.Error(), "secret") {
					t.Fatalf("error leaked raw model output: %q", failed.Err)
				}
			}
		})
	}
}

func TestRunTurnNativeStopWithCallsFailsBeforeExecution(t *testing.T) {
	t.Parallel()
	echo := &echoTool{name: "Echo", output: "ran"}
	client := &scriptedLLM{
		scripts: [][]content.Chunk{{toolUseChunk(0, "id-1", "Echo", `{"x":1}`)}},
		results: []*stream.StreamResult{{FinishReason: stream.FinishReasonStop}},
	}
	cfg, state, recorder := nativeTurnFixture(t, client, agenticToolSet([]tool.InvokableTool{echo}, 25, 100))

	terminal := runTurn(context.Background(), cfg, state)
	failed, ok := terminal.(event.TurnFailed)
	if !ok {
		t.Fatalf("terminal = %T, want TurnFailed", terminal)
	}
	var finishErr *inference.StructuredOutputFinishError
	if !errors.As(failed.Err, &finishErr) || finishErr.Reason != stream.FinishReasonStop {
		t.Fatalf("error = %T %v, want stop finish error", failed.Err, failed.Err)
	}
	if echo.runCount() != 0 || len(recorder.commits) != 0 || len(stepDones(recorder.events())) != 0 {
		t.Fatalf("invalid stop call acted/committed: runs=%d commits=%d StepDone=%d", echo.runCount(), len(recorder.commits), len(stepDones(recorder.events())))
	}
}

func TestRunTurnNativeToolFinishRequiresCompleteOrdinaryCalls(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		chunk       content.Chunk
		wantFeature string
	}{
		{
			name:        "missing call id",
			chunk:       toolUseChunk(0, "", "Echo", `{"x":1}`),
			wantFeature: "incomplete_tool_call",
		},
		{
			name:        "malformed call input",
			chunk:       toolUseChunk(0, "id-1", "Echo", `{"x":`),
			wantFeature: "incomplete_tool_call",
		},
		{
			name:        "fallback terminal frame rejected under native strategy",
			chunk:       toolUseChunk(0, "id-1", inference.StructuredOutputToolName, `{"answer":"ok"}`),
			wantFeature: "native_terminal_tool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			echo := &echoTool{name: "Echo", output: "ran"}
			client := &scriptedLLM{
				scripts: [][]content.Chunk{{tt.chunk}},
				results: []*stream.StreamResult{{FinishReason: stream.FinishReasonToolUse}},
			}
			cfg, state, recorder := nativeTurnFixture(t, client, agenticToolSet([]tool.InvokableTool{echo}, 25, 100))

			terminal := runTurn(context.Background(), cfg, state)
			failed, ok := terminal.(event.TurnFailed)
			if !ok {
				t.Fatalf("terminal = %T, want TurnFailed", terminal)
			}
			var conflict *inference.StructuredOutputConflictError
			if !errors.As(failed.Err, &conflict) || conflict.Feature != tt.wantFeature {
				t.Fatalf("error = %T %v, want feature %q", failed.Err, failed.Err, tt.wantFeature)
			}
			if echo.runCount() != 0 || len(recorder.commits) != 0 || len(stepDones(recorder.events())) != 0 {
				t.Fatalf("invalid tool frame acted/committed: runs=%d commits=%d StepDone=%d", echo.runCount(), len(recorder.commits), len(stepDones(recorder.events())))
			}
		})
	}
}

type terminalResultErrorLLM struct {
	calls int
}

func (c *terminalResultErrorLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("unused")
}

func (c *terminalResultErrorLLM) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	c.calls++
	emitted := false
	return stream.NewStreamReaderWithResult(func() (content.Chunk, error) {
		if !emitted {
			emitted = true
			return textChunk(`{"answer":"ok"}`), nil
		}
		return nil, io.EOF
	}, nil, func() (stream.StreamResult, bool, error) {
		return stream.StreamResult{}, false, errors.New("invalid terminal metadata")
	}), nil
}

func TestRunTurnNativeMalformedTerminalMetadataFailsBeforeCommit(t *testing.T) {
	t.Parallel()
	client := &terminalResultErrorLLM{}
	cfg, state, recorder := nativeTurnFixture(t, client, ToolSet{})

	terminal := runTurn(context.Background(), cfg, state)
	failed, ok := terminal.(event.TurnFailed)
	if !ok {
		t.Fatalf("terminal = %T, want TurnFailed", terminal)
	}
	var resultErr *stream.StreamResultError
	if !errors.As(failed.Err, &resultErr) {
		t.Fatalf("error = %T %v, want StreamResultError", failed.Err, failed.Err)
	}
	if client.calls != 1 || len(recorder.commits) != 0 || len(stepDones(recorder.events())) != 0 {
		t.Fatalf("metadata failure calls/commits/StepDone = %d/%d/%d, want 1/0/0", client.calls, len(recorder.commits), len(stepDones(recorder.events())))
	}
}

func TestRunTurnNativeInvalidFinalRetainsPriorCommittedToolStep(t *testing.T) {
	t.Parallel()
	echo := &echoTool{name: "Echo", output: "ran"}
	client := &scriptedLLM{
		scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-1", "Echo", `{"x":1}`)},
			{textChunk(`{"answer":"secret`)},
		},
		results: []*stream.StreamResult{
			{Usage: &content.Usage{InputTokens: 2, OutputTokens: 1}, FinishReason: stream.FinishReasonToolUse},
			{Usage: &content.Usage{InputTokens: 3, OutputTokens: 2}, FinishReason: stream.FinishReasonStop},
		},
	}
	cfg, state, recorder := nativeTurnFixture(t, client, agenticToolSet([]tool.InvokableTool{echo}, 25, 100))

	terminal := runTurn(context.Background(), cfg, state)
	failed, ok := terminal.(event.TurnFailed)
	if !ok {
		t.Fatalf("terminal = %T, want TurnFailed", terminal)
	}
	var malformed *inference.MalformedStructuredOutputError
	if !errors.As(failed.Err, &malformed) {
		t.Fatalf("error = %T %v, want malformed structured output", failed.Err, failed.Err)
	}
	if client.calls != 2 {
		t.Fatalf("Stream calls = %d, want 2 (no repair)", client.calls)
	}
	if echo.runCount() != 1 {
		t.Fatalf("tool runs = %d, want prior tool step executed once", echo.runCount())
	}
	if len(recorder.commits) != 1 || len(stepDones(recorder.events())) != 1 || len(recorder.committedMsgs()) != 2 {
		t.Fatalf("committed prior/current state = commits %d, StepDone %d, messages %d; want 1,1,2", len(recorder.commits), len(stepDones(recorder.events())), len(recorder.committedMsgs()))
	}
	priorAI := recorder.committedMsgs()[0].(*content.AIMessage)
	if priorAI.Usage == nil || priorAI.Usage.InputTokens != 2 || priorAI.Usage.OutputTokens != 1 {
		t.Fatalf("prior committed usage = %+v, want 2/1", priorAI.Usage)
	}
}
