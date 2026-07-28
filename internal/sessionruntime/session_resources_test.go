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
	activateStart chan struct{}
	activateWait  chan struct{}
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

	firstActivateErr := registry.Activate(context.Background(), tool.SessionResourceServices{})
	secondActivateErr := registry.Activate(context.Background(), tool.SessionResourceServices{})
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

		first := registry.Activate(context.Background(), tool.SessionResourceServices{})
		second := registry.Activate(context.Background(), tool.SessionResourceServices{})
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
			activated <- registry.Activate(context.Background(), tool.SessionResourceServices{})
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
			activated <- registry.Activate(context.Background(), tool.SessionResourceServices{})
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
			activated <- registry.Activate(ctx, tool.SessionResourceServices{})
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
		if err := registry.Activate(ctx, tool.SessionResourceServices{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Activate() error = %v, want %v", err, context.Canceled)
		}
		if err := registry.Activate(context.Background(), tool.SessionResourceServices{}); !errors.Is(err, context.Canceled) {
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
