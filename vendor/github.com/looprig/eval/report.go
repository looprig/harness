package eval

import (
	"context"
	"time"
	"unicode/utf8"
)

// Sink is the destination contract for a completed Report. It is the core
// reporting seam: the runner (or a continuous-eval loop) hands each Report to a
// Sink, and a Sink persists, forwards, or exports it. Implementations live
// outside the root package (a JSON file sink, an OpenTelemetry sink, an
// in-memory sink) so the root never depends on a storage or wire technology.
// WriteReport takes a context so a slow or networked sink is cancellable, and
// returns a typed error the caller can classify. A Sink must treat the Report as
// untrusted-adjacent: it must not place raw conversation text, judge
// explanations, or secrets in any external label — redaction is the wire form's
// responsibility, enforced by the concrete sink.
type Sink interface {
	WriteReport(context.Context, Report) error
}

// This file declares the output of the execution engine: Report and its parts.
// A Report is a plain, ordered data record — it performs no work. It retains the
// individual per-sample result (including the trial index and each assessment)
// so downstream reporting and multi-pass-quality-trial (MPQT) analysis can
// compute distributions and variance without re-running anything. Summary and
// Provenance are intentionally minimal here; richer reporting is layered on in a
// later task rather than baked into the runner.

// MaxReportIDBytes bounds a Report.ID in UTF-8 bytes. The runner derives its ID
// from the suite identity (Name "@" Revision, each bounded by MaxNameBytes /
// MaxRevisionBytes), and a caller may overwrite it with a globally unique run ID;
// this bound is generous enough for both while still rejecting an absurd or
// hostile value at the untrusted decode boundary.
const MaxReportIDBytes = 1024

// Report is the complete result of one Run: the samples in deterministic order,
// a minimal status summary, and the provenance needed to interpret and compare
// it. ID is a deterministic identifier derived from the suite (see
// Suite.reportID); a caller may overwrite it with a globally unique run ID.
// Suite is the suite revision; Target is the observed target revision, kept
// distinct from Suite so a report records both what was run and what it ran
// against.
type Report struct {
	ID         string
	Suite      Revision
	Target     Revision
	StartedAt  time.Time
	EndedAt    time.Time
	Samples    []SampleReport
	Summary    Summary
	Provenance Provenance
}

// Validate reports whether r satisfies the report-level invariants. It is the
// whole-report boundary check applied to an untrusted, reconstructed report (the
// codec calls it after decoding) and is also satisfied by every report the runner
// itself produces. It enforces, in order:
//
//   - Identity: ID is non-empty, valid UTF-8, and within MaxReportIDBytes; Suite
//     is a required valid revision; Target is valid when present and is present
//     exactly when at least one sample reached the target successfully. An
//     all-target-failed or fully cancelled run legitimately records an empty
//     observed Target revision.
//   - Timestamps: when both StartedAt and EndedAt are set, EndedAt is not before
//     StartedAt. Zero timestamps are permitted (the runner may leave them unset).
//   - Samples: every ScenarioID is non-empty, valid UTF-8, and within MaxIDBytes;
//     every TrialIndex is non-negative; the (ScenarioID, TrialIndex) identity is
//     unique across samples; and within each sample no two assessments share an
//     evaluator NAME (a name identifies exactly one evaluator, so a repeat is
//     rejected even when the revisions differ). Each contained Assessment is
//     itself valid.
//   - Evaluator revision consistency: report-wide, an evaluator name maps to
//     exactly one revision — the same name carrying two different revisions across
//     samples (revision drift) is rejected.
//   - Summary: the stored Summary agrees with the samples (recomputed via
//     summarize) — same sample count, target-error count, and per-status tally.
//   - Provenance: Suite is required and well-formed; Target is well-formed when
//     present; each evaluator's Name and Revision is required; no evaluator name
//     is repeated; and every successful sample carries exactly the declared
//     evaluator identity set.
//
// Diagnostics never echo a data-supplied value (a scenario ID is untrusted); a
// structural failure is reported as a ReportValidationError carrying only a
// fixed-vocabulary reason, and a contained-part failure surfaces that part's own
// typed, content-free validation error.
func (r Report) Validate() error {
	if r.ID == "" {
		return &ReportValidationError{Reason: reportReasonEmptyID}
	}
	if len(r.ID) > MaxReportIDBytes {
		return &ReportValidationError{Reason: reportReasonIDTooLong}
	}
	if !utf8.ValidString(r.ID) {
		return &ReportValidationError{Reason: reportReasonIDInvalidUTF8}
	}
	if err := r.Suite.Validate(); err != nil {
		return err
	}
	if err := validateOptionalRevision(r.Target); err != nil {
		return err
	}
	if !r.StartedAt.IsZero() && !r.EndedAt.IsZero() && r.EndedAt.Before(r.StartedAt) {
		return &ReportValidationError{Reason: reportReasonEndBeforeStart}
	}
	if err := r.validateSamples(); err != nil {
		return err
	}
	if err := r.validateTargetPresence(); err != nil {
		return err
	}
	if err := r.validateEvaluatorRevisionConsistency(); err != nil {
		return err
	}
	if !summaryConsistent(r.Summary, summarize(r.Samples)) {
		return &ReportValidationError{Reason: reportReasonSummaryMismatch}
	}
	if err := r.Provenance.validate(); err != nil {
		return err
	}
	return r.validateProvenanceConsistency()
}

// validateProvenanceConsistency rejects provenance that contradicts the report
// body. Shape validity (checked by Provenance.validate) is not enough: a
// well-formed provenance can still describe a different suite, target, or set of
// evaluators than the report actually recorded, which would silently corrupt any
// downstream interpretation or comparison. It enforces, using only the values the
// runner itself emits so a genuine report always passes:
//
//   - Provenance.Suite and Provenance.Target equal the report's Suite and Target.
//     The runner sources both from the same values, including the empty observed
//     Target of an all-target-failed run, so the equality holds for real reports.
//   - Every successful sample carries exactly the evaluator identity set declared
//     by Provenance. The runner invokes every configured evaluator for every
//     successful sample and records an assessment even when that evaluator errors
//     or cannot verify the sample, so a missing or extra identity is contradictory.
//   - When no sample reached the target successfully, the runner may still record
//     its configured evaluators in Provenance even though target errors prevented
//     every assessment. That legitimate provenance-only shape remains valid.
//
// Diagnostics carry only the fixed reason; no data-supplied identity is echoed.
func (r Report) validateProvenanceConsistency() error {
	if r.Provenance.Suite != r.Suite || r.Provenance.Target != r.Target {
		return &ReportValidationError{Reason: reportReasonProvenanceMismatch}
	}

	provSet := make(map[evaluatorKey]struct{}, len(r.Provenance.Evaluators))
	for _, e := range r.Provenance.Evaluators {
		provSet[evaluatorKey{name: e.Name, revision: e.Revision}] = struct{}{}
	}

	for i := range r.Samples {
		if r.Samples[i].TargetErr != nil {
			continue
		}
		sampleSet := make(map[evaluatorKey]struct{}, len(r.Samples[i].Assessments))
		for _, a := range r.Samples[i].Assessments {
			sampleSet[evaluatorKey{name: a.Evaluator, revision: a.Revision}] = struct{}{}
		}
		if len(sampleSet) != len(provSet) {
			return &ReportValidationError{Reason: reportReasonProvenanceMismatch}
		}
		for k := range provSet {
			if _, ok := sampleSet[k]; !ok {
				return &ReportValidationError{Reason: reportReasonProvenanceMismatch}
			}
		}
	}
	return nil
}

// validateSamples enforces the per-sample invariants: non-empty scenario ID,
// non-negative trial index, unique (ScenarioID, TrialIndex) identity across
// samples, unique evaluator identity within each sample, and a valid contained
// Assessment for every entry.
func (r Report) validateSamples() error {
	type sampleKey struct {
		scenario string
		trial    int
	}
	seen := make(map[sampleKey]struct{}, len(r.Samples))
	for i := range r.Samples {
		s := r.Samples[i]
		// The sample's scenario ID is its stable identity. A genuine Run never emits
		// an empty one (Scenario.ID is validated non-empty upstream); a decoded,
		// untrusted report may carry it and is rejected here before it can key an
		// ambiguous sample identity or a comparison case.
		if s.ScenarioID == "" {
			return &ReportValidationError{Reason: reportReasonEmptyScenarioID}
		}
		if err := validateIdentifier("SampleReport.ScenarioID", s.ScenarioID, MaxIDBytes); err != nil {
			return err
		}
		if s.TrialIndex < 0 {
			return &ReportValidationError{Reason: reportReasonNegativeTrial}
		}
		key := sampleKey{scenario: s.ScenarioID, trial: s.TrialIndex}
		if _, dup := seen[key]; dup {
			return &ReportValidationError{Reason: reportReasonDuplicateSample}
		}
		seen[key] = struct{}{}
		// A target-stage failure skips assessment: the runner never emits a sample
		// that both errored at the target AND carries assessments. Reject that
		// contradictory shape at the boundary so a forged report cannot present a
		// passing assessment for a sample whose target errored.
		if s.TargetErr != nil && len(s.Assessments) > 0 {
			return &ReportValidationError{Reason: reportReasonTargetErrorWithAssessments}
		}
		if err := validateSampleAssessments(s.Assessments); err != nil {
			return err
		}
	}
	return nil
}

// validateTargetPresence ties the report's observed target revision to what the
// samples prove. A sample without TargetErr reached the target successfully, so
// at least one such sample requires a non-empty Target. When every sample failed
// at the target stage (or cancellation left the report with no samples), no
// target revision was observed and Target must remain empty. The runner emits
// exactly these shapes through targetRevision; this check rejects contradictory
// hand-built or decoded reports without echoing report content.
func (r Report) validateTargetPresence() error {
	success := false
	for i := range r.Samples {
		if r.Samples[i].TargetErr == nil {
			success = true
			break
		}
	}
	switch {
	case success && r.Target == "":
		return &ReportValidationError{Reason: reportReasonMissingTarget}
	case !success && r.Target != "":
		return &ReportValidationError{Reason: reportReasonUnexpectedTarget}
	default:
		return nil
	}
}

// evaluatorKey is one assessment's identity, used to reject duplicates within a
// single sample (which would corrupt cross-evaluator comparison).
type evaluatorKey struct {
	name     Name
	revision Revision
}

// validateEvaluatorRevisionConsistency rejects report-wide revision drift: the
// same evaluator NAME appearing with two DIFFERENT revisions anywhere across the
// report's samples. Within a single report a name must map to exactly one
// revision — comparison keys a case by evaluator name and records one revision,
// so a name identifying two revisions is ambiguous and one would be silently
// absorbed as a trial of the other. The runner never emits this (its evaluator
// names are unique per run, enforced at preflight), so a genuine report always
// passes; a decoded, untrusted report may carry it and is rejected here.
func (r Report) validateEvaluatorRevisionConsistency() error {
	revByName := make(map[Name]Revision)
	for i := range r.Samples {
		for _, a := range r.Samples[i].Assessments {
			if rev, ok := revByName[a.Evaluator]; ok {
				if rev != a.Revision {
					return &ReportValidationError{Reason: reportReasonEvaluatorRevisionDrift}
				}
				continue
			}
			revByName[a.Evaluator] = a.Revision
		}
	}
	return nil
}

// validateSampleAssessments validates each assessment in a sample and rejects a
// repeated evaluator NAME within that sample. A name must identify exactly one
// evaluator, so a repeat is rejected on the name alone — even when the two
// assessments carry different revisions, which would otherwise slip past a
// (name, revision) uniqueness check and corrupt cross-evaluator comparison.
func validateSampleAssessments(assessments []Assessment) error {
	seen := make(map[Name]struct{}, len(assessments))
	for _, a := range assessments {
		if err := a.Validate(); err != nil {
			return err
		}
		if _, dup := seen[a.Evaluator]; dup {
			return &ReportValidationError{Reason: reportReasonDuplicateEvaluator}
		}
		seen[a.Evaluator] = struct{}{}
	}
	return nil
}

// validate reports whether every revision the provenance records is well-formed.
// Suite is required; Target is validated when present because an all-failed run
// records an empty observed Target; each evaluator's Name and Revision are
// required.
func (p Provenance) validate() error {
	if err := p.Suite.Validate(); err != nil {
		return err
	}
	if err := validateOptionalRevision(p.Target); err != nil {
		return err
	}
	seen := make(map[Name]struct{}, len(p.Evaluators))
	for _, e := range p.Evaluators {
		if err := e.Name.Validate(); err != nil {
			return err
		}
		if err := e.Revision.Validate(); err != nil {
			return err
		}
		// Provenance records one identity per evaluator; a repeated name (with the
		// same or a different revision) is ambiguous. Rejecting it here also keeps
		// provenance consistent with the body's now name-unique evaluator set.
		if _, dup := seen[e.Name]; dup {
			return &ReportValidationError{Reason: reportReasonDuplicateProvenanceEvaluator}
		}
		seen[e.Name] = struct{}{}
	}
	return nil
}

// validateOptionalRevision validates a revision that may legitimately be empty
// (the observed Target revision is empty when every sample failed at the target
// stage). A non-empty value must be a well-formed Revision.
func validateOptionalRevision(rev Revision) error {
	if rev == "" {
		return nil
	}
	return rev.Validate()
}

// summaryConsistent reports whether a stored Summary agrees with the recomputed
// one: identical sample and target-error counts and identical per-status tallies.
// It compares status counts by value so a nil map and an empty map (and an absent
// key versus a zero-valued key) are treated as equal.
func summaryConsistent(stored, want Summary) bool {
	if stored.Samples != want.Samples || stored.TargetErrors != want.TargetErrors {
		return false
	}
	for st, n := range want.Assessments {
		if stored.Assessments[st] != n {
			return false
		}
	}
	for st, n := range stored.Assessments {
		if want.Assessments[st] != n {
			return false
		}
	}
	return true
}

// SampleReport is the result of one (scenario, trial) sample. ScenarioID and
// TrialIndex are the sample's stable derived identity. Observation is what the
// target produced (the zero Observation when the target failed). TargetErr is a
// typed stage error when the target stage failed, and nil when it succeeded — a
// target failure is recorded here, never as a failed assessment. Assessments
// holds each evaluator's individual result in evaluator order; a per-evaluator
// failure appears as an error-status assessment beside its succeeding siblings,
// never discarding them.
type SampleReport struct {
	ScenarioID  string
	TrialIndex  int
	Observation Observation
	TargetErr   *TargetError
	Assessments []Assessment
}

// Summary is a minimal roll-up of a report: the sample count, how many samples
// failed at the target stage, and a count of assessments by status. It is kept
// deliberately small; distribution, quantile, and baseline-comparison reporting
// are added by a later task and do not belong in the runner.
type Summary struct {
	Samples      int
	TargetErrors int
	Assessments  map[AssessmentStatus]int
}

// EvaluatorRevision records one evaluator's identity for provenance.
type EvaluatorRevision struct {
	Name     Name
	Revision Revision
}

// Provenance records the revisions needed to interpret and reproduce a report:
// the suite revision, the observed target revision, and each evaluator's
// identity in the order they were supplied. It is minimal by design; rubric,
// judge, schema, and policy provenance are layered on later.
type Provenance struct {
	Suite      Revision
	Target     Revision
	Evaluators []EvaluatorRevision
}

// summarize builds the minimal Summary from the completed samples.
func summarize(samples []SampleReport) Summary {
	counts := make(map[AssessmentStatus]int)
	targetErrs := 0
	for i := range samples {
		if samples[i].TargetErr != nil {
			targetErrs++
		}
		for _, a := range samples[i].Assessments {
			counts[a.Status]++
		}
	}
	return Summary{
		Samples:      len(samples),
		TargetErrors: targetErrs,
		Assessments:  counts,
	}
}

// targetRevision returns the target revision to record on the report: the
// subject revision of the first sample whose target succeeded. The runner sources
// the target revision from the observation, not the scenario, because the
// scenario's revision is the revision it qualifies, and only the observation
// reports what actually ran. When every sample failed at the target stage there
// is no observed revision and the empty Revision is returned.
func targetRevision(samples []SampleReport) Revision {
	for i := range samples {
		if samples[i].TargetErr == nil {
			return samples[i].Observation.Subject.Revision
		}
	}
	return ""
}

// provenanceOf assembles the report provenance from the suite, evaluators, and
// completed samples. Evaluator order follows the order they were supplied.
func provenanceOf(suite Suite, evaluators []Evaluator, samples []SampleReport) Provenance {
	evs := make([]EvaluatorRevision, 0, len(evaluators))
	for _, ev := range evaluators {
		d := ev.Descriptor()
		evs = append(evs, EvaluatorRevision{Name: d.Name, Revision: d.Revision})
	}
	return Provenance{
		Suite:      suite.Revision,
		Target:     targetRevision(samples),
		Evaluators: evs,
	}
}
