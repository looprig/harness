package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	storage "github.com/looprig/storage"
	"github.com/looprig/storage/memstore"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
)

// errFakeAcquire is the leaf error a recordingLeaser returns when told to fail Acquire,
// so a NewSession exercises the NewSessionLeaseFailed branch deterministically.
var errFakeAcquire = errors.New("recordingLeaser: forced acquire failure")

// recordingLeaser wraps a real storage.Leaser to (a) optionally force Acquire to fail
// (driving NewSessionLeaseFailed) and (b) count SUCCESSFUL acquires and releases, so a test can
// prove a failed NewSession releases the lease it acquired (no leak). acquired is bumped only when
// a real lease is handed out, so a forced-failure acquire never counts — leaving the
// balance at 0/0 (also leak-free).
type recordingLeaser struct {
	inner       storage.Leaser
	failAcquire bool

	mu       sync.Mutex
	acquired int
	released int
}

func (r *recordingLeaser) Acquire(ctx context.Context, name string) (storage.Lease, error) {
	if r.failAcquire {
		return nil, errFakeAcquire
	}
	lease, err := r.inner.Acquire(ctx, name)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.acquired++
	r.mu.Unlock()
	return &recordingLease{inner: lease, owner: r}, nil
}

func (r *recordingLeaser) balanced() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.acquired == r.released
}

// recordingLease counts every Release call against its owning recordingLeaser and delegates
// to the wrapped storage.Lease.
type recordingLease struct {
	inner storage.Lease
	owner *recordingLeaser
}

func (l *recordingLease) Epoch() uint64         { return l.inner.Epoch() }
func (l *recordingLease) Lost() <-chan struct{} { return l.inner.Lost() }
func (l *recordingLease) Release(ctx context.Context) error {
	l.owner.mu.Lock()
	l.owner.released++
	l.owner.mu.Unlock()
	return l.inner.Release(ctx)
}

// newRecordingStore opens a sessionstore.Store over a fresh in-memory backend whose Leaser
// is wrapped for observation/fault-injection. It returns the store and the leaser so a test
// can toggle failAcquire and assert the acquire/release balance.
func newRecordingStore(t *testing.T) (*sessionstore.Store, *recordingLeaser) {
	t.Helper()
	base := memstore.New()
	rl := &recordingLeaser{inner: base.Leaser}
	composite, err := storage.NewComposite(base.Ledger, rl, base.KV, base.Blobs)
	if err != nil {
		t.Fatalf("NewComposite: %v", err)
	}
	store, err := sessionstore.Open(composite)
	if err != nil {
		t.Fatalf("sessionstore.Open: %v", err)
	}
	return store, rl
}

// badClientCfg is a loop.Definition with a valid model but a nil Client, so NewSession fails
// building the primary loop — the deterministic way to drive the NewSessionRuntimeFailed branch
// (the lease has already been acquired, so this also exercises lease-release-on-failure).
func badClientCfg() loop.Definition {
	return loop.Definition{}
}

// TestLifecycleNewLifecycle proves NewLifecycle binds cfg+store into a reusable Lifecycle and fails
// closed with a typed *MissingStoreError when handed a nil store (the durable backend is a
// required dependency).
func TestLifecycleNewLifecycle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		nilStore bool
		wantErr  bool
	}{
		{name: "valid store compiles", nilStore: false, wantErr: false},
		{name: "nil store rejected", nilStore: true, wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var store *sessionstore.Store
			if !tt.nilStore {
				store = newRestoreStore(t)
			}
			r, err := newTestLifecycle(cfg(&stubLLM{}), store)
			if tt.wantErr {
				var nse *MissingStoreError
				if !errors.As(err, &nse) {
					t.Fatalf("NewLifecycle err = %v, want *MissingStoreError", err)
				}
				if r != nil {
					t.Fatalf("NewLifecycle returned a non-nil Lifecycle on error")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewLifecycle: %v", err)
			}
			if r == nil {
				t.Fatal("NewLifecycle returned a nil Lifecycle without error")
			}
		})
	}
}

func TestNewLifecycleRequiresFingerprintProvider(t *testing.T) {
	t.Parallel()
	lifecycle, err := NewLifecycle(cfg(&stubLLM{}), newRestoreStore(t))
	if lifecycle != nil {
		t.Fatal("NewLifecycle returned a lifecycle without a fingerprint provider")
	}
	var target *MissingFingerprintProviderError
	if !errors.As(err, &target) {
		t.Fatalf("NewLifecycle error = %T %v, want *MissingFingerprintProviderError", err, err)
	}
}

// TestLifecycleRun covers NewSession's happy path and every error branch in table form: a pre-
// cancelled ctx (NewSessionContextDone), a failing Leaser (NewSessionLeaseFailed), and a session-
// construction failure (NewSessionRuntimeFailed) — the last also proving the acquired lease is
// released, not leaked. The happy rows assert a non-zero id, a live session whose
// SubscribeEvents works, and (with a ceiling factory) that the factory is minted per run.
func TestLifecycleRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		badClient    bool                // NewLifecycle with a nil-Client definition → NewSession fails
		withCeiling  bool                // wire a counting ceiling factory
		cancelCtx    bool                // pass a pre-cancelled context to NewSession
		failLease    bool                // the store's Leaser fails Acquire
		wantKind     NewSessionErrorKind // zero value => success expected
		wantBalanced bool                // assert the recording leaser released every lease it acquired
	}{
		{name: "happy path"},
		{name: "happy path with ceiling factory", withCeiling: true},
		{name: "pre-cancelled ctx", cancelCtx: true, wantKind: NewSessionContextDone},
		{name: "lease acquire fails", failLease: true, wantKind: NewSessionLeaseFailed, wantBalanced: true},
		{name: "session construction failure releases lease", badClient: true, wantKind: NewSessionRuntimeFailed, wantBalanced: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// A recording store is used wherever we must fail or observe the lease; the plain
			// store otherwise.
			var store *sessionstore.Store
			var rl *recordingLeaser
			if tt.failLease || tt.wantBalanced {
				store, rl = newRecordingStore(t)
				rl.failAcquire = tt.failLease
			} else {
				store = newRestoreStore(t)
			}

			runCfg := cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}})
			if tt.badClient {
				runCfg = badClientCfg()
			}

			var mints int64
			var copts []LifecycleOption
			if tt.withCeiling {
				copts = append(copts, WithLifecycleCeilingFactory(func() *ceiling.State {
					atomic.AddInt64(&mints, 1)
					return ceiling.New()
				}))
			}
			r, err := newTestLifecycle(runCfg, store, copts...)
			if err != nil {
				t.Fatalf("NewLifecycle: %v", err)
			}

			ctx := context.Background()
			if tt.cancelCtx {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			s, err := r.NewSession(ctx)

			if tt.wantKind != "" {
				if s != nil {
					t.Errorf("NewSession returned a non-nil session on error")
				}
				var re *NewSessionError
				if !errors.As(err, &re) {
					t.Fatalf("NewSession error = %v, want *NewSessionError", err)
				}
				if re.Kind != tt.wantKind {
					t.Errorf("NewSessionError.Kind = %q, want %q", re.Kind, tt.wantKind)
				}
				if tt.wantBalanced && !rl.balanced() {
					t.Errorf("lease leaked: acquired=%d released=%d, want equal", rl.acquired, rl.released)
				}
				return
			}

			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			sid := s.SessionID()
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
			if sid.IsZero() {
				t.Fatal("NewSession returned a zero session ID")
			}
			if s == nil {
				t.Fatal("NewSession returned a nil session")
			}
			if s.SessionID() != sid {
				t.Errorf("session.SessionID = %v, want the returned id %v", s.SessionID(), sid)
			}
			sub, err := s.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
			if err != nil {
				t.Fatalf("SubscribeEvents: %v", err)
			}
			_ = sub.Close()
			if tt.withCeiling {
				if got := atomic.LoadInt64(&mints); got != 1 {
					t.Errorf("ceiling factory minted %d times, want 1 (once per NewSession)", got)
				}
			}
		})
	}
}

// runAndShutdown performs a full original run over store+cfg — NewSession, Submit an event, wait
// for quiescence, then clean-shutdown (releasing the lease so a later RestoreSession can re-acquire)
// — and returns the minted session id. It is the durable-history setup the RestoreSession table
// rows resume from.
func runAndShutdown(t *testing.T, store *sessionstore.Store, c loop.Definition) uuid.UUID {
	t.Helper()
	r, err := newTestLifecycle(c, store)
	if err != nil {
		t.Fatalf("NewLifecycle (original run): %v", err)
	}
	s, err := r.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession (original): %v", err)
	}
	sid := s.SessionID()
	if _, err := s.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "hi"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	if err := s.WaitIdle(waitCtx); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown (original): %v", err)
	}
	return sid
}

// TestLifecycleRestore covers RestoreSession in table form: the happy round trip (NewSession → persist →
// RestoreSession), an unknown/never-run id (typed *RestoreDiscoveryError, no panic), and the
// NewLifecycle-captured config-fingerprint guard — rejecting a mismatch with *ConfigMismatchError
// unless WithLifecycleAllowConfigMismatch was compiled in.
func TestLifecycleRestore(t *testing.T) {
	t.Parallel()
	// wantErr classifies the expected typed failure ("" => success).
	tests := []struct {
		name          string
		unknownID     bool // restore an id that was never run (no journal history)
		mismatchModel bool // compile the restoring Lifecycle under a different model fingerprint
		allowMismatch bool // add WithLifecycleAllowConfigMismatch to the restoring Lifecycle
		wantErr       string
	}{
		{name: "happy path rebuilds a live session", wantErr: ""},
		{name: "unknown session id surfaces discovery error", unknownID: true, wantErr: "discovery"},
		{name: "config fingerprint mismatch rejects", mismatchModel: true, wantErr: "mismatch"},
		{name: "config mismatch allowed proceeds", mismatchModel: true, allowMismatch: true, wantErr: ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newRestoreStore(t)

			var sid uuid.UUID
			if tt.unknownID {
				sid = mustSessionID(t)
			} else {
				sid = runAndShutdown(t, store,
					restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful"))
			}

			restoreModel := "model-x"
			if tt.mismatchModel {
				restoreModel = "model-DIFFERENT"
			}
			var copts []LifecycleOption
			if tt.allowMismatch {
				copts = append(copts, WithLifecycleAllowConfigMismatch())
			}
			rr, err := newTestLifecycle(restoreCfg(&stubLLM{}, restoreModel, "be helpful"), store, copts...)
			if err != nil {
				t.Fatalf("NewLifecycle (restore): %v", err)
			}

			restored, err := rr.RestoreSession(context.Background(), sid)

			switch tt.wantErr {
			case "discovery":
				if restored != nil {
					t.Fatalf("RestoreSession returned a non-nil session for an unknown ID")
				}
				var de *RestoreDiscoveryError
				if !errors.As(err, &de) {
					t.Fatalf("RestoreSession error = %v, want *RestoreDiscoveryError", err)
				}
			case "mismatch":
				if restored != nil {
					t.Fatalf("RestoreSession returned a non-nil session on a config mismatch")
				}
				var cme *ConfigMismatchError
				if !errors.As(err, &cme) {
					t.Fatalf("RestoreSession error = %v, want *ConfigMismatchError", err)
				}
			default:
				if err != nil {
					t.Fatalf("RestoreSession error = %v, want success", err)
				}
				t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
				if restored.SessionID() != sid {
					t.Errorf("restored SessionID = %v, want %v", restored.SessionID(), sid)
				}
				if restored.PrimaryLoopID().IsZero() {
					t.Error("restored session has a zero primary loop id")
				}
			}
		})
	}
}

// TestLifecycleConcurrentReuse is the executable proof of the immutable-Lifecycle claim: one
// compiled Lifecycle drives many concurrent Runs AND concurrent Restores (of sessions
// pre-persisted under the SAME captured cfg) with no shared mutable state. Under -race it
// asserts every operation succeeds and every resulting session id is distinct (the fresh
// Runs mint unique ids; each RestoreSession recovers its own pre-persisted id).
func TestLifecycleConcurrentReuse(t *testing.T) {
	t.Parallel()
	const nRuns = 8
	const nRestores = 4

	store := newRestoreStore(t)
	c := restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful")
	r, err := newTestLifecycle(c, store)
	if err != nil {
		t.Fatalf("NewLifecycle: %v", err)
	}

	// Pre-persist the sessions the concurrent Restores will resume (each cleanly shut down so
	// its lease is free to re-acquire).
	preSids := make([]uuid.UUID, nRestores)
	for i := range preSids {
		preSids[i] = runAndShutdown(t, store, c)
	}

	type result struct {
		sid uuid.UUID
		s   *Session
		err error
	}
	results := make(chan result, nRuns+nRestores)
	var wg sync.WaitGroup

	for i := 0; i < nRuns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := r.NewSession(context.Background())
			var sid uuid.UUID
			if s != nil {
				sid = s.SessionID()
			}
			results <- result{sid: sid, s: s, err: err}
		}()
	}
	for i := 0; i < nRestores; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := r.RestoreSession(context.Background(), preSids[i])
			var sid uuid.UUID
			if s != nil {
				sid = s.SessionID()
			}
			results <- result{sid: sid, s: s, err: err}
		}()
	}
	wg.Wait()
	close(results)

	seen := make(map[uuid.UUID]struct{})
	for res := range results {
		if res.err != nil {
			t.Errorf("concurrent op failed: %v", res.err)
			continue
		}
		if res.s == nil {
			t.Error("concurrent op returned a nil session without error")
			continue
		}
		t.Cleanup(func() { _ = res.s.Shutdown(context.Background()) })
		if res.sid.IsZero() {
			t.Error("concurrent op returned a zero session id")
		}
		if _, dup := seen[res.sid]; dup {
			t.Errorf("duplicate session id %v across concurrent ops", res.sid)
		}
		seen[res.sid] = struct{}{}
	}
	if len(seen) != nRuns+nRestores {
		t.Errorf("distinct session ids = %d, want %d", len(seen), nRuns+nRestores)
	}
}
