package gate_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
)

type permissionClassifierClient struct{}

func (*permissionClassifierClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, nil
}

func (*permissionClassifierClient) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, nil
}

type permissionClassifierStub struct {
	name       hustle.Name
	revision   string
	definition hustle.Definition
	nameCalls  int
	revCalls   int
	defCalls   int
	applies    int
	marshals   int
	validates  int
	mutate     func(*gate.PermissionReviewSubject)

	// panicMarshalInput/panicValidateResult, when set, make the corresponding
	// method panic(panicValue) instead of returning normally — used to prove
	// frozenPermissionClassifier recovers a panic at this trust boundary
	// rather than letting it crash the caller's goroutine.
	panicMarshalInput   bool
	panicValidateResult bool
	panicValue          any
}

func (s *permissionClassifierStub) Name() hustle.Name {
	s.nameCalls++
	return s.name
}
func (s *permissionClassifierStub) Revision() string {
	s.revCalls++
	return s.revision
}
func (s *permissionClassifierStub) Definition() hustle.Definition {
	s.defCalls++
	return s.definition
}
func (s *permissionClassifierStub) Applies(subject gate.PermissionReviewSubject) bool {
	s.applies++
	if s.mutate != nil {
		s.mutate(&subject)
	}
	return true
}
func (s *permissionClassifierStub) MarshalInput(subject gate.PermissionReviewSubject) (json.RawMessage, error) {
	s.marshals++
	if s.panicMarshalInput {
		panic(s.panicValue)
	}
	if s.mutate != nil {
		s.mutate(&subject)
	}
	return json.RawMessage(`{}`), nil
}
func (s *permissionClassifierStub) ValidateResult(subject gate.PermissionReviewSubject, _ hustle.Result) (gate.PermissionAssessment, error) {
	s.validates++
	if s.panicValidateResult {
		panic(s.panicValue)
	}
	if s.mutate != nil {
		s.mutate(&subject)
	}
	return gate.PermissionAssessment{}, nil
}

func TestPermissionClassifierSetPreservesOrderAndDoesNotExecute(t *testing.T) {
	t.Parallel()
	first := validPermissionClassifier(t, "first", "revision-1")
	second := validPermissionClassifier(t, "second", "revision-2")
	input := []gate.PermissionClassifier{first, second}
	set, err := gate.NewPermissionClassifierSet(input...)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet() error = %v", err)
	}
	input[0] = second
	got := set.Classifiers()
	if len(got) != 2 ||
		got[0].Name() != "first" ||
		got[1].Name() != "second" {
		t.Fatalf("Classifiers() = %#v, want original order", got)
	}
	got[0] = second
	if next := set.Classifiers(); next[0].Name() != "first" {
		t.Fatal("Classifiers() aliases registry slice")
	}
	if first.applies != 0 || first.marshals != 0 || first.validates != 0 ||
		second.applies != 0 || second.marshals != 0 || second.validates != 0 {
		t.Fatal("registry executed classifier behavior")
	}
}

func TestValidatePermissionClassifierName(t *testing.T) {
	t.Parallel()
	atLimit := hustle.Name(strings.Repeat("n", gate.MaxPermissionClassifierNameBytes))
	tests := []struct {
		name    string
		value   hustle.Name
		wantErr bool
	}{
		{name: "ordinary", value: "command-safety"},
		{name: "exact byte limit", value: atLimit},
		{name: "empty", value: "", wantErr: true},
		{name: "leading whitespace", value: " command-safety", wantErr: true},
		{name: "trailing whitespace", value: "command-safety ", wantErr: true},
		{name: "nul", value: "command\x00safety", wantErr: true},
		{name: "reserved", value: "_looprig.private", wantErr: true},
		{
			name:    "over byte limit",
			value:   hustle.Name(strings.Repeat("n", gate.MaxPermissionClassifierNameBytes+1)),
			wantErr: true,
		},
		{name: "invalid utf8 ff", value: hustle.Name(string([]byte{0xff})), wantErr: true},
		{name: "invalid utf8 fe", value: hustle.Name(string([]byte{0xfe})), wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := gate.ValidatePermissionClassifierName(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidatePermissionClassifierName() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				if len(err.Error()) > 128 {
					t.Fatalf("error length = %d, want bounded", len(err.Error()))
				}
				if strings.Contains(err.Error(), string(tt.value)) && tt.value != "" {
					t.Fatalf("error %q echoes rejected classifier name", err)
				}
			}
		})
	}
}

func TestValidatePermissionClassifierNameInvalidUTF8DoesNotCollide(t *testing.T) {
	t.Parallel()
	first := hustle.Name(string([]byte{0xff}))
	second := hustle.Name(string([]byte{0xfe}))
	if first == second {
		t.Fatal("test setup collapsed distinct invalid byte strings")
	}
	firstErr := gate.ValidatePermissionClassifierName(first)
	secondErr := gate.ValidatePermissionClassifierName(second)
	if firstErr == nil || secondErr == nil {
		t.Fatalf("errors = (%v, %v), want two rejections", firstErr, secondErr)
	}
	if firstErr.Error() != secondErr.Error() {
		t.Fatalf("errors = (%q, %q), want one bounded non-echoing domain", firstErr, secondErr)
	}
}

func TestPermissionClassifierSetFreezesMetadataAndDelegatesBehavior(t *testing.T) {
	t.Parallel()
	source := validPermissionClassifier(t, "original", "revision-1")
	originalDefinition := source.definition
	set, err := gate.NewPermissionClassifierSet(source)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet() error = %v", err)
	}
	if source.nameCalls != 1 || source.revCalls != 1 || source.defCalls != 1 {
		t.Fatalf(
			"metadata calls = name:%d revision:%d definition:%d, want exactly once each",
			source.nameCalls,
			source.revCalls,
			source.defCalls,
		)
	}

	source.name = "mutated"
	source.revision = "revision-2"
	source.definition = validPermissionDefinition(
		t,
		"mutated",
		"revision-2",
		hustle.ParticipationBackground,
		false,
		true,
	)

	registered := set.Classifiers()[0]
	if got := registered.Name(); got != "original" {
		t.Fatalf("Name() = %q, want frozen original", got)
	}
	if got := registered.Revision(); got != "revision-1" {
		t.Fatalf("Revision() = %q, want frozen revision-1", got)
	}
	if got := registered.Definition().Descriptor(); got != originalDefinition.Descriptor() {
		t.Fatalf("Definition().Descriptor() = %#v, want frozen %#v", got, originalDefinition.Descriptor())
	}
	if source.nameCalls != 1 || source.revCalls != 1 || source.defCalls != 1 {
		t.Fatalf(
			"registry view reread source metadata: name:%d revision:%d definition:%d",
			source.nameCalls,
			source.revCalls,
			source.defCalls,
		)
	}

	if !registered.Applies(gate.PermissionReviewSubject{}) {
		t.Fatal("Applies() = false, want delegated true")
	}
	if _, err := registered.MarshalInput(gate.PermissionReviewSubject{}); err != nil {
		t.Fatalf("MarshalInput() error = %v", err)
	}
	if _, err := registered.ValidateResult(gate.PermissionReviewSubject{}, hustle.Result{}); err != nil {
		t.Fatalf("ValidateResult() error = %v", err)
	}
	if source.applies != 1 || source.marshals != 1 || source.validates != 1 {
		t.Fatalf(
			"behavior calls = applies:%d marshals:%d validates:%d, want delegated once each",
			source.applies,
			source.marshals,
			source.validates,
		)
	}
}

func TestPermissionClassifierSetDelegationOwnsSubject(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		call func(*testing.T, gate.PermissionClassifier, gate.PermissionReviewSubject)
	}{
		{
			name: "applies",
			call: func(t *testing.T, classifier gate.PermissionClassifier, subject gate.PermissionReviewSubject) {
				t.Helper()
				if !classifier.Applies(subject) {
					t.Fatal("Applies() = false, want delegated true")
				}
			},
		},
		{
			name: "marshal input",
			call: func(t *testing.T, classifier gate.PermissionClassifier, subject gate.PermissionReviewSubject) {
				t.Helper()
				got, err := classifier.MarshalInput(subject)
				if err != nil {
					t.Fatalf("MarshalInput() error = %v", err)
				}
				if string(got) != `{}` {
					t.Fatalf("MarshalInput() = %q, want delegated result", got)
				}
			},
		},
		{
			name: "validate result",
			call: func(t *testing.T, classifier gate.PermissionClassifier, subject gate.PermissionReviewSubject) {
				t.Helper()
				got, err := classifier.ValidateResult(subject, hustle.Result{})
				if err != nil {
					t.Fatalf("ValidateResult() error = %v", err)
				}
				if !reflect.DeepEqual(got, gate.PermissionAssessment{}) {
					t.Fatalf("ValidateResult() = %#v, want delegated zero assessment", got)
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			source := validPermissionClassifier(t, "command-safety", "command-safety-v1")
			source.mutate = func(subject *gate.PermissionReviewSubject) {
				subject.Request.Requirements[0].Description = "mutated requirement"
				subject.Request.Requirements[0].Candidates[0].Description = "mutated candidate"
				subject.Context.Entries[0].Content = "mutated context"
				subject.Basis.SubjectDigest = [32]byte{}
			}
			set, err := gate.NewPermissionClassifierSet(source)
			if err != nil {
				t.Fatalf("NewPermissionClassifierSet() error = %v", err)
			}
			if source.applies != 0 || source.marshals != 0 || source.validates != 0 {
				t.Fatal("registration executed classifier behavior")
			}
			subject := validPermissionReviewSubject(t)
			original := subject.Clone()
			tt.call(t, set.Classifiers()[0], subject)
			if !reflect.DeepEqual(subject, original) {
				t.Fatalf("delegation mutated caller subject:\ngot  %#v\nwant %#v", subject, original)
			}
			digest, err := gate.SubjectDigest(subject)
			if err != nil {
				t.Fatalf("SubjectDigest() error = %v", err)
			}
			if digest != original.Basis.SubjectDigest {
				t.Fatalf("digest = %x, want original %x", digest, original.Basis.SubjectDigest)
			}
			if source.nameCalls != 1 || source.revCalls != 1 || source.defCalls != 1 {
				t.Fatalf(
					"metadata calls = name:%d revision:%d definition:%d, want exactly once",
					source.nameCalls,
					source.revCalls,
					source.defCalls,
				)
			}
		})
	}
}

func TestPermissionClassifierSetFreezesDefinitionDescriptor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutated func(*testing.T) hustle.Definition
	}{
		{
			name: "participation",
			mutated: func(t *testing.T) hustle.Definition {
				return validPermissionDefinition(
					t, "original", "revision-1", hustle.ParticipationBackground, false, true,
				)
			},
		},
		{
			name: "model source",
			mutated: func(t *testing.T) hustle.Definition {
				return validPermissionDefinition(
					t, "original", "revision-1", hustle.ParticipationBlocking, true, true,
				)
			},
		},
		{
			name: "output schema",
			mutated: func(t *testing.T) hustle.Definition {
				return validPermissionDefinition(
					t, "original", "revision-1", hustle.ParticipationBlocking, false, false,
				)
			},
		},
		{
			name: "policy revision",
			mutated: func(t *testing.T) hustle.Definition {
				return validPermissionDefinition(
					t, "original", "revision-2", hustle.ParticipationBlocking, false, true,
				)
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			source := validPermissionClassifier(t, "original", "revision-1")
			want := source.definition.Descriptor()
			set, err := gate.NewPermissionClassifierSet(source)
			if err != nil {
				t.Fatalf("NewPermissionClassifierSet() error = %v", err)
			}
			source.definition = tt.mutated(t)
			if got := set.Classifiers()[0].Definition().Descriptor(); got != want {
				t.Fatalf("frozen descriptor = %#v, want %#v", got, want)
			}
			if source.defCalls != 1 {
				t.Fatalf("Definition() source calls = %d, want 1", source.defCalls)
			}
		})
	}
}

func TestPermissionClassifierSetRejectsInvalidRegistration(t *testing.T) {
	t.Parallel()
	var typedNil *permissionClassifierStub
	tests := []struct {
		name        string
		classifiers []gate.PermissionClassifier
	}{
		{name: "empty"},
		{name: "nil", classifiers: []gate.PermissionClassifier{nil}},
		{name: "typed nil", classifiers: []gate.PermissionClassifier{typedNil}},
		{name: "invalid name", classifiers: []gate.PermissionClassifier{classifierWithMetadata(t, " ", "revision-1")}},
		{name: "blank revision", classifiers: []gate.PermissionClassifier{classifierWithMetadata(t, "blank", " ")}},
		{name: "invalid utf8 revision", classifiers: []gate.PermissionClassifier{classifierWithMetadata(t, "utf8", string([]byte{0xff}))}},
		{name: "long revision", classifiers: []gate.PermissionClassifier{classifierWithMetadata(t, "long", strings.Repeat("r", gate.MaxPermissionClassifierRevisionBytes+1))}},
		{name: "duplicate name", classifiers: []gate.PermissionClassifier{validPermissionClassifier(t, "valid", "revision-1"), validPermissionClassifier(t, "valid", "revision-2")}},
		{name: "duplicate revision", classifiers: []gate.PermissionClassifier{validPermissionClassifier(t, "valid", "revision-1"), validPermissionClassifier(t, "other", "revision-1")}},
		{name: "zero definition", classifiers: []gate.PermissionClassifier{&permissionClassifierStub{name: "zero", revision: "revision-1"}}},
		{name: "background", classifiers: []gate.PermissionClassifier{classifierWithDefinition(t, "background", "revision-1", hustle.ParticipationBackground, false, true)}},
		{name: "current loop", classifiers: []gate.PermissionClassifier{classifierWithDefinition(t, "current", "revision-1", hustle.ParticipationBlocking, true, true)}},
		{name: "missing structured output", classifiers: []gate.PermissionClassifier{classifierWithDefinition(t, "plain", "revision-1", hustle.ParticipationBlocking, false, false)}},
		{name: "missing evidence policy", classifiers: []gate.PermissionClassifier{classifierWithDefinitionWithoutEvidence(t, "no-evidence", "revision-1")}},
		{name: "name drift", classifiers: []gate.PermissionClassifier{&permissionClassifierStub{name: "outer", revision: "revision-1", definition: validPermissionDefinition(t, "inner", "revision-1", hustle.ParticipationBlocking, false, true)}}},
		{name: "revision drift", classifiers: []gate.PermissionClassifier{&permissionClassifierStub{name: "drift", revision: "outer-revision", definition: validPermissionDefinition(t, "drift", "inner-revision", hustle.ParticipationBlocking, false, true)}}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := gate.NewPermissionClassifierSet(tt.classifiers...); err == nil {
				t.Fatal("NewPermissionClassifierSet() error = nil")
			}
			for _, classifier := range tt.classifiers {
				stub, ok := classifier.(*permissionClassifierStub)
				if ok && stub != nil && (stub.applies != 0 || stub.marshals != 0 || stub.validates != 0) {
					t.Fatal("registry executed classifier behavior on rejection")
				}
			}
		})
	}
}

func TestPermissionClassifierSetErrorsDoNotEchoMetadata(t *testing.T) {
	t.Parallel()
	secret := "classifier-metadata-secret"
	classifier := classifierWithMetadata(t, "valid", secret+strings.Repeat("x", gate.MaxPermissionClassifierRevisionBytes))
	_, err := gate.NewPermissionClassifierSet(classifier)
	if err == nil {
		t.Fatal("NewPermissionClassifierSet() error = nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error %q echoed classifier metadata", err)
	}
}

func validPermissionClassifier(t *testing.T, name hustle.Name, revision string) *permissionClassifierStub {
	t.Helper()
	return &permissionClassifierStub{
		name: name, revision: revision,
		definition: validPermissionDefinition(t, name, revision, hustle.ParticipationBlocking, false, true),
	}
}

func classifierWithMetadata(t *testing.T, name hustle.Name, revision string) *permissionClassifierStub {
	t.Helper()
	return &permissionClassifierStub{
		name: name, revision: revision,
		definition: validPermissionDefinition(
			t,
			"definition-metadata",
			"definition-revision",
			hustle.ParticipationBlocking,
			false,
			true,
		),
	}
}

func classifierWithDefinition(
	t *testing.T,
	name hustle.Name,
	revision string,
	participation hustle.Participation,
	current bool,
	structured bool,
) *permissionClassifierStub {
	t.Helper()
	return &permissionClassifierStub{
		name: name, revision: revision,
		definition: validPermissionDefinition(t, name, revision, participation, current, structured),
	}
}

func validPermissionDefinition(
	t *testing.T,
	name hustle.Name,
	revision string,
	participation hustle.Participation,
	current bool,
	structured bool,
) hustle.Definition {
	t.Helper()
	options := []hustle.Option{
		hustle.WithName(name),
		hustle.WithParticipation(participation),
		hustle.WithTimeout(time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 4096, OutputBytes: 4096}),
		hustle.WithSystemPrompt("review safely", "prompt-v1"),
		hustle.WithPolicyRevision(revision),
	}
	if current {
		options = append(options, hustle.WithCurrentLoopModel())
	} else {
		options = append(options, hustle.WithNamedInference(&permissionClassifierClient{}, permissionClassifierModel()))
	}
	if structured {
		options = append(options, hustle.WithOutputSchema(inference.OutputSchema{
			Name: "permission_assessment",
			Schema: json.RawMessage(`{
				"type":"object",
				"properties":{"recommendation":{"type":"string"}},
				"required":["recommendation"],
				"additionalProperties":false
			}`),
			Strict: true,
		}))
		if participation == hustle.ParticipationBlocking {
			options = append(options, hustle.WithEvidenceTools(permissionEvidencePolicy()))
		}
	}
	definition, err := hustle.Define(options...)
	if err != nil {
		t.Fatalf("hustle.Define() error = %v", err)
	}
	return definition
}

type permissionEvidenceTool struct{}

func (*permissionEvidenceTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name: "permission-evidence", Desc: "read permission evidence",
		Schema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, nil
}

func (*permissionEvidenceTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("ok"), nil
}

func permissionEvidencePolicy() hustle.EvidenceToolPolicy {
	return hustle.EvidenceToolPolicy{
		Revision: "permission-evidence-v1",
		Limits: hustle.ToolLoopLimits{
			MaxRounds: 1, MaxCalls: 1, MaxCallsPerRound: 1,
			MaxResultBytes: 1024, MaxEvidenceBytes: 1024,
		},
		Definitions: []tool.Definition{tool.NewEvidenceDefinition(
			"permission-evidence", 0,
			[]tool.ToolInfo{{
				Name: "permission-evidence", Desc: "read permission evidence",
				Schema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
			}},
			func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
				return []tool.InvokableTool{&permissionEvidenceTool{}}, nil
			},
		)},
	}
}

func classifierWithDefinitionWithoutEvidence(
	t *testing.T,
	name hustle.Name,
	revision string,
) *permissionClassifierStub {
	t.Helper()
	options := []hustle.Option{
		hustle.WithName(name),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 4096, OutputBytes: 4096}),
		hustle.WithSystemPrompt("review safely", "prompt-v1"),
		hustle.WithPolicyRevision(revision),
		hustle.WithNamedInference(&permissionClassifierClient{}, permissionClassifierModel()),
		hustle.WithOutputSchema(inference.OutputSchema{
			Name:   "permission_assessment",
			Schema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
			Strict: true,
		}),
	}
	definition, err := hustle.Define(options...)
	if err != nil {
		t.Fatal(err)
	}
	return &permissionClassifierStub{name: name, revision: revision, definition: definition}
}

// TestPermissionClassifierSetRecoversMarshalInputPanic proves the registry
// boundary — not just the ordinary error return — protects callers from a
// trusted-but-fallible classifier's MarshalInput panicking: an unrecovered
// panic here would crash whatever goroutine called through the registered
// PermissionClassifier, taking down every concurrent session sharing that
// process. Recovery must also never let the panic VALUE (which could embed
// raw classifier-controlled subject content) reach the returned error.
func TestPermissionClassifierSetRecoversMarshalInputPanic(t *testing.T) {
	t.Parallel()
	const marker = "secret-marker-marshal-panic-4f19c2"
	source := validPermissionClassifier(t, "panics-on-marshal", "revision-1")
	source.panicMarshalInput = true
	source.panicValue = marker
	set, err := gate.NewPermissionClassifierSet(source)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet() error = %v", err)
	}
	registered := set.Classifiers()[0]

	raw, err := registered.MarshalInput(gate.PermissionReviewSubject{})
	if err == nil {
		t.Fatal("MarshalInput() error = nil, want the panic recovered into an error")
	}
	if raw != nil {
		t.Fatalf("MarshalInput() raw = %v, want nil on a recovered panic", raw)
	}
	var panicErr *gate.PermissionClassifierPanicError
	if !errors.As(err, &panicErr) {
		t.Fatalf("MarshalInput() error = %T, want *gate.PermissionClassifierPanicError", err)
	}
	if panicErr.Method != gate.PermissionClassifierPanicMarshalInput {
		t.Fatalf("Method = %q, want %q", panicErr.Method, gate.PermissionClassifierPanicMarshalInput)
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("error %q leaks the recovered panic value", err)
	}
}

// TestPermissionClassifierSetRecoversValidateResultPanic is the
// defense-in-depth companion to the MarshalInput test above: ValidateResult
// panics are already indirectly caught by internal/hustleruntime's own
// callValidator recover, but frozenPermissionClassifier recovers them itself
// too, so every caller of a registered PermissionClassifier gets the same
// bounded-error guarantee, not only the one call site inside a Hustle run.
func TestPermissionClassifierSetRecoversValidateResultPanic(t *testing.T) {
	t.Parallel()
	const marker = "secret-marker-validate-panic-9c02af"
	source := validPermissionClassifier(t, "panics-on-validate", "revision-1")
	source.panicValidateResult = true
	source.panicValue = marker
	set, err := gate.NewPermissionClassifierSet(source)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet() error = %v", err)
	}
	registered := set.Classifiers()[0]

	assessment, err := registered.ValidateResult(gate.PermissionReviewSubject{}, hustle.Result{})
	if err == nil {
		t.Fatal("ValidateResult() error = nil, want the panic recovered into an error")
	}
	if assessment.Basis != (gate.ReviewBasis{}) || assessment.Risk != "" || assessment.Authorization != "" ||
		len(assessment.Categories) != 0 || assessment.Recommendation != "" || assessment.Rationale != "" {
		t.Fatalf("ValidateResult() assessment = %#v, want zero value on a recovered panic", assessment)
	}
	var panicErr *gate.PermissionClassifierPanicError
	if !errors.As(err, &panicErr) {
		t.Fatalf("ValidateResult() error = %T, want *gate.PermissionClassifierPanicError", err)
	}
	if panicErr.Method != gate.PermissionClassifierPanicValidateResult {
		t.Fatalf("Method = %q, want %q", panicErr.Method, gate.PermissionClassifierPanicValidateResult)
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("error %q leaks the recovered panic value", err)
	}
}

func permissionClassifierModel() model.Model {
	maxTokens := 64
	return model.Model{
		Provider: "test", APIFormat: "test", BaseURL: "https://model.example.invalid", Name: "reviewer",
		Sampling: model.Sampling{MaxTokens: &maxTokens, Effort: model.EffortMedium},
	}
}
