package sessionruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"sync"

	"github.com/looprig/harness/pkg/tool"
)

var (
	errSessionResourcesClosing = errors.New("session: resource registry closing")
	errSessionResourceKeyEmpty = errors.New("session: resource key is empty")
	errSessionResourceFactory  = errors.New("session: resource factory is nil")
	errSessionResourceNil      = errors.New("session: resource factory returned nil")
)

// sessionResources is the session-private implementation of the public resource
// registry. Its storage root is fixed at construction and every key maps to a
// stable opaque child name, so an untrusted key cannot escape or disclose a host
// path through traversal syntax.
type sessionResources struct {
	storageRoot string

	mu      sync.Mutex
	entries map[string]*sessionResourceEntry

	activated       bool
	services        tool.SessionResourceServices
	activateStarted bool
	activateDone    chan struct{}
	activateErr     error

	closing         bool
	shutdownStarted bool
	shutdownDone    chan struct{}
	shutdownErr     error
}

type sessionResourceEntry struct {
	ready    chan struct{}
	resource tool.SessionResource
	err      error

	mu sync.Mutex

	activateStarted bool
	activateDone    chan struct{}
	activateErr     error

	shutdownStarted bool
	shutdownDone    chan struct{}
	shutdownErr     error
}

// sessionResourceErrorSet preserves the already-sorted operation order while
// retaining errors.Is/errors.As traversal for every cause.
type sessionResourceErrorSet struct {
	Operation string
	Causes    []error
}

func (e *sessionResourceErrorSet) Error() string {
	return fmt.Sprintf("session: resource %s failed", e.Operation)
}

func (e *sessionResourceErrorSet) Unwrap() []error {
	return e.Causes
}

func newSessionResources(storageRoot string) *sessionResources {
	return &sessionResources{
		storageRoot: filepath.Clean(storageRoot),
		entries:     make(map[string]*sessionResourceEntry),
	}
}

var _ tool.SessionResourceRegistry = (*sessionResources)(nil)

func (r *sessionResources) GetOrCreate(
	ctx context.Context,
	key string,
	factory func(string) (tool.SessionResource, error),
) (tool.SessionResource, error) {
	ctx = nonNilSessionResourceContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, errSessionResourceKeyEmpty
	}
	if factory == nil {
		return nil, errSessionResourceFactory
	}

	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return nil, errSessionResourcesClosing
	}
	if existing := r.entries[key]; existing != nil {
		r.mu.Unlock()
		return r.awaitResource(ctx, existing)
	}
	entry := &sessionResourceEntry{
		ready:        make(chan struct{}),
		shutdownDone: make(chan struct{}),
	}
	r.entries[key] = entry
	path := r.resourcePath(key)
	r.mu.Unlock()

	resource, createErr := factory(path)
	if createErr == nil && nilSessionResource(resource) {
		createErr = errSessionResourceNil
	}
	if createErr != nil {
		r.finishFailedCreation(key, entry, createErr)
		return nil, createErr
	}

	r.mu.Lock()
	entry.resource = resource
	closing := r.closing
	activated := r.activated
	services := r.services
	r.mu.Unlock()

	var activateErr error
	if activated && !closing {
		activateErr = entry.activate(ctx, services)
	}

	r.mu.Lock()
	closing = r.closing
	if activateErr != nil {
		entry.err = activateErr
		if r.entries[key] == entry {
			delete(r.entries, key)
		}
	}
	close(entry.ready)
	r.mu.Unlock()

	if activateErr != nil {
		_ = entry.shutdown(context.WithoutCancel(ctx))
		return nil, activateErr
	}
	if closing {
		<-entry.shutdownComplete()
		return nil, combineSessionResourceErrors(
			"creation",
			errSessionResourcesClosing,
			entry.shutdownResult(),
		)
	}
	return resource, nil
}

func (r *sessionResources) finishFailedCreation(key string, entry *sessionResourceEntry, createErr error) {
	r.mu.Lock()
	entry.err = createErr
	if r.entries[key] == entry {
		delete(r.entries, key)
	}
	close(entry.ready)
	r.mu.Unlock()
}

func (r *sessionResources) resourcePath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(r.storageRoot, hex.EncodeToString(sum[:]))
}

func (r *sessionResources) awaitResource(
	ctx context.Context,
	entry *sessionResourceEntry,
) (tool.SessionResource, error) {
	select {
	case <-entry.ready:
		if entry.err != nil {
			return nil, entry.err
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	r.mu.Lock()
	closing := r.closing
	r.mu.Unlock()
	if !closing {
		return entry.resource, nil
	}
	<-entry.shutdownComplete()
	return nil, combineSessionResourceErrors(
		"lookup",
		errSessionResourcesClosing,
		entry.shutdownResult(),
	)
}

// Activate late-binds live session services exactly once. The activation
// boundary covers every resource admitted before it; a factory that completes
// after the boundary observes activated and performs its own entry activation
// before publishing the resource to waiters.
func (r *sessionResources) Activate(ctx context.Context, services tool.SessionResourceServices) error {
	ctx = nonNilSessionResourceContext(ctx)

	r.mu.Lock()
	if r.activateStarted {
		done := r.activateDone
		r.mu.Unlock()
		return awaitSessionResourceOperation(ctx, done, func() error {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.activateErr
		})
	}
	if r.closing {
		r.mu.Unlock()
		return errSessionResourcesClosing
	}
	r.activateStarted = true
	r.activateDone = make(chan struct{})
	r.activated = true
	r.services = services
	snapshot := r.snapshotLocked()
	r.mu.Unlock()

	causes := make([]error, 0, len(snapshot))
	for _, entry := range snapshot {
		select {
		case <-entry.ready:
			if entry.err != nil {
				if entry.resource != nil {
					causes = append(causes, entry.err)
				}
				continue
			}
			if err := entry.activate(ctx, services); err != nil {
				causes = append(causes, err)
			}
		case <-ctx.Done():
			causes = append(causes, ctx.Err())
		}
	}
	result := combineSessionResourceErrors("activation", causes...)

	r.mu.Lock()
	r.activateErr = result
	close(r.activateDone)
	r.mu.Unlock()
	return result
}

// Shutdown closes admission before taking its deterministic snapshot. It then
// waits for every admitted factory and shuts resources down in key order.
// Repeated callers join the same teardown and receive the same result.
func (r *sessionResources) Shutdown(ctx context.Context) error {
	ctx = nonNilSessionResourceContext(ctx)

	r.mu.Lock()
	if r.shutdownStarted {
		done := r.shutdownDone
		r.mu.Unlock()
		<-done
		r.mu.Lock()
		result := r.shutdownErr
		r.mu.Unlock()
		return combineSessionResourceErrors("shutdown", result, ctx.Err())
	}
	r.shutdownStarted = true
	r.shutdownDone = make(chan struct{})
	r.closing = true
	snapshot := r.snapshotLocked()
	r.mu.Unlock()

	causes := make([]error, 0, len(snapshot)+1)
	for _, entry := range snapshot {
		<-entry.ready
		if entry.resource == nil {
			continue
		}
		if err := entry.shutdown(ctx); err != nil {
			causes = append(causes, err)
		}
	}
	if err := ctx.Err(); err != nil {
		causes = append(causes, err)
	}
	result := combineSessionResourceErrors("shutdown", causes...)

	r.mu.Lock()
	r.shutdownErr = result
	close(r.shutdownDone)
	r.mu.Unlock()
	return result
}

func (r *sessionResources) snapshotLocked() []*sessionResourceEntry {
	keys := make([]string, 0, len(r.entries))
	for key := range r.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	snapshot := make([]*sessionResourceEntry, 0, len(keys))
	for _, key := range keys {
		snapshot = append(snapshot, r.entries[key])
	}
	return snapshot
}

func (e *sessionResourceEntry) activate(
	ctx context.Context,
	services tool.SessionResourceServices,
) error {
	e.mu.Lock()
	if e.activateStarted {
		done := e.activateDone
		e.mu.Unlock()
		return awaitSessionResourceOperation(ctx, done, func() error {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.activateErr
		})
	}
	if e.shutdownStarted {
		e.mu.Unlock()
		return errSessionResourcesClosing
	}
	e.activateStarted = true
	e.activateDone = make(chan struct{})
	e.mu.Unlock()

	err := e.resource.Activate(ctx, services)

	e.mu.Lock()
	e.activateErr = err
	close(e.activateDone)
	e.mu.Unlock()
	return err
}

func (e *sessionResourceEntry) shutdown(ctx context.Context) error {
	e.mu.Lock()
	if e.shutdownStarted {
		done := e.shutdownDone
		e.mu.Unlock()
		<-done
		return e.shutdownResult()
	}
	e.shutdownStarted = true
	activateDone := e.activateDone
	e.mu.Unlock()

	if activateDone != nil {
		<-activateDone
	}
	err := e.resource.Shutdown(ctx)

	e.mu.Lock()
	e.shutdownErr = err
	close(e.shutdownDone)
	e.mu.Unlock()
	return err
}

func (e *sessionResourceEntry) shutdownComplete() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.shutdownDone
}

func (e *sessionResourceEntry) shutdownResult() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.shutdownErr
}

func awaitSessionResourceOperation(ctx context.Context, done <-chan struct{}, result func() error) error {
	select {
	case <-done:
		return result()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func combineSessionResourceErrors(operation string, causes ...error) error {
	filtered := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			filtered = append(filtered, cause)
		}
	}
	switch len(filtered) {
	case 0:
		return nil
	case 1:
		return filtered[0]
	default:
		return &sessionResourceErrorSet{Operation: operation, Causes: filtered}
	}
}

func nonNilSessionResourceContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func nilSessionResource(resource tool.SessionResource) bool {
	if resource == nil {
		return true
	}
	value := reflect.ValueOf(resource)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
