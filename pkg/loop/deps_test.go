package loop

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/uuid"
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
	sessionID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	loopID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	// Config carries a fully zero-value ToolSet: nil Permission/Registry/
	// Middlewares and zero caps. New must accept it and the loop must run.
	rec := &recordingPublisher{}
	l, err := New(ctx, sessionID, loopID, Provenance{}, rec, Config{
		Client:       &fakeLLM{chunks: []content.Chunk{textChunk("hi")}},
		Model:        llm.ModelSpec{Model: "m"},
		DrainTimeout: 200 * time.Millisecond,
		Tools:        ToolSet{},
	})
	if err != nil {
		t.Fatalf("New with zero ToolSet = %v, want nil", err)
	}
	startTurn(t, l, rec, nil)
	if _, ok := drainToTerminal(t, rec).(event.TurnDone); !ok {
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
	_                = ToolPolicy{Tool: "x", Effect: EffectAsk, Match: []string{"*"}}
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

// TestEffectMarshalJSON checks each defined Effect maps to its user-facing
// string, and an out-of-range Effect fails to marshal (fail-secure: an unknown
// numeric effect must never silently serialize to "allow").
func TestEffectMarshalJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      Effect
		want    string
		wantErr bool
	}{
		{name: "ask -> ask", in: EffectAsk, want: `"ask"`},
		{name: "auto-approve -> allow", in: EffectAutoApprove, want: `"allow"`},
		{name: "deny -> deny", in: EffectDeny, want: `"deny"`},
		{name: "out-of-range errors", in: Effect(99), wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Marshal(%d) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if tt.wantErr {
				var ie *InvalidEffectError
				if !errors.As(err, &ie) {
					t.Fatalf("Marshal error = %T (%v), want *InvalidEffectError", err, err)
				}
				return
			}
			if string(got) != tt.want {
				t.Errorf("Marshal(%d) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

// TestEffectUnmarshalJSON checks the three valid strings decode, and every other
// input (unknown string, number, bool, null, malformed) is rejected with a typed
// *InvalidEffectError — fail-secure: a malformed approval is never silently
// treated as auto-approve.
func TestEffectUnmarshalJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		in            string
		want          Effect
		wantErr       bool
		suppressTyped bool // true: the stdlib parser rejects before our method runs
	}{
		{name: "allow -> auto-approve", in: `"allow"`, want: EffectAutoApprove},
		{name: "ask -> ask", in: `"ask"`, want: EffectAsk},
		{name: "deny -> deny", in: `"deny"`, want: EffectDeny},
		{name: "unknown string errors", in: `"yolo"`, wantErr: true},
		{name: "empty string errors", in: `""`, wantErr: true},
		{name: "uppercase ALLOW errors (exact match)", in: `"ALLOW"`, wantErr: true},
		{name: "numeric errors", in: `5`, wantErr: true},
		{name: "bool errors", in: `true`, wantErr: true},
		{name: "null errors", in: `null`, wantErr: true},
		{name: "object errors", in: `{}`, wantErr: true},
		// Structurally malformed JSON (an unterminated string) fails in the stdlib
		// parser BEFORE UnmarshalJSON is dispatched, so the error is a
		// *json.SyntaxError, not our typed error. It is still fail-secure (an error,
		// never auto-approve); we assert that weaker contract via suppressTyped.
		{name: "malformed json errors (stdlib syntax)", in: `"allow`, wantErr: true, suppressTyped: true},
	}
	for _, tt := range tests {
		tt := tt
		// wantTypedErr defaults true for the cases that reach our UnmarshalJSON; the
		// malformed-syntax case overrides it to false (the parser rejects it first).
		typed := !tt.suppressTyped
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Effect(99) // poison (out-of-range): a successful decode must overwrite it
			err := json.Unmarshal([]byte(tt.in), &got)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Unmarshal(%s) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if tt.wantErr {
				// Fail-secure invariant for EVERY error case: never EffectAutoApprove.
				if got == EffectAutoApprove {
					t.Fatalf("Unmarshal(%s) errored but left value = EffectAutoApprove (fail-open!)", tt.in)
				}
				if typed {
					var ie *InvalidEffectError
					if !errors.As(err, &ie) {
						t.Fatalf("Unmarshal(%s) error = %T (%v), want *InvalidEffectError", tt.in, err, err)
					}
				}
				return
			}
			if got != tt.want {
				t.Errorf("Unmarshal(%s) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestEffectJSONRoundTrip marshals each valid effect and unmarshals it back to
// the same value.
func TestEffectJSONRoundTrip(t *testing.T) {
	t.Parallel()
	for _, e := range []Effect{EffectAsk, EffectAutoApprove, EffectDeny} {
		e := e
		t.Run(map[Effect]string{EffectAsk: "ask", EffectAutoApprove: "allow", EffectDeny: "deny"}[e], func(t *testing.T) {
			t.Parallel()
			b, err := json.Marshal(e)
			if err != nil {
				t.Fatalf("Marshal(%d): %v", e, err)
			}
			var got Effect
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("Unmarshal(%s): %v", b, err)
			}
			if got != e {
				t.Errorf("round-trip: got %d, want %d", got, e)
			}
		})
	}
}
