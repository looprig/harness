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

func TestNewInModeWithRuntimeRetainsHooksOnNativeLoop(t *testing.T) {
	t.Parallel()

	runner, err := hook.Compile(hook.Set{})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	created, err := NewInModeWithRuntime(
		ctx,
		mustID(t),
		mustID(t),
		Provenance{},
		noopPublisher{},
		modeDefinition(t, &fakeLLM{}),
		"",
		RuntimeDependencies{Hooks: runner},
	)
	if err != nil {
		t.Fatalf("NewInModeWithRuntime: %v", err)
	}
	if created.hooks != runner {
		t.Fatalf("Loop.hooks = %p, want %p", created.hooks, runner)
	}
}
