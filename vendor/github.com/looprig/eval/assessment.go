package eval

import (
	"math"
	"strconv"
	"time"
	"unicode/utf8"
)

// This file declares an evaluator's verdict: Assessment and its parts,
// Measurement and Finding. An assessment carries several measurements and
// findings rather than one forced scalar, so a semantic judge score can never
// conceal a deterministic operational or security failure.
//
// Assessment.Validate is the boundary check applied before an assessment enters
// a report. It enforces four families of invariant:
//
//   - Measurements: unique names, finite values, valid units.
//   - Findings: unique codes (duplicates are forbidden within one assessment —
//     a code identifies a distinct check), valid codes/severities, bounded
//     messages, and evidence references that resolve.
//   - Evidence: each entry valid, with unique IDs within the assessment.
//   - Status consistency: the declared status must agree with the contents.
//
// Status-consistency rules (documented and enforced):
//
//   - pass may not carry a high- or critical-severity finding. A passing verdict
//     cannot coexist with a serious defect. Lower-severity findings (info, low,
//     medium) are permitted on a pass as advisories.
//   - unverified, error, and skipped may not carry any Measurement. A measurement
//     is a produced quality signal; if the evaluator could not verify
//     (unverified), failed to decide (error), or did not run (skipped), there is
//     no quality score to report. This structurally enforces "unknown is not
//     pass" and keeps an infrastructure failure from smuggling a numeric score.
//   - pass and fail are the verdict statuses and may carry measurements.
//
// Dangling-reference scope: a Finding's EvidenceRef is resolved against
// Assessment.Evidence only. An assessment does not carry the conversation, so a
// message-index component of a ref is shape-checked (non-negative) but not
// resolved here; resolving message indexes against a conversation is the
// observation's responsibility. Every ref that names an EvidenceID must resolve
// to an entry in Assessment.Evidence, or Validate rejects it as dangling.

// Byte bound for a Finding.Message and count bounds for an assessment's
// collections. The message bound keeps an evaluator-authored explanation from
// ballooning a report; the collection bounds reject absurd inputs.
const (
	// MaxFindingMessageBytes bounds a Finding.Message in UTF-8 bytes. The message
	// is evaluator-authored free text: it is bounded here and must be treated as
	// untrusted downstream — in particular it must never be placed in a metric
	// label.
	MaxFindingMessageBytes = 2048
	// MaxAssessmentMeasurements bounds how many measurements one assessment may
	// carry.
	MaxAssessmentMeasurements = 256
	// MaxAssessmentFindings bounds how many findings one assessment may carry.
	MaxAssessmentFindings = 256
	// MaxAssessmentEvidence bounds how many evidence entries one assessment may
	// carry.
	MaxAssessmentEvidence = 1024
)

// FindingCode is a stable, safe identifier for a distinct check a finding
// reports on (for example "canary_leak" or "missing_required_evidence"). It is
// evaluator-defined but must be a well-formed identifier so it can key a finding
// set and appear safely in reports. A valid FindingCode is non-empty, valid
// UTF-8, and no longer than MaxNameBytes bytes.
type FindingCode string

// Validate reports whether c is a well-formed FindingCode.
func (c FindingCode) Validate() error {
	return validateIdentifier("FindingCode", string(c), MaxNameBytes)
}

// FindingMissingRequiredEvidence is the code CheckRequires attaches to the
// unverified assessment it produces when a required EvidenceKind is absent.
const FindingMissingRequiredEvidence FindingCode = "missing_required_evidence"

// Measurement is one named numeric result an evaluator produced. Value must be
// finite; Unit names its dimension. Measurement names are unique within an
// assessment so a report can key on them.
type Measurement struct {
	Name  Name
	Value float64
	Unit  Unit
}

// Validate reports whether m is a well-formed measurement: a valid Name, a
// finite Value (never NaN or ±Inf), and a known Unit. The value is safe to
// render, but the offending name is not echoed on failure.
func (m Measurement) Validate() error {
	if err := m.Name.Validate(); err != nil {
		return err
	}
	if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
		return &ValidationError{Field: "Measurement.Value", Reason: "must be finite"}
	}
	return m.Unit.Validate()
}

// Finding is one qualitative issue an evaluator reports. Code identifies the
// check; Severity ranks it; Message is bounded evaluator-authored prose (treated
// as untrusted downstream); Evidence points at supporting evidence in the
// enclosing assessment. Findings never carry a raw transcript.
type Finding struct {
	Code     FindingCode
	Severity Severity
	Message  string
	Evidence []EvidenceRef
}

// validateShape reports whether f is well-formed in isolation: a valid Code and
// Severity, a bounded valid-UTF-8 Message, and well-shaped evidence references.
// It does not resolve references against the assessment's evidence;
// Assessment.Validate does that.
func (f Finding) validateShape() error {
	if err := f.Code.Validate(); err != nil {
		return err
	}
	if err := f.Severity.Validate(); err != nil {
		return err
	}
	if len(f.Message) > MaxFindingMessageBytes {
		return &ValidationError{Field: "Finding.Message", Reason: "exceeds " + strconv.Itoa(MaxFindingMessageBytes) + " bytes"}
	}
	if !utf8.ValidString(f.Message) {
		return &ValidationError{Field: "Finding.Message", Reason: "must be valid UTF-8"}
	}
	for _, ref := range f.Evidence {
		if err := ref.validateShape(); err != nil {
			return err
		}
	}
	return nil
}

// Assessment is an evaluator's verdict on a sample. Evaluator and Revision echo
// the descriptor's identity (the evaluator, not the target). Status is the
// terminal disposition. Measurements and Findings carry the quantitative and
// qualitative results; Evidence carries the facts they reference; Duration is
// how long the evaluation took.
type Assessment struct {
	Evaluator    Name
	Revision     Revision
	Status       AssessmentStatus
	Measurements []Measurement
	Findings     []Finding
	Evidence     []Evidence
	Duration     time.Duration
}

// Validate reports whether a is well-formed and internally consistent. See the
// file comment for the measurement, finding, evidence, dangling-reference, and
// status-consistency rules it enforces.
func (a Assessment) Validate() error {
	if err := a.Evaluator.Validate(); err != nil {
		return err
	}
	if err := a.Revision.Validate(); err != nil {
		return err
	}
	if err := a.Status.Validate(); err != nil {
		return err
	}
	if a.Duration < 0 {
		return &ValidationError{Field: "Assessment.Duration", Reason: "must not be negative"}
	}
	if err := a.validateMeasurements(); err != nil {
		return err
	}
	ids, err := a.validateEvidence()
	if err != nil {
		return err
	}
	if err := a.validateFindings(ids); err != nil {
		return err
	}
	return a.validateStatusConsistency()
}

// validateMeasurements validates each measurement and rejects a repeated Name.
func (a Assessment) validateMeasurements() error {
	if len(a.Measurements) > MaxAssessmentMeasurements {
		return &ValidationError{Field: "Assessment.Measurements", Reason: "exceeds " + strconv.Itoa(MaxAssessmentMeasurements) + " measurements"}
	}
	seen := make(map[Name]struct{}, len(a.Measurements))
	for _, m := range a.Measurements {
		if err := m.Validate(); err != nil {
			return err
		}
		if _, dup := seen[m.Name]; dup {
			return &DuplicateMeasurementError{}
		}
		seen[m.Name] = struct{}{}
	}
	return nil
}

// validateEvidence validates each evidence entry and rejects duplicate IDs. It
// returns the set of evidence IDs for finding-reference resolution.
func (a Assessment) validateEvidence() (map[EvidenceID]struct{}, error) {
	if len(a.Evidence) > MaxAssessmentEvidence {
		return nil, &ValidationError{Field: "Assessment.Evidence", Reason: "exceeds " + strconv.Itoa(MaxAssessmentEvidence) + " entries"}
	}
	ids := make(map[EvidenceID]struct{}, len(a.Evidence))
	for _, ev := range a.Evidence {
		if err := ev.Validate(); err != nil {
			return nil, err
		}
		if _, dup := ids[ev.ID]; dup {
			return nil, &DuplicateEvidenceError{}
		}
		ids[ev.ID] = struct{}{}
	}
	return ids, nil
}

// validateFindings validates each finding, rejects a repeated Code, and resolves
// every finding's evidence references against ids. A reference that names an
// EvidenceID absent from the assessment's evidence is a dangling reference and
// rejected.
func (a Assessment) validateFindings(ids map[EvidenceID]struct{}) error {
	if len(a.Findings) > MaxAssessmentFindings {
		return &ValidationError{Field: "Assessment.Findings", Reason: "exceeds " + strconv.Itoa(MaxAssessmentFindings) + " findings"}
	}
	seen := make(map[FindingCode]struct{}, len(a.Findings))
	for _, f := range a.Findings {
		if err := f.validateShape(); err != nil {
			return err
		}
		if _, dup := seen[f.Code]; dup {
			return &DuplicateFindingError{}
		}
		seen[f.Code] = struct{}{}
		for _, ref := range f.Evidence {
			if ref.Evidence != "" {
				if _, ok := ids[ref.Evidence]; !ok {
					return &UnknownEvidenceError{}
				}
			}
		}
	}
	return nil
}

// validateStatusConsistency enforces the status/content agreement rules. It runs
// last, after the parts are individually valid, so its judgement rests on
// well-formed measurements and findings.
func (a Assessment) validateStatusConsistency() error {
	switch a.Status {
	case StatusPass:
		for _, f := range a.Findings {
			if f.Severity == SeverityHigh || f.Severity == SeverityCritical {
				return &StatusConsistencyError{Status: a.Status, Reason: statusReasonPassSevereFinding}
			}
		}
	case StatusUnverified, StatusError, StatusSkipped:
		if len(a.Measurements) > 0 {
			return &StatusConsistencyError{Status: a.Status, Reason: statusReasonMeasurementOnNonVerdict}
		}
	case StatusFail:
		// A fail is a quality verdict and may carry any severity and measurements.
	}
	return nil
}

// Pass returns a passing assessment for desc carrying the given measurements. A
// pass asserts the subject met the expectation; callers must not attach a high-
// or critical-severity finding (Validate rejects it).
func Pass(desc Descriptor, measurements ...Measurement) Assessment {
	return Assessment{
		Evaluator:    desc.Name,
		Revision:     desc.Revision,
		Status:       StatusPass,
		Measurements: measurements,
	}
}

// Fail returns a failing quality verdict for desc, explained by the given
// findings. A fail means the subject fell short — it is not an evaluator error.
func Fail(desc Descriptor, findings ...Finding) Assessment {
	return Assessment{
		Evaluator: desc.Name,
		Revision:  desc.Revision,
		Status:    StatusFail,
		Findings:  findings,
	}
}

// Unverified returns an unverified assessment for desc, explained by the given
// findings. Unverified means no authoritative evidence was available; it is
// never an inferred pass (design principle #4). It carries no measurements, so
// an unknown result can never present a numeric score.
func Unverified(desc Descriptor, findings ...Finding) Assessment {
	return Assessment{
		Evaluator: desc.Name,
		Revision:  desc.Revision,
		Status:    StatusUnverified,
		Findings:  findings,
	}
}

// Errored returns an error-status assessment for desc: the evaluator itself
// failed to reach a verdict (infrastructure, cancellation, malformed judge
// output). Findings may describe the failure. It carries no measurements — an
// infrastructure failure is not a quality score. Prefer returning a non-nil
// error from Evaluate; use Errored when the failure should flow through the
// report as data rather than abort the run.
func Errored(desc Descriptor, findings ...Finding) Assessment {
	return Assessment{
		Evaluator: desc.Name,
		Revision:  desc.Revision,
		Status:    StatusError,
		Findings:  findings,
	}
}

// Skipped returns a skipped assessment for desc: the evaluator was intentionally
// not run. It carries no measurements.
func Skipped(desc Descriptor) Assessment {
	return Assessment{
		Evaluator: desc.Name,
		Revision:  desc.Revision,
		Status:    StatusSkipped,
	}
}
