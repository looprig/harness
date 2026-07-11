package serve

import (
	"context"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
)

// fakeLiveSession is a no-op LiveSession used only to exercise the registry's
// membership bookkeeping. The registry stores the value and never calls its
// methods, so the bodies are inert. A unique id field lets tests assert that a
// specific stored value survived a putIfAbsent collision.
type fakeLiveSession struct {
	id int
}

func (fakeLiveSession) SessionID() uuid.UUID { return uuid.UUID{} }

func (fakeLiveSession) Submit(context.Context, []content.Block) (uuid.UUID, error) {
	return uuid.UUID{}, nil
}
func (fakeLiveSession) SubscribeEvents(event.EventFilter) (event.Subscription, error) {
	return nil, nil
}
func (fakeLiveSession) RespondGate(context.Context, gate.GateResponse) error { return nil }
func (fakeLiveSession) Interrupt(context.Context) (bool, error)              { return false, nil }

// mustUUID mints a fresh id for a test, failing the test if generation errors.
func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New(): %v", err)
	}
	return id
}

func TestRegistryGet(t *testing.T) {
	t.Parallel()
	id := mustUUID(t)
	sess := fakeLiveSession{id: 1}

	tests := []struct {
		name    string
		seed    bool
		lookup  uuid.UUID
		wantOK  bool
		wantVal LiveSession
	}{
		{name: "present after put", seed: true, lookup: id, wantOK: true, wantVal: sess},
		{name: "missing returns nil,false", seed: false, lookup: id, wantOK: false, wantVal: nil},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRegistry()
			if tt.seed {
				r.put(id, sess)
			}
			got, ok := r.get(tt.lookup)
			if ok != tt.wantOK {
				t.Fatalf("get() ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.wantVal {
				t.Errorf("get() = %v, want %v", got, tt.wantVal)
			}
		})
	}
}

func TestRegistryPutIfAbsent(t *testing.T) {
	t.Parallel()
	id := mustUUID(t)
	original := fakeLiveSession{id: 1}
	intruder := fakeLiveSession{id: 2}

	tests := []struct {
		name       string
		seed       bool
		wantStored bool
		wantVal    LiveSession
	}{
		{name: "empty stores and reports true", seed: false, wantStored: true, wantVal: intruder},
		{name: "existing rejects and keeps original", seed: true, wantStored: false, wantVal: original},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRegistry()
			if tt.seed {
				r.put(id, original)
			}
			stored := r.putIfAbsent(id, intruder)
			if stored != tt.wantStored {
				t.Fatalf("putIfAbsent() = %v, want %v", stored, tt.wantStored)
			}
			got, ok := r.get(id)
			if !ok {
				t.Fatalf("get() after putIfAbsent: entry missing")
			}
			if got != tt.wantVal {
				t.Errorf("stored value = %v, want %v (no-overwrite violated)", got, tt.wantVal)
			}
		})
	}
}

func TestRegistryDelete(t *testing.T) {
	t.Parallel()
	id := mustUUID(t)
	sess := fakeLiveSession{id: 1}

	tests := []struct {
		name    string
		seed    bool
		wantOK  bool
		wantVal LiveSession
	}{
		{name: "present returns entry and removes it", seed: true, wantOK: true, wantVal: sess},
		{name: "missing returns nil,false", seed: false, wantOK: false, wantVal: nil},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRegistry()
			if tt.seed {
				r.put(id, sess)
			}
			got, ok := r.delete(id)
			if ok != tt.wantOK {
				t.Fatalf("delete() ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.wantVal {
				t.Errorf("delete() = %v, want %v", got, tt.wantVal)
			}
			// After delete the entry must be gone regardless of prior state.
			if _, still := r.get(id); still {
				t.Errorf("get() after delete: entry still present")
			}
		})
	}
}

// TestRegistryConcurrent hammers the registry from many goroutines on both
// distinct and overlapping ids. It exists to be run under -race: the map is only
// safe because every method holds the lock, so any missing lock surfaces here as
// a data race. It also asserts a consistent final state (every id that was put
// and not deleted is still resolvable, and every deleted id is gone).
func TestRegistryConcurrent(t *testing.T) {
	t.Parallel()
	r := newRegistry()

	const workers = 32
	// A small shared pool of ids so goroutines overlap on the same keys.
	shared := make([]uuid.UUID, 4)
	for i := range shared {
		shared[i] = mustUUID(t)
	}

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		w := w
		go func() {
			defer wg.Done()
			// Each worker owns a distinct id it puts then deletes, and also
			// churns the shared ids to force overlap.
			own := mustUUID(t)
			sess := fakeLiveSession{id: w}
			for i := 0; i < 200; i++ {
				r.put(own, sess)
				r.get(own)
				sid := shared[i%len(shared)]
				r.putIfAbsent(sid, sess)
				r.get(sid)
				r.delete(sid)
				r.delete(own)
			}
		}()
	}
	wg.Wait()

	// Final state: seed one known id and confirm the registry is still coherent
	// after the concurrent churn.
	final := mustUUID(t)
	fs := fakeLiveSession{id: 99}
	r.put(final, fs)
	got, ok := r.get(final)
	if !ok || got != fs {
		t.Fatalf("post-churn get() = %v,%v, want %v,true", got, ok, fs)
	}
	if _, ok := r.delete(final); !ok {
		t.Errorf("post-churn delete() ok = false, want true")
	}
}
