package eval

import (
	"context"
	"sync"
)

// This file declares Run: the non-fail-fast execution engine that turns a Suite
// into a Report. It coordinates the pipeline
//
//	Scenario -> Target -> Observation -> Evaluators -> SampleReport
//
// under three invariants:
//
//   - Stage separation. A target failure is recorded on the sample's TargetErr
//     (a typed stage error) and its evaluators are skipped; it never becomes a
//     failed quality assessment and never aborts the run. An evaluator's own
//     failure becomes an error-status assessment beside its siblings; it never
//     becomes a fail and never discards a sibling's completed assessment.
//   - Determinism. Trials are expanded into a stable, scenario-major sample
//     order before any work starts, and each sample is written to a fixed slot
//     index — never appended from a goroutine — so the report order is identical
//     whether the run is sequential or concurrent.
//   - Bounded, cancellable concurrency. Concurrency is opt-in and capped by a
//     counting semaphore; on context cancellation the runner stops starting new
//     work and returns the partial report plus the context error, retaining
//     every completed sample.
//
// FindingEvaluatorError is the safe code attached to the error-status assessment
// the runner synthesises when an evaluator returns a non-nil error. The
// underlying error is never echoed (it may carry untrusted content); the failure
// is recorded as an evaluator-stage error, not a quality verdict.
const FindingEvaluatorError FindingCode = "evaluator_error"

// FindingEvaluatorInvalidAssessment is the safe code attached to the error-status
// assessment the runner synthesises when an evaluator returns a nil error but an
// Assessment that fails Assessment.Validate. The invalid verdict is discarded
// (fail-secure): a buggy or hostile evaluator must not place an unvalidated
// verdict — a zero value, a pass carrying a severe finding, a dangling evidence
// reference, a mismatched identity — into the report. The validation error is
// never echoed (it may carry untrusted content); the failure is recorded as an
// evaluator-stage error, not a quality verdict.
const FindingEvaluatorInvalidAssessment FindingCode = "evaluator_invalid_assessment"

// Run executes suite against target, applying evaluators to every resulting
// observation, and returns the report. It validates all inputs at preflight and
// returns the zero Report with a typed error if any input is ill-formed. During
// execution it never fails fast: target and evaluator failures are recorded as
// data. It returns a non-nil error only from preflight or from context
// cancellation; on cancellation the returned Report holds every sample that
// completed and the error is the context error.
func Run(ctx context.Context, cfg RunConfig, suite Suite, target Target, evaluators ...Evaluator) (Report, error) {
	if err := preflight(cfg, suite, target, evaluators); err != nil {
		return Report{}, err
	}

	clock := cfg.clock()
	started := clock()

	trials := cfg.trials()
	units := expandTrials(len(suite.Scenarios), trials)
	slots := make([]*SampleReport, len(units))

	r := &runner{cfg: cfg, scenarios: suite.Scenarios, target: target, evaluators: evaluators}
	r.execute(ctx, units, slots)

	samples := compact(slots)
	ended := clock()

	report := Report{
		ID:         suite.reportID(),
		Suite:      suite.Revision,
		Target:     targetRevision(samples),
		StartedAt:  started,
		EndedAt:    ended,
		Samples:    samples,
		Summary:    summarize(samples),
		Provenance: provenanceOf(suite, evaluators, samples),
	}
	// Surface the context error only when work was actually truncated: at least
	// one slot was left unfilled because a unit never started (or did not
	// complete) before cancellation. compact drops those nil slots, so a shorter
	// sample slice than the slot count is the signal. When every slot completed,
	// the report is whole and we return a nil error even if ctx was cancelled
	// after the final unit finished — otherwise a late, harmless cancellation
	// would make the common `if err != nil { discard }` caller throw away a
	// complete report.
	var runErr error
	if len(samples) < len(slots) {
		runErr = ctx.Err()
	}
	return report, runErr
}

// preflight validates every input before any execution. It rejects an ill-formed
// config or suite (including duplicate scenario IDs), a nil target, and a nil or
// ill-formed evaluator (validating each descriptor and its evidence
// requirements). Empty evaluators is permitted: it produces observations with no
// assessments.
func preflight(cfg RunConfig, suite Suite, target Target, evaluators []Evaluator) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := suite.Validate(); err != nil {
		return err
	}
	if target == nil {
		return &NilTargetError{}
	}
	for _, ev := range evaluators {
		if ev == nil {
			return &NilEvaluatorError{}
		}
		if err := ev.Descriptor().Validate(); err != nil {
			return err
		}
	}
	return nil
}

// sampleUnit is one unit of work: a scenario (by index) executed for one trial.
type sampleUnit struct {
	scenarioIndex int
	trialIndex    int
}

// expandTrials expands the scenarios into the flat, deterministic sample order:
// scenario-major, trial-minor. It is computed once, before any work starts, so
// the report order does not depend on execution scheduling.
func expandTrials(scenarios, trials int) []sampleUnit {
	units := make([]sampleUnit, 0, scenarios*trials)
	for i := 0; i < scenarios; i++ {
		for t := 0; t < trials; t++ {
			units = append(units, sampleUnit{scenarioIndex: i, trialIndex: t})
		}
	}
	return units
}

// runner holds the immutable inputs shared by every unit of one run.
type runner struct {
	cfg        RunConfig
	scenarios  []Scenario
	target     Target
	evaluators []Evaluator
}

// execute runs every unit, writing each result to its own fixed slot. It bounds
// concurrency with a counting semaphore and stops scheduling new work as soon as
// ctx is cancelled. It never appends to slots from a goroutine: each goroutine
// writes only its own index, and wg.Wait establishes the happens-before edge for
// the subsequent read.
func (r *runner) execute(ctx context.Context, units []sampleUnit, slots []*SampleReport) {
	n := r.cfg.concurrency()
	if n > len(units) {
		n = len(units)
	}
	if n < 1 {
		return
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup

scheduling:
	for idx := range units {
		// Stop starting new work once the run is cancelled.
		if ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
			break scheduling
		case sem <- struct{}{}:
			// Re-check after acquiring a slot: cancellation may have raced with the
			// acquire, and we must not start new work after it.
			if ctx.Err() != nil {
				<-sem
				break scheduling
			}
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			slots[idx] = r.runUnit(ctx, units[idx])
		}(idx)
	}
	wg.Wait()
}

// runUnit executes one sample end to end: target, then (on target success)
// sample validation and every evaluator. A target failure or an invalid
// observation is recorded as a stage error and stops the sample there; it never
// reaches the evaluators and never discards sibling samples.
func (r *runner) runUnit(ctx context.Context, u sampleUnit) *SampleReport {
	// Shallow-copy the scenario so a target cannot reach the caller's backing
	// array through the top-level struct header. This guards the header only:
	// Input, Labels, and Expectation still share backing with the caller. That is
	// sufficient because the Target.Observe contract declares the Scenario and
	// those fields read-only; a compliant target never mutates them, so no deep
	// copy is warranted.
	scenario := r.scenarios[u.scenarioIndex]
	rep := &SampleReport{ScenarioID: scenario.ID, TrialIndex: u.trialIndex}

	obs, terr := r.observe(ctx, scenario)
	if terr != nil {
		rep.TargetErr = terr
		return rep
	}
	rep.Observation = obs

	sample := Sample{Scenario: &scenario, Observation: obs}
	if err := sample.Validate(); err != nil {
		// The target produced a malformed observation (or one for the wrong
		// revision): a stage error, not a verdict.
		rep.TargetErr = &TargetError{Cause: err}
		return rep
	}
	rep.Assessments = r.assess(ctx, sample)
	return rep
}

// observe runs the target under the per-target timeout (when configured) and
// wraps any failure as a stage error. It returns the zero Observation on failure.
func (r *runner) observe(ctx context.Context, scenario Scenario) (Observation, *TargetError) {
	octx := ctx
	if r.cfg.TargetTimeout > 0 {
		var cancel context.CancelFunc
		octx, cancel = context.WithTimeout(ctx, r.cfg.TargetTimeout)
		defer cancel()
	}
	obs, err := r.target.Observe(octx, scenario)
	if err != nil {
		return Observation{}, &TargetError{Cause: err}
	}
	return obs, nil
}

// assess runs every evaluator against the sample in order, returning their
// assessments in the same order. A missing required evidence kind yields that
// evaluator's unverified assessment (never pass); an evaluator error yields an
// error-status assessment (never fail). Each sibling is independent.
func (r *runner) assess(ctx context.Context, sample Sample) []Assessment {
	assessments := make([]Assessment, 0, len(r.evaluators))
	for _, ev := range r.evaluators {
		desc := ev.Descriptor()
		if a, ok := desc.CheckRequires(sample); !ok {
			assessments = append(assessments, a)
			continue
		}
		assessments = append(assessments, r.evaluate(ctx, ev, desc, sample))
	}
	return assessments
}

// evaluate runs one evaluator under the per-evaluator timeout (when configured).
// A non-nil error return is the evaluator's own failure to reach a verdict; it is
// converted to an error-status assessment and never leaks the underlying error
// text. When the evaluator returns no error, its assessment is trusted only after
// it passes Assessment.Validate: a verdict that fails validation is discarded and
// contained as an evaluator-stage error (fail-secure), so a buggy or hostile
// evaluator can never place an invalid verdict into the report.
func (r *runner) evaluate(ctx context.Context, ev Evaluator, desc Descriptor, sample Sample) Assessment {
	ectx := ctx
	if r.cfg.EvaluatorTimeout > 0 {
		var cancel context.CancelFunc
		ectx, cancel = context.WithTimeout(ctx, r.cfg.EvaluatorTimeout)
		defer cancel()
	}
	a, err := ev.Evaluate(ectx, sample)
	if err != nil {
		return Errored(desc, evaluatorErrorFinding())
	}
	if err := a.Validate(); err != nil {
		// Fail secure: the returned verdict is ill-formed (a zero value, a pass
		// carrying a severe finding, a duplicate measurement, a dangling evidence
		// reference, or an identity that does not match the descriptor). Do not
		// trust it; contain it as an evaluator-stage error without echoing the
		// validation error's untrusted content.
		return Errored(desc, evaluatorInvalidAssessmentFinding())
	}
	return a
}

// evaluatorErrorFinding is the fixed, safe finding attached to a synthesised
// error-status assessment. It never embeds the evaluator's error text.
func evaluatorErrorFinding() Finding {
	return Finding{
		Code:     FindingEvaluatorError,
		Severity: SeverityHigh,
		Message:  "evaluator failed to produce a verdict",
	}
}

// evaluatorInvalidAssessmentFinding is the fixed, safe finding attached to the
// error-status assessment synthesised when an evaluator returns an assessment
// that fails validation. It never embeds the validation error text.
func evaluatorInvalidAssessmentFinding() Finding {
	return Finding{
		Code:     FindingEvaluatorInvalidAssessment,
		Severity: SeverityHigh,
		Message:  "evaluator returned an invalid assessment",
	}
}

// compact collects the filled slots into the report's sample slice, preserving
// slot (i.e. scenario-major, trial-minor) order and skipping units that never
// ran (left nil by cancellation).
func compact(slots []*SampleReport) []SampleReport {
	out := make([]SampleReport, 0, len(slots))
	for _, s := range slots {
		if s != nil {
			out = append(out, *s)
		}
	}
	return out
}
