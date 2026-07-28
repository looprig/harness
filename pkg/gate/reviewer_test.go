package gate_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
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
	applies    int
	marshals   int
	validates  int
}

func (s *permissionClassifierStub) Name() hustle.Name             { return s.name }
func (s *permissionClassifierStub) Revision() string              { return s.revision }
func (s *permissionClassifierStub) Definition() hustle.Definition { return s.definition }
func (s *permissionClassifierStub) Applies(gate.PermissionReviewSubject) bool {
	s.applies++
	return true
}
func (s *permissionClassifierStub) MarshalInput(gate.PermissionReviewSubject) (json.RawMessage, error) {
	s.marshals++
	return json.RawMessage(`{}`), nil
}
func (s *permissionClassifierStub) ValidateResult(gate.PermissionReviewSubject, hustle.Result) (gate.PermissionAssessment, error) {
	s.validates++
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
	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("Classifiers() = %#v, want original order", got)
	}
	got[0] = second
	if next := set.Classifiers(); next[0] != first {
		t.Fatal("Classifiers() aliases registry slice")
	}
	if first.applies != 0 || first.marshals != 0 || first.validates != 0 ||
		second.applies != 0 || second.marshals != 0 || second.validates != 0 {
		t.Fatal("registry executed classifier behavior")
	}
}

func TestPermissionClassifierSetRejectsInvalidRegistration(t *testing.T) {
	t.Parallel()
	valid := validPermissionClassifier(t, "valid", "revision-1")
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
		{name: "duplicate name", classifiers: []gate.PermissionClassifier{valid, validPermissionClassifier(t, "valid", "revision-2")}},
		{name: "duplicate revision", classifiers: []gate.PermissionClassifier{valid, validPermissionClassifier(t, "other", "revision-1")}},
		{name: "zero definition", classifiers: []gate.PermissionClassifier{&permissionClassifierStub{name: "zero", revision: "revision-1"}}},
		{name: "background", classifiers: []gate.PermissionClassifier{classifierWithDefinition(t, "background", "revision-1", hustle.ParticipationBackground, false, true)}},
		{name: "current loop", classifiers: []gate.PermissionClassifier{classifierWithDefinition(t, "current", "revision-1", hustle.ParticipationBlocking, true, true)}},
		{name: "missing structured output", classifiers: []gate.PermissionClassifier{classifierWithDefinition(t, "plain", "revision-1", hustle.ParticipationBlocking, false, false)}},
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
	}
	definition, err := hustle.Define(options...)
	if err != nil {
		t.Fatalf("hustle.Define() error = %v", err)
	}
	return definition
}

func permissionClassifierModel() model.Model {
	maxTokens := 64
	return model.Model{
		Provider: "test", APIFormat: "test", BaseURL: "https://model.example.invalid", Name: "reviewer",
		Sampling: model.Sampling{MaxTokens: &maxTokens, Effort: model.EffortMedium},
	}
}
