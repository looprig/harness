package loopruntime

import (
	"context"
	"errors"
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
			temperature := 0.25
			topP := 0.75
			maxTokens := 128
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
			base := content.AgenticMessages{&content.AIMessage{
				Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{
					&content.TextBlock{Text: "base text"},
					&content.ToolUseBlock{ID: "base-call", Name: "Base", Input: []byte(`{"base":true}`)},
				}},
				Usage: &content.Usage{InputTokens: 7, OutputTokens: 3},
			}}
			input := []content.Block{&content.ImageBlock{
				MediaType: "image/png",
				Source:    content.ImageSource{Data: []byte{1, 2, 3}},
			}}
			cfg, state, _ := newTurnFixture(input, base, agenticToolSet([]tool.InvokableTool{ordinary}, 1, 1), client, noGateReg())
			cfg.model = outputModel(tt.caps)
			cfg.model.Sampling = model.Sampling{
				Temperature: &temperature,
				TopP:        &topP,
				MaxTokens:   &maxTokens,
				Stop:        []string{"END"},
			}
			cfg.system = "frozen system"
			runtimeDocument := &content.DocumentBlock{
				MediaType: "text/plain", Name: "runtime.txt", Data: []byte{4, 5, 6}, Text: "runtime tail",
			}
			cfg.runtimeContext = fakeRuntimeContextProvider{blocks: []content.Block{runtimeDocument}}
			configured := testLoopOutput()
			cfg.output = &configured

			measurementCalls := 0
			continuations := make([]bool, 0, 3)
			cfg.measure = func(_ context.Context, request inference.Request, _ string, runtimeTail *content.UserMessage, continuation bool) error {
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
				assertFrozenMeasuredRequest(t, request, tt.wantNative, tt.wantTools, tt.wantChoice)
				measuredRuntimeDocument := runtimeTail.Blocks[0].(*content.DocumentBlock)
				if measuredRuntimeDocument.Text != "runtime tail" || measuredRuntimeDocument.Data[0] != 4 {
					t.Fatalf("measurement %d runtime tail = %#v, want frozen values", measurementCalls, measuredRuntimeDocument)
				}
				if !continuation {
					if len(request.Messages) < 2 {
						t.Fatal("final measurement has no staged history")
					}
					assertTerminalOutputMessage(t, request.Messages[len(request.Messages)-2], `{"answer":"done"}`)
				}

				// A hostile measurement collaborator may mutate every reference-backed
				// field it receives. Neither the immediately following provider call nor
				// a later continuation may observe any of these writes.
				mutateMeasuredRequest(request)
				mutateRuntimeTail(runtimeTail)
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
			requests := client.requests()
			if len(requests) != 2 {
				t.Fatalf("provider requests = %d, want 2", len(requests))
			}
			for index := range requests {
				assertFrozenMeasuredRequest(t, requests[index], tt.wantNative, tt.wantTools, tt.wantChoice)
			}

			// Requests from separate provider calls must own independent graphs too.
			mutateMeasuredRequest(requests[0])
			assertFrozenMeasuredRequest(t, requests[1], tt.wantNative, tt.wantTools, tt.wantChoice)
			if *cfg.model.Sampling.Temperature != 0.25 || cfg.model.Sampling.Stop[0] != "END" {
				t.Fatalf("measurement mutated cfg model sampling: %#v", cfg.model.Sampling)
			}
			baseAI := base[0].(*content.AIMessage)
			if baseAI.Blocks[0].(*content.TextBlock).Text != "base text" || baseAI.Blocks[1].(*content.ToolUseBlock).Input[0] != '{' || baseAI.Usage.InputTokens != 7 {
				t.Fatalf("measurement mutated base history: %#v", baseAI)
			}
			if initialUser(state).Blocks[0].(*content.ImageBlock).Source.Data[0] != 1 {
				t.Fatalf("measurement mutated staged history: %#v", initialUser(state))
			}
			if runtimeDocument.Text != "runtime tail" || runtimeDocument.Data[0] != 4 {
				t.Fatalf("measurement mutated runtime provider storage: %#v", runtimeDocument)
			}
		})
	}
}

func TestMeasureTurnCandidateDeepClonesRequestAndRuntimeTail(t *testing.T) {
	temperature := 0.2
	topP := 0.8
	maxTokens := 64
	overrideTemperature := 0.4
	overrideTopP := 0.6
	overrideMaxTokens := 32
	typedNil := (*content.TextBlock)(nil)
	request := inference.Request{
		Model: model.Model{Sampling: model.Sampling{
			Temperature: &temperature, TopP: &topP, MaxTokens: &maxTokens, Stop: []string{"MODEL_STOP"},
		}},
		System: "system",
		Messages: content.AgenticMessages{&content.AIMessage{
			Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{
				typedNil,
				&content.ToolUseBlock{ID: "call", Name: "Tool", Input: []byte(`{"value":1}`)},
				&content.ToolResultBlock{ToolUseID: "nested", Content: []content.Block{&content.ImageBlock{Source: content.ImageSource{Data: []byte{9, 8, 7}}}}},
			}},
			Usage: &content.Usage{InputTokens: 11, OutputTokens: 5},
		}},
		Tools:  []inference.Tool{{Name: "Tool", Description: "tool", Schema: []byte(`{"type":"object"}`)}},
		Output: &inference.OutputSchema{Name: "result", Description: "result", Schema: []byte(`{"type":"object"}`), Strict: true},
		Override: &model.Sampling{
			Temperature: &overrideTemperature, TopP: &overrideTopP, MaxTokens: &overrideMaxTokens, Stop: []string{"OVERRIDE_STOP"},
		},
	}
	runtimeTail := &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{
		&content.AudioBlock{MediaType: "audio/wav", Data: []byte{6, 5, 4}},
	}}}
	directive := &contextReplacementDirective{}
	cfg := turnConfig{measure: func(_ context.Context, measured inference.Request, _ string, measuredTail *content.UserMessage, _ bool) error {
		if len(measured.Messages) != 1 || len(measured.Messages[0].(*content.AIMessage).Blocks) != 3 {
			t.Fatalf("measured message shape = %#v, want preserved", measured.Messages)
		}
		if block, ok := measured.Messages[0].(*content.AIMessage).Blocks[0].(*content.TextBlock); !ok || block != nil {
			t.Fatalf("typed-nil block = %#v, want typed nil text block", measured.Messages[0].(*content.AIMessage).Blocks[0])
		}
		mutateMeasuredRequest(measured)
		mutateRuntimeTail(measuredTail)
		return directive
	}}

	gotDirective, err := measureTurnCandidate(context.Background(), cfg, request, "revision", runtimeTail, true)
	if err != nil || gotDirective != directive {
		t.Fatalf("measureTurnCandidate() = (%p, %v), want (%p, nil)", gotDirective, err, directive)
	}
	if *request.Model.Sampling.Temperature != 0.2 || *request.Model.Sampling.TopP != 0.8 || *request.Model.Sampling.MaxTokens != 64 || request.Model.Sampling.Stop[0] != "MODEL_STOP" {
		t.Fatalf("source model sampling mutated: %#v", request.Model.Sampling)
	}
	if *request.Override.Temperature != 0.4 || *request.Override.TopP != 0.6 || *request.Override.MaxTokens != 32 || request.Override.Stop[0] != "OVERRIDE_STOP" {
		t.Fatalf("source override mutated: %#v", request.Override)
	}
	if request.Tools[0].Name != "Tool" || request.Tools[0].Schema[0] != '{' || request.Output.Name != "result" || request.Output.Schema[0] != '{' {
		t.Fatalf("source tool/output mutated: tools=%#v output=%#v", request.Tools, request.Output)
	}
	ai := request.Messages[0].(*content.AIMessage)
	if ai.Blocks[1].(*content.ToolUseBlock).Name != "Tool" || ai.Blocks[1].(*content.ToolUseBlock).Input[0] != '{' || ai.Usage.InputTokens != 11 {
		t.Fatalf("source message mutated: %#v", ai)
	}
	nestedImage := ai.Blocks[2].(*content.ToolResultBlock).Content[0].(*content.ImageBlock)
	if nestedImage.Source.Data[0] != 9 {
		t.Fatalf("source nested message block mutated: %#v", nestedImage)
	}
	if runtimeTail.Blocks[0].(*content.AudioBlock).Data[0] != 6 {
		t.Fatalf("source runtime tail mutated: %#v", runtimeTail)
	}

	wantErr := errors.New("measurement failed")
	cfg.measure = func(context.Context, inference.Request, string, *content.UserMessage, bool) error { return wantErr }
	if _, err := measureTurnCandidate(context.Background(), cfg, inference.Request{}, "", nil, false); !errors.Is(err, wantErr) {
		t.Fatalf("measureTurnCandidate() error = %v, want %v", err, wantErr)
	}
}

func TestMeasureTurnCandidateClonePreservesNilAndEmptySlices(t *testing.T) {
	tests := []struct {
		name     string
		messages content.AgenticMessages
		tools    []inference.Tool
	}{
		{name: "nil"},
		{name: "empty", messages: content.AgenticMessages{}, tools: []inference.Tool{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := turnConfig{measure: func(_ context.Context, request inference.Request, _ string, tail *content.UserMessage, _ bool) error {
				if (request.Messages == nil) != (tt.messages == nil) || (request.Tools == nil) != (tt.tools == nil) {
					t.Fatalf("nil preservation messages/tools = %v/%v, want %v/%v", request.Messages == nil, request.Tools == nil, tt.messages == nil, tt.tools == nil)
				}
				if tail != nil {
					t.Fatalf("nil runtime tail cloned as %#v", tail)
				}
				return nil
			}}
			if _, err := measureTurnCandidate(context.Background(), cfg, inference.Request{Messages: tt.messages, Tools: tt.tools}, "", nil, true); err != nil {
				t.Fatalf("measureTurnCandidate() error = %v", err)
			}
		})
	}
}

func mutateMeasuredRequest(request inference.Request) {
	if request.Model.Sampling.Temperature != nil {
		*request.Model.Sampling.Temperature = 9
	}
	if request.Model.Sampling.TopP != nil {
		*request.Model.Sampling.TopP = 9
	}
	if request.Model.Sampling.MaxTokens != nil {
		*request.Model.Sampling.MaxTokens = 9
	}
	if len(request.Model.Sampling.Stop) > 0 {
		request.Model.Sampling.Stop[0] = "MUTATED_MODEL_STOP"
	}
	for _, message := range request.Messages {
		switch typed := message.(type) {
		case *content.UserMessage:
			mutateBlocks(typed.Blocks)
		case *content.AIMessage:
			mutateBlocks(typed.Blocks)
			if typed.Usage != nil {
				typed.Usage.InputTokens = 999
			}
		case *content.SystemMessage:
			mutateBlocks(typed.Blocks)
		case *content.ToolResultMessage:
			mutateBlocks(typed.Blocks)
		}
	}
	for index := range request.Tools {
		request.Tools[index].Name = "mutated-tool"
		if len(request.Tools[index].Schema) > 0 {
			request.Tools[index].Schema[0] = '['
		}
	}
	if request.Output != nil {
		request.Output.Name = "mutated-output"
		if len(request.Output.Schema) > 0 {
			request.Output.Schema[0] = '['
		}
	}
	if request.Override != nil {
		if request.Override.Temperature != nil {
			*request.Override.Temperature = 9
		}
		if request.Override.TopP != nil {
			*request.Override.TopP = 9
		}
		if request.Override.MaxTokens != nil {
			*request.Override.MaxTokens = 9
		}
		if len(request.Override.Stop) > 0 {
			request.Override.Stop[0] = "MUTATED_OVERRIDE_STOP"
		}
	}
}

func mutateRuntimeTail(runtimeTail *content.UserMessage) {
	if runtimeTail != nil {
		mutateBlocks(runtimeTail.Blocks)
	}
}

func mutateBlocks(blocks []content.Block) {
	for _, block := range blocks {
		switch typed := block.(type) {
		case *content.TextBlock:
			if typed != nil {
				typed.Text = "mutated text"
			}
		case *content.ImageBlock:
			if typed != nil && len(typed.Source.Data) > 0 {
				typed.Source.Data[0] = 0
			}
		case *content.AudioBlock:
			if typed != nil && len(typed.Data) > 0 {
				typed.Data[0] = 0
			}
		case *content.DocumentBlock:
			if typed != nil {
				typed.Text = "mutated runtime"
				if len(typed.Data) > 0 {
					typed.Data[0] = 0
				}
			}
		case *content.ToolUseBlock:
			if typed != nil {
				typed.Name = "mutated block tool"
				if len(typed.Input) > 0 {
					typed.Input[0] = '['
				}
			}
		case *content.ToolResultBlock:
			if typed != nil {
				mutateBlocks(typed.Content)
			}
		}
	}
}

func assertFrozenMeasuredRequest(t *testing.T, request inference.Request, wantNative bool, wantTools []string, wantChoice inference.ToolChoice) {
	t.Helper()
	if request.System != "frozen system" || request.ToolChoice != wantChoice {
		t.Fatalf("provider system/tool choice = %q/%v, want frozen values", request.System, request.ToolChoice)
	}
	if request.Model.Sampling.Temperature == nil || *request.Model.Sampling.Temperature != 0.25 || request.Model.Sampling.Stop[0] != "END" {
		t.Fatalf("provider model sampling = %#v, want frozen values", request.Model.Sampling)
	}
	gotTools := make([]string, len(request.Tools))
	for index := range request.Tools {
		gotTools[index] = request.Tools[index].Name
		if len(request.Tools[index].Schema) == 0 || request.Tools[index].Schema[0] != '{' {
			t.Fatalf("provider tool %d schema = %q, want object", index, request.Tools[index].Schema)
		}
	}
	if !reflect.DeepEqual(gotTools, wantTools) {
		t.Fatalf("provider tools = %v, want %v", gotTools, wantTools)
	}
	if (request.Output != nil) != wantNative {
		t.Fatalf("provider native output present = %v, want %v", request.Output != nil, wantNative)
	}
	if request.Output != nil && (request.Output.Name != testLoopOutput().Name || request.Output.Schema[0] != '{') {
		t.Fatalf("provider native output = %#v, want frozen policy", request.Output)
	}
	if len(request.Messages) < 3 {
		t.Fatalf("provider messages = %d, want base/staged/runtime", len(request.Messages))
	}
	base := request.Messages[0].(*content.AIMessage)
	if base.Blocks[0].(*content.TextBlock).Text != "base text" || base.Blocks[1].(*content.ToolUseBlock).Name != "Base" || base.Usage.InputTokens != 7 {
		t.Fatalf("provider base message = %#v, want frozen values", base)
	}
	initial := request.Messages[1].(*content.UserMessage)
	if initial.Blocks[0].(*content.ImageBlock).Source.Data[0] != 1 {
		t.Fatalf("provider staged input = %#v, want frozen image", initial)
	}
	tail := request.Messages[len(request.Messages)-1].(*content.UserMessage)
	document := tail.Blocks[0].(*content.DocumentBlock)
	if document.Text != "runtime tail" || document.Data[0] != 4 {
		t.Fatalf("provider runtime tail = %#v, want frozen values", document)
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
