package session

import (
	"context"

	"github.com/looprig/harness/pkg/event"
)

// RestoreDecision is an application's answer to a drift assessment. Source and
// Actor are recorded durably on the resulting ConfigurationAdopted (later task).
type RestoreDecision struct {
	Accept  bool
	Source  event.DecisionSource // user | policy | operator (never migration)
	Actor   string
	Message string
}

// RestoreDecider answers a restore drift assessment. It runs while the restore
// lease is held; ctx carries the restore deadline, and a timeout is a rejection.
type RestoreDecider interface {
	DecideRestore(ctx context.Context, assessment event.DriftAssessment) (RestoreDecision, error)
}

// DefaultPolicyDecider is the fail-secure default: accept when every change is
// Info, reject when any change is Warn.
type DefaultPolicyDecider struct{}

func (DefaultPolicyDecider) DecideRestore(_ context.Context, a event.DriftAssessment) (RestoreDecision, error) {
	return RestoreDecision{Accept: !a.AnyWarn(), Source: event.DecisionSourcePolicy}, nil
}

// AcceptAllDecider accepts every assessment. It backs the deprecated
// WithAllowConfigMismatch shim (wired in a later task).
type AcceptAllDecider struct{}

func (AcceptAllDecider) DecideRestore(_ context.Context, _ event.DriftAssessment) (RestoreDecision, error) {
	return RestoreDecision{Accept: true, Source: event.DecisionSourcePolicy}, nil
}
