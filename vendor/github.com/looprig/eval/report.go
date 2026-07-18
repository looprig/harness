package eval

import (
	"context"
	"time"
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
