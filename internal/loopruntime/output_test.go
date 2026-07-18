package loopruntime

import (
	"context"
	"encoding/json"
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

func testLoopOutput() inference.OutputSchema {
	return inference.OutputSchema{
		Name:        "turn_result",
		Description: "The final result.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`),
		Strict:      true,
	}
}

func outputModel(caps model.Capabilities) model.Model {
	configured := testModel()
	configured.Caps = caps
	return configured
}

func TestResolveTurnOutputStrategy(t *testing.T) {
	t.Parallel()
	output := testLoopOutput()
	ordinary := []inference.Tool{{Name: "Read", Description: "read", Schema: json.RawMessage(`{"type":"object"}`)}}
	tests := []struct {
		name       string
		output     *inference.OutputSchema
		tools      []inference.Tool
		caps       model.Capabilities
		want       outputStrategy
		wantOutput bool
		wantTools  []string
		wantChoice inference.ToolChoice
		wantErr    bool
	}{
		{name: "no configured output", tools: ordinary, want: outputStrategyNone, wantTools: []string{"Read"}, wantChoice: inference.ToolChoiceAuto},
		{name: "output only native", output: &output, caps: model.Capabilities{StructuredOutput: true}, want: outputStrategyNative, wantOutput: true, wantChoice: inference.ToolChoiceAuto},
		{name: "ordinary tools and native combined", output: &output, tools: ordinary, caps: model.Capabilities{Tools: true, StructuredOutput: true, StructuredOutputWithTools: true}, want: outputStrategyNative, wantOutput: true, wantTools: []string{"Read"}, wantChoice: inference.ToolChoiceAuto},
		{name: "ordinary tools terminal fallback despite base native", output: &output, tools: ordinary, caps: model.Capabilities{Tools: true, StructuredOutput: true}, want: outputStrategyTerminalTool, wantTools: []string{"Read", inference.StructuredOutputToolName}, wantChoice: inference.ToolChoiceRequired},
		{name: "output only terminal fallback", output: &output, caps: model.Capabilities{Tools: true}, want: outputStrategyTerminalTool, wantTools: []string{inference.StructuredOutputToolName}, wantChoice: inference.ToolChoiceRequired},
		{name: "unsupported", output: &output, wantErr: true},
		{name: "invalid combined capabilities", output: &output, caps: model.Capabilities{StructuredOutputWithTools: true}, wantErr: true},
		{name: "reserved terminal tool collision", output: &output, tools: []inference.Tool{{Name: inference.StructuredOutputToolName}}, caps: model.Capabilities{Tools: true}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plan, err := resolveTurnOutput(outputModel(tt.caps), tt.output, tt.tools)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveTurnOutput() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.name == "unsupported" {
					var target *inference.StructuredOutputUnsupportedError
					if !errors.As(err, &target) {
						t.Fatalf("error = %T, want StructuredOutputUnsupportedError", err)
					}
				}
				return
			}
			if plan.strategy != tt.want {
				t.Fatalf("strategy = %v, want %v", plan.strategy, tt.want)
			}
			req := plan.apply(inference.Request{})
			if (req.Output != nil) != tt.wantOutput {
				t.Fatalf("Output present = %v, want %v", req.Output != nil, tt.wantOutput)
			}
			if req.ToolChoice != tt.wantChoice {
				t.Fatalf("ToolChoice = %v, want %v", req.ToolChoice, tt.wantChoice)
			}
			gotNames := make([]string, len(req.Tools))
			for i := range req.Tools {
				gotNames[i] = req.Tools[i].Name
			}
			if len(gotNames) != len(tt.wantTools) {
				t.Fatalf("tool names = %v, want %v", gotNames, tt.wantTools)
			}
			for i := range gotNames {
				if gotNames[i] != tt.wantTools[i] {
					t.Fatalf("tool names = %v, want %v", gotNames, tt.wantTools)
				}
			}
		})
	}
}

func TestTerminalOutputToolUsesReservedControlIdentityAndClonesSchema(t *testing.T) {
	t.Parallel()
	output := testLoopOutput()
	output.Name = "public_result_name"
	toolDefinition := terminalOutputTool(output)
	if toolDefinition.Name != inference.StructuredOutputToolName {
		t.Fatalf("terminal tool name = %q, want reserved control name", toolDefinition.Name)
	}
	if toolDefinition.Description != output.Description {
		t.Fatalf("terminal tool description = %q, want %q", toolDefinition.Description, output.Description)
	}
	if !reflect.DeepEqual(toolDefinition.Schema, output.Schema) {
		t.Fatalf("terminal tool schema = %s, want %s", toolDefinition.Schema, output.Schema)
	}
	toolDefinition.Schema[0] = '['
	if output.Schema[0] == '[' {
		t.Fatal("terminal tool schema aliases output schema")
	}

	// Empty descriptions are valid for ordinary model-facing tools, so the
	// fallback preserves that convention instead of inventing policy text.
	output.Description = ""
	if got := terminalOutputTool(output).Description; got != "" {
		t.Fatalf("empty output description became %q, want empty", got)
	}
}

func TestResolveTurnOutputRejectsReservedOrdinaryToolWithoutOutput(t *testing.T) {
	t.Parallel()
	_, err := resolveTurnOutput(testModel(), nil, []inference.Tool{{Name: inference.StructuredOutputToolName}})
	var conflict *inference.StructuredOutputConflictError
	if !errors.As(err, &conflict) || conflict.Feature != "reserved_structured_output_tool" {
		t.Fatalf("resolveTurnOutput() error = %T %v, want reserved-name conflict", err, err)
	}
}

func TestResolveTurnOutputRejectsInvalidSchemaWithoutRawSchema(t *testing.T) {
	t.Parallel()
	const secret = "terminal-schema-secret"
	tests := []struct {
		name   string
		schema json.RawMessage
	}{
		{name: "nil", schema: nil},
		{name: "malformed", schema: json.RawMessage(`{"type":"object","` + secret)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			output := testLoopOutput()
			output.Schema = tt.schema
			_, err := resolveTurnOutput(outputModel(model.Capabilities{Tools: true}), &output, nil)
			var schemaErr *inference.SchemaValidationError
			if !errors.As(err, &schemaErr) {
				t.Fatalf("resolveTurnOutput() error = %T %v, want SchemaValidationError", err, err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("schema validation error exposed raw schema: %v", err)
			}
		})
	}
}

func TestTerminalOutputPlanDoesNotMutateExecutableToolRegistry(t *testing.T) {
	t.Parallel()
	ordinary := &echoTool{name: "Echo", output: "ok"}
	registry := []tool.InvokableTool{ordinary}
	defs := toolDefs(context.Background(), registry)
	output := testLoopOutput()
	plan, err := resolveTurnOutput(outputModel(model.Capabilities{Tools: true}), &output, defs)
	if err != nil {
		t.Fatalf("resolveTurnOutput() error = %v", err)
	}
	request := plan.apply(inference.Request{})
	if len(request.Tools) != 2 || request.Tools[1].Name != inference.StructuredOutputToolName {
		t.Fatalf("model-facing tools = %#v, want ordinary then terminal", request.Tools)
	}
	if len(registry) != 1 || registry[0] != ordinary {
		t.Fatalf("executable registry mutated: %#v", registry)
	}
	if lookupTool(context.Background(), registry, inference.StructuredOutputToolName) != nil {
		t.Fatal("reserved terminal definition became executable")
	}
}

func TestTurnOutputPlanFreezesAndClonesRequestShape(t *testing.T) {
	t.Parallel()
	output := testLoopOutput()
	tools := []inference.Tool{{Name: "Read", Description: "read", Schema: json.RawMessage(`{"type":"object"}`)}}
	plan, err := resolveTurnOutput(outputModel(model.Capabilities{Tools: true}), &output, tools)
	if err != nil {
		t.Fatalf("resolveTurnOutput: %v", err)
	}

	output.Description = "mutated"
	output.Schema[0] = '['
	tools[0].Name = "mutated"
	tools[0].Schema[0] = '['

	first := plan.apply(inference.Request{})
	second := plan.apply(inference.Request{})
	if first.Output != nil || second.Output != nil {
		t.Fatal("terminal fallback unexpectedly carried native Output")
	}
	if !reflect.DeepEqual(first.Tools, second.Tools) || first.ToolChoice != second.ToolChoice {
		t.Fatalf("continuation shape differs: first=%#v second=%#v", first, second)
	}
	if got := []string{first.Tools[0].Name, first.Tools[1].Name}; !reflect.DeepEqual(got, []string{"Read", inference.StructuredOutputToolName}) {
		t.Fatalf("frozen tool names = %v", got)
	}
	if string(first.Tools[1].Schema) != string(testLoopOutput().Schema) || first.Tools[1].Description != testLoopOutput().Description {
		t.Fatalf("terminal tool did not freeze configured output: %#v", first.Tools[1])
	}
	first.Tools[0].Schema[0] = '['
	first.Tools[1].Schema[0] = '['
	if second.Tools[0].Schema[0] == '[' || second.Tools[1].Schema[0] == '[' {
		t.Fatal("request tool schemas alias across continuations")
	}
	if &first.Tools[0] == &second.Tools[0] {
		t.Fatal("request tool slices alias across continuations")
	}
}

func TestRunTurnOutputStrategyIsStableAcrossContinuation(t *testing.T) {
	t.Parallel()
	ordinary := &echoTool{name: "Echo", output: "done"}
	client := &scriptedLLM{
		scripts: [][]content.Chunk{
			{toolUseChunk(0, "call-1", "Echo", `{}`)},
			{textChunk(`{"answer":"done"}`)},
		},
		results: []*stream.StreamResult{
			{FinishReason: stream.FinishReasonToolUse},
			{FinishReason: stream.FinishReasonStop},
		},
	}
	cfg, state, _ := newTurnFixture(nil, nil, ToolSet{
		Registry:            []tool.InvokableTool{ordinary},
		MaxToolIterations:   2,
		MaxToolCallsPerTurn: 2,
	}, client, nil)
	cfg.model = outputModel(model.Capabilities{Tools: true, StructuredOutput: true, StructuredOutputWithTools: true})
	configured := testLoopOutput()
	cfg.output = &configured
	client.onStreamN = map[int]func(){
		0: func() {
			configured.Schema[0] = '['
			cfg.model.Caps = model.Capabilities{Tools: true}
		},
	}
	terminal := runTurn(context.Background(), cfg, state)
	if _, ok := terminal.(event.TurnDone); !ok {
		t.Fatalf("terminal = %T %v, want TurnDone", terminal, terminal)
	}
	requests := client.requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	for i, request := range requests {
		if request.Output == nil || request.Output.Name != configured.Name || request.ToolChoice != inference.ToolChoiceAuto {
			t.Fatalf("request %d output shape = %#v", i, request)
		}
		if len(request.Tools) != 1 || request.Tools[0].Name != "Echo" {
			t.Fatalf("request %d tools = %#v", i, request.Tools)
		}
		if string(request.Output.Schema) != string(testLoopOutput().Schema) {
			t.Fatalf("request %d schema changed after turn source mutation", i)
		}
	}
	requests[0].Output.Schema[0] = '['
	requests[0].Tools[0].Schema[0] = '['
	if requests[1].Output.Schema[0] == '[' || requests[1].Tools[0].Schema[0] == '[' {
		t.Fatal("continuation requests alias schema bytes")
	}
}

func TestRunTurnUnsupportedOutputFailsBeforeClientIO(t *testing.T) {
	t.Parallel()
	client := &scriptedLLM{scripts: [][]content.Chunk{{textChunk(`{"answer":"unused"}`)}}}
	cfg, state, _ := newTurnFixture(nil, nil, ToolSet{}, client, nil)
	output := testLoopOutput()
	cfg.output = &output
	terminal := runTurn(context.Background(), cfg, state)
	failed, ok := terminal.(event.TurnFailed)
	if !ok {
		t.Fatalf("terminal = %T %v, want TurnFailed", terminal, terminal)
	}
	var target *inference.StructuredOutputUnsupportedError
	if !errors.As(failed.Err, &target) {
		t.Fatalf("error = %T %v, want StructuredOutputUnsupportedError", failed.Err, failed.Err)
	}
	if got := len(client.requests()); got != 0 {
		t.Fatalf("client requests = %d, want 0", got)
	}
}
