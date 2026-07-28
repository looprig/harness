package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/storage/memstore"
)

type testSessionResource struct {
	activateCalls atomic.Int32
	shutdownCalls atomic.Int32
	activateErr   error
	shutdownErr   error
	activateStart chan struct{}
	activateWait  chan struct{}
	shutdownStart chan struct{}
	shutdownWait  chan struct{}
	shutdownHook  func()
}

func (r *testSessionResource) Activate(ctx context.Context, _ tool.SessionResourceServices) error {
	r.activateCalls.Add(1)
	if r.activateStart != nil {
		close(r.activateStart)
	}
	if r.activateWait != nil {
		select {
		case <-r.activateWait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.activateErr
}

func (r *testSessionResource) Shutdown(context.Context) error {
	r.shutdownCalls.Add(1)
	if r.shutdownStart != nil {
		close(r.shutdownStart)
	}
	if r.shutdownHook != nil {
		r.shutdownHook()
	}
	if r.shutdownWait != nil {
		<-r.shutdownWait
	}
	return r.shutdownErr
}

type signalingContext struct {
	context.Context
	doneObserved chan struct{}
	once         sync.Once
}

func (c *signalingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.doneObserved) })
	return c.Context.Done()
}

type sessionResourceResult struct {
	resource tool.SessionResource
	err      error
}

type orderedSessionResource struct {
	mu         *sync.Mutex
	order      *[]string
	onActivate func()
	shutdown   atomic.Int32
}

func (r *orderedSessionResource) Activate(context.Context, tool.SessionResourceServices) error {
	r.mu.Lock()
	*r.order = append(*r.order, "activate")
	r.mu.Unlock()
	if r.onActivate != nil {
		r.onActivate()
	}
	return nil
}

func (r *orderedSessionResource) Shutdown(context.Context) error {
	r.shutdown.Add(1)
	return nil
}

func TestSessionResourcesGetOrCreateSingleFlight(t *testing.T) {
	registry := newSessionResources(t.TempDir())
	want := &testSessionResource{}
	var factoryCalls atomic.Int32

	const callers = 32
	start := make(chan struct{})
	results := make(chan tool.SessionResource, callers)
	errs := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	for range callers {
		go func() {
			ready.Done()
			<-start
			got, err := registry.GetOrCreate(context.Background(), "shared", func(path string) (tool.SessionResource, error) {
				factoryCalls.Add(1)
				if filepath.Dir(path) != registry.storageRoot {
					t.Errorf("factory path %q is outside storage root %q", path, registry.storageRoot)
				}
				return want, nil
			})
			results <- got
			errs <- err
		}()
	}
	ready.Wait()
	close(start)

	for range callers {
		if err := <-errs; err != nil {
			t.Fatalf("GetOrCreate() error = %v", err)
		}
		if got := <-results; got != want {
			t.Fatalf("GetOrCreate() resource = %p, want %p", got, want)
		}
	}
	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("factory calls = %d, want 1", got)
	}
}

func TestSessionResourcesCreationFailureCanRetry(t *testing.T) {
	registry := newSessionResources(t.TempDir())
	wantErr := errors.New("creation failed")
	wantResource := &testSessionResource{}
	var calls atomic.Int32
	var firstPath string

	got, err := registry.GetOrCreate(context.Background(), "retry", func(path string) (tool.SessionResource, error) {
		firstPath = path
		calls.Add(1)
		return nil, wantErr
	})
	if got != nil || !errors.Is(err, wantErr) {
		t.Fatalf("first GetOrCreate() = (%v, %v), want (nil, %v)", got, err, wantErr)
	}

	got, err = registry.GetOrCreate(context.Background(), "retry", func(path string) (tool.SessionResource, error) {
		calls.Add(1)
		if path != firstPath {
			t.Fatalf("retry path = %q, want stable path %q", path, firstPath)
		}
		return wantResource, nil
	})
	if err != nil {
		t.Fatalf("retry GetOrCreate() error = %v", err)
	}
	if got != wantResource {
		t.Fatalf("retry GetOrCreate() resource = %p, want %p", got, wantResource)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("factory calls = %d, want 2", got)
	}

	var typedNil *testSessionResource
	got, err = registry.GetOrCreate(context.Background(), "typed-nil", func(string) (tool.SessionResource, error) {
		return typedNil, nil
	})
	if got != nil || !errors.Is(err, errSessionResourceNil) {
		t.Fatalf("typed-nil GetOrCreate() = (%v, %v), want (nil, %v)", got, err, errSessionResourceNil)
	}
}

func TestRestorePlanningAndLiveBindingsShareResourceRegistry(t *testing.T) {
	var registriesMu sync.Mutex
	var registries []tool.SessionResourceRegistry
	definition := processResourceDefinition(t, loop.EngineNative, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		registriesMu.Lock()
		registries = append(registries, bindings.Process.Registry)
		registriesMu.Unlock()
		_, err := bindings.Process.Registry.GetOrCreate(context.Background(), "shared", func(string) (tool.SessionResource, error) {
			return &testSessionResource{}, nil
		})
		if err != nil {
			return nil, err
		}
		return []tool.InvokableTool{primerTestTool{name: "process"}}, nil
	})
	store := processResourceStore(t)
	root := filepath.Join(t.TempDir(), "resources")
	lifecycle, err := newTestLifecycle(
		definition,
		store,
		WithLifecycleSessionResourceStorage(func(context.Context, uuid.UUID) (string, string, error) {
			return root, "stable-owner", nil
		}),
	)
	if err != nil {
		t.Fatalf("NewTopologyLifecycle() error = %v", err)
	}

	live, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	id := live.SessionID()
	if err := live.Shutdown(context.Background()); err != nil {
		t.Fatalf("new Shutdown() error = %v", err)
	}
	restored, err := lifecycle.RestoreSession(context.Background(), id)
	if err != nil {
		t.Fatalf("RestoreSession() error = %v", err)
	}
	defer func() {
		if err := restored.Shutdown(context.Background()); err != nil {
			t.Errorf("restored Shutdown() error = %v", err)
		}
	}()

	registriesMu.Lock()
	got := append([]tool.SessionResourceRegistry(nil), registries...)
	registriesMu.Unlock()
	if len(got) != 2 {
		t.Fatalf("captured registries = %d, want new and restore", len(got))
	}
	if got[0] == got[1] {
		t.Fatal("new and restored live constructions unexpectedly share one registry")
	}
	if restored.resources != got[1] {
		t.Fatalf("restored Session registry = %p, planning registry = %p", restored.resources, got[1])
	}
	restored.loopsMu.RLock()
	handle := restored.loops[restored.activeLoopID]
	restored.loopsMu.RUnlock()
	if handle == nil || handle.bindings.Process == nil || handle.bindings.Process.Registry != got[1] {
		t.Fatal("restored live loop did not retain the planning registry")
	}
}

func TestRestoreDoesNotPublishBeforeResourceBridgeActivation(t *testing.T) {
	var orderMu sync.Mutex
	var order []string
	var sessionID uuid.UUID
	var loopID uuid.UUID
	var store *sessionstore.Store
	var restoring atomic.Bool
	definition := processResourceDefinition(t, loop.EngineNative, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		sessionID = bindings.SessionID
		loopID = bindings.LoopID
		orderMu.Lock()
		order = append(order, "bind")
		orderMu.Unlock()
		_, err := bindings.Process.Registry.GetOrCreate(context.Background(), "ordered", func(string) (tool.SessionResource, error) {
			return &orderedSessionResource{
				mu:    &orderMu,
				order: &order,
				onActivate: func() {
					tail := restoreEventTail(t, store, sessionID, loopID)
					var started, done bool
					for _, candidate := range tail {
						switch candidate.(type) {
						case event.RestoreStarted:
							started = true
						case event.RestoreDone:
							done = true
						}
					}
					if restoring.Load() {
						if !started {
							t.Fatal("resource activated before durable RestoreStarted")
						}
						if done {
							t.Fatal("resource activated after durable RestoreDone")
						}
					}
				},
			}, nil
		})
		if err != nil {
			return nil, err
		}
		return []tool.InvokableTool{primerTestTool{name: "process"}}, nil
	})
	store = processResourceStore(t)
	root := filepath.Join(t.TempDir(), "resources")
	lifecycle, err := newTestLifecycle(
		definition,
		store,
		WithLifecycleSessionResourceStorage(func(context.Context, uuid.UUID) (string, string, error) {
			return root, "stable-owner", nil
		}),
	)
	if err != nil {
		t.Fatalf("NewTopologyLifecycle() error = %v", err)
	}
	live, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	id := live.SessionID()
	if err := live.Shutdown(context.Background()); err != nil {
		t.Fatalf("new Shutdown() error = %v", err)
	}
	orderMu.Lock()
	order = nil
	orderMu.Unlock()
	restoring.Store(true)

	restored, err := lifecycle.RestoreSession(context.Background(), id)
	if err != nil {
		t.Fatalf("RestoreSession() error = %v", err)
	}
	defer func() {
		if err := restored.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}
	}()
	orderMu.Lock()
	got := append([]string(nil), order...)
	orderMu.Unlock()
	if len(got) != 2 || got[0] != "bind" || got[1] != "activate" {
		t.Fatalf("restore resource order = %v, want [bind activate]", got)
	}
}

func TestForeignLoopRejectsProcessServices(t *testing.T) {
	var factoryCalls atomic.Int32
	definition := processResourceDefinition(t, loop.EngineForeignClaude, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		factoryCalls.Add(1)
		return []tool.InvokableTool{primerTestTool{name: "process"}}, nil
	})
	lifecycle, err := newTestLifecycle(
		definition,
		processResourceStore(t),
		WithLifecycleSessionResourceStorage(func(context.Context, uuid.UUID) (string, string, error) {
			return filepath.Join(t.TempDir(), "resources"), "stable-owner", nil
		}),
	)
	if err != nil {
		t.Fatalf("NewTopologyLifecycle() error = %v", err)
	}

	live, err := lifecycle.NewSession(context.Background(), "")
	var unsupported *ProcessServicesUnsupportedError
	if live != nil || !errors.As(err, &unsupported) {
		t.Fatalf("NewSession() = (%v, %T %v), want process_notifications_unsupported", live, err, err)
	}
	if got := factoryCalls.Load(); got != 0 {
		t.Fatalf("foreign process factory calls = %d, want zero", got)
	}
}

func TestResourceStorageAnchorRejectsSymlinkAndOversize(t *testing.T) {
	root := t.TempDir()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	validAnchor := fmt.Sprintf("{\"version\":1,\"session_id\":%q,\"identity\":\"owner\"}\n", id.String())
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte(validAnchor), 0o600); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}
	anchor := filepath.Join(root, sessionResourceAnchorName)
	if err := os.Symlink(target, anchor); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	var storageErr *SessionResourceStorageError
	if err := validateSessionResourceAnchor(anchor, id, "owner"); !errors.As(err, &storageErr) ||
		storageErr.Kind != SessionResourceStorageAnchorCorrupt {
		t.Fatalf("symlink anchor error = %T %v, want anchor_corrupt", err, err)
	}
	if err := os.Remove(anchor); err != nil {
		t.Fatalf("Remove(anchor) error = %v", err)
	}
	if err := os.WriteFile(anchor, []byte(validAnchor+strings.Repeat(" ", 1025)), 0o600); err != nil {
		t.Fatalf("WriteFile(oversize) error = %v", err)
	}
	storageErr = nil
	if err := validateSessionResourceAnchor(anchor, id, "owner"); !errors.As(err, &storageErr) ||
		storageErr.Kind != SessionResourceStorageAnchorCorrupt {
		t.Fatalf("oversize anchor error = %T %v, want anchor_corrupt", err, err)
	}
}

func TestResourceStorageRejectsWorkspaceIdentityOverlap(t *testing.T) {
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}

	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatalf("Mkdir(workspace) error = %v", err)
	}
	descendant := filepath.Join(workspace, "resources")
	ancestor := filepath.Dir(workspace)
	symlink := filepath.Join(base, "workspace-link")
	if err := os.Symlink(workspace, symlink); err != nil {
		t.Fatalf("Symlink(workspace) error = %v", err)
	}

	tests := []struct {
		name          string
		resourceRoot  string
		workspaceRoot string
		mustNotCreate string
	}{
		{name: "equal", resourceRoot: workspace, workspaceRoot: workspace},
		{name: "resource ancestor", resourceRoot: ancestor, workspaceRoot: workspace},
		{
			name:          "resource descendant",
			resourceRoot:  descendant,
			workspaceRoot: workspace,
			mustNotCreate: descendant,
		},
		{name: "workspace symlink alias", resourceRoot: workspace, workspaceRoot: symlink},
		{
			name:          "resource symlink parent",
			resourceRoot:  filepath.Join(symlink, "resources"),
			workspaceRoot: workspace,
			mustNotCreate: descendant,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveSessionResources(
				context.Background(),
				id,
				func(context.Context, uuid.UUID) (string, string, error) {
					return tt.resourceRoot, "owner", nil
				},
				tt.workspaceRoot,
				false,
			)
			var storageErr *SessionResourceStorageError
			if !errors.As(err, &storageErr) || storageErr.Kind != SessionResourceStorageWorkspaceOverlap {
				t.Fatalf("resolveSessionResources() error = %T %v, want workspace_overlap", err, err)
			}
			if tt.mustNotCreate != "" {
				if _, statErr := os.Lstat(tt.mustNotCreate); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("overlapping resource path was created: Lstat(%q) error = %v", tt.mustNotCreate, statErr)
				}
			}
		})
	}
}

func TestResourceStorageRejectsWorkspaceCaseAliasOnCaseInsensitiveFilesystem(t *testing.T) {
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	base := t.TempDir()
	workspace := filepath.Join(base, "CaseSensitiveProbe")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatalf("Mkdir(workspace) error = %v", err)
	}
	alias := filepath.Join(base, "cASEsENSITIVEpROBE")
	workspaceInfo, err := os.Stat(workspace)
	if err != nil {
		t.Fatalf("Stat(workspace) error = %v", err)
	}
	aliasInfo, err := os.Stat(alias)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("filesystem is case-sensitive")
	}
	if err != nil {
		t.Fatalf("Stat(case alias) error = %v", err)
	}
	if !os.SameFile(workspaceInfo, aliasInfo) {
		t.Skip("filesystem does not resolve the case alias to the same directory")
	}

	_, err = resolveSessionResources(
		context.Background(),
		id,
		func(context.Context, uuid.UUID) (string, string, error) {
			return alias, "owner", nil
		},
		workspace,
		false,
	)
	var storageErr *SessionResourceStorageError
	if !errors.As(err, &storageErr) || storageErr.Kind != SessionResourceStorageWorkspaceOverlap {
		t.Fatalf("resolveSessionResources() error = %T %v, want workspace_overlap", err, err)
	}
}

func TestSessionResourceWindowsACEFlagsArePrivate(t *testing.T) {
	const (
		objectInherit    uint8 = 0x01
		containerInherit uint8 = 0x02
		inherited        uint8 = 0x10
	)
	tests := []struct {
		name      string
		flags     uint8
		directory bool
		want      bool
	}{
		{name: "directory object and container inheritance", flags: objectInherit | containerInherit, directory: true, want: true},
		{name: "directory no inheritance", directory: true},
		{name: "directory object inheritance only", flags: objectInherit, directory: true},
		{name: "directory container inheritance only", flags: containerInherit, directory: true},
		{name: "directory inherited ACE", flags: objectInherit | containerInherit | inherited, directory: true},
		{name: "file no inheritance", want: true},
		{name: "file object inheritance", flags: objectInherit},
		{name: "file container inheritance", flags: containerInherit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionResourceWindowsACEFlagsArePrivate(tt.flags, tt.directory); got != tt.want {
				t.Fatalf("sessionResourceWindowsACEFlagsArePrivate(%#x, %t) = %t, want %t",
					tt.flags, tt.directory, got, tt.want)
			}
		})
	}
}

func processResourceDefinition(
	t *testing.T,
	engine loop.Engine,
	factory tool.Factory,
) loop.Definition {
	t.Helper()
	processes := tool.NewDefinition("process", tool.RequiresProcessServices, factory)
	definition, err := loop.Define(
		loop.WithName(identity.AgentName("agent")),
		loop.WithInference(&stubLLM{}, validModel("model")),
		loop.WithTools(processes),
		loop.WithEngine(engine),
	)
	if err != nil {
		t.Fatalf("loop.Define() error = %v", err)
	}
	return definition
}

func processResourceStore(t *testing.T) *sessionstore.Store {
	t.Helper()
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("sessionstore.Open() error = %v", err)
	}
	return store
}

func TestSessionResourcesShutdownWinsCreationRace(t *testing.T) {
	registry := newSessionResources(t.TempDir())
	resource := &testSessionResource{}
	factoryEntered := make(chan struct{})
	releaseFactory := make(chan struct{})
	getResource := make(chan tool.SessionResource, 1)
	getErr := make(chan error, 1)

	go func() {
		got, err := registry.GetOrCreate(context.Background(), "racing", func(string) (tool.SessionResource, error) {
			close(factoryEntered)
			<-releaseFactory
			return resource, nil
		})
		getResource <- got
		getErr <- err
	}()
	<-factoryEntered

	shutdownErr := make(chan error, 1)
	go func() {
		shutdownErr <- registry.Shutdown(context.Background())
	}()
	for {
		registry.mu.Lock()
		closing := registry.closing
		registry.mu.Unlock()
		if closing {
			break
		}
		runtime.Gosched()
	}

	_, err := registry.GetOrCreate(context.Background(), "after-closing", func(string) (tool.SessionResource, error) {
		t.Fatal("factory called after shutdown closed admission")
		return nil, nil
	})
	if !errors.Is(err, errSessionResourcesClosing) {
		t.Fatalf("GetOrCreate() after shutdown error = %v, want %v", err, errSessionResourcesClosing)
	}

	close(releaseFactory)
	if err := <-shutdownErr; err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if got := <-getResource; got != nil {
		t.Fatalf("racing GetOrCreate() resource = %v, want nil", got)
	}
	if err := <-getErr; !errors.Is(err, errSessionResourcesClosing) {
		t.Fatalf("racing GetOrCreate() error = %v, want %v", err, errSessionResourcesClosing)
	}
	if got := resource.shutdownCalls.Load(); got != 1 {
		t.Fatalf("resource Shutdown calls = %d, want 1", got)
	}

	nilRegistry := newSessionResources(t.TempDir())
	var typedNil *testSessionResource
	factoryErr := errors.New("typed-nil factory failed")
	nilFactoryEntered := make(chan struct{})
	releaseNilFactory := make(chan struct{})
	nilGetErr := make(chan error, 1)
	go func() {
		_, err := nilRegistry.GetOrCreate(context.Background(), "typed-nil-race", func(string) (tool.SessionResource, error) {
			close(nilFactoryEntered)
			<-releaseNilFactory
			return typedNil, factoryErr
		})
		nilGetErr <- err
	}()
	<-nilFactoryEntered
	nilShutdownErr := make(chan error, 1)
	go func() {
		nilShutdownErr <- nilRegistry.Shutdown(context.Background())
	}()
	for {
		nilRegistry.mu.Lock()
		closing := nilRegistry.closing
		nilRegistry.mu.Unlock()
		if closing {
			break
		}
		runtime.Gosched()
	}
	close(releaseNilFactory)
	if err := <-nilGetErr; !errors.Is(err, factoryErr) {
		t.Fatalf("typed-nil racing GetOrCreate() error = %v, want %v", err, factoryErr)
	}
	if err := <-nilShutdownErr; err != nil {
		t.Fatalf("typed-nil racing Shutdown() error = %v", err)
	}
}

func TestSessionResourcesActivateAndShutdownOnce(t *testing.T) {
	registry := newSessionResources(t.TempDir())
	shutdownA := errors.New("shutdown a")
	shutdownB := errors.New("shutdown b")
	a := &testSessionResource{shutdownErr: shutdownA}
	b := &testSessionResource{shutdownErr: shutdownB}

	for key, resource := range map[string]*testSessionResource{"b": b, "a": a} {
		if _, err := registry.GetOrCreate(context.Background(), key, func(string) (tool.SessionResource, error) {
			return resource, nil
		}); err != nil {
			t.Fatalf("GetOrCreate(%q) error = %v", key, err)
		}
	}

	firstActivateErr := registry.Activate(context.Background())
	secondActivateErr := registry.Activate(context.Background())
	if firstActivateErr != nil || secondActivateErr != nil {
		t.Fatalf("Activate() errors = (%v, %v), want nil", firstActivateErr, secondActivateErr)
	}

	c := &testSessionResource{}
	got, err := registry.GetOrCreate(context.Background(), "c", func(string) (tool.SessionResource, error) {
		return c, nil
	})
	if err != nil {
		t.Fatalf("GetOrCreate() after activation error = %v", err)
	}
	if got != c {
		t.Fatalf("GetOrCreate() after activation resource = %p, want %p", got, c)
	}
	if got := c.activateCalls.Load(); got != 1 {
		t.Fatalf("post-activation resource Activate calls = %d, want 1 before return", got)
	}

	firstShutdownErr := registry.Shutdown(context.Background())
	secondShutdownErr := registry.Shutdown(context.Background())
	assertResourceErrors(t, firstShutdownErr, shutdownA, shutdownB)
	assertResourceErrors(t, secondShutdownErr, shutdownA, shutdownB)

	for name, resource := range map[string]*testSessionResource{"a": a, "b": b, "c": c} {
		if got := resource.activateCalls.Load(); got != 1 {
			t.Errorf("%s Activate calls = %d, want 1", name, got)
		}
		if got := resource.shutdownCalls.Load(); got != 1 {
			t.Errorf("%s Shutdown calls = %d, want 1", name, got)
		}
	}

	racingRegistry := newSessionResources(t.TempDir())
	racingActivateErr := errors.New("racing activation")
	racingResource := &testSessionResource{activateErr: racingActivateErr}
	factoryEntered := make(chan struct{})
	releaseFactory := make(chan struct{})
	getErr := make(chan error, 1)
	go func() {
		_, err := racingRegistry.GetOrCreate(context.Background(), "racing", func(string) (tool.SessionResource, error) {
			close(factoryEntered)
			<-releaseFactory
			return racingResource, nil
		})
		getErr <- err
	}()
	<-factoryEntered

	activateErr := make(chan error, 1)
	go func() {
		activateErr <- racingRegistry.Activate(context.Background())
	}()
	for {
		racingRegistry.mu.Lock()
		started := racingRegistry.activateStarted
		racingRegistry.mu.Unlock()
		if started {
			break
		}
		runtime.Gosched()
	}
	close(releaseFactory)

	if err := <-getErr; !errors.Is(err, racingActivateErr) {
		t.Fatalf("racing GetOrCreate() error = %v, want %v", err, racingActivateErr)
	}
	if err := <-activateErr; !errors.Is(err, racingActivateErr) {
		t.Fatalf("racing Activate() error = %v, want %v", err, racingActivateErr)
	}
	if err := racingRegistry.Shutdown(context.Background()); err != nil {
		t.Fatalf("racing registry Shutdown() error = %v", err)
	}
	if got := racingResource.activateCalls.Load(); got != 1 {
		t.Fatalf("racing resource Activate calls = %d, want 1", got)
	}
	if got := racingResource.shutdownCalls.Load(); got != 1 {
		t.Fatalf("racing resource Shutdown calls = %d, want 1", got)
	}

	t.Run("activation errors aggregate in key order once", func(t *testing.T) {
		registry := newSessionResources(t.TempDir())
		activateA := errors.New("activate a")
		activateB := errors.New("activate b")
		resources := map[string]*testSessionResource{
			"b": {activateErr: activateB},
			"a": {activateErr: activateA},
		}
		for key, resource := range resources {
			if _, err := registry.GetOrCreate(context.Background(), key, func(string) (tool.SessionResource, error) {
				return resource, nil
			}); err != nil {
				t.Fatalf("GetOrCreate(%q) error = %v", key, err)
			}
		}

		first := registry.Activate(context.Background())
		second := registry.Activate(context.Background())
		assertResourceErrors(t, first, activateA, activateB)
		assertResourceErrors(t, second, activateA, activateB)
		for key, resource := range resources {
			if got := resource.activateCalls.Load(); got != 1 {
				t.Errorf("%q Activate calls = %d, want 1", key, got)
			}
			if got := resource.shutdownCalls.Load(); got != 1 {
				t.Errorf("%q Shutdown calls = %d, want 1", key, got)
			}
		}
	})

	t.Run("lookup joins blocking failed activation", func(t *testing.T) {
		registry := newSessionResources(t.TempDir())
		activateErr := errors.New("blocked activation failed")
		resource := &testSessionResource{
			activateErr:   activateErr,
			activateStart: make(chan struct{}),
			activateWait:  make(chan struct{}),
		}
		if _, err := registry.GetOrCreate(context.Background(), "blocked", func(string) (tool.SessionResource, error) {
			return resource, nil
		}); err != nil {
			t.Fatalf("GetOrCreate() error = %v", err)
		}

		activated := make(chan error, 1)
		go func() {
			activated <- registry.Activate(context.Background())
		}()
		<-resource.activateStart

		lookupCtx := &signalingContext{Context: context.Background(), doneObserved: make(chan struct{})}
		lookup := make(chan sessionResourceResult, 1)
		go func() {
			got, err := registry.GetOrCreate(lookupCtx, "blocked", func(string) (tool.SessionResource, error) {
				t.Error("lookup factory called for an admitted resource")
				return nil, nil
			})
			lookup <- sessionResourceResult{resource: got, err: err}
		}()
		<-lookupCtx.doneObserved
		var earlyLookup *sessionResourceResult
		select {
		case early := <-lookup:
			t.Errorf("lookup completed during activation: (%v, %v)", early.resource, early.err)
			earlyLookup = &early
		default:
		}

		close(resource.activateWait)
		if err := <-activated; !errors.Is(err, activateErr) {
			t.Fatalf("Activate() error = %v, want %v", err, activateErr)
		}
		var result sessionResourceResult
		if earlyLookup != nil {
			result = *earlyLookup
		} else {
			result = <-lookup
		}
		if result.resource != nil || !errors.Is(result.err, activateErr) {
			t.Fatalf("lookup result = (%v, %v), want (nil, %v)", result.resource, result.err, activateErr)
		}
		got, err := registry.GetOrCreate(context.Background(), "blocked", func(string) (tool.SessionResource, error) {
			t.Error("factory called after terminal activation failure")
			return &testSessionResource{}, nil
		})
		if got != nil || !errors.Is(err, activateErr) {
			t.Fatalf("later lookup = (%v, %v), want (nil, %v)", got, err, activateErr)
		}
		if got := resource.shutdownCalls.Load(); got != 1 {
			t.Fatalf("resource Shutdown calls = %d, want 1", got)
		}
	})

	t.Run("registry shutdown joins activation failure cleanup", func(t *testing.T) {
		registry := newSessionResources(t.TempDir())
		activateErr := errors.New("activation failed before cleanup")
		resource := &testSessionResource{
			activateErr:   activateErr,
			shutdownStart: make(chan struct{}),
			shutdownWait:  make(chan struct{}),
		}
		if _, err := registry.GetOrCreate(context.Background(), "cleanup", func(string) (tool.SessionResource, error) {
			return resource, nil
		}); err != nil {
			t.Fatalf("GetOrCreate() error = %v", err)
		}

		activated := make(chan error, 1)
		go func() {
			activated <- registry.Activate(context.Background())
		}()
		<-resource.shutdownStart
		registry.mu.Lock()
		retainedDuringCleanup := registry.entries["cleanup"] != nil
		registry.mu.Unlock()
		if !retainedDuringCleanup {
			t.Error("activation cleanup removed its registry tombstone before Shutdown could join")
		}

		shutdown := make(chan error, 1)
		go func() {
			shutdown <- registry.Shutdown(context.Background())
		}()
		for {
			registry.mu.Lock()
			started := registry.shutdownStarted
			registry.mu.Unlock()
			if started {
				break
			}
			runtime.Gosched()
		}
		var earlyShutdown *error
		for range 10_000 {
			select {
			case err := <-shutdown:
				t.Errorf("registry Shutdown returned before activation cleanup: %v", err)
				earlyShutdown = &err
			default:
				runtime.Gosched()
			}
			if earlyShutdown != nil {
				break
			}
		}

		close(resource.shutdownWait)
		if err := <-activated; !errors.Is(err, activateErr) {
			t.Fatalf("Activate() error = %v, want %v", err, activateErr)
		}
		var shutdownErr error
		if earlyShutdown != nil {
			shutdownErr = *earlyShutdown
		} else {
			shutdownErr = <-shutdown
		}
		if shutdownErr != nil {
			t.Fatalf("Shutdown() error = %v", shutdownErr)
		}
		if got := resource.shutdownCalls.Load(); got != 1 {
			t.Fatalf("resource Shutdown calls = %d, want 1", got)
		}
	})

	t.Run("post-boundary cleanup may reenter its key", func(t *testing.T) {
		registry := newSessionResources(t.TempDir())
		if err := registry.Activate(context.Background()); err != nil {
			t.Fatalf("initial Activate() error = %v", err)
		}

		activateErr := errors.New("late activation failed")
		var factoryCalls atomic.Int32
		resource := &testSessionResource{activateErr: activateErr}
		resource.shutdownHook = func() {
			registry.mu.Lock()
			entry := registry.entries["late"]
			if entry == nil {
				registry.mu.Unlock()
				t.Error("failed entry removed before activation cleanup")
				return
			}
			publicationOpen := entry.publicationOpen
			usabilityErr := entry.usabilityErr
			registry.mu.Unlock()
			if publicationOpen || !errors.Is(usabilityErr, activateErr) {
				t.Errorf("cleanup tombstone = (open %v, err %v), want (false, %v)", publicationOpen, usabilityErr, activateErr)
				return
			}

			got, err := registry.GetOrCreate(context.Background(), "late", func(string) (tool.SessionResource, error) {
				t.Error("reentrant lookup invoked a new factory during cleanup")
				return &testSessionResource{}, nil
			})
			if got != nil || !errors.Is(err, activateErr) {
				t.Errorf("reentrant lookup = (%v, %v), want (nil, %v)", got, err, activateErr)
			}
		}

		got, err := registry.GetOrCreate(context.Background(), "late", func(string) (tool.SessionResource, error) {
			factoryCalls.Add(1)
			return resource, nil
		})
		if got != nil || !errors.Is(err, activateErr) {
			t.Fatalf("GetOrCreate() = (%v, %v), want (nil, %v)", got, err, activateErr)
		}
		if got := factoryCalls.Load(); got != 1 {
			t.Fatalf("factory calls = %d, want 1", got)
		}
		if got := resource.shutdownCalls.Load(); got != 1 {
			t.Fatalf("resource Shutdown calls = %d, want 1", got)
		}
	})

	t.Run("crossed activation boundary never publishes early", func(t *testing.T) {
		registry := newSessionResources(t.TempDir())
		activateErr := errors.New("crossed activation failed")
		resource := &testSessionResource{
			activateErr:   activateErr,
			activateStart: make(chan struct{}),
			activateWait:  make(chan struct{}),
		}
		factoryEntered := make(chan struct{})
		releaseFactory := make(chan struct{})
		created := make(chan sessionResourceResult, 1)
		go func() {
			got, err := registry.GetOrCreate(context.Background(), "crossed", func(string) (tool.SessionResource, error) {
				close(factoryEntered)
				<-releaseFactory
				return resource, nil
			})
			created <- sessionResourceResult{resource: got, err: err}
		}()
		<-factoryEntered

		activated := make(chan error, 1)
		go func() {
			activated <- registry.Activate(context.Background())
		}()
		for {
			registry.mu.Lock()
			started := registry.activateStarted
			registry.mu.Unlock()
			if started {
				break
			}
			runtime.Gosched()
		}
		close(releaseFactory)
		<-resource.activateStart
		select {
		case early := <-created:
			t.Errorf("creator returned before activation resolved: (%v, %v)", early.resource, early.err)
		default:
		}

		close(resource.activateWait)
		result := <-created
		if result.resource != nil || !errors.Is(result.err, activateErr) {
			t.Fatalf("creator result = (%v, %v), want (nil, %v)", result.resource, result.err, activateErr)
		}
		if err := <-activated; !errors.Is(err, activateErr) {
			t.Fatalf("Activate() error = %v, want %v", err, activateErr)
		}
		if got := resource.shutdownCalls.Load(); got != 1 {
			t.Fatalf("resource Shutdown calls = %d, want 1", got)
		}
	})

	t.Run("boundary failure revokes every snapshot entry", func(t *testing.T) {
		registry := newSessionResources(t.TempDir())
		a := &testSessionResource{}
		b := &testSessionResource{
			activateStart: make(chan struct{}),
			activateWait:  make(chan struct{}),
		}
		for key, resource := range map[string]*testSessionResource{"a": a, "b": b} {
			if _, err := registry.GetOrCreate(context.Background(), key, func(string) (tool.SessionResource, error) {
				return resource, nil
			}); err != nil {
				t.Fatalf("GetOrCreate(%q) error = %v", key, err)
			}
		}

		ctx, cancel := context.WithCancel(context.Background())
		activated := make(chan error, 1)
		go func() {
			activated <- registry.Activate(ctx)
		}()
		<-b.activateStart

		lookupCtx := &signalingContext{Context: context.Background(), doneObserved: make(chan struct{})}
		lookup := make(chan sessionResourceResult, 1)
		go func() {
			got, err := registry.GetOrCreate(lookupCtx, "a", func(string) (tool.SessionResource, error) {
				t.Error("factory called for snapshot entry")
				return nil, nil
			})
			lookup <- sessionResourceResult{resource: got, err: err}
		}()
		<-lookupCtx.doneObserved
		var earlyLookup *sessionResourceResult
		select {
		case early := <-lookup:
			t.Errorf("successful entry published before boundary resolved: (%v, %v)", early.resource, early.err)
			earlyLookup = &early
		default:
		}

		cancel()
		if err := <-activated; !errors.Is(err, context.Canceled) {
			t.Fatalf("Activate() error = %v, want %v", err, context.Canceled)
		}
		var result sessionResourceResult
		if earlyLookup != nil {
			result = *earlyLookup
		} else {
			result = <-lookup
		}
		if result.resource != nil || !errors.Is(result.err, context.Canceled) {
			t.Fatalf("lookup result = (%v, %v), want (nil, %v)", result.resource, result.err, context.Canceled)
		}
		for key, resource := range map[string]*testSessionResource{"a": a, "b": b} {
			if got := resource.shutdownCalls.Load(); got != 1 {
				t.Errorf("%q Shutdown calls = %d, want 1", key, got)
			}
		}
	})

	t.Run("canceled activation fails every entry closed", func(t *testing.T) {
		registry := newSessionResources(t.TempDir())
		resources := map[string]*testSessionResource{
			"a": {},
			"b": {},
		}
		for key, resource := range resources {
			if _, err := registry.GetOrCreate(context.Background(), key, func(string) (tool.SessionResource, error) {
				return resource, nil
			}); err != nil {
				t.Fatalf("GetOrCreate(%q) error = %v", key, err)
			}
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := registry.Activate(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Activate() error = %v, want %v", err, context.Canceled)
		}
		if err := registry.Activate(context.Background()); !errors.Is(err, context.Canceled) {
			t.Fatalf("second Activate() error = %v, want terminal %v", err, context.Canceled)
		}
		for key, resource := range resources {
			got, err := registry.GetOrCreate(context.Background(), key, func(string) (tool.SessionResource, error) {
				t.Errorf("factory called for %q after canceled activation", key)
				return &testSessionResource{}, nil
			})
			if got != nil || !errors.Is(err, context.Canceled) {
				t.Errorf("lookup %q = (%v, %v), want (nil, %v)", key, got, err, context.Canceled)
			}
			if got := resource.activateCalls.Load(); got != 0 {
				t.Errorf("%q Activate calls = %d, want 0", key, got)
			}
			if got := resource.shutdownCalls.Load(); got != 1 {
				t.Errorf("%q Shutdown calls = %d, want 1", key, got)
			}
		}
	})
}

func assertResourceErrors(t *testing.T, got error, want ...error) {
	t.Helper()
	var set *sessionResourceErrorSet
	if !errors.As(got, &set) {
		t.Fatalf("error = %v (%T), want *sessionResourceErrorSet", got, got)
	}
	if len(set.Causes) != len(want) {
		t.Fatalf("causes = %v, want %v", set.Causes, want)
	}
	for i := range want {
		if set.Causes[i] != want[i] {
			t.Fatalf("cause[%d] = %v, want %v", i, set.Causes[i], want[i])
		}
	}
}
