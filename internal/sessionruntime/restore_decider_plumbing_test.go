package sessionruntime

import (
	"context"
	"testing"

	"github.com/looprig/harness/pkg/event"
)

// recordingDecider is a comparable stand-in used to prove a specific decider
// instance reaches the stored field, distinct from the DefaultPolicyDecider default.
type recordingDecider struct{ tag string }

func (recordingDecider) DecideRestore(context.Context, event.DriftAssessment) (RestoreDecision, error) {
	return RestoreDecision{Accept: true, Source: event.DecisionSourcePolicy}, nil
}

func TestWithRestoreDeciderStoresOnSession(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts []Option
		want RestoreDecider
	}{
		{name: "default is fail-secure policy", want: DefaultPolicyDecider{}},
		{name: "custom decider is stored", opts: []Option{WithRestoreDecider(recordingDecider{tag: "custom"})}, want: recordingDecider{tag: "custom"}},
		{name: "nil decider keeps the default", opts: []Option{WithRestoreDecider(nil)}, want: DefaultPolicyDecider{}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := newTestSession(context.Background(), cfg(&stubLLM{}), tt.opts...)
			if err != nil {
				t.Fatalf("newTestSession: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
			if s.restoreDecider != tt.want {
				t.Errorf("restoreDecider = %#v, want %#v", s.restoreDecider, tt.want)
			}
		})
	}
}

func TestWithLifecycleRestoreDeciderStoresOnLifecycle(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	custom := recordingDecider{tag: "lifecycle"}
	lc, err := newTestLifecycle(cfg(&stubLLM{}), store, WithLifecycleRestoreDecider(custom))
	if err != nil {
		t.Fatalf("newTestLifecycle: %v", err)
	}
	if lc.restoreDecider != custom {
		t.Errorf("Lifecycle.restoreDecider = %#v, want %#v", lc.restoreDecider, custom)
	}

	nilLc, err := newTestLifecycle(cfg(&stubLLM{}), store, WithLifecycleRestoreDecider(nil))
	if err != nil {
		t.Fatalf("newTestLifecycle (nil): %v", err)
	}
	if nilLc.restoreDecider != nil {
		t.Errorf("Lifecycle.restoreDecider = %#v, want nil (RestoreSession then defaults to DefaultPolicyDecider)", nilLc.restoreDecider)
	}
}
