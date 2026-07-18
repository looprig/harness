package rubric

// This file declares the typed, classifiable errors returned by rubric
// validation. Callers classify with errors.As, never by matching strings.
// Diagnostic text is drawn only from a fixed vocabulary of field names and
// reasons; it never embeds rubric content (definitions, descriptions, IDs), so
// an oversized or hostile rubric value cannot leak through an error.

// ValidationError reports that a rubric, criterion, or anchor failed validation.
// Field names the offending domain field; Reason is a short, safe explanation
// drawn only from package constants and bounds. Neither field ever contains the
// offending value.
type ValidationError struct {
	// Field is the domain field name, e.g. "Rubric.Definition" or
	// "Criterion.Score".
	Field string
	// Reason is a bounded, safe explanation, e.g. "must not be empty".
	Reason string
}

func (e *ValidationError) Error() string {
	return "rubric: invalid " + e.Field + ": " + e.Reason
}

// DuplicateCriterionError reports that a rubric declared two criteria with the
// same ID, which would make the criterion set ambiguous. The offending ID is
// withheld from the message: it is caller-supplied and must not leak through a
// diagnostic.
type DuplicateCriterionError struct{}

func (e *DuplicateCriterionError) Error() string {
	return "rubric: duplicate criterion id"
}
