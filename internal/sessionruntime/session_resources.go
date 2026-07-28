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

	activated        bool
	services         tool.SessionResourceServices
	activateStarted  bool
	activateFinished bool
	activateDone     chan struct{}
	activateErr      error

	closing         bool
	shutdownStarted bool
	shutdownDone    chan struct{}
	shutdownErr     error
}

type sessionResourceEntry struct {
	key              string
	creationDone     chan struct{}
	creationFinished bool
	creationErr      error
	resource         tool.SessionResource

	publication     chan struct{}
	publicationOpen bool
	usabilityErr    error

	activationPending bool

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

	for {
		r.mu.Lock()
		if r.closing {
			r.mu.Unlock()
			return nil, errSessionResourcesClosing
		}
		if r.activateErr != nil {
			err := r.activateErr
			r.mu.Unlock()
			return nil, err
		}
		if existing := r.entries[key]; existing != nil {
			r.mu.Unlock()
			return r.awaitResource(ctx, existing)
		}
		if r.activateStarted && !r.activateFinished {
			done := r.activateDone
			r.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		entry := &sessionResourceEntry{
			key:             key,
			creationDone:    make(chan struct{}),
			publication:     make(chan struct{}),
			publicationOpen: true,
			shutdownDone:    make(chan struct{}),
		}
		r.entries[key] = entry
		path := r.resourcePath(key)
		r.mu.Unlock()

		// Factories run without a registry lock and must return; their public
		// contract has no cancellation parameter.
		resource, createErr := factory(path)
		if createErr == nil && nilSessionResource(resource) {
			createErr = errSessionResourceNil
		}
		if createErr != nil {
			// A failed factory transfers no resource ownership. Clearing even a
			// typed-nil or partial result keeps concurrent shutdown panic-free.
			resource = nil
		}
		activateHere := r.finishCreation(key, entry, resource, createErr)
		if activateHere {
			activateErr := entry.activate(ctx, r.servicesForActivation())
			r.finishActivation(entry, activateErr, ctx)
		}
		return r.awaitResource(ctx, entry)
	}
}

func (r *sessionResources) finishCreation(
	key string,
	entry *sessionResourceEntry,
	resource tool.SessionResource,
	createErr error,
) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry.creationFinished = true
	entry.creationErr = createErr
	entry.resource = resource
	close(entry.creationDone)
	if createErr != nil {
		entry.usabilityErr = createErr
		entry.activationPending = false
		if r.entries[key] == entry {
			delete(r.entries, key)
		}
		r.closePublicationLocked(entry)
		return false
	}
	if r.closing {
		r.closePublicationLocked(entry)
		return false
	}
	if entry.activationPending {
		return false
	}
	if r.activated {
		entry.activationPending = true
		return true
	}
	r.closePublicationLocked(entry)
	return false
}

func (r *sessionResources) servicesForActivation() tool.SessionResourceServices {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.services
}

func (r *sessionResources) resourcePath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(r.storageRoot, hex.EncodeToString(sum[:]))
}

func (r *sessionResources) awaitResource(
	ctx context.Context,
	entry *sessionResourceEntry,
) (tool.SessionResource, error) {
	for {
		r.mu.Lock()
		publication := entry.publication
		r.mu.Unlock()

		select {
		case <-publication:
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		r.mu.Lock()
		if publication != entry.publication {
			r.mu.Unlock()
			continue
		}
		usabilityErr := entry.usabilityErr
		resource := entry.resource
		closing := r.closing
		r.mu.Unlock()
		if usabilityErr != nil {
			return nil, usabilityErr
		}
		if !closing {
			return resource, nil
		}
		<-entry.shutdownComplete()
		return nil, combineSessionResourceErrors(
			"lookup",
			errSessionResourcesClosing,
			entry.shutdownResult(),
		)
	}
}

// Activate late-binds live session services exactly once. Under the registry
// lock it moves every admitted entry to a new usability gate; lookups and
// in-flight creators then join that gate until activation and failure cleanup
// finish. Resources admitted after a successful boundary activate themselves
// synchronously before publication.
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
	for _, entry := range snapshot {
		entry.activationPending = true
		if entry.creationFinished && entry.creationErr == nil {
			r.openPublicationLocked(entry)
		}
	}
	r.mu.Unlock()

	causes := make([]error, 0, len(snapshot))
	activationEntries := make([]*sessionResourceEntry, 0, len(snapshot))
	for _, entry := range snapshot {
		<-entry.creationDone
		r.mu.Lock()
		createErr := entry.creationErr
		r.mu.Unlock()
		if createErr != nil {
			continue
		}
		activationEntries = append(activationEntries, entry)

		activateErr := ctx.Err()
		if activateErr == nil {
			// Activate runs without the registry lock. Implementations must
			// honor ctx and return; bounded owner cleanup is added separately.
			activateErr = entry.activate(ctx, services)
		}
		if activateErr != nil {
			causes = append(causes, activateErr)
		}
	}
	result := combineSessionResourceErrors("activation", causes...)

	if result != nil {
		r.mu.Lock()
		// Publish the terminal failure to newly requested keys before cleanup so
		// a resource callback may safely reenter the registry without waiting
		// for the activation operation that owns that callback.
		r.activateErr = result
		for _, entry := range activationEntries {
			entry.activationPending = false
			entry.usabilityErr = result
		}
		r.mu.Unlock()

		for _, entry := range activationEntries {
			if err := entry.shutdown(context.WithoutCancel(ctx)); err != nil {
				causes = append(causes, err)
			}
		}
		result = combineSessionResourceErrors("activation", causes...)
	}

	r.mu.Lock()
	r.activateErr = result
	r.activateFinished = true
	for _, entry := range activationEntries {
		entry.activationPending = false
		entry.usabilityErr = result
		if result != nil && r.entries[entry.key] == entry {
			delete(r.entries, entry.key)
		}
		r.closePublicationLocked(entry)
	}
	close(r.activateDone)
	r.mu.Unlock()
	return result
}

func (r *sessionResources) finishActivation(
	entry *sessionResourceEntry,
	activateErr error,
	ctx context.Context,
) error {
	r.mu.Lock()
	entry.activationPending = false
	entry.usabilityErr = activateErr
	if activateErr == nil {
		r.closePublicationLocked(entry)
		r.mu.Unlock()
		return nil
	}
	// Publish the per-key terminal error while retaining its tombstone. A
	// Shutdown callback may then reenter GetOrCreate for this key without
	// waiting on the cleanup operation it currently owns.
	r.closePublicationLocked(entry)
	r.mu.Unlock()

	// Failed activation must never publish a live resource. Shutdown is invoked
	// outside the registry lock and must honor its context and return. Task 25
	// adds the bounded owner-cleanup policy around this contract.
	shutdownErr := entry.shutdown(context.WithoutCancel(ctx))
	result := combineSessionResourceErrors("activation cleanup", activateErr, shutdownErr)

	r.mu.Lock()
	entry.usabilityErr = result
	if r.entries[entry.key] == entry {
		delete(r.entries, entry.key)
	}
	r.mu.Unlock()
	return result
}

func (r *sessionResources) openPublicationLocked(entry *sessionResourceEntry) {
	if entry.publicationOpen {
		return
	}
	entry.publication = make(chan struct{})
	entry.publicationOpen = true
}

func (r *sessionResources) closePublicationLocked(entry *sessionResourceEntry) {
	if !entry.publicationOpen {
		return
	}
	close(entry.publication)
	entry.publicationOpen = false
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
		<-entry.creationDone
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

	// Resource callbacks run without registry locks. Activate must honor ctx and
	// return; Task 25 supplies the bounded owner policy around this contract.
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
	// Shutdown likewise runs without registry locks and must honor ctx and
	// return. The session owner supplies its bounded cleanup context.
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
