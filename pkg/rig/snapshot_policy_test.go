package rig

import (
	"errors"
	"testing"
	"time"
)

func TestSnapshotPolicyResolveDefaultsAndValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		in           SnapshotPolicy
		wantTrigger  SnapshotTrigger
		wantPriority SnapshotPriority
		wantTimeout  time.Duration
		wantKind     SnapshotPolicyErrorKind
	}{
		{name: "zero defaults to idle best effort sixty seconds", wantTrigger: SnapshotOnIdle, wantPriority: SnapshotBestEffort, wantTimeout: 60 * time.Second},
		{name: "manual remains explicit", in: SnapshotPolicy{Trigger: SnapshotManual}, wantTrigger: SnapshotManual, wantPriority: SnapshotBestEffort, wantTimeout: 60 * time.Second},
		{name: "required step preserved", in: SnapshotPolicy{Trigger: SnapshotOnStepDone, Priority: SnapshotRequired, Timeout: time.Second}, wantTrigger: SnapshotOnStepDone, wantPriority: SnapshotRequired, wantTimeout: time.Second},
		{name: "unknown trigger rejected", in: SnapshotPolicy{Trigger: SnapshotTrigger(99)}, wantKind: SnapshotPolicyInvalidTrigger},
		{name: "unknown priority rejected", in: SnapshotPolicy{Priority: SnapshotPriority(99)}, wantKind: SnapshotPolicyInvalidPriority},
		{name: "negative timeout rejected", in: SnapshotPolicy{Timeout: -time.Nanosecond}, wantKind: SnapshotPolicyInvalidTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.in.resolve()
			if tt.wantKind != "" {
				var target *SnapshotPolicyError
				if !errors.As(err, &target) || target.Kind != tt.wantKind {
					t.Fatalf("resolve() err = %T %v, want kind %q", err, err, tt.wantKind)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve() err = %v", err)
			}
			if got.Trigger != tt.wantTrigger || got.Priority != tt.wantPriority || got.Timeout != tt.wantTimeout {
				t.Fatalf("resolve() = %+v, want trigger=%v priority=%v timeout=%v", got, tt.wantTrigger, tt.wantPriority, tt.wantTimeout)
			}
		})
	}
}

func TestDefineRequiresExactlyOneSnapshotPolicyWithWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := sessionStoreT(t)
	workspace := wsStoreT(t)

	_, err := defineWith(t, store, WithSessionWorkspaces(workspace, root))
	var missing *SnapshotPolicyError
	if !errors.As(err, &missing) || missing.Kind != SnapshotPolicyRequired {
		t.Fatalf("placement without WithSnapshots err = %T %v, want required", err, err)
	}

	_, err = defineWith(t, sessionStoreT(t), WithSnapshots(SnapshotPolicy{}))
	var unavailable *SnapshotPolicyError
	if !errors.As(err, &unavailable) || unavailable.Kind != SnapshotPolicyWithoutWorkspace {
		t.Fatalf("WithSnapshots without placement err = %T %v, want without-workspace", err, err)
	}

	if _, err = defineWith(t, sessionStoreT(t), WithSessionWorkspaces(wsStoreT(t), t.TempDir()), WithSnapshots(SnapshotPolicy{Trigger: SnapshotManual})); err != nil {
		t.Fatalf("manual policy with placement = %v, want nil", err)
	}

	_, err = defineWith(t, sessionStoreT(t), WithSharedWorkspace(wsStoreT(t), t.TempDir()), WithSnapshots(SnapshotPolicy{Priority: SnapshotRequired}))
	var shared *SnapshotPolicyError
	if !errors.As(err, &shared) || shared.Kind != SnapshotPolicySharedRequired {
		t.Fatalf("shared required err = %T %v, want shared-required", err, err)
	}

	_, err = defineWith(t, sessionStoreT(t), WithSessionWorkspaces(wsStoreT(t), t.TempDir()), WithSnapshots(SnapshotPolicy{}), WithSnapshots(SnapshotPolicy{}))
	var duplicate *DefinitionError
	if !errors.As(err, &duplicate) || duplicate.Kind != DefinitionDuplicateOption {
		t.Fatalf("duplicate WithSnapshots err = %T %v, want duplicate option", err, err)
	}
}
