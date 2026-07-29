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

// TestDefaultPolicyDeciderRejectsSilentPermissionReviewActivation proves the
// fix for design §21's "never silently resumes with a different reviewer":
// a restore whose live rig enables permission-review classifiers while the
// adopted baseline had none configured must NOT auto-accept under the
// default policy decider. The scenario uses a real event.ConfigManifest pair
// and event.AssessDrift, not a hand-built DriftAssessment, so it proves the
// real drift-classification wiring, not just the decider in isolation.
func TestDefaultPolicyDeciderRejectsSilentPermissionReviewActivation(t *testing.T) {
	t.Parallel()
	baseline := event.ConfigManifest{
		SchemaVersion: event.ManifestSchemaVersion, ModelID: "m", TopologyRev: "topo",
		PermissionReviewConfigured: false,
	}
	candidate := baseline
	candidate.PermissionReviewConfigured = true

	assessment := event.AssessDrift(baseline, candidate)
	decision, err := DefaultPolicyDecider{}.DecideRestore(context.Background(), assessment)
	if err != nil {
		t.Fatalf("DecideRestore() error = %v", err)
	}
	if decision.Accept {
		t.Fatalf("DefaultPolicyDecider silently accepted permission-review activation on restore: %+v", assessment)
	}

	// AcceptAllDecider remains the documented, explicit opt-in escape hatch
	// (WithAllowConfigMismatch's successor seam) — it must still accept.
	allowed, err := AcceptAllDecider{}.DecideRestore(context.Background(), assessment)
	if err != nil || !allowed.Accept {
		t.Fatalf("AcceptAllDecider = (%+v, %v), want unconditional accept for the explicit opt-in", allowed, err)
	}
}

// TestDefaultPolicyDeciderPermissionReviewOtherTransitions proves the
// remaining three transitions do not regress: enabled -> enabled with a
// different classifier identity, enabled -> disabled, and absent -> absent
// all still auto-accept under the default policy decider.
func TestDefaultPolicyDeciderPermissionReviewOtherTransitions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		baseline event.ConfigManifest
		modify   func(*event.ConfigManifest)
	}{
		{
			name:     "enabled to enabled different classifier identity",
			baseline: event.ConfigManifest{SchemaVersion: event.ManifestSchemaVersion, TopologyRev: "topo-a", PermissionReviewConfigured: true},
			modify:   func(m *event.ConfigManifest) { m.TopologyRev = "topo-b" },
		},
		{
			name:     "enabled to disabled",
			baseline: event.ConfigManifest{SchemaVersion: event.ManifestSchemaVersion, TopologyRev: "topo", PermissionReviewConfigured: true},
			modify:   func(m *event.ConfigManifest) { m.PermissionReviewConfigured = false },
		},
		{
			name:     "absent to absent",
			baseline: event.ConfigManifest{SchemaVersion: event.ManifestSchemaVersion, TopologyRev: "topo"},
			modify:   func(*event.ConfigManifest) {},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			candidate := tt.baseline
			tt.modify(&candidate)
			assessment := event.AssessDrift(tt.baseline, candidate)
			decision, err := DefaultPolicyDecider{}.DecideRestore(context.Background(), assessment)
			if err != nil {
				t.Fatalf("DecideRestore() error = %v", err)
			}
			if !decision.Accept {
				t.Errorf("DefaultPolicyDecider rejected %s: %+v", tt.name, assessment)
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
