package loopruntime

import (
	"context"
	"fmt"
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

func TestRunTurnTerminalFinalCommitCancellationIsAtomic(t *testing.T) {
	ordinary := &echoTool{name: "Echo", output: "ran"}
	const rawTerminal = `{"answer":"terminal-cancel-secret"}`
	client := &scriptedLLM{
		scripts: [][]content.Chunk{
			{toolUseChunk(0, "action-1", "Echo", `{}`)},
			{terminalOutputChunk(0, "terminal-1", rawTerminal)},
		},
		results: []*stream.StreamResult{
			{FinishReason: stream.FinishReasonToolUse},
			{Usage: &content.Usage{InputTokens: 4, OutputTokens: 2}, FinishReason: stream.FinishReasonToolUse},
		},
	}
	cfg, state, recorder := terminalTurnFixture(t, client, agenticToolSet([]tool.InvokableTool{ordinary}, 1, 1))

	healthyCommit := cfg.commit
	commitAttempt := 0
	eventsBeforeRejectedFinal := -1
	cfg.commit = func(ctx context.Context, candidate turnCommit) error {
		commitAttempt++
		if commitAttempt == 2 {
			// Reaching this assertion proves the terminal frame was validated and
			// materialized as canonical text before the actor rejected its commit.
			if len(candidate.Messages) != 1 {
				t.Fatalf("terminal candidate messages = %d, want 1", len(candidate.Messages))
			}
			assertTerminalOutputMessage(t, candidate.Messages[0], rawTerminal)
			eventsBeforeRejectedFinal = len(recorder.events())
			return &CommitError{Reason: CommitTurnCancelled, Cause: context.Canceled}
		}
		return healthyCommit(ctx, candidate)
	}

	terminal := runTurn(context.Background(), cfg, state)
	if _, ok := terminal.(event.TurnInterrupted); !ok {
		t.Fatalf("terminal = %T %v, want TurnInterrupted", terminal, terminal)
	}
	if commitAttempt != 2 || eventsBeforeRejectedFinal < 0 {
		t.Fatalf("commit attempts/final boundary = %d/%d, want 2/non-negative", commitAttempt, eventsBeforeRejectedFinal)
	}
	if len(recorder.commits) != 1 || len(stepDones(recorder.events())) != 1 || len(recorder.committedMsgs()) != 2 {
		t.Fatalf("durable prior/current state = commits %d StepDone %d messages %d, want 1/1/2", len(recorder.commits), len(stepDones(recorder.events())), len(recorder.committedMsgs()))
	}
	if got := len(recorder.events()); got != eventsBeforeRejectedFinal {
		t.Fatalf("events after rejected terminal commit = %d, want 0", got-eventsBeforeRejectedFinal)
	}
	if ordinary.runCount() != 1 || client.calls != 2 {
		t.Fatalf("ordinary runs/streams = %d/%d, want 1/2", ordinary.runCount(), client.calls)
	}
	if strings.Contains(fmt.Sprintf("%+v", terminal), "terminal-cancel-secret") {
		t.Fatalf("rejected terminal JSON reached terminal surface through %T", terminal)
	}
	for _, message := range recorder.committedMsgs() {
		if strings.Contains(fmt.Sprintf("%+v", message), "terminal-cancel-secret") {
			t.Fatalf("rejected terminal JSON reached durable message through %T", message)
		}
	}
	for _, emitted := range recorder.events() {
		if strings.Contains(fmt.Sprintf("%+v", emitted), "terminal-cancel-secret") {
			t.Fatalf("rejected terminal JSON reached event surface through %T", emitted)
		}
	}
}

func TestRunTurnContextMeasurementKeepsSelectedStructuredRequestShape(t *testing.T) {
	tests := []struct {
		name       string
		caps       model.Capabilities
		final      []content.Chunk
		wantNative bool
		wantTools  []string
		wantChoice inference.ToolChoice
	}{
		{
			name: "native output with ordinary tools",
			caps: model.Capabilities{
				Tools: true, StructuredOutput: true, StructuredOutputWithTools: true,
			},
			final:      []content.Chunk{textChunk(` { "answer": "done" } `)},
			wantNative: true,
			wantTools:  []string{"Echo"},
			wantChoice: inference.ToolChoiceAuto,
		},
		{
			name:       "terminal fallback with ordinary tools",
			caps:       model.Capabilities{Tools: true},
			final:      []content.Chunk{terminalOutputChunk(0, "terminal-1", ` { "answer": "done" } `)},
			wantTools:  []string{"Echo", inference.StructuredOutputToolName},
			wantChoice: inference.ToolChoiceRequired,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ordinary := &echoTool{name: "Echo", output: "ran"}
			client := &scriptedLLM{
				scripts: [][]content.Chunk{
					{toolUseChunk(0, "action-1", "Echo", `{}`)},
					tt.final,
				},
				results: []*stream.StreamResult{
					{FinishReason: stream.FinishReasonToolUse},
					{FinishReason: func() stream.FinishReason {
						if tt.wantNative {
							return stream.FinishReasonStop
						}
						return stream.FinishReasonToolUse
					}()},
				},
			}
			cfg, state, _ := newTurnFixture(nil, nil, agenticToolSet([]tool.InvokableTool{ordinary}, 1, 1), client, noGateReg())
			cfg.model = outputModel(tt.caps)
			configured := testLoopOutput()
			cfg.output = &configured

			measurementCalls := 0
			continuations := make([]bool, 0, 3)
			cfg.measure = func(_ context.Context, request inference.Request, _ string, _ *content.UserMessage, continuation bool) error {
				measurementCalls++
				continuations = append(continuations, continuation)
				if got := request.Output != nil; got != tt.wantNative {
					t.Fatalf("measurement %d native output present = %v, want %v", measurementCalls, got, tt.wantNative)
				}
				if request.ToolChoice != tt.wantChoice {
					t.Fatalf("measurement %d ToolChoice = %q, want %q", measurementCalls, request.ToolChoice, tt.wantChoice)
				}
				gotTools := make([]string, len(request.Tools))
				for index := range request.Tools {
					gotTools[index] = request.Tools[index].Name
				}
				if !reflect.DeepEqual(gotTools, tt.wantTools) {
					t.Fatalf("measurement %d tools = %v, want %v", measurementCalls, gotTools, tt.wantTools)
				}
				if tt.wantNative {
					if request.Output.Name != testLoopOutput().Name || string(request.Output.Schema) != string(testLoopOutput().Schema) {
						t.Fatalf("measurement %d native output = %#v, want frozen policy", measurementCalls, request.Output)
					}
				} else {
					terminalTool := request.Tools[len(request.Tools)-1]
					if string(terminalTool.Schema) != string(testLoopOutput().Schema) {
						t.Fatalf("measurement %d terminal schema = %s, want frozen policy", measurementCalls, terminalTool.Schema)
					}
				}
				if !continuation {
					if len(request.Messages) == 0 {
						t.Fatal("final measurement has no staged history")
					}
					assertTerminalOutputMessage(t, request.Messages[len(request.Messages)-1], `{"answer":"done"}`)
				}

				// A hostile measurement collaborator may mutate the request it was
				// handed. Every later candidate must still come from the frozen plan.
				if measurementCalls == 1 {
					request.Tools[0].Name = "mutated"
					request.Tools[0].Schema[0] = '['
					if request.Output != nil {
						request.Output.Schema[0] = '['
					} else {
						request.Tools[len(request.Tools)-1].Schema[0] = '['
					}
				}
				return nil
			}
			client.onStreamN = map[int]func(){
				0: func() {
					configured.Description = "mutated after strategy selection"
					configured.Schema[0] = '['
					if tt.wantNative {
						cfg.model.Caps = model.Capabilities{Tools: true}
					} else {
						cfg.model.Caps = model.Capabilities{Tools: true, StructuredOutput: true, StructuredOutputWithTools: true}
					}
				},
			}

			terminal := runTurn(context.Background(), cfg, state)
			if _, ok := terminal.(event.TurnDone); !ok {
				t.Fatalf("terminal = %T %v, want TurnDone", terminal, terminal)
			}
			if measurementCalls != 3 || !reflect.DeepEqual(continuations, []bool{true, true, false}) {
				t.Fatalf("measurements/continuations = %d/%v, want 3/[true true false]", measurementCalls, continuations)
			}
			if client.calls != 2 || ordinary.runCount() != 1 {
				t.Fatalf("streams/tool runs = %d/%d, want 2/1", client.calls, ordinary.runCount())
			}
		})
	}
}

func TestNativeAndTerminalFinalsHaveIdenticalDurableShape(t *testing.T) {
	usage := &content.Usage{InputTokens: 9, OutputTokens: 3}
	run := func(t *testing.T, native bool) (event.TurnDone, content.AgenticMessages, []event.StepDone, []event.Event) {
		t.Helper()
		chunks := []content.Chunk{textChunk(` { "answer": "same" } `)}
		finish := stream.FinishReasonStop
		caps := model.Capabilities{StructuredOutput: true}
		if !native {
			chunks = []content.Chunk{terminalOutputChunk(0, "terminal-1", ` { "answer": "same" } `)}
			finish = stream.FinishReasonToolUse
			caps = model.Capabilities{Tools: true}
		}
		client := &scriptedLLM{
			scripts: [][]content.Chunk{chunks},
			results: []*stream.StreamResult{{Usage: usage, FinishReason: finish}},
		}
		cfg, state, recorder := newTurnFixture(nil, nil, ToolSet{}, client, noGateReg())
		cfg.model = outputModel(caps)
		output := testLoopOutput()
		cfg.output = &output
		terminal := runTurn(context.Background(), cfg, state)
		done, ok := terminal.(event.TurnDone)
		if !ok {
			t.Fatalf("terminal = %T %v, want TurnDone", terminal, terminal)
		}
		return done, recorder.committedMsgs(), stepDones(recorder.events()), recorder.events()
	}

	nativeDone, nativeMessages, nativeSteps, nativeEvents := run(t, true)
	fallbackDone, fallbackMessages, fallbackSteps, fallbackEvents := run(t, false)
	if !reflect.DeepEqual(nativeDone.Message, fallbackDone.Message) || !reflect.DeepEqual(nativeDone.Usage, fallbackDone.Usage) {
		t.Fatalf("native/fallback TurnDone differ:\nnative=%#v\nfallback=%#v", nativeDone, fallbackDone)
	}
	if !reflect.DeepEqual(nativeMessages, fallbackMessages) {
		t.Fatalf("native/fallback committed messages differ:\nnative=%#v\nfallback=%#v", nativeMessages, fallbackMessages)
	}
	if len(nativeSteps) != 1 || len(fallbackSteps) != 1 || !reflect.DeepEqual(nativeSteps[0].Messages, fallbackSteps[0].Messages) {
		t.Fatalf("native/fallback StepDone messages differ: %#v / %#v", nativeSteps, fallbackSteps)
	}
	if len(nativeEvents) != len(fallbackEvents) {
		t.Fatalf("native/fallback event counts = %d/%d", len(nativeEvents), len(fallbackEvents))
	}
	for index := range nativeEvents {
		if reflect.TypeOf(nativeEvents[index]) != reflect.TypeOf(fallbackEvents[index]) {
			t.Fatalf("native/fallback event %d types = %T/%T", index, nativeEvents[index], fallbackEvents[index])
		}
		switch nativeEvents[index].(type) {
		case event.PermissionRequested, event.PermissionDecided, event.UserInputRequested,
			event.ToolCallStarted, event.ToolCallCompleted:
			t.Fatalf("native final emitted tool/gate lifecycle event %T", nativeEvents[index])
		}
		switch fallbackEvents[index].(type) {
		case event.PermissionRequested, event.PermissionDecided, event.UserInputRequested,
			event.ToolCallStarted, event.ToolCallCompleted:
			t.Fatalf("fallback final emitted tool/gate lifecycle event %T", fallbackEvents[index])
		}
	}
	assertTerminalOutputMessage(t, nativeDone.Message, `{"answer":"same"}`)
	assertTerminalOutputMessage(t, fallbackDone.Message, `{"answer":"same"}`)
}
