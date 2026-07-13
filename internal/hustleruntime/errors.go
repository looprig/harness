package hustleruntime

import (
	"github.com/looprig/harness/pkg/hustle"
)

// ConfigErrorReason identifies an invalid controller construction boundary.
type ConfigErrorReason string

const (
	ConfigInvalidContext    ConfigErrorReason = "invalid_context"
	ConfigInvalidConcurrent ConfigErrorReason = "invalid_concurrent"
	ConfigInvalidQueued     ConfigErrorReason = "invalid_queued"
	ConfigCapacityOverflow  ConfigErrorReason = "capacity_overflow"
)

// ConfigError reports invalid scheduler limits without retaining collaborators.
type ConfigError struct {
	Reason ConfigErrorReason
	Field  string
}

func (e *ConfigError) Error() string {
	message := "hustleruntime: invalid controller configuration: " + string(e.Reason)
	if e.Field != "" {
		message += " (" + e.Field + ")"
	}
	return message
}

// AdmissionErrorReason identifies a rejection before lane ownership commits.
type AdmissionErrorReason string

const (
	AdmissionInvalidContext       AdmissionErrorReason = "invalid_context"
	AdmissionInvalidParticipation AdmissionErrorReason = "invalid_participation"
	AdmissionNilFinalizer         AdmissionErrorReason = "nil_finalizer"
	AdmissionRunID                AdmissionErrorReason = "run_id"
	AdmissionFull                 AdmissionErrorReason = "full"
	AdmissionClosed               AdmissionErrorReason = "closed"
)

// AdmissionError has no RunID because admission failures are pre-ownership.
type AdmissionError struct {
	Reason        AdmissionErrorReason
	Participation hustle.Participation
	Cause         error
}

func (e *AdmissionError) Error() string {
	return "hustleruntime: admission rejected: " + string(e.Reason)
}

func (e *AdmissionError) Unwrap() error { return e.Cause }

// QueueFailureReason classifies a terminal failure while an owned node waits for
// an execution slot.
type QueueFailureReason string

const (
	QueueFailureCanceled QueueFailureReason = "canceled"
	QueueFailureClosed   QueueFailureReason = "closed"
)

// QueueFailureError is both the returned owned-run failure and the exact error
// supplied to its finalizer.
type QueueFailureError struct {
	RunID         hustle.RunID
	Participation hustle.Participation
	Stage         hustle.Stage
	Reason        QueueFailureReason
	Cause         error
	FinalizerErr  *FinalizerError
}

func (e *QueueFailureError) Error() string {
	return "hustleruntime: owned run failed in queue: " + string(e.Reason)
}

func (e *QueueFailureError) Unwrap() []error {
	errors := make([]error, 0, 2)
	if e.Cause != nil {
		errors = append(errors, e.Cause)
	}
	if e.FinalizerErr != nil {
		errors = append(errors, e.FinalizerErr)
	}
	return errors
}

// FinalizerError preserves a consumer callback failure with owned-run identity.
type FinalizerError struct {
	RunID hustle.RunID
	Cause error
}

func (e *FinalizerError) Error() string { return "hustleruntime: run finalizer failed" }

func (e *FinalizerError) Unwrap() error { return e.Cause }

// RunStateError reports misuse of the internal ownership seam without silently
// corrupting lane accounting.
type RunStateError struct {
	RunID hustle.RunID
	State runState
}

func (e *RunStateError) Error() string { return "hustleruntime: invalid owned-run state" }

// CloseError reports finalizer failures encountered while resolving queued nodes.
type CloseError struct {
	Failures []*FinalizerError
}

func (e *CloseError) Error() string { return "hustleruntime: close finalization failed" }

func (e *CloseError) Unwrap() []error {
	errors := make([]error, len(e.Failures))
	for index, failure := range e.Failures {
		errors[index] = failure
	}
	return errors
}
