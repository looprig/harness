//go:build evalmigration

// Package evalmigration is a build-tagged compatibility proof that the reusable
// github.com/looprig/eval module can express the evaluation examples the legacy
// harness/pkg/eval package provided. It re-expresses the two original examples —
// the deterministic Contains metric and the model Judge metric — with the new
// eval API (content.AgenticMessages, exact.RequiredText, judge.New, and
// evaltest.RunScenario) and asserts both reach a passing verdict.
//
// It is a PROOF, not a live test: the judge runs against an in-test fake
// inference.Client that returns a valid structured ScoreOutput, so the whole
// file compiles and passes offline with no network and no credentials. The
// dedicated evalmigration build tag keeps it out of the default `go test ./...`
// suite; run it with `go test -race -tags evalmigration ./pkg/evalmigration`.
package evalmigration

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/eval"
	"github.com/looprig/eval/evaltest"
	"github.com/looprig/eval/exact"
	"github.com/looprig/eval/judge"
	"github.com/looprig/eval/rubric"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
)

// migrationRevision is the target revision the scenario qualifies. The scenario
// and the observation the target returns must agree on it, or eval.Run rejects
// the sample as a stage error rather than evaluating it.
const migrationRevision eval.Revision = "v1"

// userText builds a user turn carrying a single text block.
func userText(s string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{
		Role:   content.RoleUser,
		Blocks: []content.Block{&content.TextBlock{Text: s}},
	}}
}

// aiText builds an assistant turn carrying a single text block. It is the agent's
// answer — the value the legacy TestCase.ActualOutput held.
func aiText(s string) *content.AIMessage {
	return &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.TextBlock{Text: s}},
	}}
}

// answerTarget is a trivial in-test eval.Target: it drives the scenario by
// echoing the scenario's input thread and appending a fixed assistant answer,
// producing the observation both evaluators assess. This re-expresses the legacy
// Runner+RunCases flow that filled a TestCase.ActualOutput. It never mutates the
// read-only Scenario: it appends into a fresh slice.
type answerTarget struct {
	answer string
}

func (answerTarget) Name() string { return "migration-fake-agent" }

func (t answerTarget) Observe(_ context.Context, s eval.Scenario) (eval.Observation, error) {
	conv := make(content.AgenticMessages, 0, len(s.Input)+1)
	conv = append(conv, s.Input...)
	conv = append(conv, aiText(t.answer))
	return eval.Observation{
		Conversation: conv,
		Scope:        eval.ScopeCase,
		Subject: eval.Subject{
			ID:       "agent-under-eval",
			Kind:     eval.SubjectAgent,
			Name:     s.Name,
			Revision: s.Revision,
		},
	}, nil
}

// fakeJudgeClient is an inference.Client that returns a canned structured-output
// response. It stands in for a real judge model so the proof runs offline: the
// judge decodes its assistant text as a ScoreOutput exactly as it would a real
// provider's structured output.
type fakeJudgeClient struct {
	scoreJSON string
}

func (c fakeJudgeClient) Invoke(_ context.Context, _ inference.Request) (*inference.Response, error) {
	return &inference.Response{
		Message:      aiText(c.scoreJSON),
		Usage:        &content.Usage{InputTokens: 32, OutputTokens: 8},
		Model:        "migration-judge-model",
		FinishReason: stream.FinishReasonStop,
	}, nil
}

func (fakeJudgeClient) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, errors.New("stream is not used by the rubric judge")
}

// structuredModel advertises native structured output so the judge's request
// passes inference.ValidateRequestFeatures rather than failing fail-secure.
func structuredModel() model.Model {
	return model.CustomModel("test", "test", "", "migration-judge-model", model.WithStructuredOutput())
}

// TestMigrationContainsAndJudge re-expresses the two legacy harness/pkg/eval
// examples with the new eval module and proves both pass through one run:
//
//   - Contains -> exact.RequiredText("Paris") over the assistant's text output.
//   - Judge    -> judge.New(rubric.AnswerRelevanceV1, ...) scoring the same
//     conversation against a rubric, with a fake client returning a valid
//     structured ScoreOutput above the rubric's pass threshold.
//
// evaltest.RunScenario drives the scenario through the target once and applies
// both evaluators; evaltest.RequirePass gates the resulting report.
func TestMigrationContainsAndJudge(t *testing.T) {
	t.Parallel()

	scenario := eval.Scenario{
		ID:       "capital-of-france",
		Name:     "migration-agent",
		Revision: migrationRevision,
		Input:    content.AgenticMessages{userText("What is the capital of France?")},
	}

	target := answerTarget{answer: "The capital of France is Paris."}

	// The Contains example: assert the required substring appears in the
	// assistant's output, exactly as the legacy TestCase.ExpectedOutput did.
	contains := exact.RequiredText("Paris")

	// The Judge example: a structured-output rubric judge. The fake client returns
	// a valid ScoreOutput whose 0.9 score clears the AnswerRelevanceV1 pass
	// threshold (0.5), so the judge produces a genuine pass. Empty evidence is a
	// valid structured answer, so the proof needs no message-index coupling.
	judgeClient := fakeJudgeClient{scoreJSON: `{"score":0.9,"reason":"the response directly answers the question","evidence":[]}`}
	relevance := judge.New(
		rubric.AnswerRelevanceV1,
		judgeClient,
		inference.Request{Model: structuredModel()},
	)

	report := evaltest.RunScenario(t, scenario, target, contains, relevance)
	evaltest.RequirePass(t, report)

	// Sanity: both evaluators actually ran (Contains + Judge), so RequirePass is
	// not vacuously satisfied by an empty assessment set.
	if len(report.Samples) != 1 {
		t.Fatalf("got %d samples, want 1", len(report.Samples))
	}
	if got := len(report.Samples[0].Assessments); got != 2 {
		t.Fatalf("got %d assessments, want 2 (exact.RequiredText + judge)", got)
	}
}
