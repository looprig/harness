package exact

import (
	"context"

	"github.com/looprig/eval"
)

// codeSchemaResultInvalid identifies a structured-output schema failure.
const codeSchemaResultInvalid eval.FindingCode = "schema_result_invalid"

// codeSchemaResultSatisfied identifies a positive structured-output success.
const codeSchemaResultSatisfied eval.FindingCode = "schema_result_satisfied"

// codeSchemaResultUnverified identifies the absence of any structured-output
// evidence, which is unknown — never a pass.
const codeSchemaResultUnverified eval.FindingCode = "schema_result_unverified"

// schemaResult evaluates whether the subject's structured output satisfied its
// schema, keyed entirely on typed structured-output evidence rather than
// re-parsing model text.
//
// Semantics (three-way, fail-secure):
//
//   - A structured_output_error evidence entry means the output failed schema
//     validation: the verdict is Fail, citing that evidence. A failure always
//     wins over a success signal in the same trace.
//   - A positive structured_output evidence entry (and no error) means the output
//     validated against its schema: the verdict is Pass, citing that evidence.
//   - Otherwise there is no structured-output evidence at all: the verdict is
//     Unverified. Generic model-usage evidence is emitted for ANY completion and
//     is NOT proof of structured output, so its presence can never confer a pass.
//
// The adapter that runs structured inference is responsible for emitting a
// structured_output evidence on success and a structured_output_error on schema
// failure; only those positive signals move the verdict off Unverified.
type schemaResult struct {
	desc eval.Descriptor
}

// SchemaResult returns an evaluator that reports whether the sample's structured
// output satisfied its schema. It has no configuration and is never vacuous.
func SchemaResult() eval.Evaluator {
	return schemaResult{
		desc: eval.Descriptor{
			Name:        "exact/schema_result",
			Revision:    evaluatorRevision,
			Method:      eval.MethodProgrammatic,
			Description: "reports whether the subject's structured output satisfied its schema",
		},
	}
}

func (e schemaResult) Descriptor() eval.Descriptor { return e.desc }

func (e schemaResult) Evaluate(_ context.Context, s eval.Sample) (eval.Assessment, error) {
	var positive *eval.Evidence
	for i := range s.Observation.Trace.Evidence {
		ev := s.Observation.Trace.Evidence[i]
		switch {
		case ev.Kind == eval.EvidenceStructuredError && ev.StructuredError != nil:
			// The reason is a closed-enum classification and safe to render; the raw
			// model text is never stored on the evidence, so nothing untrusted leaks.
			// A failure wins immediately, even over a success signal.
			a := eval.Fail(e.desc, eval.Finding{
				Code:     codeSchemaResultInvalid,
				Severity: eval.SeverityHigh,
				Message:  "structured output failed schema validation: " + string(ev.StructuredError.Reason),
				Evidence: []eval.EvidenceRef{{Evidence: ev.ID}},
			})
			a.Evidence = []eval.Evidence{ev}
			return a, nil
		case ev.Kind == eval.EvidenceStructuredOutput && ev.StructuredOutput != nil:
			if positive == nil {
				captured := ev
				positive = &captured
			}
		}
	}

	if positive != nil {
		// A positive structured-output signal: the subject produced output that
		// validated against its schema. Cite it via a resolving reference; the info
		// severity keeps the pass well-formed.
		a := eval.Pass(e.desc)
		a.Evidence = []eval.Evidence{*positive}
		a.Findings = []eval.Finding{{
			Code:     codeSchemaResultSatisfied,
			Severity: eval.SeverityInfo,
			Message:  "structured output validated against its declared schema",
			Evidence: []eval.EvidenceRef{{Evidence: positive.ID}},
		}}
		return a, nil
	}

	// No structured-output evidence at all. Generic usage is not sufficient: we
	// cannot confirm schema conformance, so the result is unknown, not a pass.
	return eval.Unverified(e.desc, eval.Finding{
		Code:     codeSchemaResultUnverified,
		Severity: eval.SeverityMedium,
		Message:  "no structured-output evidence: cannot confirm schema conformance",
	}), nil
}
