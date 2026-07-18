package judge

import (
	"strconv"

	"github.com/looprig/eval"
)

// This file declares the typed, classifiable errors the judge returns. Every
// distinct failure mode is a concrete struct with an Error() method (and
// Unwrap() when it carries a cause), so callers classify with errors.As, never
// by matching strings. No error echoes untrusted content: not the model's raw
// output, not its reason text, not a quote, not the conversation. Only safe
// classifications, bounds, and non-negative indices appear in a message.

// UnsupportedStructuredOutputError reports that the configured model cannot
// satisfy the structured-output contract the judge requires. The judge fails
// secure: it never falls back to parsing free-form text into a score. Cause is
// the underlying inference feature error and is available via Unwrap; the
// message never renders it, since it may carry model metadata.
type UnsupportedStructuredOutputError struct {
	Cause error
}

func (e *UnsupportedStructuredOutputError) Error() string {
	return "judge: model does not support the required structured output"
}

func (e *UnsupportedStructuredOutputError) Unwrap() error { return e.Cause }

// RequestInvalidError reports that the judge assembled a request the inference
// layer rejected before any model was called (an invalid schema or feature
// combination). It is a configuration failure, not a verdict. Cause is available
// via Unwrap but never rendered.
type RequestInvalidError struct {
	Cause error
}

func (e *RequestInvalidError) Error() string {
	return "judge: assembled inference request is invalid"
}

func (e *RequestInvalidError) Unwrap() error { return e.Cause }

// InferenceError reports that the inference call itself failed: an unreachable
// provider, a transport error, a cancelled context, or an exceeded deadline. The
// underlying error is available via Unwrap (so callers can test for
// context.DeadlineExceeded) but is never rendered, since it may originate outside
// the process and carry untrusted content.
type InferenceError struct {
	Cause error
}

func (e *InferenceError) Error() string {
	return "judge: inference call failed"
}

func (e *InferenceError) Unwrap() error { return e.Cause }

// MalformedOutputError reports that the model's structured output could not be
// extracted or decoded into the score schema: not valid JSON, not the schema
// shape, or an oversized reason or evidence set. Reason is a closed-enum
// classification (safe to render); the raw model text is never retained. Cause,
// when set, is the underlying inference extraction error, available via Unwrap.
type MalformedOutputError struct {
	Reason eval.StructuredErrorReason
	Cause  error
}

func (e *MalformedOutputError) Error() string {
	return "judge: malformed structured output: " + string(e.Reason)
}

func (e *MalformedOutputError) Unwrap() error { return e.Cause }

// ScoreRangeError reports that the decoded score was non-finite or fell outside
// the rubric's declared range. Score, Min, and Max are safe numbers computed or
// bounded by the judge, so they are rendered; HasScore distinguishes a
// non-finite score (whose value is withheld as meaningless) from an in-band but
// out-of-range one.
type ScoreRangeError struct {
	Score    float64
	Min      float64
	Max      float64
	HasScore bool
}

func (e *ScoreRangeError) Error() string {
	rng := "[" + formatScore(e.Min) + "," + formatScore(e.Max) + "]"
	if !e.HasScore {
		return "judge: score is not a finite number in range " + rng
	}
	return "judge: score " + formatScore(e.Score) + " outside range " + rng
}

// MessageIndexError reports that a quoted-evidence entry addressed a message
// index outside the conversation. Index and Len are safe integers; no
// conversation content is embedded.
type MessageIndexError struct {
	Index int
	Len   int
}

func (e *MessageIndexError) Error() string {
	return "judge: evidence message index " + strconv.Itoa(e.Index) +
		" out of range [0," + strconv.Itoa(e.Len) + ")"
}

// QuoteNotFoundError reports that a quoted-evidence entry's quote was empty,
// oversized, or did not appear verbatim in the message it named — a provenance
// failure. The offending quote is deliberately withheld: it is untrusted
// conversation-derived text and must not leak through a diagnostic. Only the
// safe message index is rendered.
type QuoteNotFoundError struct {
	Index int
}

func (e *QuoteNotFoundError) Error() string {
	return "judge: quoted evidence not found in message " + strconv.Itoa(e.Index)
}

// RubricInvalidError reports that the judge was configured with a rubric that
// does not validate. Cause is the rubric validation error, available via Unwrap;
// it is drawn from the rubric package's safe vocabulary and carries no content,
// but the judge still does not render it here to keep the message fixed.
type RubricInvalidError struct {
	Cause error
}

func (e *RubricInvalidError) Error() string {
	return "judge: configured rubric is invalid"
}

func (e *RubricInvalidError) Unwrap() error { return e.Cause }

// formatScore renders a safe float score with minimal digits for a diagnostic.
func formatScore(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
