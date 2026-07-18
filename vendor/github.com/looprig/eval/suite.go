package eval

import (
	"strconv"
	"time"
)

// This file declares the runnable inputs of the execution engine: Suite (a named,
// versioned set of scenarios) and RunConfig (the knobs that shape one run).
// Neither performs any work — Run (see run.go) consumes them. Both validate at
// the boundary so an ill-formed suite or an out-of-range config is rejected at
// preflight, before any target is executed.

// MaxTrials bounds RunConfig.Trials. Repeated trials expose per-case flakiness
// and variance for nondeterministic targets, but a request for an unreasonable
// number of trials is a configuration mistake, not a workload, and is rejected
// at preflight. The bound is a guard against accidental fan-out, not a tuning
// parameter.
const MaxTrials = 1000

// Suite is a named, versioned set of scenarios to run against a single target.
// Name and Revision identify the suite for provenance and reporting (Revision
// becomes Report.Suite); Scenarios is the ordered set of cases. Scenario order
// is preserved in the report, so a suite is reproducible.
type Suite struct {
	Name      Name
	Revision  Revision
	Scenarios []Scenario
}

// Validate reports whether s is a well-formed suite: a valid Name and Revision,
// a non-empty scenario set with no duplicate scenario IDs, and a valid
// Scenario for every entry. A duplicate ID would make two cases indistinguishable
// in the report and in baseline comparison, so it is rejected here.
func (s Suite) Validate() error {
	if err := s.Name.Validate(); err != nil {
		return err
	}
	if err := s.Revision.Validate(); err != nil {
		return err
	}
	if len(s.Scenarios) == 0 {
		return &ValidationError{Field: "Suite.Scenarios", Reason: "must not be empty"}
	}
	seen := make(map[string]struct{}, len(s.Scenarios))
	for _, sc := range s.Scenarios {
		if err := sc.Validate(); err != nil {
			return err
		}
		if _, dup := seen[sc.ID]; dup {
			return &DuplicateScenarioError{}
		}
		seen[sc.ID] = struct{}{}
	}
	return nil
}

// reportID derives a deterministic report identifier from the suite identity.
// It deliberately avoids time and randomness so a report is reproducible and
// tests are not flaky; a caller that needs a globally unique run identifier can
// overwrite Report.ID after Run returns. Name and Revision are bounded domain
// identifiers (validated at preflight) and safe to compose here.
func (s Suite) reportID() string {
	return string(s.Name) + "@" + string(s.Revision)
}

// RunConfig shapes one execution: how many trials per scenario, how much target
// concurrency to allow, and the per-stage timeouts. Its zero value is a valid,
// conservative default — one trial, sequential, no per-stage timeout — so
// RunConfig{} runs a suite deterministically.
type RunConfig struct {
	// Trials is how many times each scenario is executed. A value of 0 means one
	// trial. Negative or above MaxTrials is rejected at preflight.
	Trials int
	// Concurrency is the maximum number of samples executed simultaneously. A
	// value of 0 (or 1) means sequential execution. Negative is rejected.
	// Concurrency is opt-in: the default never runs targets in parallel.
	Concurrency int
	// TargetTimeout bounds a single target execution. Zero means no per-target
	// timeout (the run context still applies). Negative is rejected.
	TargetTimeout time.Duration
	// EvaluatorTimeout bounds a single evaluator execution. Zero means no
	// per-evaluator timeout. Negative is rejected.
	EvaluatorTimeout time.Duration

	// now injects the clock so tests can pin StartedAt/EndedAt. It is unexported:
	// external callers always get time.Now, and only same-package tests may set
	// it. A nil value means time.Now.
	now func() time.Time
}

// Validate reports whether c is a well-formed config. It enforces the trial and
// concurrency bounds and rejects negative timeouts. It does not apply defaults;
// trials, concurrency, and clock accessors do that.
func (c RunConfig) Validate() error {
	if c.Trials < 0 {
		return &ValidationError{Field: "RunConfig.Trials", Reason: "must not be negative"}
	}
	if c.Trials > MaxTrials {
		return &ValidationError{Field: "RunConfig.Trials", Reason: "exceeds " + strconv.Itoa(MaxTrials)}
	}
	if c.Concurrency < 0 {
		return &ValidationError{Field: "RunConfig.Concurrency", Reason: "must not be negative"}
	}
	if c.TargetTimeout < 0 {
		return &ValidationError{Field: "RunConfig.TargetTimeout", Reason: "must not be negative"}
	}
	if c.EvaluatorTimeout < 0 {
		return &ValidationError{Field: "RunConfig.EvaluatorTimeout", Reason: "must not be negative"}
	}
	return nil
}

// trials returns the effective trial count, applying the one-trial default.
func (c RunConfig) trials() int {
	if c.Trials <= 0 {
		return 1
	}
	return c.Trials
}

// concurrency returns the effective concurrency, applying the sequential default.
func (c RunConfig) concurrency() int {
	if c.Concurrency <= 0 {
		return 1
	}
	return c.Concurrency
}

// clock returns the effective clock, defaulting to time.Now.
func (c RunConfig) clock() func() time.Time {
	if c.now == nil {
		return time.Now
	}
	return c.now
}
