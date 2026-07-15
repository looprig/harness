package rig

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/storage/memstore"
)

func validRigHustle(t *testing.T, name hustle.Name, options ...hustle.Option) hustle.Definition {
	t.Helper()
	base := []hustle.Option{
		hustle.WithName(name),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 1024, OutputBytes: 512}),
		hustle.WithCurrentLoopModel(),
		hustle.WithSystemPrompt("summarize safely", "prompt-v1"),
		hustle.WithPolicyRevision("policy-v1"),
	}
	if len(options) > 0 {
		base = options
	}
	definition, err := hustle.Define(base...)
	if err != nil {
		t.Fatalf("hustle.Define: %v", err)
	}
	return definition
}

func validHustleLimits() HustleLimits {
	return HustleLimits{
		BlockingConcurrent: 1, BlockingQueued: 0,
		BackgroundConcurrent: 2, BackgroundQueued: 3,
		AuditTimeout: time.Second, FinalizationTimeout: 2 * time.Second,
		WorkerDrainTimeout: 3 * time.Second,
	}
}

func validRigOptions(t *testing.T, options ...Option) []Option {
	t.Helper()
	definition := mustDefine(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("model")))
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("sessionstore.Open: %v", err)
	}
	base := []Option{WithLoops(definition), WithPrimers("agent"), WithSessionStore(store)}
	return append(base, options...)
}

func TestDefineHustlesAreAdditiveAndDefensivelyCopied(t *testing.T) {
	t.Parallel()
	first := validRigHustle(t, "first")
	second := validRigHustle(t, "second")
	input := []hustle.Definition{first}
	firstOption := WithHustles(input...)
	input[0] = second

	tests := []struct {
		name string
		opts []Option
		want []hustle.Name
	}{
		{name: "one call captures input", opts: []Option{firstOption}, want: []hustle.Name{"first"}},
		{name: "separate calls append", opts: []Option{firstOption, WithHustles(second)}, want: []hustle.Name{"first", "second"}},
		{name: "empty call is additive no-op", opts: []Option{WithHustles()}, want: nil},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := &definitionState{seen: make(map[singletonKey]bool)}
			for _, option := range tt.opts {
				if err := option(state); err != nil {
					t.Fatalf("option error = %v", err)
				}
			}
			if len(state.hustles) != len(tt.want) {
				t.Fatalf("hustles len = %d, want %d", len(state.hustles), len(tt.want))
			}
			for index, want := range tt.want {
				if got := state.hustles[index].Name(); got != want {
					t.Errorf("hustles[%d].Name() = %q, want %q", index, got, want)
				}
			}
		})
	}
}

func TestDefineHustleValidation(t *testing.T) {
	t.Parallel()
	valid := validRigHustle(t, "valid")
	tests := []struct {
		name string
		opts []Option
		kind DefinitionErrorKind
	}{
		{name: "zero definition", opts: validRigOptions(t, WithHustles(hustle.Definition{}), WithHustleLimits(validHustleLimits())), kind: DefinitionInvalidHustle},
		{name: "duplicate name in one call", opts: validRigOptions(t, WithHustles(valid, valid), WithHustleLimits(validHustleLimits())), kind: DefinitionDuplicateHustle},
		{name: "duplicate name across calls", opts: validRigOptions(t, WithHustles(valid), WithHustles(valid), WithHustleLimits(validHustleLimits())), kind: DefinitionDuplicateHustle},
		{name: "limits missing", opts: validRigOptions(t, WithHustles(valid)), kind: DefinitionMissingHustleLimits},
		{name: "limits unused", opts: validRigOptions(t, WithHustleLimits(validHustleLimits())), kind: DefinitionUnusedHustleLimits},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Define(tt.opts...)
			var target *DefinitionError
			if !errors.As(err, &target) || target.Kind != tt.kind {
				t.Fatalf("Define() error = %T %v, want DefinitionError kind %q", err, err, tt.kind)
			}
		})
	}
}

func TestDefineHustleLimitsBoundaries(t *testing.T) {
	t.Parallel()
	valid := validHustleLimits()
	tests := []struct {
		name    string
		limits  HustleLimits
		wantErr bool
	}{
		{name: "minimum valid", limits: HustleLimits{BlockingConcurrent: 1, BackgroundConcurrent: 1, AuditTimeout: time.Nanosecond, FinalizationTimeout: time.Nanosecond, WorkerDrainTimeout: time.Nanosecond}},
		{name: "queue cap valid", limits: HustleLimits{BlockingConcurrent: 1, BlockingQueued: MaxHustleQueued, BackgroundConcurrent: 1, BackgroundQueued: MaxHustleQueued, AuditTimeout: time.Nanosecond, FinalizationTimeout: time.Nanosecond, WorkerDrainTimeout: time.Nanosecond}},
		{name: "zero blocking concurrent", limits: func() HustleLimits { value := valid; value.BlockingConcurrent = 0; return value }(), wantErr: true},
		{name: "negative blocking queued", limits: func() HustleLimits { value := valid; value.BlockingQueued = -1; return value }(), wantErr: true},
		{name: "blocking queue above cap", limits: func() HustleLimits { value := valid; value.BlockingQueued = MaxHustleQueued + 1; return value }(), wantErr: true},
		{name: "zero background concurrent", limits: func() HustleLimits { value := valid; value.BackgroundConcurrent = 0; return value }(), wantErr: true},
		{name: "negative background queued", limits: func() HustleLimits { value := valid; value.BackgroundQueued = -1; return value }(), wantErr: true},
		{name: "background queue above cap", limits: func() HustleLimits { value := valid; value.BackgroundQueued = MaxHustleQueued + 1; return value }(), wantErr: true},
		{name: "zero audit timeout", limits: func() HustleLimits { value := valid; value.AuditTimeout = 0; return value }(), wantErr: true},
		{name: "zero finalization timeout", limits: func() HustleLimits { value := valid; value.FinalizationTimeout = 0; return value }(), wantErr: true},
		{name: "zero worker drain timeout", limits: func() HustleLimits { value := valid; value.WorkerDrainTimeout = 0; return value }(), wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := &definitionState{seen: make(map[singletonKey]bool)}
			err := WithHustleLimits(tt.limits)(state)
			var target *DefinitionError
			if tt.wantErr && (!errors.As(err, &target) || target.Kind != DefinitionInvalidHustleLimits) {
				t.Fatalf("WithHustleLimits() error = %T %v, want invalid limits", err, err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("WithHustleLimits() error = %v", err)
			}
		})
	}
}

func TestDefineHustleLimitsAreSingleton(t *testing.T) {
	t.Parallel()
	valid := validHustleLimits()
	invalid := valid
	invalid.BlockingConcurrent = 0
	tests := []struct {
		name   string
		first  HustleLimits
		second HustleLimits
	}{
		{name: "valid second occurrence rejected", first: valid, second: valid},
		{name: "invalid second occurrence is still duplicate", first: valid, second: invalid},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			state := &definitionState{seen: make(map[singletonKey]bool)}
			if err := WithHustleLimits(tt.first)(state); err != nil {
				t.Fatalf("first option error = %v", err)
			}
			var target *DefinitionError
			if err := WithHustleLimits(tt.second)(state); !errors.As(err, &target) || target.Kind != DefinitionDuplicateOption {
				t.Fatalf("second option error = %T %v, want duplicate option", err, err)
			}
		})
	}
}

func TestDefineForwardsLifecycleHustlesExactlyOnce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		file string
	}{
		{name: "definition composition", file: "definition.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), tt.file, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if ok && selector.Sel.Name == "WithLifecycleHustles" {
					calls++
				}
				return true
			})
			if calls != 1 {
				t.Fatalf("WithLifecycleHustles calls = %d, want exactly 1", calls)
			}
		})
	}
}

func TestDefineHustleLimitTranslation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input HustleLimits
	}{
		{name: "all fields", input: validHustleLimits()},
		{name: "minimum queue boundary", input: HustleLimits{BlockingConcurrent: 1, BackgroundConcurrent: 1, AuditTimeout: time.Nanosecond, FinalizationTimeout: time.Nanosecond, WorkerDrainTimeout: time.Nanosecond}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := lifecycleHustleLimits(tt.input)
			if got.BlockingConcurrent != tt.input.BlockingConcurrent || got.BlockingQueued != tt.input.BlockingQueued ||
				got.BackgroundConcurrent != tt.input.BackgroundConcurrent || got.BackgroundQueued != tt.input.BackgroundQueued ||
				got.AuditTimeout != tt.input.AuditTimeout || got.FinalizationTimeout != tt.input.FinalizationTimeout ||
				got.WorkerDrainTimeout != tt.input.WorkerDrainTimeout {
				t.Fatalf("lifecycleHustleLimits() = %#v, want fields from %#v", got, tt.input)
			}
		})
	}
}
