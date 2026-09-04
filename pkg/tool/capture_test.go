package tool

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// streamingTool is the reference capturing tool: it writes its complete raw
// result to the sink and returns a bounded model-facing ToolResult.
type streamingTool struct{ raw string }

func (streamingTool) Info(ctx context.Context) (*ToolInfo, error) { return &ToolInfo{}, nil }

func (s streamingTool) InvokableRun(ctx context.Context, argsJSON string) (*ToolResult, error) {
	return TextResult(s.raw), nil
}

func (s streamingTool) InvokableRunCaptured(ctx context.Context, argsJSON string, sink ResultCaptureSink) (*ToolResult, error) {
	if _, err := io.WriteString(sink, s.raw); err != nil {
		return nil, err
	}
	return TextResult(s.raw[:min(len(s.raw), 4)]), nil
}

// TestResultCaptureSinkIsAnIOWriter pins the declared shape: the sink is exactly
// io.Writer, so any *bytes.Buffer, file or counting writer satisfies it without
// importing this package's internals.
func TestResultCaptureSinkIsAnIOWriter(t *testing.T) {
	t.Parallel()
	var sink ResultCaptureSink = &bytes.Buffer{}
	var writer io.Writer = sink
	if _, err := io.WriteString(writer, "x"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if got := sink.(*bytes.Buffer).String(); got != "x" {
		t.Fatalf("buffer = %q, want %q", got, "x")
	}
}

// TestCapturingInvokableToolIsProbedFromInvokableTool holds the capability at the
// layer that has the reader: the runner holds an InvokableTool and reaches the
// streaming path only by type assertion, so a tool that does NOT implement the
// capability must fail that assertion while remaining a valid InvokableTool.
func TestCapturingInvokableToolIsProbedFromInvokableTool(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tool InvokableTool
		want bool
	}{
		{name: "streaming tool", tool: streamingTool{raw: "abcdefgh"}, want: true},
		{name: "materialized tool", tool: fakeTool{name: "plain"}, want: false},
		{name: "tool implementing every other optional", tool: everyOtherOptionalTool{}, want: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			capturing, ok := tt.tool.(CapturingInvokableTool)
			if ok != tt.want {
				t.Fatalf("CapturingInvokableTool assertion = %v, want %v", ok, tt.want)
			}
			if !ok {
				return
			}
			var sink bytes.Buffer
			result, err := capturing.InvokableRunCaptured(context.Background(), `{}`, &sink)
			if err != nil {
				t.Fatalf("InvokableRunCaptured: %v", err)
			}
			if got := sink.String(); got != "abcdefgh" {
				t.Fatalf("captured = %q, want the complete raw stream", got)
			}
			if len(result.Content) != 1 {
				t.Fatalf("result blocks = %d, want 1", len(result.Content))
			}
		})
	}
}

// TestInvokableToolIsUnchanged pins the additive requirement from the other
// direction: the streaming capability must not have been folded into
// InvokableTool, so a tool implementing ONLY InvokableRun still satisfies it.
func TestInvokableToolIsUnchanged(t *testing.T) {
	t.Parallel()
	var _ InvokableTool = fakeTool{name: "plain"}
	var _ InvokableTool = streamingTool{raw: "x"}
}

// TestProjectCaptureSafetyClassifiesEveryDefinition enumerates the whole
// declaration space — undeclared, declared-materialized, declared-streaming,
// each crossed with high-output — rather than pinning one row, because the
// descriptor's only job is to answer "for all definitions" questions.
func TestProjectCaptureSafetyClassifiesEveryDefinition(t *testing.T) {
	t.Parallel()
	defs := []Definition{
		NewDefinition("undeclared", 0, nil),
		declaredDefinition{Definition: NewDefinition("materialized-low", 0, nil)},
		declaredDefinition{Definition: NewDefinition("materialized-high", 0, nil), declared: DeclaredCaptureSafety{HighOutput: true}},
		declaredDefinition{Definition: NewDefinition("streaming-low", 0, nil), declared: DeclaredCaptureSafety{Streaming: true}},
		declaredDefinition{Definition: NewDefinition("streaming-high", 0, nil), declared: DeclaredCaptureSafety{Streaming: true, HighOutput: true}},
	}
	descriptor := ProjectCaptureSafety(defs, 32<<20)
	want := []ToolCaptureSafety{
		{Definition: "materialized-high", Tools: []string{"materialized-high"}, Class: CaptureClassMaterialized, HighOutput: true},
		{Definition: "materialized-low", Tools: []string{"materialized-low"}, Class: CaptureClassMaterialized},
		{Definition: "streaming-high", Tools: []string{"streaming-high"}, Class: CaptureClassStreaming, HighOutput: true},
		{Definition: "streaming-low", Tools: []string{"streaming-low"}, Class: CaptureClassStreaming},
		{Definition: "undeclared", Tools: []string{"undeclared"}, Class: CaptureClassMaterialized},
	}
	if len(descriptor.Definitions) != len(want) {
		t.Fatalf("definitions = %d, want %d", len(descriptor.Definitions), len(want))
	}
	for i := range want {
		got := descriptor.Definitions[i]
		if got.Definition != want[i].Definition || got.Class != want[i].Class || got.HighOutput != want[i].HighOutput {
			t.Errorf("row %d = %+v, want %+v", i, got, want[i])
		}
		if strings.Join(got.Tools, ",") != strings.Join(want[i].Tools, ",") {
			t.Errorf("row %d tools = %v, want %v", i, got.Tools, want[i].Tools)
		}
	}
	if descriptor.MaterializedMaxBytes != 32<<20 {
		t.Errorf("MaterializedMaxBytes = %d, want %d", descriptor.MaterializedMaxBytes, 32<<20)
	}
}

// TestCaptureSafetyRuleHoldsForEveryCombination derives the rule's truth table
// from the mechanism instead of asserting one safe descriptor: a high-output
// definition is safe when it streams OR when a finite materialized maximum is
// declared, and unsafe only when neither holds.
func TestCaptureSafetyRuleHoldsForEveryCombination(t *testing.T) {
	t.Parallel()
	for _, maxBytes := range []int{-1, 0, 1, 32 << 20} {
		for _, streaming := range []bool{false, true} {
			for _, highOutput := range []bool{false, true} {
				defs := []Definition{declaredDefinition{
					Definition: NewDefinition("t", 0, nil),
					declared:   DeclaredCaptureSafety{Streaming: streaming, HighOutput: highOutput},
				}}
				descriptor := ProjectCaptureSafety(defs, maxBytes)
				wantUnsafe := highOutput && !streaming && maxBytes <= 0
				if got := descriptor.Safe(); got == wantUnsafe {
					t.Errorf("max=%d streaming=%v high=%v: Safe = %v, want %v", maxBytes, streaming, highOutput, got, !wantUnsafe)
				}
				unsafe := descriptor.Unsafe()
				if wantUnsafe && (len(unsafe) != 1 || unsafe[0] != "t") {
					t.Errorf("max=%d streaming=%v high=%v: Unsafe = %v, want [t]", maxBytes, streaming, highOutput, unsafe)
				}
				if !wantUnsafe && len(unsafe) != 0 {
					t.Errorf("max=%d streaming=%v high=%v: Unsafe = %v, want none", maxBytes, streaming, highOutput, unsafe)
				}
			}
		}
	}
}

// everyOtherOptionalTool implements every optional capability EXCEPT the
// streaming one, so the probe cannot be satisfied by a merely "rich" tool.
type everyOtherOptionalTool struct{ capableTool }

func (everyOtherOptionalTool) InvokableRunCaptured() {}

// TestProjectCaptureSafetyMergesADuplicateNameConservatively pins the merge rule
// in the direction that can lose safety: a name contributed once as streaming
// and once as materialized must project as materialized, and the ORDER of the
// two contributions must not change the answer.
func TestProjectCaptureSafetyMergesADuplicateNameConservatively(t *testing.T) {
	t.Parallel()
	streaming := declaredDefinition{Definition: NewBundleDefinition("dup", []string{"a"}, 0, nil), declared: DeclaredCaptureSafety{Streaming: true}}
	materialized := declaredDefinition{Definition: NewBundleDefinition("dup", []string{"b"}, 0, nil), declared: DeclaredCaptureSafety{HighOutput: true}}
	for _, order := range [][]Definition{{streaming, materialized}, {materialized, streaming}} {
		descriptor := ProjectCaptureSafety(order, 0)
		if len(descriptor.Definitions) != 1 {
			t.Fatalf("definitions = %d, want 1 merged row", len(descriptor.Definitions))
		}
		row := descriptor.Definitions[0]
		if row.Class != CaptureClassMaterialized {
			t.Errorf("class = %q, want %q", row.Class, CaptureClassMaterialized)
		}
		if !row.HighOutput {
			t.Error("HighOutput = false, want true (any contributor declaring it wins)")
		}
		if strings.Join(row.Tools, ",") != "a,b" && strings.Join(row.Tools, ",") != "b,a" {
			t.Errorf("tools = %v, want both produced names", row.Tools)
		}
		if descriptor.Safe() {
			t.Error("Safe = true, want false: a high-output materialized row with no finite maximum")
		}
	}
}

// TestProjectCaptureSafetySkipsNilDefinitions pins the nil handling against the
// alternative that would silently add an unnamed materialized row.
func TestProjectCaptureSafetySkipsNilDefinitions(t *testing.T) {
	t.Parallel()
	descriptor := ProjectCaptureSafety([]Definition{nil, NewDefinition("real", 0, nil), nil}, 1)
	if len(descriptor.Definitions) != 1 || descriptor.Definitions[0].Definition != "real" {
		t.Fatalf("definitions = %+v, want exactly the real one", descriptor.Definitions)
	}
}

// declaredDefinition wraps a Definition with an explicit capture-safety
// declaration, which is how a tools module opts into the descriptor.
type declaredDefinition struct {
	Definition
	declared DeclaredCaptureSafety
}

func (d declaredDefinition) DeclaredCaptureSafety() DeclaredCaptureSafety { return d.declared }
