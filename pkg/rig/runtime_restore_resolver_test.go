package rig

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
)

type rigRuntimeRestoreResolver struct{}

func (rigRuntimeRestoreResolver) ResolveRuntimeRestore(context.Context, session.RuntimeRestoreRequest) (loop.Resolved, error) {
	return loop.Resolved{}, nil
}

func TestWithRuntimeRestoreResolverRejectsNilAndDuplicate(t *testing.T) {
	t.Parallel()
	state := &definitionState{seen: make(map[singletonKey]bool)}
	if err := WithRuntimeRestoreResolver(nil)(state); err == nil {
		t.Fatal("WithRuntimeRestoreResolver(nil) error = nil")
	}
	resolver := rigRuntimeRestoreResolver{}
	if err := WithRuntimeRestoreResolver(resolver)(state); err != nil {
		t.Fatalf("first option: %v", err)
	}
	err := WithRuntimeRestoreResolver(resolver)(state)
	var definitionErr *DefinitionError
	if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionDuplicateOption {
		t.Fatalf("second option error = %T %v, want duplicate", err, err)
	}
}
