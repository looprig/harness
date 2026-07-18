package session

import (
	"context"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/event"
)

func TestDefaultPolicyDecider(t *testing.T) {
	tests := []struct {
		name       string
		assessment event.DriftAssessment
		wantAccept bool
	}{
		{name: "no drift accepts", wantAccept: true},
		{name: "info-only accepts", assessment: event.DriftAssessment{Changes: []event.DriftChange{
			{Category: event.DriftModel, Severity: event.DriftInfo},
		}}, wantAccept: true},
		{name: "any warn rejects", assessment: event.DriftAssessment{Changes: []event.DriftChange{
			{Category: event.DriftModel, Severity: event.DriftInfo},
			{Category: event.DriftWorkspace, Severity: event.DriftWarn},
		}}, wantAccept: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			decision, err := DefaultPolicyDecider{}.DecideRestore(context.Background(), tt.assessment)
			if err != nil {
				t.Fatalf("DecideRestore() error = %v", err)
			}
			if decision.Accept != tt.wantAccept {
				t.Errorf("Accept = %v, want %v", decision.Accept, tt.wantAccept)
			}
			if decision.Source != event.DecisionSourcePolicy {
				t.Errorf("Source = %s, want policy", decision.Source)
			}
		})
	}
}

func TestAcceptAllDecider(t *testing.T) {
	t.Parallel()
	warn := event.DriftAssessment{Changes: []event.DriftChange{
		{Category: event.DriftWorkspace, Severity: event.DriftWarn},
	}}
	decision, err := AcceptAllDecider{}.DecideRestore(context.Background(), warn)
	if err != nil || !decision.Accept {
		t.Fatalf("AcceptAllDecider = (%+v, %v), want unconditional accept", decision, err)
	}
}

func TestRestoreRejectedError(t *testing.T) {
	t.Parallel()
	err := &RestoreRejectedError{
		Assessment: event.DriftAssessment{Changes: []event.DriftChange{
			{Category: event.DriftModel, Severity: event.DriftInfo},
			{Category: event.DriftWorkspace, Severity: event.DriftWarn},
			{Category: event.DriftPermission, Severity: event.DriftWarn},
		}},
		Source: event.DecisionSourcePolicy,
	}
	msg := err.Error()
	for _, want := range []string{"workspace", "permission", "2", "1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, want it to mention %q", msg, want)
		}
	}
}
