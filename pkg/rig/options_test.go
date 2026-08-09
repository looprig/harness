package rig

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/loop"
)

func testServicesBuilder() foreign.ServicesBuilder {
	return func(context.Context, uuid.UUID, uuid.UUID, loop.Provenance, foreign.EventPublisher,
		loop.BoundDefinition, func() (uuid.UUID, error), *event.Factory, foreign.Services) (loop.Backend, string, error) {
		return nil, "", nil
	}
}

func testServicesRestoredBuilder() foreign.ServicesRestoredBuilder {
	return func(context.Context, uuid.UUID, uuid.UUID, loop.Provenance, foreign.EventPublisher,
		loop.BoundDefinition, func() (uuid.UUID, error), *event.Factory, foreign.RestoredForeign,
		foreign.Services) (loop.Backend, error) {
		return nil, nil
	}
}

func testLegacyBuilder() foreign.Builder {
	return func(context.Context, uuid.UUID, uuid.UUID, loop.Provenance, foreign.EventPublisher,
		loop.BoundDefinition, func() (uuid.UUID, error), *event.Factory) (loop.Backend, string, error) {
		return nil, "", nil
	}
}

func testLegacyRestoredBuilder() foreign.RestoredBuilder {
	return func(context.Context, uuid.UUID, uuid.UUID, loop.Provenance, foreign.EventPublisher,
		loop.BoundDefinition, func() (uuid.UUID, error), *event.Factory, foreign.RestoredForeign) (loop.Backend, error) {
		return nil, nil
	}
}

func TestWithForeignServicesBuildersCompilesAndRejectsDuplicate(t *testing.T) {
	t.Parallel()
	state := &definitionState{seen: make(map[singletonKey]bool)}
	option := WithForeignServicesBuilders(testServicesBuilder(), testServicesRestoredBuilder())
	if err := option(state); err != nil {
		t.Fatalf("first WithForeignServicesBuilders: %v", err)
	}
	if got := len(state.lifecycleOptions); got != 1 {
		t.Fatalf("lifecycle options = %d, want 1", got)
	}
	err := option(state)
	var definitionErr *DefinitionError
	if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionDuplicateOption || definitionErr.Name != "foreign_services_builders" {
		t.Fatalf("second WithForeignServicesBuilders error = %T %v, want duplicate foreign_services_builders", err, err)
	}
}

func TestForeignServiceBuilderOptionCanPrecedeLegacyFallback(t *testing.T) {
	t.Parallel()
	state := &definitionState{seen: make(map[singletonKey]bool)}
	if err := WithForeignBuilders(testLegacyBuilder(), testLegacyRestoredBuilder())(state); err != nil {
		t.Fatalf("legacy builders: %v", err)
	}
	if err := WithForeignServicesBuilders(testServicesBuilder(), testServicesRestoredBuilder())(state); err != nil {
		t.Fatalf("services builders should remain an additive higher-precedence option: %v", err)
	}
	if got := len(state.lifecycleOptions); got != 2 {
		t.Fatalf("lifecycle options = %d, want 2", got)
	}
}
