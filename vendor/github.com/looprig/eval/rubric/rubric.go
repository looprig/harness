// Package rubric declares evaluation rubrics: the trusted definition of what
// "good" means for a model judge. A rubric names the quality being judged, the
// prose definition of it, the criteria a judge weighs, and the labeled anchor
// points that give the numeric scale meaning. Rubrics are pure data validated at
// the trust boundary; they carry no model, no client, and no inference
// dependency. The judge package (which does depend on inference) turns a rubric
// into a structured-output request.
//
// A rubric scores on a single overall scale whose bounds are the envelope of its
// criteria score ranges (the minimum MinScore to the maximum MaxScore across all
// criteria). Every built-in rubric uses a uniform [0,1] scale on which a higher
// score always means "better": for the safety rubrics (toxicity, vulgarity) the
// criterion measures the ABSENCE of the undesirable trait, so 1.0 is best and a
// pass is a high score, keeping pass-high semantics uniform across the catalog.
// The pass/fail threshold is the midpoint of the scale (see PassThreshold).
package rubric

import (
	"math"
	"strconv"
	"unicode/utf8"

	"github.com/looprig/eval"
)

// Byte and count bounds. They reject absurd or hostile rubric values before a
// rubric is expanded into a judge prompt or a report. Byte counts, not runes.
const (
	// MaxDefinitionBytes bounds a Rubric.Definition. It is <= the eval
	// Descriptor.Description bound so a rubric definition can be carried verbatim
	// as the evaluator description.
	MaxDefinitionBytes = 1024
	// MaxCriterionDescriptionBytes bounds a Criterion.Description.
	MaxCriterionDescriptionBytes = 512
	// MaxAnchorDescriptionBytes bounds an Anchor.Description.
	MaxAnchorDescriptionBytes = 512
	// MaxCriteria bounds how many criteria one rubric may declare.
	MaxCriteria = 64
	// MaxAnchors bounds how many anchors one rubric may declare.
	MaxAnchors = 64
)

// Criterion is one qualitative dimension a judge weighs when scoring a rubric.
// ID names the dimension and keys the criterion set (unique within a rubric);
// Description is bounded prose the judge is shown; MinScore and MaxScore bound
// the criterion's contribution and must satisfy MinScore < MaxScore with both
// finite. Every built-in criterion uses the same [0,1] range so the rubric's
// overall scale is unambiguous.
type Criterion struct {
	ID          eval.Name
	Description string
	MinScore    float64
	MaxScore    float64
}

// Validate reports whether c is a well-formed criterion.
func (c Criterion) Validate() error {
	if err := c.ID.Validate(); err != nil {
		return &ValidationError{Field: "Criterion.ID", Reason: "must be a valid name"}
	}
	if len(c.Description) > MaxCriterionDescriptionBytes {
		return &ValidationError{Field: "Criterion.Description", Reason: "exceeds " + strconv.Itoa(MaxCriterionDescriptionBytes) + " bytes"}
	}
	if !utf8.ValidString(c.Description) {
		return &ValidationError{Field: "Criterion.Description", Reason: "must be valid UTF-8"}
	}
	if !finite(c.MinScore) || !finite(c.MaxScore) {
		return &ValidationError{Field: "Criterion.Score", Reason: "scores must be finite"}
	}
	if !(c.MinScore < c.MaxScore) {
		return &ValidationError{Field: "Criterion.Score", Reason: "MinScore must be less than MaxScore"}
	}
	return nil
}

// Anchor is a labeled reference point on the rubric's overall score scale. It
// gives a specific numeric Score a human meaning (Label) and an explanation
// (Description) the judge is shown, so the scale is not left to the model's
// imagination. Score must be finite and lie within the rubric's overall range.
type Anchor struct {
	Score       float64
	Label       eval.Name
	Description string
}

// validate reports whether a is a well-formed anchor in isolation. Its
// membership in the rubric's overall score range is checked by Rubric.Validate,
// which alone knows that range.
func (a Anchor) validate() error {
	if !finite(a.Score) {
		return &ValidationError{Field: "Anchor.Score", Reason: "must be finite"}
	}
	if err := a.Label.Validate(); err != nil {
		return &ValidationError{Field: "Anchor.Label", Reason: "must be a valid name"}
	}
	if len(a.Description) > MaxAnchorDescriptionBytes {
		return &ValidationError{Field: "Anchor.Description", Reason: "exceeds " + strconv.Itoa(MaxAnchorDescriptionBytes) + " bytes"}
	}
	if !utf8.ValidString(a.Description) {
		return &ValidationError{Field: "Anchor.Description", Reason: "must be valid UTF-8"}
	}
	return nil
}

// Rubric is the trusted definition of a quality a model judge scores. It is pure
// data: Name and Revision identify the versioned rubric, Scope names the
// granularity it applies to, Definition is the prose meaning of the quality,
// Criteria are the dimensions the judge weighs, and Anchors give the numeric
// scale meaning.
type Rubric struct {
	Name       eval.Name
	Revision   eval.Revision
	Scope      eval.Scope
	Definition string
	Criteria   []Criterion
	Anchors    []Anchor
}

// Validate reports whether r is well-formed: a valid Name, Revision, and Scope; a
// bounded, non-empty, valid-UTF-8 Definition; a bounded, non-empty set of valid
// criteria with unique IDs; and a bounded set of valid anchors whose scores lie
// within the rubric's overall score range. It never echoes rubric content in a
// diagnostic.
func (r Rubric) Validate() error {
	if err := r.Name.Validate(); err != nil {
		return &ValidationError{Field: "Rubric.Name", Reason: "must be a valid name"}
	}
	if err := r.Revision.Validate(); err != nil {
		return &ValidationError{Field: "Rubric.Revision", Reason: "must be a valid revision"}
	}
	if err := r.Scope.Validate(); err != nil {
		return &ValidationError{Field: "Rubric.Scope", Reason: "must be a known scope"}
	}
	if r.Definition == "" {
		return &ValidationError{Field: "Rubric.Definition", Reason: "must not be empty"}
	}
	if len(r.Definition) > MaxDefinitionBytes {
		return &ValidationError{Field: "Rubric.Definition", Reason: "exceeds " + strconv.Itoa(MaxDefinitionBytes) + " bytes"}
	}
	if !utf8.ValidString(r.Definition) {
		return &ValidationError{Field: "Rubric.Definition", Reason: "must be valid UTF-8"}
	}
	if err := r.validateCriteria(); err != nil {
		return err
	}
	return r.validateAnchors()
}

// validateCriteria validates each criterion and rejects a repeated ID.
func (r Rubric) validateCriteria() error {
	if len(r.Criteria) == 0 {
		return &ValidationError{Field: "Rubric.Criteria", Reason: "must not be empty"}
	}
	if len(r.Criteria) > MaxCriteria {
		return &ValidationError{Field: "Rubric.Criteria", Reason: "exceeds " + strconv.Itoa(MaxCriteria) + " criteria"}
	}
	seen := make(map[eval.Name]struct{}, len(r.Criteria))
	for _, c := range r.Criteria {
		if err := c.Validate(); err != nil {
			return err
		}
		if _, dup := seen[c.ID]; dup {
			return &DuplicateCriterionError{}
		}
		seen[c.ID] = struct{}{}
	}
	return nil
}

// validateAnchors validates each anchor and confirms its score lies within the
// rubric's overall score range. It runs after validateCriteria so the range is
// well-defined.
func (r Rubric) validateAnchors() error {
	if len(r.Anchors) > MaxAnchors {
		return &ValidationError{Field: "Rubric.Anchors", Reason: "exceeds " + strconv.Itoa(MaxAnchors) + " anchors"}
	}
	minScore, maxScore := r.scoreRange()
	for _, a := range r.Anchors {
		if err := a.validate(); err != nil {
			return err
		}
		if a.Score < minScore || a.Score > maxScore {
			return &ValidationError{Field: "Anchor.Score", Reason: "outside the rubric score range"}
		}
	}
	return nil
}

// ScoreRange returns the rubric's overall score scale [min, max]: the envelope of
// its criteria score ranges. It is only meaningful for a rubric that has passed
// Validate (non-empty criteria); on an empty rubric it returns the unit range.
func (r Rubric) ScoreRange() (float64, float64) {
	return r.scoreRange()
}

// scoreRange computes the envelope of the criteria score ranges.
func (r Rubric) scoreRange() (float64, float64) {
	if len(r.Criteria) == 0 {
		return 0, 1
	}
	minScore := math.Inf(1)
	maxScore := math.Inf(-1)
	for _, c := range r.Criteria {
		if c.MinScore < minScore {
			minScore = c.MinScore
		}
		if c.MaxScore > maxScore {
			maxScore = c.MaxScore
		}
	}
	return minScore, maxScore
}

// PassThreshold returns the score at or above which the rubric's verdict is a
// pass: the midpoint of the overall score range. A judge score below the
// threshold is a fail. Because every rubric scores "better" as higher, a single
// midpoint threshold is a uniform, documented rule; a rubric that needs a
// different cut can express it through its criteria range.
func (r Rubric) PassThreshold() float64 {
	minScore, maxScore := r.scoreRange()
	return minScore + (maxScore-minScore)/2
}

// finite reports whether f is a real, finite number (not NaN or ±Inf).
func finite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
