package rig

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/storage/memstore"
)

type resourceStorageProviderStub struct{}

func (*resourceStorageProviderStub) StorageForSession(context.Context, uuid.UUID) (SessionResourceStorage, error) {
	return SessionResourceStorage{Path: "/storage/session", Identity: "storage-identity"}, nil
}

func TestRigRequiresResourceStorageForProcessDefinitions(t *testing.T) {
	t.Parallel()

	_, err := Define(resourceStorageRigOptions(t)...)
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionMissingResourceStorage {
		t.Fatalf("Define() error = %T %v, want missing-resource-storage DefinitionError", err, err)
	}
}

func TestRigRejectsNilResourceStorageProvider(t *testing.T) {
	t.Parallel()

	_, err := Define(WithSessionResourceStorage(nil))
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionInvalidResourceStorage {
		t.Fatalf("Define() error = %T %v, want invalid-resource-storage DefinitionError", err, err)
	}
}

func TestRigRejectsTypedNilResourceStorageProvider(t *testing.T) {
	t.Parallel()

	_, err := Define(WithSessionResourceStorage((*resourceStorageProviderStub)(nil)))
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionInvalidResourceStorage {
		t.Fatalf("Define() error = %T %v, want invalid-resource-storage DefinitionError", err, err)
	}
}

func TestRigRejectsDuplicateResourceStorageProvider(t *testing.T) {
	t.Parallel()

	_, err := Define(
		WithSessionResourceStorage(&resourceStorageProviderStub{}),
		WithSessionResourceStorage(&resourceStorageProviderStub{}),
	)
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionDuplicateOption {
		t.Fatalf("Define() error = %T %v, want duplicate-option DefinitionError", err, err)
	}
}

func TestRigDefinitionCapturesResourceStorageProviderImmutably(t *testing.T) {
	t.Parallel()

	provider := &resourceStorageProviderStub{}
	options := append(resourceStorageRigOptions(t), WithSessionResourceStorage(provider))
	defined, err := Define(options...)
	if err != nil {
		t.Fatalf("Define() error = %v", err)
	}
	if defined.resourceStorageProvider != provider {
		t.Fatalf("Rig resource storage provider = %T %p, want captured %T %p",
			defined.resourceStorageProvider, defined.resourceStorageProvider, provider, provider)
	}
}

func resourceStorageRigOptions(t *testing.T) []Option {
	t.Helper()

	processes := tool.NewDefinition(
		"processes",
		tool.RequiresProcessServices,
		func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) { return nil, nil },
	)
	definition, err := loop.Define(
		loop.WithName("agent"),
		loop.WithInference(&stubLLM{}, validModel("model")),
		loop.WithTools(processes),
	)
	if err != nil {
		t.Fatalf("loop.Define: %v", err)
	}
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("sessionstore.Open: %v", err)
	}
	return []Option{
		WithLoops(definition),
		WithPrimers("agent"),
		WithSessionStore(store),
	}
}
