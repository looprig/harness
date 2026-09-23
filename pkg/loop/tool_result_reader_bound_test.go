package loop

import (
	"context"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

type readerBoundTestTool struct{ name string }

func (r readerBoundTestTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: r.name, Schema: []byte(`{"type":"object"}`)}, nil
}

func (readerBoundTestTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult(""), nil
}

type readerBoundTestReader struct{}

func (readerBoundTestReader) ReadToolResult(context.Context, tool.ToolResultPageRequest) (tool.ToolResultPage, error) {
	return tool.ToolResultPage{}, nil
}

// TestBoundModeToolResultReaderBoundRequiresTheRequirementBit pins that the
// marker's licence is the requirement, not the name: a tool NAMED
// read_tool_result whose definition did not declare RequiresToolResultReader
// was never handed a reader, so its mode does not report one.
func TestBoundModeToolResultReaderBoundRequiresTheRequirementBit(t *testing.T) {
	t.Parallel()
	named := func(name string, requirements tool.Requirements) tool.Definition {
		return tool.NewDefinition(name, requirements, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{readerBoundTestTool{name: name}}, nil
		})
	}
	tests := []struct {
		name  string
		tools []tool.Definition
		want  bool
	}{
		{name: "declared reader", tools: []tool.Definition{named(ReadToolResultToolName, tool.RequiresToolResultReader)}, want: true},
		{name: "same name without the bit", tools: []tool.Definition{named(ReadToolResultToolName, 0)}},
		{name: "bit on another name", tools: []tool.Definition{named("other_reader", tool.RequiresToolResultReader)}},
		{name: "no reader", tools: []tool.Definition{named("Big", 0)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			definition, err := Define(WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithTools(tt.tools...),
				WithModes(Mode{Name: "focused", Tools: tt.tools}), WithInitialMode("focused"))
			if err != nil {
				t.Fatalf("Define: %v", err)
			}
			bound, err := definition.Bind(context.Background(), tool.Bindings{
				SessionID: uuid.MustParse("0192d1c4-6d7e-7abc-8def-0123456789ab"), LoopID: uuid.MustParse("0192d1c4-6d7e-7abc-8def-0123456789ac"),
				ToolResults: readerBoundTestReader{},
			})
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			for _, mode := range bound.Modes() {
				if got := mode.ToolResultReaderBound(); got != tt.want {
					t.Errorf("mode %q ToolResultReaderBound = %v, want %v", mode.Name, got, tt.want)
				}
			}
		})
	}
}
