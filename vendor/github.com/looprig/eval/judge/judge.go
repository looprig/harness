// Package judge implements the structured-output model judge: an eval.Evaluator
// that scores a sample's conversation against a rubric by calling an
// inference.Client with strict structured output. It is the first eval package
// permitted to depend on github.com/looprig/inference.
//
// The judge is fail-secure end to end. It builds an inference.Request whose only
// instructions come from the trusted rubric and whose data is the untrusted
// conversation (see prompt.go); it requires the model to answer through a strict
// output schema (see schema.go); and it re-validates the decoded answer locally
// before trusting it. Every path that cannot produce a well-formed, in-range,
// provenance-checked score returns a typed error and never an inferred pass. A
// model that cannot satisfy the schema yields an unsupported/malformed error, not
// a guessed verdict.
package judge

import (
	"context"
	"errors"

	"github.com/looprig/core/content"
	"github.com/looprig/eval"
	"github.com/looprig/eval/rubric"
	"github.com/looprig/inference"
)

// Finding codes and evidence IDs the judge attaches to its assessment. They are
// safe, fixed identifiers, never derived from model output.
const (
	findingScore        eval.FindingCode = "rubric_judge_score"
	evidenceIDUsage     eval.EvidenceID  = "judge_usage"
	evidenceIDDiagnosis eval.EvidenceID  = "judge_diagnostic"
	diagnosticCode      eval.Name        = "rubric_judge"
)

// options holds the tunable judge configuration set through Option values.
type options struct {
	measurementName eval.Name
}

// Option configures a judge built by New.
type Option func(*options)

// WithMeasurementName overrides the name of the score measurement the judge
// produces. It defaults to the rubric's Name, so reports key the score on the
// rubric; a caller scoring several subjects under one rubric can disambiguate.
func WithMeasurementName(name eval.Name) Option {
	return func(o *options) { o.measurementName = name }
}

// evaluator is the concrete judge. It holds the rubric it scores against, the
// inference client it calls, and the request template that carries the Model and
// any System/sampling defaults. The template's Messages and Output are ignored
// for the data and schema — Evaluate fills those — but its System is prepended to
// the rubric instruction so a caller can add a persona.
type evaluator struct {
	rubric          rubric.Rubric
	client          inference.Client
	template        inference.Request
	desc            eval.Descriptor
	measurementName eval.Name
}

// New returns a rubric judge as an eval.Evaluator. The template supplies the
// Model (structured-output-capable) and any System or sampling defaults; the
// judge fills the request's Messages (the untrusted conversation) and Output (the
// strict score schema) on each Evaluate. The returned evaluator's Descriptor uses
// the rubric's Name and Revision and reports Method = MethodModel.
//
// New does not itself reject an invalid rubric or client: an invalid rubric
// surfaces as a typed error from Evaluate (and from Descriptor().Validate() at a
// runner's preflight), and a nil client surfaces as an inference failure, so the
// failure is contained rather than panicking at construction.
func New(r rubric.Rubric, client inference.Client, template inference.Request, opts ...Option) eval.Evaluator {
	cfg := options{measurementName: r.Name}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &evaluator{
		rubric:   r,
		client:   client,
		template: template,
		desc: eval.Descriptor{
			Name:        r.Name,
			Revision:    r.Revision,
			Method:      eval.MethodModel,
			Description: r.Definition,
		},
		measurementName: cfg.measurementName,
	}
}

// Descriptor returns the judge's versioned metadata.
func (e *evaluator) Descriptor() eval.Descriptor { return e.desc }

// Evaluate scores the sample's conversation against the rubric. It returns a
// validated MethodModel assessment on success, or a typed error (never a pass) on
// any failure to reach a trustworthy score.
func (e *evaluator) Evaluate(ctx context.Context, s eval.Sample) (eval.Assessment, error) {
	if err := e.rubric.Validate(); err != nil {
		return eval.Assessment{}, &RubricInvalidError{Cause: err}
	}

	conv := s.Observation.Conversation
	req, err := e.buildRequest(conv)
	if err != nil {
		return eval.Assessment{}, err
	}

	resp, err := e.client.Invoke(ctx, req)
	if err != nil {
		return eval.Assessment{}, &InferenceError{Cause: err}
	}

	raw, err := inference.StructuredResult(resp)
	if err != nil {
		return eval.Assessment{}, &MalformedOutputError{Reason: classifyExtractionError(err), Cause: err}
	}

	out, err := ScoreSchemaV1.Decode(raw)
	if err != nil {
		return eval.Assessment{}, err
	}

	minScore, maxScore := e.rubric.ScoreRange()
	if err := ScoreSchemaV1.Validate(out, minScore, maxScore, conv); err != nil {
		return eval.Assessment{}, err
	}

	return e.assess(out, resp), nil
}

// buildRequest assembles the strict structured-output request: the trusted rubric
// instruction as System (prefixed by any template System), the untrusted
// conversation as the final data message, and the score schema as strict Output.
// It validates the request's features before returning, mapping an unsupported
// structured-output model to a typed, fail-secure error rather than a fallback.
func (e *evaluator) buildRequest(conv content.AgenticMessages) (inference.Request, error) {
	req := e.template
	req.System = combineSystem(e.template.System, systemInstruction(e.rubric))

	msgs := make(content.AgenticMessages, 0, len(e.template.Messages)+1)
	msgs = append(msgs, e.template.Messages...)
	msgs = append(msgs, dataMessage(conv))
	req.Messages = msgs

	schema := ScoreSchemaV1.OutputSchema()
	req.Output = &schema

	if err := inference.ValidateRequestFeatures(req); err != nil {
		var unsupported *inference.StructuredOutputUnsupportedError
		var unsupportedWithTools *inference.StructuredOutputWithToolsUnsupportedError
		if errors.As(err, &unsupported) || errors.As(err, &unsupportedWithTools) {
			return inference.Request{}, &UnsupportedStructuredOutputError{Cause: err}
		}
		return inference.Request{}, &RequestInvalidError{Cause: err}
	}
	return req, nil
}

// assess builds the validated MethodModel assessment from a re-validated score.
// It records the score as a ratio measurement, derives the pass/fail verdict from
// the rubric threshold, and attaches usage and a diagnostic as MethodModel
// evidence that the verdict's finding resolves against.
func (e *evaluator) assess(out ScoreOutput, resp *inference.Response) eval.Assessment {
	evidence := []eval.Evidence{e.usageEvidence(resp), diagnosticEvidence()}

	status := eval.StatusFail
	severity := eval.SeverityMedium
	message := "rubric score " + formatScore(out.Score) + " is below the pass threshold " + formatScore(e.rubric.PassThreshold())
	if out.Score >= e.rubric.PassThreshold() {
		status = eval.StatusPass
		severity = eval.SeverityInfo
		message = "rubric score " + formatScore(out.Score) + " meets the pass threshold " + formatScore(e.rubric.PassThreshold())
	}

	return eval.Assessment{
		Evaluator: e.desc.Name,
		Revision:  e.desc.Revision,
		Status:    status,
		Measurements: []eval.Measurement{{
			Name:  e.measurementName,
			Value: out.Score,
			Unit:  eval.UnitRatio,
		}},
		Findings: []eval.Finding{{
			Code:     findingScore,
			Severity: severity,
			Message:  message,
			Evidence: []eval.EvidenceRef{{Evidence: evidenceIDUsage}, {Evidence: evidenceIDDiagnosis}},
		}},
		Evidence: evidence,
	}
}

// usageEvidence records the judge's own inference token usage as safe counts. It
// never fails the assessment: a nil usage becomes a zero usage, and a model name
// that would not validate as a Revision is dropped rather than embedded.
func (e *evaluator) usageEvidence(resp *inference.Response) eval.Evidence {
	var usage content.Usage
	if resp != nil && resp.Usage != nil {
		usage = *resp.Usage
	}
	var modelRev eval.Revision
	if name := e.template.Model.Name; name != "" {
		if rev := eval.Revision(name); rev.Validate() == nil {
			modelRev = rev
		}
	}
	return eval.Evidence{
		ID:    evidenceIDUsage,
		Kind:  eval.EvidenceUsage,
		Usage: &eval.UsageEvidence{Model: modelRev, Usage: usage},
	}
}

// diagnosticEvidence records that the model judge ran, as a safe, content-free
// evidence entry. Its message is deliberately empty: the model's reason and the
// conversation are untrusted and never placed on evidence here.
func diagnosticEvidence() eval.Evidence {
	return eval.Evidence{
		ID:   evidenceIDDiagnosis,
		Kind: eval.EvidenceDiagnostic,
		Diagnostic: &eval.DiagnosticEvidence{
			Code:     diagnosticCode,
			Severity: eval.SeverityInfo,
		},
	}
}

// combineSystem prepends any template system prompt to the trusted rubric
// instruction, keeping the rubric instruction present and authoritative.
func combineSystem(templateSystem, rubricInstruction string) string {
	if templateSystem == "" {
		return rubricInstruction
	}
	return templateSystem + "\n\n" + rubricInstruction
}

// classifyExtractionError maps an inference structured-output extraction failure
// to a safe eval classification, without retaining the underlying model bytes.
func classifyExtractionError(err error) eval.StructuredErrorReason {
	var malformed *inference.MalformedStructuredOutputError
	if errors.As(err, &malformed) {
		switch malformed.ReasonCode {
		case inference.MalformedReasonEmpty, inference.MalformedReasonNilMessage, inference.MalformedReasonNilResponse:
			return eval.StructuredErrorEmptyOutput
		case inference.MalformedReasonMalformedJSON, inference.MalformedReasonRootNotObject:
			return eval.StructuredErrorInvalidJSON
		default:
			return eval.StructuredErrorSchemaMismatch
		}
	}
	return eval.StructuredErrorSchemaMismatch
}
