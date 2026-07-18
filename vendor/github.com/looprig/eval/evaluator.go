package eval

import (
	"context"
	"strconv"
	"unicode/utf8"
)

// This file declares the evaluator contract: Descriptor (an evaluator's
// versioned, self-describing metadata) and the Evaluator interface itself. An
// evaluator observes a Sample and reports an Assessment; it never authorizes,
// blocks, retries, or otherwise acts on a session (design principle #1).
//
// The interface is deliberately small (interface segregation): a caller depends
// only on Descriptor and Evaluate. Composite evaluators call named component
// evaluators and disclose those components in their descriptor.
//
// The error-vs-quality boundary is a first-class contract of Evaluate: an
// evaluator's own infrastructure failure (an unreachable judge, a malformed
// structured response, a cancelled context) MUST surface as the error return,
// or as an error-status Assessment built with Errored — never as a StatusFail
// quality verdict. A fail means the subject fell short; an error means the
// evaluator could not decide. See Assessment for the status-consistency rules
// that keep those apart, and CheckRequires for the missing-evidence path that
// yields unverified rather than pass (design principle #4).

// Byte bound for a Descriptor.Description and count bound for its required
// evidence kinds. Both reject absurd or hostile inputs before they reach the
// engine or reports.
const (
	// MaxDescriptionBytes bounds a Descriptor.Description in UTF-8 bytes.
	MaxDescriptionBytes = 1024
	// MaxDescriptorRequires bounds how many required EvidenceKinds one Descriptor
	// may declare.
	MaxDescriptorRequires = 64
)

// Descriptor is an evaluator's versioned, self-describing metadata. Name and
// Revision identify the evaluator (not the target under evaluation); Method is
// descriptive metadata for filtering, cost accounting, and reporting;
// Description is a short, optional prose summary; Requires lists the evidence
// kinds the evaluator needs a sample to carry before it can produce a verdict.
// When a required kind is absent, the evaluator yields unverified — never pass.
type Descriptor struct {
	Name        Name
	Revision    Revision
	Method      Method
	Description string
	Requires    []EvidenceKind
}

// Validate reports whether d is a well-formed descriptor: a valid Name and
// Revision, a known Method, a bounded valid-UTF-8 Description, and a bounded set
// of valid, non-duplicated required EvidenceKinds. An empty Description is valid.
func (d Descriptor) Validate() error {
	if err := d.Name.Validate(); err != nil {
		return err
	}
	if err := d.Revision.Validate(); err != nil {
		return err
	}
	if err := d.Method.Validate(); err != nil {
		return err
	}
	if len(d.Description) > MaxDescriptionBytes {
		return &ValidationError{Field: "Descriptor.Description", Reason: "exceeds " + strconv.Itoa(MaxDescriptionBytes) + " bytes"}
	}
	if !utf8.ValidString(d.Description) {
		return &ValidationError{Field: "Descriptor.Description", Reason: "must be valid UTF-8"}
	}
	return d.validateRequires()
}

// validateRequires validates each required EvidenceKind and rejects a repeated
// kind. Iteration order is not modified.
func (d Descriptor) validateRequires() error {
	if len(d.Requires) > MaxDescriptorRequires {
		return &ValidationError{Field: "Descriptor.Requires", Reason: "exceeds " + strconv.Itoa(MaxDescriptorRequires) + " kinds"}
	}
	seen := make(map[EvidenceKind]struct{}, len(d.Requires))
	for _, k := range d.Requires {
		if err := k.Validate(); err != nil {
			return err
		}
		if _, dup := seen[k]; dup {
			return &DuplicateEvidenceKindError{}
		}
		seen[k] = struct{}{}
	}
	return nil
}

// Evaluator observes a Sample and reports an Assessment. Descriptor returns the
// evaluator's versioned metadata; Evaluate produces the assessment.
//
// Evaluate's error return is reserved for the evaluator's own failure to reach a
// verdict (infrastructure, cancellation, malformed judge output). Such a failure
// must never be encoded as a StatusFail assessment. An implementation may instead
// return a nil error with an Errored assessment when it wants the failure to flow
// through the report as data; it must not silently downgrade an infrastructure
// failure to a quality score.
type Evaluator interface {
	Descriptor() Descriptor
	Evaluate(context.Context, Sample) (Assessment, error)
}

// CheckRequires verifies that s carries every EvidenceKind in d.Requires. When
// all required kinds are present it returns the zero Assessment and ok=true, so
// the caller proceeds with evaluation. When one or more required kinds are
// absent it returns ok=false and an unverified Assessment (built with Unverified)
// naming the missing kinds in a finding — never a pass. This is the enforcement
// point for design principle #4: missing required evidence is unknown, and
// unknown is not pass.
//
// Availability is judged against the sample's observation trace evidence only;
// the required EvidenceKind constants are safe to render, so the finding message
// lists them.
func (d Descriptor) CheckRequires(s Sample) (Assessment, bool) {
	missing := d.missingRequires(s)
	if len(missing) == 0 {
		return Assessment{}, true
	}
	msg := truncateUTF8("missing required evidence: "+joinEvidenceKinds(missing), MaxFindingMessageBytes)
	return Unverified(d, Finding{
		Code:     FindingMissingRequiredEvidence,
		Severity: SeverityMedium,
		Message:  msg,
	}), false
}

// missingRequires returns the required EvidenceKinds absent from s, in the order
// they are declared on the descriptor.
func (d Descriptor) missingRequires(s Sample) []EvidenceKind {
	if len(d.Requires) == 0 {
		return nil
	}
	have := make(map[EvidenceKind]struct{}, len(s.Observation.Trace.Evidence))
	for _, ev := range s.Observation.Trace.Evidence {
		have[ev.Kind] = struct{}{}
	}
	var missing []EvidenceKind
	for _, k := range d.Requires {
		if _, ok := have[k]; !ok {
			missing = append(missing, k)
		}
	}
	return missing
}

// truncateUTF8 returns s truncated to at most maxBytes bytes without splitting a
// multibyte rune, so the result is always valid UTF-8 when s is. It backs the cut
// point up to the nearest rune boundary at or below maxBytes.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	b := maxBytes
	for b > 0 && !utf8.RuneStart(s[b]) {
		b--
	}
	return s[:b]
}

// joinEvidenceKinds renders a list of EvidenceKinds as a comma-separated string.
// The kinds are package constants and safe to render.
func joinEvidenceKinds(kinds []EvidenceKind) string {
	out := make([]byte, 0, len(kinds)*24)
	for i, k := range kinds {
		if i > 0 {
			out = append(out, ", "...)
		}
		out = append(out, string(k)...)
	}
	return string(out)
}
