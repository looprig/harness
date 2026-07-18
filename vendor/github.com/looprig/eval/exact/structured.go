package exact

import (
	"context"

	"github.com/looprig/eval"
)

// codeSchemaResultInvalid identifies a structured-output schema failure.
const codeSchemaResultInvalid eval.FindingCode = "schema_result_invalid"

// schemaResult evaluates whether the subject's structured output satisfied its
// schema, keyed entirely on typed evidence rather than re-parsing model text.
//
// Semantics (three-way, fail-secure):
//
//   - It requires EvidenceUsage, treated as proof the subject actually produced
//     terminal output to assess. If usage evidence is absent, there is nothing to
//     judge and Descriptor.CheckRequires yields Unverified — never a pass.
//   - With usage present, a structured_output_error evidence entry means the
//     output failed schema validation: the verdict is Fail, citing that evidence.
//   - With usage present and no structured-output-error evidence, the output
//     satisfied its schema: the verdict is Pass.
//
// The adapter that runs structured inference is responsible for emitting a
// structured_output_error evidence on any schema failure; its absence alongside
// recorded usage is the positive signal of success.
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
			Requires:    []eval.EvidenceKind{eval.EvidenceUsage},
		},
	}
}

func (e schemaResult) Descriptor() eval.Descriptor { return e.desc }

func (e schemaResult) Evaluate(_ context.Context, s eval.Sample) (eval.Assessment, error) {
	if a, ok := e.desc.CheckRequires(s); !ok {
		return a, nil
	}
	for _, ev := range s.Observation.Trace.Evidence {
		if ev.Kind != eval.EvidenceStructuredError || ev.StructuredError == nil {
			continue
		}
		// The reason is a closed-enum classification and safe to render; the raw
		// model text is never stored on the evidence, so nothing untrusted leaks.
		a := eval.Fail(e.desc, eval.Finding{
			Code:     codeSchemaResultInvalid,
			Severity: eval.SeverityHigh,
			Message:  "structured output failed schema validation: " + string(ev.StructuredError.Reason),
			Evidence: []eval.EvidenceRef{{Evidence: ev.ID}},
		})
		a.Evidence = []eval.Evidence{ev}
		return a, nil
	}
	return eval.Pass(e.desc), nil
}
