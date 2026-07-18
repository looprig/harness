package eval

import (
	"strconv"

	"github.com/looprig/core/content"
)

// This file declares Scenario (an active qualification case), Label (a typed
// scenario tag), and Sample (a scenario paired with its resulting observation).
// A Scenario separates the description of a test from its execution: a Target
// (see target.go) turns a Scenario into an Observation, and a Sample binds the
// two so an evaluator can assess them together. Continuous evaluation already
// has an observation and constructs a Sample with no Scenario.

// Count bound for a scenario's labels, and byte bound for a label value. Labels
// are a small set of safe key/value tags, not a payload.
const (
	// MaxScenarioLabels bounds how many labels one Scenario may carry.
	MaxScenarioLabels = 64
	// MaxLabelValueBytes bounds a single Label value in UTF-8 bytes.
	MaxLabelValueBytes = 256
	// MaxScenarioInputMessages bounds the length of a Scenario.Input thread. It
	// rejects an absurd fixture; per-message content is core/content's concern.
	MaxScenarioInputMessages = 4096
)

// Label is a typed key/value tag on a scenario, used to group and filter cases
// (suite, risk tier, capability, and so on). Key carries domain meaning and
// reuses the Name identity rules; Value is a bounded free-form string. Within a
// scenario a Key is unique — a label set is a map, not a multimap — so a
// repeated Key is a duplicate and rejected.
type Label struct {
	Key   Name
	Value string
}

// Validate reports whether l is a well-formed label. Its diagnostic references
// the field name only, never the key or value.
func (l Label) Validate() error {
	if err := l.Key.Validate(); err != nil {
		return err
	}
	if len(l.Value) > MaxLabelValueBytes {
		return &ValidationError{Field: "Label.Value", Reason: "exceeds " + strconv.Itoa(MaxLabelValueBytes) + " bytes"}
	}
	return nil
}

// Scenario is an active qualification case: a stable identity, the input thread
// to drive a target with, optional qualification expectations, and labels. ID
// is the stable case identity used to reject duplicate scenarios and correlate
// reports across runs; Name and Revision identify the target revision the
// scenario qualifies (see Sample.Validate). Input must be non-empty — an empty
// scenario drives nothing.
type Scenario struct {
	ID          string
	Name        Name
	Revision    Revision
	Input       content.AgenticMessages
	Expectation *Expectation
	Labels      []Label
}

// Validate reports whether s is a well-formed scenario: a bounded non-empty ID,
// a valid Name and Revision, a non-empty and bounded Input thread, unique valid
// labels, and a valid Expectation when present.
func (s Scenario) Validate() error {
	if err := validateIdentifier("Scenario.ID", s.ID, MaxIDBytes); err != nil {
		return err
	}
	if err := s.Name.Validate(); err != nil {
		return err
	}
	if err := s.Revision.Validate(); err != nil {
		return err
	}
	if len(s.Input) == 0 {
		return &ValidationError{Field: "Scenario.Input", Reason: "must not be empty"}
	}
	if len(s.Input) > MaxScenarioInputMessages {
		return &ValidationError{Field: "Scenario.Input", Reason: "exceeds " + strconv.Itoa(MaxScenarioInputMessages) + " messages"}
	}
	if err := s.validateLabels(); err != nil {
		return err
	}
	return s.Expectation.Validate()
}

// validateLabels validates each label and rejects a repeated Key. Iteration
// order is not modified.
func (s Scenario) validateLabels() error {
	if len(s.Labels) > MaxScenarioLabels {
		return &ValidationError{Field: "Scenario.Labels", Reason: "exceeds " + strconv.Itoa(MaxScenarioLabels) + " labels"}
	}
	seen := make(map[Name]struct{}, len(s.Labels))
	for _, l := range s.Labels {
		if err := l.Validate(); err != nil {
			return err
		}
		if _, dup := seen[l.Key]; dup {
			return &DuplicateLabelError{}
		}
		seen[l.Key] = struct{}{}
	}
	return nil
}

// Sample binds a scenario to the observation it produced so an evaluator can
// assess them together. Scenario is nil for pure continuous observation, where
// an observation already exists and no target was executed. Observation is
// always present.
type Sample struct {
	Scenario    *Scenario
	Observation Observation
}

// Validate reports whether the sample is well-formed. The Observation is always
// validated. When a Scenario is present it is validated too, and the
// observation's subject revision must match the target revision the scenario
// declares (Scenario.Revision): a target that returns an observation for a
// different revision than the scenario qualifies is a stage error, not a
// verdict, and must be rejected here rather than silently evaluated. With no
// Scenario (continuous observation) only the Observation is checked.
func (s Sample) Validate() error {
	if err := s.Observation.Validate(); err != nil {
		return err
	}
	if s.Scenario == nil {
		return nil
	}
	if err := s.Scenario.Validate(); err != nil {
		return err
	}
	if s.Observation.Subject.Revision != s.Scenario.Revision {
		return &SampleSubjectMismatchError{}
	}
	return nil
}
