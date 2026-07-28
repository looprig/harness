package sessionruntime

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

type testSessionResource struct {
	activateCalls atomic.Int32
	shutdownCalls atomic.Int32
	activateErr   error
	shutdownErr   error
}

func (r *testSessionResource) Activate(context.Context, tool.SessionResourceServices) error {
	r.activateCalls.Add(1)
	return r.activateErr
}

func (r *testSessionResource) Shutdown(context.Context) error {
	r.shutdownCalls.Add(1)
	return r.shutdownErr
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
}

func TestSessionResourcesActivateAndShutdownOnce(t *testing.T) {
	registry := newSessionResources(t.TempDir())
	activateA := errors.New("activate a")
	activateB := errors.New("activate b")
	shutdownA := errors.New("shutdown a")
	shutdownB := errors.New("shutdown b")
	a := &testSessionResource{activateErr: activateA, shutdownErr: shutdownA}
	b := &testSessionResource{activateErr: activateB, shutdownErr: shutdownB}

	for key, resource := range map[string]*testSessionResource{"b": b, "a": a} {
		if _, err := registry.GetOrCreate(context.Background(), key, func(string) (tool.SessionResource, error) {
			return resource, nil
		}); err != nil {
			t.Fatalf("GetOrCreate(%q) error = %v", key, err)
		}
	}

	firstActivateErr := registry.Activate(context.Background(), tool.SessionResourceServices{})
	secondActivateErr := registry.Activate(context.Background(), tool.SessionResourceServices{})
	assertResourceErrors(t, firstActivateErr, activateA, activateB)
	assertResourceErrors(t, secondActivateErr, activateA, activateB)

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
		activateErr <- racingRegistry.Activate(context.Background(), tool.SessionResourceServices{})
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
