package judge

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/looprig/core/content"
	"github.com/looprig/eval"
	"github.com/looprig/inference"
)

// This file declares the versioned structured-output contract the scalar rubric
// judge uses: the ScoreOutput Go type the model must produce, the JSON Schema
// that constrains it on the wire, and the local re-validation that is the real
// authority. The wire schema is a hint to the model and the provider codec; it
// can express shape but not the domain invariants (score range, evidence-index
// bounds, quote provenance). Those are enforced here, after decoding, so a model
// that returns a well-shaped but invalid object is caught and never inferred
// into a pass.

const (
	// ScoreSchemaRevision is the stable revision of the score schema contract.
	// Bumping the schema's meaning requires a new revision.
	ScoreSchemaRevision eval.Revision = "score/v1"

	// scoreSchemaName is the OutputSchema name sent to the provider. It satisfies
	// the inference name grammar (leading letter/underscore, then word/dash).
	scoreSchemaName = "rubric_score_v1"

	// MaxReasonBytes bounds the model-authored reason. The reason is untrusted and
	// never echoed in a diagnostic; this bound keeps a hostile model from
	// ballooning the response before it is discarded.
	MaxReasonBytes = 4096

	// MaxEvidenceQuotes bounds how many quoted-evidence entries the model may
	// return. It backs both the bounded evidence instruction in the prompt and the
	// local validation cap, so the two can never drift.
	MaxEvidenceQuotes = 8

	// MaxQuoteBytes bounds a single evidence quote before provenance is checked.
	MaxQuoteBytes = 512
)

// QuotedEvidence is one verbatim quote the judge cites to justify its score,
// addressing the conversation message it came from by index. Both fields cross
// the JSON boundary and are validated immediately; the quote is never rendered
// back into a diagnostic.
type QuotedEvidence struct {
	MessageIndex int    `json:"message_index"`
	Quote        string `json:"quote"`
}

// ScoreOutput is the decoded judge answer: a scalar Score, a bounded Reason, and
// a bounded set of QuotedEvidence. It is the immediate-narrowing target of the
// model's structured output; nothing else in the judge trusts raw model text.
type ScoreOutput struct {
	Score    float64          `json:"score"`
	Reason   string           `json:"reason"`
	Evidence []QuotedEvidence `json:"evidence"`
}

// scoreSchemaJSON is the portable JSON Schema constraining ScoreOutput on the
// wire. It uses only the bounded subset inference.ValidateOutputSchema accepts:
// object roots with additionalProperties:false and every property required, and
// no numeric-range keywords. The score range, index bounds, reason length, and
// quote provenance are therefore NOT expressible here and are enforced by
// scoreSchema.Validate after decoding.
const scoreSchemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "score": {
      "type": "number",
      "description": "The overall rubric score on the rubric's declared scale."
    },
    "reason": {
      "type": "string",
      "description": "A brief justification for the score, grounded in the DATA messages."
    },
    "evidence": {
      "type": "array",
      "description": "Short verbatim quotes from the DATA messages that justify the score.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "message_index": {
            "type": "integer",
            "description": "Zero-based index of the DATA message the quote is taken from."
          },
          "quote": {
            "type": "string",
            "description": "A short verbatim substring of that message."
          }
        },
        "required": ["message_index", "quote"]
      }
    }
  },
  "required": ["score", "reason", "evidence"]
}`

// scoreSchema is the score schema contract value. It bundles the wire schema and
// the local re-validation so the two are versioned together.
type scoreSchema struct {
	revision eval.Revision
	name     string
	schema   string
}

// ScoreSchemaV1 is the version-1 score schema: the wire JSON Schema plus the
// authoritative local validation of score range, evidence-index bounds, bounded
// reason length, and quote provenance.
var ScoreSchemaV1 = scoreSchema{
	revision: ScoreSchemaRevision,
	name:     scoreSchemaName,
	schema:   scoreSchemaJSON,
}

// Revision returns the schema's stable revision.
func (s scoreSchema) Revision() eval.Revision { return s.revision }

// OutputSchema returns the inference OutputSchema that requests this contract as
// strict structured output. Strict is always true: the judge never accepts
// best-effort structured output.
func (s scoreSchema) OutputSchema() inference.OutputSchema {
	return inference.OutputSchema{
		Name:        s.name,
		Description: "A rubric score with justification and quoted evidence.",
		Schema:      json.RawMessage(s.schema),
		Strict:      true,
	}
}

// Decode strictly decodes one structured-output JSON object into a ScoreOutput,
// rejecting unknown fields and trailing data. It narrows the JSON boundary
// immediately; it does not apply domain invariants (Validate does). A decode
// failure yields a MalformedOutputError carrying only a safe classification.
func (s scoreSchema) Decode(raw []byte) (ScoreOutput, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var out ScoreOutput
	if err := decoder.Decode(&out); err != nil {
		return ScoreOutput{}, &MalformedOutputError{Reason: eval.StructuredErrorSchemaMismatch}
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ScoreOutput{}, &MalformedOutputError{Reason: eval.StructuredErrorSchemaMismatch}
	}
	return out, nil
}

// Validate re-checks a decoded ScoreOutput against the domain invariants the
// wire schema cannot express, using the rubric's score range [minScore,maxScore]
// and the conversation the quotes must come from. Every failure is a typed,
// classifiable error that never echoes the model's text or the conversation.
//
//   - Score must be finite and within [minScore, maxScore].
//   - Reason must be valid UTF-8 and within MaxReasonBytes.
//   - Evidence must hold at most MaxEvidenceQuotes entries.
//   - Each evidence MessageIndex must address a message in conv.
//   - Each quote must be non-empty, bounded, and a verbatim substring of the
//     text of the conversation message it names (provenance).
func (s scoreSchema) Validate(out ScoreOutput, minScore, maxScore float64, conv content.AgenticMessages) error {
	if !finite(out.Score) {
		return &ScoreRangeError{Min: minScore, Max: maxScore}
	}
	if out.Score < minScore || out.Score > maxScore {
		return &ScoreRangeError{Score: out.Score, Min: minScore, Max: maxScore, HasScore: true}
	}
	if !utf8.ValidString(out.Reason) || len(out.Reason) > MaxReasonBytes {
		return &MalformedOutputError{Reason: eval.StructuredErrorSchemaMismatch}
	}
	if len(out.Evidence) > MaxEvidenceQuotes {
		return &MalformedOutputError{Reason: eval.StructuredErrorSchemaMismatch}
	}
	for _, ev := range out.Evidence {
		if ev.MessageIndex < 0 || ev.MessageIndex >= len(conv) {
			return &MessageIndexError{Index: ev.MessageIndex, Len: len(conv)}
		}
		if ev.Quote == "" || len(ev.Quote) > MaxQuoteBytes || !utf8.ValidString(ev.Quote) {
			return &QuoteNotFoundError{Index: ev.MessageIndex}
		}
		if !strings.Contains(messageText(conv[ev.MessageIndex]), ev.Quote) {
			return &QuoteNotFoundError{Index: ev.MessageIndex}
		}
	}
	return nil
}

// finite reports whether f is a real, finite number (not NaN or ±Inf).
func finite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
