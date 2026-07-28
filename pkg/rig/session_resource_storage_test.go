package rig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/storage/memstore"
)

type resourceStorageProviderStub struct{}

func (*resourceStorageProviderStub) StorageForSession(context.Context, uuid.UUID) (SessionResourceStorage, error) {
	return SessionResourceStorage{Path: "/storage/session", Identity: "storage-identity"}, nil
}

type recordingResourceStorageProvider struct {
	mu       sync.Mutex
	root     string
	identity string
	err      error
	ids      []uuid.UUID
}

func (p *recordingResourceStorageProvider) StorageForSession(_ context.Context, id uuid.UUID) (SessionResourceStorage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids = append(p.ids, id)
	if p.err != nil {
		return SessionResourceStorage{}, p.err
	}
	return SessionResourceStorage{Path: p.root, Identity: p.identity}, nil
}

func (p *recordingResourceStorageProvider) setIdentity(identity string) {
	p.mu.Lock()
	p.identity = identity
	p.mu.Unlock()
}

func (p *recordingResourceStorageProvider) setRoot(root string) {
	p.mu.Lock()
	p.root = root
	p.mu.Unlock()
}

func (p *recordingResourceStorageProvider) calls() []uuid.UUID {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]uuid.UUID(nil), p.ids...)
}

type rigTestSessionResource struct {
	activateCalls atomic.Int32
	shutdownCalls atomic.Int32
}

func (r *rigTestSessionResource) Activate(context.Context, tool.SessionResourceServices) error {
	r.activateCalls.Add(1)
	return nil
}

func (r *rigTestSessionResource) Shutdown(context.Context) error {
	r.shutdownCalls.Add(1)
	return nil
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

	tests := []struct {
		name   string
		second SessionResourceStorageProvider
	}{
		{name: "valid", second: &resourceStorageProviderStub{}},
		{name: "nil", second: nil},
		{name: "typed nil", second: (*resourceStorageProviderStub)(nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Define(
				WithSessionResourceStorage(&resourceStorageProviderStub{}),
				WithSessionResourceStorage(tt.second),
			)
			var target *DefinitionError
			if !errors.As(err, &target) || target.Kind != DefinitionDuplicateOption {
				t.Fatalf("Define() error = %T %v, want duplicate-option DefinitionError", err, err)
			}
		})
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

func TestResourceStorageStableAcrossRestore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "resources")
	provider := &recordingResourceStorageProvider{root: root, identity: "stable-owner"}
	var pathsMu sync.Mutex
	var paths []string
	var resourcesMu sync.Mutex
	var resources []*rigTestSessionResource
	defined := resourceStorageLifecycleRig(t, provider, func(path string) (tool.SessionResource, error) {
		resource := &rigTestSessionResource{}
		pathsMu.Lock()
		paths = append(paths, path)
		pathsMu.Unlock()
		resourcesMu.Lock()
		resources = append(resources, resource)
		resourcesMu.Unlock()
		return resource, nil
	})

	live, err := defined.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	id := live.SessionID()
	if err := live.Shutdown(context.Background()); err != nil {
		t.Fatalf("new Shutdown() error = %v", err)
	}
	restored, err := defined.RestoreSession(context.Background(), id)
	if err != nil {
		t.Fatalf("RestoreSession() error = %v", err)
	}
	if err := restored.Shutdown(context.Background()); err != nil {
		t.Fatalf("restored Shutdown() error = %v", err)
	}

	if calls := provider.calls(); len(calls) != 2 || calls[0] != id || calls[1] != id {
		t.Fatalf("provider session IDs = %v, want [%v %v]", calls, id, id)
	}
	pathsMu.Lock()
	gotPaths := append([]string(nil), paths...)
	pathsMu.Unlock()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(resource root) error = %v", err)
	}
	if len(gotPaths) != 2 || gotPaths[0] != gotPaths[1] || filepath.Dir(gotPaths[0]) != canonicalRoot {
		t.Fatalf("resource paths = %v, want two stable children of %q", gotPaths, root)
	}
	resourcesMu.Lock()
	gotResources := append([]*rigTestSessionResource(nil), resources...)
	resourcesMu.Unlock()
	if len(gotResources) != 2 {
		t.Fatalf("resource instances = %d, want one per live construction", len(gotResources))
	}
	for i, resource := range gotResources {
		if resource.activateCalls.Load() != 1 || resource.shutdownCalls.Load() != 1 {
			t.Fatalf("resource[%d] calls = activate:%d shutdown:%d, want 1/1",
				i, resource.activateCalls.Load(), resource.shutdownCalls.Load())
		}
	}
}

func TestResourceStorageRejectsIdentityMismatch(t *testing.T) {
	provider := &recordingResourceStorageProvider{
		root:     filepath.Join(t.TempDir(), "resources"),
		identity: "owner-a",
	}
	var factoryCalls atomic.Int32
	defined := resourceStorageLifecycleRig(t, provider, func(string) (tool.SessionResource, error) {
		factoryCalls.Add(1)
		return &rigTestSessionResource{}, nil
	})

	live, err := defined.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	id := live.SessionID()
	if err := live.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	provider.setIdentity("owner-b")

	restored, err := defined.RestoreSession(context.Background(), id)
	var storageErr *sessionruntime.SessionResourceStorageError
	if restored != nil || !errors.As(err, &storageErr) || storageErr.Kind != sessionruntime.SessionResourceStorageIdentityMismatch {
		t.Fatalf("RestoreSession() = (%v, %T %v), want identity-mismatch SessionResourceStorageError", restored, err, err)
	}
	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("factory calls = %d, want restore rejected before second factory call", got)
	}
}

func TestResourceStorageUnavailableFailsConstruction(t *testing.T) {
	unavailable := errors.New("resource storage unavailable")
	provider := &recordingResourceStorageProvider{err: unavailable}
	var factoryCalls atomic.Int32
	defined := resourceStorageLifecycleRig(t, provider, func(string) (tool.SessionResource, error) {
		factoryCalls.Add(1)
		return &rigTestSessionResource{}, nil
	})

	live, err := defined.NewSession(context.Background())
	if live != nil || !errors.Is(err, unavailable) {
		t.Fatalf("NewSession() = (%v, %v), want nil wrapping %v", live, err, unavailable)
	}
	if got := factoryCalls.Load(); got != 0 {
		t.Fatalf("factory calls = %d, want zero", got)
	}
}

func TestResourceStorageMissingRestoreRootDoesNotCreate(t *testing.T) {
	provider := &recordingResourceStorageProvider{
		root:     filepath.Join(t.TempDir(), "original"),
		identity: "stable-owner",
	}
	defined := resourceStorageLifecycleRig(t, provider, func(string) (tool.SessionResource, error) {
		return &rigTestSessionResource{}, nil
	})
	live, err := defined.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	id := live.SessionID()
	if err := live.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	provider.setRoot(missing)

	restored, err := defined.RestoreSession(context.Background(), id)
	var storageErr *sessionruntime.SessionResourceStorageError
	if restored != nil || !errors.As(err, &storageErr) || storageErr.Kind != sessionruntime.SessionResourceStorageAnchorMissing {
		t.Fatalf("RestoreSession() = (%v, %T %v), want missing-anchor SessionResourceStorageError", restored, err, err)
	}
	if _, statErr := os.Lstat(missing); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("missing restore root Lstat error = %v, want os.ErrNotExist", statErr)
	}
}

func resourceStorageLifecycleRig(
	t *testing.T,
	provider SessionResourceStorageProvider,
	factory func(string) (tool.SessionResource, error),
) *Rig {
	t.Helper()
	processes := tool.NewDefinition(
		"processes",
		tool.RequiresProcessServices,
		func(ctx context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
			if bindings.Process == nil {
				t.Fatal("process binding is nil")
			}
			resource, err := bindings.Process.Registry.GetOrCreate(ctx, "shared-process-runner", factory)
			if err != nil {
				return nil, err
			}
			if resource == nil {
				t.Fatal("registry returned nil resource")
			}
			return []tool.InvokableTool{fpTool{name: "processes"}}, nil
		},
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
	defined, err := Define(
		WithLoops(definition),
		WithPrimers("agent"),
		WithSessionStore(store),
		WithSessionResourceStorage(provider),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	return defined
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
