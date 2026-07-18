package loopruntime

import (
	"context"
	"errors"
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

func terminalTurnFixture(
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
	cfg.model = outputModel(model.Capabilities{Tools: true})
	return cfg, state, recorder
}

func terminalOutputChunk(index int, id, input string) content.Chunk {
	return toolUseChunk(index, id, inference.StructuredOutputToolName, input)
}

func TestRunTurnTerminalOutputCommitsCanonicalTextWithoutExecutingControlFrame(t *testing.T) {
	t.Parallel()

	ordinary := &echoTool{name: "Echo", output: "must not run"}
	usage := &content.Usage{InputTokens: 7, OutputTokens: 3}
	client := &scriptedLLM{
		scripts: [][]content.Chunk{{
			&content.ThinkingChunk{Thinking: "private reasoning"},
			terminalOutputChunk(0, "terminal-1", " {\n\t\"answer\""),
			terminalOutputChunk(0, "", ": \"ok\" } "),
		}},
		results: []*stream.StreamResult{{Usage: usage, FinishReason: stream.FinishReasonToolUse}},
	}
	// Zero limits prove the control frame consumes neither an ordinary-tool
	// iteration nor an ordinary-tool call.
	tools := ToolSet{Registry: []tool.InvokableTool{ordinary}}
	cfg, state, recorder := terminalTurnFixture(t, client, tools)
	type measurement struct {
		request      inference.Request
		continuation bool
	}
	var measured []measurement
	cfg.measure = func(_ context.Context, request inference.Request, _ string, _ *content.UserMessage, continuation bool) error {
		measured = append(measured, measurement{request: request, continuation: continuation})
		return nil
	}

	terminal := runTurn(context.Background(), cfg, state)
	done, ok := terminal.(event.TurnDone)
	if !ok {
		t.Fatalf("terminal = %T %v, want TurnDone", terminal, terminal)
	}
	if client.calls != 1 {
		t.Fatalf("Stream calls = %d, want exactly 1 (no repair)", client.calls)
	}
	if ordinary.runCount() != 0 {
		t.Fatalf("ordinary tool runs = %d, want 0", ordinary.runCount())
	}
	if recorder.drainCalls != 0 {
		t.Fatalf("drain calls = %d, want 0 for terminal final", recorder.drainCalls)
	}
	if len(recorder.commits) != 1 || len(stepDones(recorder.events())) != 1 {
		t.Fatalf("commits/StepDone = %d/%d, want 1/1", len(recorder.commits), len(stepDones(recorder.events())))
	}

	wantBlocks := []content.Block{&content.TextBlock{Text: `{"answer":"ok"}`}}
	if !reflect.DeepEqual(done.Message.Blocks, wantBlocks) {
		t.Fatalf("TurnDone blocks = %#v, want canonical text %#v", done.Message.Blocks, wantBlocks)
	}
	if done.Message.Usage == nil || !reflect.DeepEqual(*done.Message.Usage, *usage) || !reflect.DeepEqual(done.Usage, *usage) {
		t.Fatalf("TurnDone message/turn usage = %v/%+v, want %+v", done.Message.Usage, done.Usage, *usage)
	}
	if done.Message.Usage == usage {
		t.Fatal("TurnDone message usage aliases provider usage")
	}
	assertTerminalOutputMessage(t, recorder.committedMsgs()[0], `{"answer":"ok"}`)
	step := stepDones(recorder.events())[0]
	if len(step.Messages) != 1 {
		t.Fatalf("StepDone messages = %d, want 1", len(step.Messages))
	}
	assertTerminalOutputMessage(t, step.Messages[0], `{"answer":"ok"}`)

	for _, emitted := range recorder.events() {
		switch emitted.(type) {
		case event.PermissionRequested, event.PermissionDecided, event.UserInputRequested,
			event.ToolCallStarted, event.ToolCallCompleted:
			t.Fatalf("terminal control frame emitted tool/gate lifecycle event %T", emitted)
		}
	}
	if len(measured) != 2 || !measured[0].continuation || measured[1].continuation {
		t.Fatalf("measurements = %#v, want pre-stream continuation and final non-continuation candidates", measured)
	}
	finalMessages := measured[1].request.Messages
	if len(finalMessages) == 0 {
		t.Fatal("final candidate measurement has no messages")
	}
	assertTerminalOutputMessage(t, finalMessages[len(finalMessages)-1], `{"answer":"ok"}`)
}

func TestRunTurnTerminalOutputRejectsInvalidBatchAtomically(t *testing.T) {
	t.Parallel()

	tooLarge := `{"answer":"` + strings.Repeat("x", inference.MaxStructuredResultBytes) + `"}`
	tests := []struct {
		name       string
		chunks     []content.Chunk
		finish     stream.FinishReason
		wantFinish stream.FinishReason
		wantReason inference.MalformedStructuredOutputReason
		secret     string
	}{
		{
			name: "ordinary call before terminal",
			chunks: []content.Chunk{
				toolUseChunk(0, "action-1", "Echo", `{"secret":"action"}`),
				terminalOutputChunk(1, "terminal-1", `{"answer":"ok"}`),
			},
			finish: stream.FinishReasonToolUse,
		},
		{
			name: "invalid ordinary call before terminal",
			chunks: []content.Chunk{
				toolUseChunk(0, "action-1", "Echo", `{"secret":`),
				terminalOutputChunk(1, "terminal-1", `{"answer":"ok"}`),
			},
			finish: stream.FinishReasonToolUse,
			secret: "secret",
		},
		{
			name: "duplicate terminal",
			chunks: []content.Chunk{
				terminalOutputChunk(0, "terminal-1", `{"answer":"one"}`),
				terminalOutputChunk(1, "terminal-2", `{"answer":"two"}`),
			},
			finish: stream.FinishReasonToolUse,
		},
		{
			name: "terminal with semantic text across fragments",
			chunks: []content.Chunk{
				textChunk("semantic "), textChunk("text"),
				terminalOutputChunk(0, "terminal-1", `{"answer":"ok"}`),
			},
			finish: stream.FinishReasonToolUse,
		},
		{
			name: "terminal with whitespace text is ambiguous per shared extractor",
			chunks: []content.Chunk{
				textChunk(" \t\n"), terminalOutputChunk(0, "terminal-1", `{"answer":"ok"}`),
			},
			finish: stream.FinishReasonToolUse,
		},
		{
			name:       "malformed terminal input",
			chunks:     []content.Chunk{terminalOutputChunk(0, "terminal-1", `{"answer":"terminal-secret`)},
			finish:     stream.FinishReasonToolUse,
			wantReason: inference.MalformedReasonMalformedJSON,
			secret:     "terminal-secret",
		},
		{
			name:       "non-object terminal input",
			chunks:     []content.Chunk{terminalOutputChunk(0, "terminal-1", `7`)},
			finish:     stream.FinishReasonToolUse,
			wantReason: inference.MalformedReasonRootNotObject,
		},
		{
			name:       "duplicate-key terminal input",
			chunks:     []content.Chunk{terminalOutputChunk(0, "terminal-1", `{"answer":"secret","answer":"other"}`)},
			finish:     stream.FinishReasonToolUse,
			wantReason: inference.MalformedReasonMalformedJSON,
			secret:     "secret",
		},
		{
			name:       "oversized terminal input",
			chunks:     []content.Chunk{terminalOutputChunk(0, "terminal-1", tooLarge)},
			finish:     stream.FinishReasonToolUse,
			wantReason: inference.MalformedReasonTooLarge,
		},
		{
			name:       "stop finish",
			chunks:     []content.Chunk{terminalOutputChunk(0, "terminal-1", `{"answer":"ok"}`)},
			finish:     stream.FinishReasonStop,
			wantFinish: stream.FinishReasonStop,
		},
		{
			name:       "unknown finish",
			chunks:     []content.Chunk{terminalOutputChunk(0, "terminal-1", `{"answer":"ok"}`)},
			finish:     stream.FinishReasonUnknown,
			wantFinish: inference.StructuredOutputFinishReasonOther,
		},
		{
			name:       "length finish",
			chunks:     []content.Chunk{terminalOutputChunk(0, "terminal-1", `{"answer":"ok"}`)},
			finish:     stream.FinishReasonLength,
			wantFinish: stream.FinishReasonLength,
		},
		{
			name:       "content filter finish",
			chunks:     []content.Chunk{terminalOutputChunk(0, "terminal-1", `{"answer":"ok"}`)},
			finish:     stream.FinishReasonContentFilter,
			wantFinish: stream.FinishReasonContentFilter,
		},
		{
			name:   "missing terminal id",
			chunks: []content.Chunk{terminalOutputChunk(0, "", `{"answer":"ok"}`)},
			finish: stream.FinishReasonToolUse,
		},
		{
			name:   "missing call name",
			chunks: []content.Chunk{toolUseChunk(0, "terminal-1", "", `{"answer":"ok"}`)},
			finish: stream.FinishReasonToolUse,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ordinary := &echoTool{name: "Echo", output: "must not run"}
			client := &scriptedLLM{
				scripts: [][]content.Chunk{tt.chunks},
				results: []*stream.StreamResult{{
					Usage:        &content.Usage{InputTokens: 1},
					FinishReason: tt.finish,
				}},
			}
			cfg, state, recorder := terminalTurnFixture(t, client, ToolSet{Registry: []tool.InvokableTool{ordinary}})
			// If invalid terminal usage were accounted before validation, this would
			// replace the expected structured error with UsageOverflowError.
			state.usage.InputTokens = ^content.TokenCount(0)
			measureCalls := 0
			cfg.measure = func(context.Context, inference.Request, string, *content.UserMessage, bool) error {
				measureCalls++
				return nil
			}

			terminal := runTurn(context.Background(), cfg, state)
			failed, ok := terminal.(event.TurnFailed)
			if !ok {
				t.Fatalf("terminal = %T %v, want TurnFailed", terminal, terminal)
			}
			if client.calls != 1 || measureCalls != 1 {
				t.Fatalf("stream/measure calls = %d/%d, want 1/1 (no repair or invalid-final measurement)", client.calls, measureCalls)
			}
			if ordinary.runCount() != 0 {
				t.Fatalf("ordinary tool runs = %d, want 0", ordinary.runCount())
			}
			if len(recorder.commits) != 0 || len(stepDones(recorder.events())) != 0 || len(recorder.committedMsgs()) != 0 {
				t.Fatalf("invalid batch committed: commits=%d StepDone=%d messages=%d", len(recorder.commits), len(stepDones(recorder.events())), len(recorder.committedMsgs()))
			}
			for _, emitted := range recorder.events() {
				switch emitted.(type) {
				case event.PermissionRequested, event.PermissionDecided, event.UserInputRequested,
					event.ToolCallStarted, event.ToolCallCompleted:
					t.Fatalf("invalid batch emitted action lifecycle event %T", emitted)
				}
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
					t.Fatalf("error = %T %v, want malformed reason %q", failed.Err, failed.Err, tt.wantReason)
				}
			}
			if tt.secret != "" && strings.Contains(failed.Err.Error(), tt.secret) {
				t.Fatalf("error leaked raw terminal/action input: %v", failed.Err)
			}
		})
	}
}

func TestRunTurnTerminalOutputRequiresReservedControlFrame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		chunks []content.Chunk
		finish stream.FinishReason
	}{
		{name: "stop text", chunks: []content.Chunk{textChunk(`{"answer":"not-terminal"}`)}, finish: stream.FinishReasonStop},
		{name: "unknown text", chunks: []content.Chunk{textChunk(`{"answer":"not-terminal"}`)}, finish: stream.FinishReasonUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &scriptedLLM{
				scripts: [][]content.Chunk{tt.chunks},
				results: []*stream.StreamResult{{FinishReason: tt.finish}},
			}
			cfg, state, recorder := terminalTurnFixture(t, client, ToolSet{})
			terminal := runTurn(context.Background(), cfg, state)
			if _, ok := terminal.(event.TurnFailed); !ok {
				t.Fatalf("terminal = %T %v, want TurnFailed", terminal, terminal)
			}
			if client.calls != 1 || len(recorder.commits) != 0 || len(stepDones(recorder.events())) != 0 {
				t.Fatalf("calls/commits/StepDone = %d/%d/%d, want 1/0/0", client.calls, len(recorder.commits), len(stepDones(recorder.events())))
			}
		})
	}
}

func TestRunTurnTerminalOutputAllowsOrdinaryContinuationThenFinal(t *testing.T) {
	t.Parallel()

	ordinary := &echoTool{name: "Echo", output: "ran"}
	client := &scriptedLLM{
		scripts: [][]content.Chunk{
			{toolUseChunk(0, "action-1", "Echo", `{}`)},
			{terminalOutputChunk(0, "terminal-1", ` { "answer": "done" } `)},
		},
		results: []*stream.StreamResult{
			{FinishReason: stream.FinishReasonToolUse},
			{FinishReason: stream.FinishReasonToolUse},
		},
	}
	cfg, state, recorder := terminalTurnFixture(t, client, agenticToolSet([]tool.InvokableTool{ordinary}, 1, 1))

	terminal := runTurn(context.Background(), cfg, state)
	done, ok := terminal.(event.TurnDone)
	if !ok {
		t.Fatalf("terminal = %T %v, want TurnDone", terminal, terminal)
	}
	if client.calls != 2 || ordinary.runCount() != 1 {
		t.Fatalf("stream/tool calls = %d/%d, want 2/1", client.calls, ordinary.runCount())
	}
	if len(recorder.commits) != 2 || len(stepDones(recorder.events())) != 2 {
		t.Fatalf("commits/StepDone = %d/%d, want 2/2", len(recorder.commits), len(stepDones(recorder.events())))
	}
	if len(recorder.committedMsgs()) != 3 {
		t.Fatalf("committed messages = %d, want ordinary AI + result + canonical final", len(recorder.committedMsgs()))
	}
	assertTerminalOutputMessage(t, recorder.committedMsgs()[2], `{"answer":"done"}`)
	if got, want := done.Message.Blocks, []content.Block{&content.TextBlock{Text: `{"answer":"done"}`}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("TurnDone blocks = %#v, want %#v", got, want)
	}
}

func TestRunTurnTerminalOutputMalformedLaterFinalRetainsPriorCommittedStep(t *testing.T) {
	t.Parallel()

	ordinary := &echoTool{name: "Echo", output: "ran"}
	client := &scriptedLLM{
		scripts: [][]content.Chunk{
			{toolUseChunk(0, "action-1", "Echo", `{}`)},
			{terminalOutputChunk(0, "terminal-1", `{"answer":"secret`)},
		},
		results: []*stream.StreamResult{
			{FinishReason: stream.FinishReasonToolUse},
			{FinishReason: stream.FinishReasonToolUse},
		},
	}
	cfg, state, recorder := terminalTurnFixture(t, client, agenticToolSet([]tool.InvokableTool{ordinary}, 1, 1))
	measureCalls := 0
	cfg.measure = func(context.Context, inference.Request, string, *content.UserMessage, bool) error {
		measureCalls++
		return nil
	}

	terminal := runTurn(context.Background(), cfg, state)
	failed, ok := terminal.(event.TurnFailed)
	if !ok {
		t.Fatalf("terminal = %T %v, want TurnFailed", terminal, terminal)
	}
	var malformed *inference.MalformedStructuredOutputError
	if !errors.As(failed.Err, &malformed) {
		t.Fatalf("error = %T %v, want MalformedStructuredOutputError", failed.Err, failed.Err)
	}
	if strings.Contains(failed.Err.Error(), "secret") {
		t.Fatalf("error leaked malformed terminal input: %v", failed.Err)
	}
	if len(failed.Err.Error()) > 512 {
		t.Fatalf("malformed terminal error is unbounded: %d bytes", len(failed.Err.Error()))
	}
	if client.calls != 2 || measureCalls != 2 || ordinary.runCount() != 1 {
		t.Fatalf("stream/measure/tool calls = %d/%d/%d, want 2/2/1 (no repair or invalid-final measurement)", client.calls, measureCalls, ordinary.runCount())
	}
	if len(recorder.commits) != 1 || len(stepDones(recorder.events())) != 1 || len(recorder.committedMsgs()) != 2 {
		t.Fatalf("prior committed state = commits %d StepDone %d messages %d, want 1/1/2", len(recorder.commits), len(stepDones(recorder.events())), len(recorder.committedMsgs()))
	}
	started, completed, permissionRequested, permissionDecided := 0, 0, 0, 0
	for _, emitted := range recorder.events() {
		switch emitted.(type) {
		case event.ToolCallStarted:
			started++
		case event.ToolCallCompleted:
			completed++
		case event.PermissionRequested:
			permissionRequested++
		case event.PermissionDecided:
			permissionDecided++
		case event.UserInputRequested:
			t.Fatalf("malformed terminal emitted user-input lifecycle event %T", emitted)
		}
	}
	if started != 1 || completed != 1 || permissionRequested != 0 || permissionDecided != 1 {
		t.Fatalf("tool/gate lifecycle counts = %d/%d/%d/%d, want only prior ordinary call 1/1/0/1", started, completed, permissionRequested, permissionDecided)
	}
}

func TestValidateTerminalOutputRejectsTypedNilOrInconsistentRawFrames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message *content.AIMessage
		calls   []content.ToolUseBlock
	}{
		{
			name: "typed nil block",
			message: &content.AIMessage{Message: content.Message{
				Role: content.RoleAssistant, Blocks: []content.Block{(*content.ToolUseBlock)(nil)},
			}},
		},
		{
			name: "message terminal absent from raw calls",
			message: &content.AIMessage{Message: content.Message{
				Role: content.RoleAssistant,
				Blocks: []content.Block{&content.ToolUseBlock{
					ID: "terminal-1", Name: inference.StructuredOutputToolName, Input: []byte(`{"answer":"ok"}`),
				}},
			}},
		},
		{
			name: "typed nil block with ordinary raw call",
			message: &content.AIMessage{Message: content.Message{
				Role: content.RoleAssistant,
				Blocks: []content.Block{
					(*content.ToolUseBlock)(nil),
					&content.ToolUseBlock{ID: "action-1", Name: "Echo", Input: []byte(`{}`)},
				},
			}},
			calls: []content.ToolUseBlock{{ID: "action-1", Name: "Echo", Input: []byte(`{}`)}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := validateTerminalStep(tt.message, tt.calls, &stream.StreamResult{FinishReason: stream.FinishReasonToolUse})
			if err == nil {
				t.Fatal("validateTerminalStep() error = nil, want fail closed")
			}
		})
	}
}

func assertTerminalOutputMessage(t *testing.T, message content.Conversation, want string) {
	t.Helper()
	ai, ok := message.(*content.AIMessage)
	if !ok {
		t.Fatalf("message = %T, want *content.AIMessage", message)
	}
	if ai.Role != content.RoleAssistant || len(ai.Blocks) != 1 {
		t.Fatalf("assistant message = %#v, want exactly one block", ai)
	}
	text, ok := ai.Blocks[0].(*content.TextBlock)
	if !ok || text == nil || text.Text != want {
		t.Fatalf("assistant block = %#v, want TextBlock %q", ai.Blocks[0], want)
	}
}
