package loopruntime

import (
	"context"
	"testing"

	"github.com/looprig/harness/pkg/hook"
)

func TestRuntimeDependenciesRetainHooks(t *testing.T) {
	t.Parallel()

	runner, err := hook.Compile(hook.Set{})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	cfg := runtimeConfig{}
	if err := installRuntimeDependencies(context.Background(), &cfg, RuntimeDependencies{Hooks: runner}); err != nil {
		t.Fatalf("installRuntimeDependencies: %v", err)
	}
	if cfg.Hooks != runner {
		t.Fatalf("runtimeConfig.Hooks = %p, want same compiled runner %p", cfg.Hooks, runner)
	}
}
