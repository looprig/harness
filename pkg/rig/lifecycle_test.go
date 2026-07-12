package rig

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
	storage "github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

type lifecycleRecordingLeaser struct {
	inner     storage.Leaser
	mu        sync.Mutex
	acquired  int
	released  int
	onRelease func()
}

func (l *lifecycleRecordingLeaser) Acquire(ctx context.Context, name string) (storage.Lease, error) {
	lease, err := l.inner.Acquire(ctx, name)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.acquired++
	l.mu.Unlock()
	return &lifecycleRecordingLease{Lease: lease, owner: l}, nil
}

func (l *lifecycleRecordingLeaser) balanced() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.acquired == l.released
}

type lifecycleRecordingLease struct {
	storage.Lease
	owner *lifecycleRecordingLeaser
}

func (l *lifecycleRecordingLease) Release(ctx context.Context) error {
	l.owner.mu.Lock()
	l.owner.released++
	if l.owner.onRelease != nil {
		l.owner.onRelease()
	}
	l.owner.mu.Unlock()
	return l.Lease.Release(ctx)
}

type lifecycleFailLedger struct {
	storage.Ledger
	err error
}

func (l lifecycleFailLedger) Append(context.Context, string, uint64, []byte) error { return l.err }

type lifecycleFailLeaser struct{ err error }

func (l lifecycleFailLeaser) Acquire(context.Context, string) (storage.Lease, error) {
	return nil, l.err
}

func lifecycleStore(t *testing.T) (*sessionstore.Store, *lifecycleRecordingLeaser) {
	t.Helper()
	backend := memstore.New()
	leaser := &lifecycleRecordingLeaser{inner: backend.Leaser}
	composite, err := storage.NewComposite(backend.Ledger, leaser, backend.KV, backend.Blobs)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sessionstore.Open(composite)
	if err != nil {
		t.Fatal(err)
	}
	return store, leaser
}

func TestNewSessionRejectsNilCeilingFromFactoryAndCleansUp(t *testing.T) {
	store, leases := lifecycleStore(t)
	rootBackend := memstore.New()
	rootLeases := &lifecycleRecordingLeaser{inner: rootBackend.Leaser}
	workspace, err := workspacestore.Open(rootBackend.Blobs)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := loop.Define(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("model")))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Define(
		WithLoops(definition),
		WithPrimers("agent"),
		WithSessionStore(store),
		WithCeilingFactory(func() *ceiling.State { return nil }),
		WithExclusiveWorkspace(workspace, t.TempDir(), rootLeases),
		WithSnapshots(SnapshotPolicy{Trigger: SnapshotManual}),
	)
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.NewSession(context.Background())
	if s != nil {
		t.Fatal("NewSession returned partial session")
	}
	var target *LifecycleError
	if !errors.As(err, &target) || target.Kind != LifecycleCeilingFailed {
		t.Fatalf("NewSession error = %T %v, want ceiling stage", err, err)
	}
	if !leases.balanced() {
		t.Fatalf("lease counts = acquired %d released %d", leases.acquired, leases.released)
	}
	if rootLeases.acquired != 0 {
		t.Fatalf("root lease acquisitions = %d, want 0 after ceiling-stage failure", rootLeases.acquired)
	}
}

func TestRestoreSessionRejectsNilCeilingFromFactoryBeforeAdmission(t *testing.T) {
	store, leases := lifecycleStore(t)
	original := lifecycleRig(t, store)
	s, err := original.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id := s.SessionID()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	restoringDefinition, err := loop.Define(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("model")))
	if err != nil {
		t.Fatal(err)
	}
	restoring, err := Define(
		WithLoops(restoringDefinition), WithPrimers("agent"), WithSessionStore(store),
		WithCeilingFactory(func() *ceiling.State { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restoring.RestoreSession(context.Background(), id)
	if restored != nil {
		_ = restored.Shutdown(context.Background())
		t.Fatal("RestoreSession returned partial session")
	}
	var target *LifecycleError
	if !errors.As(err, &target) || target.Kind != LifecycleCeilingFailed {
		t.Fatalf("RestoreSession error = %T %v, want ceiling stage", err, err)
	}
	if !leases.balanced() {
		t.Fatalf("lease counts = acquired %d released %d", leases.acquired, leases.released)
	}
}

func TestNewSessionFailureStagesReleaseAcquiredResourcesInReverse(t *testing.T) {
	tests := []struct {
		name      string
		build     func(t *testing.T, sessionLeaser *lifecycleRecordingLeaser, release func(string)) (*Rig, LifecycleErrorKind)
		wantOrder []string
	}{
		{
			name: "journal fence after session lease",
			build: func(t *testing.T, sessionLeaser *lifecycleRecordingLeaser, _ func(string)) (*Rig, LifecycleErrorKind) {
				backend := memstore.New()
				composite, err := storage.NewComposite(lifecycleFailLedger{Ledger: backend.Ledger, err: errors.New("journal")}, sessionLeaser, backend.KV, backend.Blobs)
				if err != nil {
					t.Fatal(err)
				}
				store, err := sessionstore.Open(composite)
				if err != nil {
					t.Fatal(err)
				}
				return lifecycleRig(t, store), LifecycleJournalFailed
			},
			wantOrder: []string{"session"},
		},
		{
			name: "root acquisition after session lease",
			build: func(t *testing.T, sessionLeaser *lifecycleRecordingLeaser, _ func(string)) (*Rig, LifecycleErrorKind) {
				store := lifecycleStoreWithLeaser(t, sessionLeaser)
				ws, err := workspacestore.Open(memstore.New().Blobs)
				if err != nil {
					t.Fatal(err)
				}
				r := lifecycleRig(t, store,
					WithExclusiveWorkspace(ws, t.TempDir(), lifecycleFailLeaser{err: errors.New("root")}),
					WithSnapshots(SnapshotPolicy{Trigger: SnapshotManual}),
				)
				return r, LifecycleLeaseFailed
			},
			wantOrder: []string{"session"},
		},
		{
			name: "loop bind after session and root leases",
			build: func(t *testing.T, sessionLeaser *lifecycleRecordingLeaser, release func(string)) (*Rig, LifecycleErrorKind) {
				store := lifecycleStoreWithLeaser(t, sessionLeaser)
				rootBackend := memstore.New()
				rootLeaser := &lifecycleRecordingLeaser{inner: rootBackend.Leaser, onRelease: func() { release("root") }}
				ws, err := workspacestore.Open(rootBackend.Blobs)
				if err != nil {
					t.Fatal(err)
				}
				badTool := tool.NewDefinition("bad", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
					return nil, errors.New("bind")
				})
				definition, err := loop.Define(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("model")), loop.WithTools(badTool))
				if err != nil {
					t.Fatal(err)
				}
				r, err := Define(
					WithLoops(definition), WithPrimers("agent"), WithSessionStore(store),
					WithExclusiveWorkspace(ws, t.TempDir(), rootLeaser),
					WithSnapshots(SnapshotPolicy{Trigger: SnapshotManual}),
				)
				if err != nil {
					t.Fatal(err)
				}
				return r, LifecycleSessionFailed
			},
			wantOrder: []string{"root", "session"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var orderMu sync.Mutex
			var order []string
			release := func(label string) { orderMu.Lock(); order = append(order, label); orderMu.Unlock() }
			base := memstore.New()
			sessionLeaser := &lifecycleRecordingLeaser{inner: base.Leaser, onRelease: func() { release("session") }}
			r, wantKind := tt.build(t, sessionLeaser, release)
			s, err := r.NewSession(context.Background())
			if s != nil {
				t.Fatal("NewSession returned partial session")
			}
			var target *LifecycleError
			if !errors.As(err, &target) || target.Kind != wantKind {
				t.Fatalf("NewSession error = %T %v, want %q", err, err, wantKind)
			}
			if !sessionLeaser.balanced() {
				t.Fatalf("session lease leaked: acquired=%d released=%d", sessionLeaser.acquired, sessionLeaser.released)
			}
			orderMu.Lock()
			defer orderMu.Unlock()
			if len(order) != len(tt.wantOrder) {
				t.Fatalf("release order = %v, want %v", order, tt.wantOrder)
			}
			for i := range order {
				if order[i] != tt.wantOrder[i] {
					t.Fatalf("release order = %v, want %v", order, tt.wantOrder)
				}
			}
		})
	}
}

func lifecycleStoreWithLeaser(t *testing.T, leaser storage.Leaser) *sessionstore.Store {
	t.Helper()
	backend := memstore.New()
	composite, err := storage.NewComposite(backend.Ledger, leaser, backend.KV, backend.Blobs)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sessionstore.Open(composite)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func lifecycleRig(t *testing.T, store *sessionstore.Store, extra ...Option) *Rig {
	t.Helper()
	definition, err := loop.Define(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("model")))
	if err != nil {
		t.Fatal(err)
	}
	opts := []Option{WithLoops(definition), WithPrimers("agent"), WithSessionStore(store)}
	opts = append(opts, extra...)
	r, err := Define(opts...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRigForwardsDelegationAndGateCaps(t *testing.T) {
	t.Run("delegation quota", func(t *testing.T) {
		store, _ := lifecycleStore(t)
		child, err := loop.Define(loop.WithName("child"), loop.WithInference(&stubLLM{}, validModel("child")))
		if err != nil {
			t.Fatal(err)
		}
		root, err := loop.Define(loop.WithName("root"), loop.WithInference(&stubLLM{}, validModel("root")), loop.WithDelegates("child"))
		if err != nil {
			t.Fatal(err)
		}
		r, err := Define(WithLoops(root, child), WithPrimers("root"), WithSessionStore(store), WithDelegationLimits(DelegationLimits{Depth: 2, Quota: 1}))
		if err != nil {
			t.Fatal(err)
		}
		controller, err := r.NewSession(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = controller.Shutdown(context.Background()) }()
		spawner := controller.(interface {
			NewLoop(loop.Provenance, loop.Definition) (uuid.UUID, error)
		})
		parent := loop.Provenance{LoopID: controller.ActiveLoop().ID()}
		if _, err := spawner.NewLoop(parent, child); err != nil {
			t.Fatalf("first spawn: %v", err)
		}
		_, err = spawner.NewLoop(parent, child)
		var target *session.SessionError
		if !errors.As(err, &target) || target.Kind != session.SessionLoopQuotaExceeded {
			t.Fatalf("second spawn error = %T %v, want quota exceeded", err, err)
		}
	})

	t.Run("open gate cap", func(t *testing.T) {
		store, _ := lifecycleStore(t)
		r := lifecycleRig(t, store, WithGateCaps(GateCaps{MaxOpen: 1}))
		controller, err := r.NewSession(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = controller.Shutdown(context.Background()) }()
		preparer := controller.(interface {
			PrepareGateOpen(context.Context, uuid.UUID, gate.Gate, gate.Payload) (gate.ID, error)
		})
		turnID, _ := uuid.New()
		stepID, _ := uuid.New()
		envelope := gate.Gate{
			Kind: gate.KindPermission, Resolver: gate.ResolverLoop, Blocks: gate.BlocksToolCall, Effect: gate.EffectResume,
			Subject: gate.Subject{TurnID: turnID, StepID: stepID},
		}
		payload := gate.PermissionPayload{Request: tool.BashRequest{Command: "echo ok"}}
		if _, err := preparer.PrepareGateOpen(context.Background(), controller.ActiveLoop().ID(), envelope, payload); err != nil {
			t.Fatalf("first gate: %v", err)
		}
		_, err = preparer.PrepareGateOpen(context.Background(), controller.ActiveLoop().ID(), envelope, payload)
		var target *session.GateError
		if !errors.As(err, &target) || target.Kind != session.GateCapacity {
			t.Fatalf("second gate error = %T %v, want capacity", err, err)
		}
	})
}

func TestRestoreSessionFingerprintFieldsMismatchPolicy(t *testing.T) {
	store, _ := lifecycleStore(t)
	definition, err := loop.Define(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("model")))
	if err != nil {
		t.Fatal(err)
	}
	defineRig := func(kind string, allow bool) *Rig {
		opts := []Option{
			WithLoops(definition), WithPrimers("agent"), WithSessionStore(store),
			WithFingerprintFields(ConfigFingerprintFields{AgentKind: kind}),
		}
		if allow {
			opts = append(opts, WithAllowConfigMismatch())
		}
		r, defineErr := Define(opts...)
		if defineErr != nil {
			t.Fatal(defineErr)
		}
		return r
	}
	original := defineRig("operator-a", false)
	s, err := original.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id := s.SessionID()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	if restored, err := defineRig("operator-b", false).RestoreSession(context.Background(), id); restored != nil {
		_ = restored.Shutdown(context.Background())
		t.Fatal("mismatched restore returned a session")
	} else {
		var mismatch *session.ConfigMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("mismatched restore error = %T %v, want ConfigMismatchError", err, err)
		}
	}
	restored, err := defineRig("operator-b", true).RestoreSession(context.Background(), id)
	if err != nil {
		t.Fatalf("allowed mismatch restore: %v", err)
	}
	if err := restored.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
