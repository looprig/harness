package loop

import (
	"context"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// resolveCapTests exercises the per-field runaway-guard resolvers the same way
// TestResolveDrainTimeout exercises resolveDrainTimeout: zero/negative default,
// positive preserved.
func TestResolveMaxToolIterations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"zero defaults", 0, defaultMaxToolIterations},
		{"negative defaults", -1, defaultMaxToolIterations},
		{"positive preserved", 7, 7},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveMaxToolIterations(tt.in); got != tt.want {
				t.Errorf("resolveMaxToolIterations(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestResolveMaxToolCallsPerTurn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"zero defaults", 0, defaultMaxToolCallsPerTurn},
		{"negative defaults", -1, defaultMaxToolCallsPerTurn},
		{"positive preserved", 42, 42},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveMaxToolCallsPerTurn(tt.in); got != tt.want {
				t.Errorf("resolveMaxToolCallsPerTurn(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestResolveMaxParallelToolCalls(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"zero defaults", 0, defaultMaxParallelToolCalls},
		{"negative defaults", -1, defaultMaxParallelToolCalls},
		{"positive preserved", 3, 3},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveMaxParallelToolCalls(tt.in); got != tt.want {
				t.Errorf("resolveMaxParallelToolCalls(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestResolveToolSetCaps verifies the exact resolution New applies to ToolSet:
// each zero (or negative) runaway-guard field is defaulted, non-zero fields are
// preserved. The resolved caps live inside the actor goroutine's cfg copy and
// are not otherwise observable, so we test resolveToolSetCaps (the helper New
// calls) directly, then assert end-to-end acceptance in TestNewAppliesToolSetDefaults.
func TestResolveToolSetCaps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   ToolSet
		want ToolSet
	}{
		{
			name: "zero value gets all three defaults",
			in:   ToolSet{},
			want: ToolSet{
				MaxToolIterations:    defaultMaxToolIterations,
				MaxToolCallsPerTurn:  defaultMaxToolCallsPerTurn,
				MaxParallelToolCalls: defaultMaxParallelToolCalls,
			},
		},
		{
			name: "all non-zero preserved",
			in: ToolSet{
				MaxToolIterations:    5,
				MaxToolCallsPerTurn:  9,
				MaxParallelToolCalls: 2,
			},
			want: ToolSet{
				MaxToolIterations:    5,
				MaxToolCallsPerTurn:  9,
				MaxParallelToolCalls: 2,
			},
		},
		{
			name: "mixed: only zero fields defaulted",
			in: ToolSet{
				MaxToolIterations:   11,
				MaxToolCallsPerTurn: 0,
			},
			want: ToolSet{
				MaxToolIterations:    11,
				MaxToolCallsPerTurn:  defaultMaxToolCallsPerTurn,
				MaxParallelToolCalls: defaultMaxParallelToolCalls,
			},
		},
		{
			name: "negative treated as unset",
			in: ToolSet{
				MaxToolIterations:    -1,
				MaxToolCallsPerTurn:  -1,
				MaxParallelToolCalls: -1,
			},
			want: ToolSet{
				MaxToolIterations:    defaultMaxToolIterations,
				MaxToolCallsPerTurn:  defaultMaxToolCallsPerTurn,
				MaxParallelToolCalls: defaultMaxParallelToolCalls,
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolveToolSetCaps(tt.in)
			if got.MaxToolIterations != tt.want.MaxToolIterations {
				t.Errorf("MaxToolIterations = %d, want %d", got.MaxToolIterations, tt.want.MaxToolIterations)
			}
			if got.MaxToolCallsPerTurn != tt.want.MaxToolCallsPerTurn {
				t.Errorf("MaxToolCallsPerTurn = %d, want %d", got.MaxToolCallsPerTurn, tt.want.MaxToolCallsPerTurn)
			}
			if got.MaxParallelToolCalls != tt.want.MaxParallelToolCalls {
				t.Errorf("MaxParallelToolCalls = %d, want %d", got.MaxParallelToolCalls, tt.want.MaxParallelToolCalls)
			}
		})
	}
}

// TestNewAppliesToolSetDefaults drives New end-to-end with a zero-value ToolSet
// and confirms the loop starts and runs a turn (the zero ToolSet must be a valid
// Config — New must not reject it, and Permission/Registry/Middlewares nil is
// valid). It complements the resolver unit tests above.
func TestNewAppliesToolSetDefaults(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	// Config carries a fully zero-value ToolSet: nil Permission/Registry/
	// Middlewares and zero caps. New must accept it and the loop must run.
	l, err := New(ctx, id, Config{
		Client:       &fakeLLM{chunks: []content.Chunk{textChunk("hi")}},
		Model:        llm.ModelSpec{Model: "m"},
		DrainTimeout: 200 * time.Millisecond,
		Tools:        ToolSet{},
	})
	if err != nil {
		t.Fatalf("New with zero ToolSet = %v, want nil", err)
	}
	ev, _ := startTurn(t, l, context.Background(), nil)
	if _, ok := drainToTerminal(t, ev).(event.TurnDone); !ok {
		t.Fatal("turn did not complete to TurnDone with zero-value ToolSet")
	}
}

// compile-time assertions that the deps.go interfaces are satisfiable and the
// ToolSet field types reference internal/tool (layering check).
var (
	_ = ToolSet{
		Permission:  PermissionGate(nil),
		Registry:    []tool.InvokableTool(nil),
		Middlewares: []tool.ToolMiddleware(nil),
	}
	_ = ToolPolicy{Tool: "x", Effect: EffectAsk, Match: []string{"*"}}
	_ ReadGuard      = readGuardStub{}
	_ PermissionGate = permissionGateStub{}
)

type readGuardStub struct{}

func (readGuardStub) DeniedRead(string) bool { return false }
func (readGuardStub) MaxReadBytes() int64    { return 0 }

type permissionGateStub struct{}

func (permissionGateStub) Check(context.Context, tool.InvokableTool, string, string) Effect {
	return EffectAsk
}
func (permissionGateStub) Grant(context.Context, string, string, tool.ApprovalScope) error {
	return nil
}
