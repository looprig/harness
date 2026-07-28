package rig

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/gate"
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

func TestPermissionReviewOptionsFreezeOrderedClassifierIdentity(t *testing.T) {
	t.Parallel()
	first := defineRigPermissionClassifier(t, "alpha", rigEvidencePolicy("status"))
	second := defineRigPermissionClassifier(t, "zulu", rigEvidencePolicy("diff"))
	set := rigPermissionClassifierSet(t, first, second)

	state := &definitionState{seen: make(map[singletonKey]bool)}
	if err := WithPermissionClassifiers(set)(state); err != nil {
		t.Fatalf("WithPermissionClassifiers: %v", err)
	}
	if err := WithPermissionReviewPolicyRevision("review-policy-v1")(state); err != nil {
		t.Fatalf("WithPermissionReviewPolicyRevision: %v", err)
	}
	got := state.permissionClassifiers.Classifiers()
	if len(got) != 2 || got[0].Name() != "alpha" || got[1].Name() != "zulu" {
		t.Fatalf("classifier order = %#v", got)
	}
	if len(state.hustles) != 2 ||
		state.hustles[0].Name() != "alpha" ||
		state.hustles[1].Name() != "zulu" {
		t.Fatalf("classifier hustle definitions = %#v", state.hustles)
	}
	if state.permissionReviewPolicyRevision != "review-policy-v1" {
		t.Fatalf("review policy revision = %q", state.permissionReviewPolicyRevision)
	}
}

func TestPermissionReviewOptionsRejectInvalidAndDuplicateValues(t *testing.T) {
	t.Parallel()
	valid := rigPermissionClassifierSet(
		t, defineRigPermissionClassifier(t, "alpha", rigEvidencePolicy("status")),
	)
	tests := []struct {
		name string
		run  func(*definitionState) error
		kind DefinitionErrorKind
	}{
		{name: "zero classifier set", run: func(state *definitionState) error {
			return WithPermissionClassifiers(gate.PermissionClassifierSet{})(state)
		}, kind: DefinitionInvalidPermissionClassifiers},
		{name: "blank review policy", run: func(state *definitionState) error {
			return WithPermissionReviewPolicyRevision(" ")(state)
		}, kind: DefinitionInvalidPermissionReviewPolicy},
		{name: "invalid utf8 review policy", run: func(state *definitionState) error {
			return WithPermissionReviewPolicyRevision("policy-\xff")(state)
		}, kind: DefinitionInvalidPermissionReviewPolicy},
		{name: "nul review policy", run: func(state *definitionState) error {
			return WithPermissionReviewPolicyRevision("policy-\x00")(state)
		}, kind: DefinitionInvalidPermissionReviewPolicy},
		{name: "overlong review policy", run: func(state *definitionState) error {
			return WithPermissionReviewPolicyRevision(strings.Repeat("r", gate.MaxPermissionReviewPolicyRevisionBytes+1))(state)
		}, kind: DefinitionInvalidPermissionReviewPolicy},
		{name: "classifier duplicate option", run: func(state *definitionState) error {
			if err := WithPermissionClassifiers(valid)(state); err != nil {
				return err
			}
			return WithPermissionClassifiers(valid)(state)
		}, kind: DefinitionDuplicateOption},
		{name: "policy duplicate option", run: func(state *definitionState) error {
			if err := WithPermissionReviewPolicyRevision("review-v1")(state); err != nil {
				return err
			}
			return WithPermissionReviewPolicyRevision("review-v2")(state)
		}, kind: DefinitionDuplicateOption},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			state := &definitionState{seen: make(map[singletonKey]bool)}
			err := testCase.run(state)
			var target *DefinitionError
			if !errors.As(err, &target) || target.Kind != testCase.kind {
				t.Fatalf("option error = %T %v, want DefinitionError kind %q", err, err, testCase.kind)
			}
		})
	}
}

func TestPermissionReviewPolicyRevisionAcceptsExactBound(t *testing.T) {
	t.Parallel()
	revision := strings.Repeat("r", gate.MaxPermissionReviewPolicyRevisionBytes)
	state := &definitionState{seen: make(map[singletonKey]bool)}
	if err := WithPermissionReviewPolicyRevision(revision)(state); err != nil {
		t.Fatalf("WithPermissionReviewPolicyRevision(exact bound): %v", err)
	}
	if state.permissionReviewPolicyRevision != revision {
		t.Fatalf("stored revision length = %d, want %d", len(state.permissionReviewPolicyRevision), len(revision))
	}
}

func TestDefinePermissionReviewRequiresCompleteConfiguration(t *testing.T) {
	t.Parallel()
	set := rigPermissionClassifierSet(
		t, defineRigPermissionClassifier(t, "alpha", rigEvidencePolicy("status")),
	)
	tests := []struct {
		name    string
		options []Option
	}{
		{name: "classifiers without review policy", options: []Option{WithPermissionClassifiers(set)}},
		{name: "review policy without classifiers", options: []Option{WithPermissionReviewPolicyRevision("review-v1")}},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := Define(validRigOptions(t, testCase.options...)...)
			var target *DefinitionError
			if !errors.As(err, &target) || target.Kind != DefinitionIncompletePermissionReview {
				t.Fatalf("Define() error = %T %v, want incomplete permission review", err, err)
			}
		})
	}
}

func TestDefinePermissionReviewUsesHustleRegistrationAndBindingPath(t *testing.T) {
	t.Parallel()
	classifier := defineRigPermissionClassifier(t, "alpha", rigEvidencePolicy("status"))
	set := rigPermissionClassifierSet(t, classifier)
	base := []Option{
		WithPermissionClassifiers(set),
		WithPermissionReviewPolicyRevision("review-v1"),
	}
	_, err := Define(validRigOptions(t, base...)...)
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionMissingHustleLimits {
		t.Fatalf("Define() without shared hustle limits error = %T %v", err, err)
	}
	if _, err := Define(validRigOptions(t, append(base, WithHustleLimits(validHustleLimits()))...)...); err != nil {
		t.Fatalf("Define() with classifier hustle registration: %v", err)
	}
	if _, err := Define(validRigOptions(
		t,
		append(base,
			WithHustles(classifier.Definition()),
			WithHustleLimits(validHustleLimits()),
		)...,
	)...); !errors.As(err, &target) || target.Kind != DefinitionDuplicateHustle {
		t.Fatalf("explicit duplicate classifier hustle error = %T %v", err, err)
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
