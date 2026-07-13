package rig

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	harnesstools "github.com/looprig/harness/pkg/tools"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/inference"
	storage "github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

type lifecycleLLM struct{}

func (lifecycleLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("Invoke not used")
}

func (lifecycleLLM) Stream(context.Context, inference.Request) (*inference.StreamReader[content.Chunk], error) {
	chunks := []content.Chunk{&content.TextChunk{Text: "done"}}
	index := 0
	return inference.NewStreamReader(func() (content.Chunk, error) {
		if index == len(chunks) {
			return nil, io.EOF
		}
		chunk := chunks[index]
		index++
		return chunk, nil
	}, nil), nil
}

type lifecycleRecordingLeaser struct {
	inner           storage.Leaser
	mu              sync.Mutex
	acquired        int
	released        int
	onRelease       func()
	rejectCanceled  bool
	canceledRelease bool
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
	if l.owner.rejectCanceled && ctx.Err() != nil {
		l.owner.canceledRelease = true
		l.owner.mu.Unlock()
		return errors.New("release received canceled context")
	}
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

type lifecycleNthFailLedger struct {
	storage.Ledger
	failAt   int32
	calls    atomic.Int32
	err      error
	lastName string
}

func (l *lifecycleNthFailLedger) Append(ctx context.Context, name string, expected uint64, payload []byte) error {
	l.lastName = name
	if l.calls.Add(1) == l.failAt {
		return l.err
	}
	return l.Ledger.Append(ctx, name, expected, payload)
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

func TestRestoreFingerprintDecisionPrecedesWorkspaceAndBinding(t *testing.T) {
	for _, allow := range []bool{false, true} {
		name := "deny"
		if allow {
			name = "allow"
		}
		t.Run(name, func(t *testing.T) {
			store, _ := lifecycleStore(t)
			var binds atomic.Int32
			definition, err := loop.Define(
				loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("model")),
				loop.WithPolicyRevision("permission-v1"),
				loop.WithPermissionFactory(func(context.Context, tool.Bindings) (loop.PermissionGate, error) {
					binds.Add(1)
					return lifecyclePermissionGate{}, nil
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			original, err := Define(WithLoops(definition), WithPrimers("agent"), WithSessionStore(store))
			if err != nil {
				t.Fatal(err)
			}
			s, err := original.NewSession(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			id := s.SessionID()
			if err := s.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			binds.Store(0)

			rootBackend := memstore.New()
			rootLeases := &lifecycleRecordingLeaser{inner: rootBackend.Leaser}
			workspace, err := workspacestore.Open(rootBackend.Blobs)
			if err != nil {
				t.Fatal(err)
			}
			opts := []Option{
				WithLoops(definition), WithPrimers("agent"), WithSessionStore(store),
				WithExclusiveWorkspace(workspace, t.TempDir(), rootLeases),
				WithSnapshots(SnapshotPolicy{Trigger: SnapshotManual}),
			}
			if allow {
				opts = append(opts, WithAllowConfigMismatch())
			}
			restoring, err := Define(opts...)
			if err != nil {
				t.Fatal(err)
			}
			restored, restoreErr := restoring.RestoreSession(context.Background(), id)
			if allow {
				if restoreErr != nil {
					t.Fatalf("allowed restore: %v", restoreErr)
				}
				if binds.Load() == 0 || rootLeases.acquired == 0 {
					t.Fatalf("allowed restore binds=%d root acquires=%d, want both", binds.Load(), rootLeases.acquired)
				}
				_ = restored.Shutdown(context.Background())
				return
			}
			if restored != nil {
				_ = restored.Shutdown(context.Background())
				t.Fatal("denied mismatch returned a session")
			}
			var mismatch *session.ConfigMismatchError
			if !errors.As(restoreErr, &mismatch) {
				t.Fatalf("restore error = %T %v, want mismatch", restoreErr, restoreErr)
			}
			if binds.Load() != 0 || rootLeases.acquired != 0 {
				t.Fatalf("denied restore performed side effects: binds=%d root acquires=%d", binds.Load(), rootLeases.acquired)
			}
		})
	}
}

type lifecyclePermissionGate struct{}

func (lifecyclePermissionGate) Check(context.Context, tool.InvokableTool, string, string) loop.Effect {
	return loop.EffectAsk
}

func (lifecyclePermissionGate) Grant(context.Context, string, string, tool.ApprovalScope) error {
	return nil
}

type lifecycleReadGuard struct{}

func (lifecycleReadGuard) DeniedRead(string) bool { return false }
func (lifecycleReadGuard) MaxReadBytes() int64    { return 1 << 20 }

func TestNewSessionWithSeedCommitsCheckpointBeforeAnyLoopStarts(t *testing.T) {
	store, _ := lifecycleStore(t)
	workspace, err := workspacestore.Open(memstore.New().Blobs)
	if err != nil {
		t.Fatal(err)
	}
	seedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(seedDir, "seed.txt"), []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	seed, err := workspace.Snapshot(context.Background(), seedDir)
	if err != nil {
		t.Fatal(err)
	}
	r := lifecycleRig(t, store,
		WithSessionWorkspaces(workspace, t.TempDir()),
		WithSnapshots(SnapshotPolicy{Trigger: SnapshotManual}),
	)
	s, err := r.NewSession(context.Background(), WithSeedSnapshot(seed))
	if err != nil {
		t.Fatal(err)
	}
	id := s.SessionID()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	replayer, err := store.OpenEventReplayer(id, sessionstore.ReplayRequest{FromSeq: 0})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{From: journal.Beginning()})
	if err != nil {
		t.Fatal(err)
	}
	defer cursor.Close()
	var ordered []string
	var persistedConfig event.ConfigFingerprint
	for {
		ev, _, nextErr := cursor.Next(context.Background())
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		switch ev.(type) {
		case event.SessionStarted:
			persistedConfig = ev.(event.SessionStarted).Config
			ordered = append(ordered, "session")
		case event.WorkspaceCheckpointed:
			ordered = append(ordered, "seed")
		case event.LoopStarted:
			ordered = append(ordered, "loop")
		}
	}
	if len(ordered) < 3 || ordered[0] != "session" || ordered[1] != "seed" || ordered[2] != "loop" {
		t.Fatalf("durable construction order = %v, want [session seed loop ...]", ordered)
	}
	if persistedConfig.ModelID != "model" || persistedConfig.SystemPromptRev == "" || persistedConfig.ToolPolicyRev == "" || persistedConfig.TopologyRev == "" {
		t.Fatalf("persisted frozen SessionStarted config = %+v", persistedConfig)
	}
}

func TestNewSessionAppendFailuresAbortBeforeLeaseRelease(t *testing.T) {
	tests := []struct {
		name           string
		failAt         int32
		primers        int
		seed           bool
		wantLoopStarts int
	}{
		{name: "seed checkpoint append", failAt: 3, primers: 1, seed: true, wantLoopStarts: 0},
		{name: "second primer loop append", failAt: 4, primers: 2, wantLoopStarts: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := memstore.New()
			ledger := &lifecycleNthFailLedger{Ledger: backend.Ledger, failAt: tt.failAt, err: errors.New("append stage")}
			var orderMu sync.Mutex
			var releases []string
			recordRelease := func(label string) func() {
				return func() { orderMu.Lock(); releases = append(releases, label); orderMu.Unlock() }
			}
			sessionLeaser := &lifecycleRecordingLeaser{inner: backend.Leaser, onRelease: recordRelease("session")}
			composite, err := storage.NewComposite(ledger, sessionLeaser, backend.KV, backend.Blobs)
			if err != nil {
				t.Fatal(err)
			}
			store, err := sessionstore.Open(composite)
			if err != nil {
				t.Fatal(err)
			}
			workspaceBackend := memstore.New()
			rootLeaser := &lifecycleRecordingLeaser{inner: workspaceBackend.Leaser, onRelease: recordRelease("root")}
			workspace, err := workspacestore.Open(workspaceBackend.Blobs)
			if err != nil {
				t.Fatal(err)
			}
			definitions := make([]loop.Definition, 0, tt.primers)
			primerNames := make([]string, 0, tt.primers)
			for i := 0; i < tt.primers; i++ {
				name := fmt.Sprintf("agent-%d", i)
				definition, defineErr := loop.Define(loop.WithName(identity.AgentName(name)), loop.WithInference(&stubLLM{}, validModel(name)))
				if defineErr != nil {
					t.Fatal(defineErr)
				}
				definitions = append(definitions, definition)
				primerNames = append(primerNames, name)
			}
			rigOptions := []Option{
				WithLoops(definitions...), WithPrimers(primerNames...), WithActivePrimer(primerNames[0]), WithSessionStore(store),
				WithExclusiveWorkspace(workspace, t.TempDir(), rootLeaser), WithSnapshots(SnapshotPolicy{Trigger: SnapshotManual}),
			}
			r, err := Define(rigOptions...)
			if err != nil {
				t.Fatal(err)
			}
			var controller session.SessionController
			if tt.seed {
				seedDir := t.TempDir()
				if err := os.WriteFile(filepath.Join(seedDir, "seed"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				seed, snapErr := workspace.Snapshot(context.Background(), seedDir)
				if snapErr != nil {
					t.Fatal(snapErr)
				}
				controller, err = r.NewSession(context.Background(), WithSeedSnapshot(seed))
			} else {
				controller, err = r.NewSession(context.Background())
			}
			if controller != nil {
				_ = controller.Shutdown(context.Background())
				t.Fatal("append failure returned partial session")
			}
			if err == nil {
				t.Fatal("append failure returned nil error")
			}
			if !sessionLeaser.balanced() || !rootLeaser.balanced() {
				t.Fatalf("lease leak: session=%v root=%v", sessionLeaser.balanced(), rootLeaser.balanced())
			}
			orderMu.Lock()
			if len(releases) != 2 || releases[0] != "root" || releases[1] != "session" {
				t.Fatalf("release order = %v, want [root session]", releases)
			}
			orderMu.Unlock()
			sid, parseErr := uuid.Parse(strings.TrimPrefix(ledger.lastName, "sessions/"))
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			events := replayRigEvents(t, store, sid)
			loopStarts := 0
			for _, ev := range events {
				switch ev.(type) {
				case event.LoopStarted:
					loopStarts++
				case event.SessionStopped:
					t.Fatal("construction abort durably appended SessionStopped")
				}
			}
			if loopStarts != tt.wantLoopStarts {
				t.Fatalf("durable LoopStarted count = %d, want %d", loopStarts, tt.wantLoopStarts)
			}
		})
	}
}

func TestNewSessionSecondPrimerBindFailureUnwindsFirstBinding(t *testing.T) {
	store, sessionLeases := lifecycleStore(t)
	workspaceBackend := memstore.New()
	rootLeases := &lifecycleRecordingLeaser{inner: workspaceBackend.Leaser}
	workspace, err := workspacestore.Open(workspaceBackend.Blobs)
	if err != nil {
		t.Fatal(err)
	}
	var firstBinds atomic.Int32
	firstTool := tool.NewDefinition("first", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		firstBinds.Add(1)
		return nil, nil
	})
	secondTool := tool.NewDefinition("second", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return nil, errors.New("second bind")
	})
	first, err := loop.Define(loop.WithName("first"), loop.WithInference(&stubLLM{}, validModel("first")), loop.WithTools(firstTool))
	if err != nil {
		t.Fatal(err)
	}
	second, err := loop.Define(loop.WithName("second"), loop.WithInference(&stubLLM{}, validModel("second")), loop.WithTools(secondTool))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Define(
		WithLoops(first, second), WithPrimers("first", "second"), WithActivePrimer("first"), WithSessionStore(store),
		WithExclusiveWorkspace(workspace, t.TempDir(), rootLeases), WithSnapshots(SnapshotPolicy{Trigger: SnapshotManual}),
	)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := r.NewSession(context.Background())
	if controller != nil {
		_ = controller.Shutdown(context.Background())
		t.Fatal("partial bind failure returned a session")
	}
	if err == nil || firstBinds.Load() != 1 {
		t.Fatalf("error=%v first binds=%d, want failure after one first bind", err, firstBinds.Load())
	}
	if !sessionLeases.balanced() || !rootLeases.balanced() {
		t.Fatalf("lease leak: session=%v root=%v", sessionLeases.balanced(), rootLeases.balanced())
	}
}

func replayRigEvents(t *testing.T, store *sessionstore.Store, id uuid.UUID) []event.Event {
	t.Helper()
	replayer, err := store.OpenEventReplayer(id, sessionstore.ReplayRequest{FromSeq: 0})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{From: journal.Beginning()})
	if err != nil {
		t.Fatal(err)
	}
	defer cursor.Close()
	var result []event.Event
	for {
		ev, _, nextErr := cursor.Next(context.Background())
		if errors.Is(nextErr, io.EOF) {
			return result
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		result = append(result, ev)
	}
}

func TestRestoreInstallsRequiredCheckpointPolicyBeforeFirstWork(t *testing.T) {
	store, _ := lifecycleStore(t)
	workspace, err := workspacestore.Open(memstore.New().Blobs)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := loop.Define(loop.WithName("agent"), loop.WithInference(lifecycleLLM{}, validModel("model")))
	if err != nil {
		t.Fatal(err)
	}
	baseDir := t.TempDir()
	r, err := Define(
		WithLoops(definition), WithPrimers("agent"), WithSessionStore(store),
		WithSessionWorkspaces(workspace, baseDir),
		WithSnapshots(SnapshotPolicy{Trigger: SnapshotOnTurnDone, Priority: SnapshotRequired, Timeout: 5 * time.Second}),
	)
	if err != nil {
		t.Fatal(err)
	}
	original, err := r.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id := original.SessionID()
	if err := os.MkdirAll(filepath.Join(baseDir, id.String()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := original.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, err := r.RestoreSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := restored.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if _, err := restored.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "first restored turn"}}); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	seenTurn, seenCheckpoint := false, false
	for !seenCheckpoint {
		select {
		case delivery := <-sub.Events():
			switch delivery.Event.(type) {
			case event.TurnDone:
				seenTurn = true
			case event.WorkspaceCheckpointed:
				if !seenTurn {
					t.Fatal("checkpoint preceded restored turn terminal")
				}
				seenCheckpoint = true
			}
		case <-waitCtx.Done():
			t.Fatal(waitCtx.Err())
		}
	}
	if err := restored.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := replayRigEvents(t, store, id)
	index := map[string]int{"restore": -1, "turn": -1, "checkpoint": -1, "idle": -1}
	for i, ev := range events {
		switch ev.(type) {
		case event.RestoreDone:
			index["restore"] = i
		case event.TurnDone:
			if i > index["restore"] && index["turn"] < 0 {
				index["turn"] = i
			}
		case event.WorkspaceCheckpointed:
			if i > index["turn"] && index["checkpoint"] < 0 {
				index["checkpoint"] = i
			}
		case event.LoopIdle:
			if i > index["turn"] && index["idle"] < 0 {
				index["idle"] = i
			}
		}
	}
	if !(index["restore"] >= 0 && index["turn"] > index["restore"] && index["checkpoint"] > index["turn"] && index["idle"] > index["checkpoint"]) {
		t.Fatalf("restore/turn/checkpoint/idle indices = %v", index)
	}
}

func TestRestoreDoneAppendFailureAbortsBeforeReverseLeaseRelease(t *testing.T) {
	backend := memstore.New()
	ledger := &lifecycleNthFailLedger{Ledger: backend.Ledger, failAt: 1 << 30, err: errors.New("restore done")}
	var orderMu sync.Mutex
	var releases []string
	recordRelease := func(label string) func() {
		return func() { orderMu.Lock(); releases = append(releases, label); orderMu.Unlock() }
	}
	sessionLeaser := &lifecycleRecordingLeaser{inner: backend.Leaser, onRelease: recordRelease("session"), rejectCanceled: true}
	composite, err := storage.NewComposite(ledger, sessionLeaser, backend.KV, backend.Blobs)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sessionstore.Open(composite)
	if err != nil {
		t.Fatal(err)
	}
	workspaceBackend := memstore.New()
	rootLeaser := &lifecycleRecordingLeaser{inner: workspaceBackend.Leaser, onRelease: recordRelease("root"), rejectCanceled: true}
	workspace, err := workspacestore.Open(workspaceBackend.Blobs)
	if err != nil {
		t.Fatal(err)
	}
	r := lifecycleRig(t, store,
		WithExclusiveWorkspace(workspace, t.TempDir(), rootLeaser),
		WithSnapshots(SnapshotPolicy{Trigger: SnapshotManual}),
	)
	original, err := r.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id := original.SessionID()
	if err := original.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	orderMu.Lock()
	releases = nil
	orderMu.Unlock()
	// Restore appends an opening fence, RestoreStarted, then RestoreDone for this idle
	// stream. Fail the third new append, after the restored loop/controller/hub exist.
	ledger.failAt = ledger.calls.Load() + 3
	restored, restoreErr := r.RestoreSession(context.Background(), id)
	if restored != nil {
		_ = restored.Shutdown(context.Background())
		t.Fatal("RestoreDone append failure returned partial session")
	}
	var typed *session.RestoreError
	if !errors.As(restoreErr, &typed) || typed.Kind != session.RestoreAppendFailed {
		t.Fatalf("restore error = %T %v, want RestoreAppendFailed", restoreErr, restoreErr)
	}
	orderMu.Lock()
	if len(releases) != 2 || releases[0] != "root" || releases[1] != "session" {
		t.Fatalf("release order = %v, want [root session]", releases)
	}
	orderMu.Unlock()
	sessionLeaser.mu.Lock()
	sessionCanceled := sessionLeaser.canceledRelease
	sessionLeaser.mu.Unlock()
	rootLeaser.mu.Lock()
	rootCanceled := rootLeaser.canceledRelease
	rootLeaser.mu.Unlock()
	if sessionCanceled || rootCanceled {
		t.Fatal("restore cleanup attempted lease release with canceled context")
	}
	events := replayRigEvents(t, store, id)
	stopped := 0
	errored := 0
	for _, ev := range events {
		switch ev.(type) {
		case event.SessionStopped:
			stopped++
		case event.RestoreErrored:
			errored++
		}
	}
	if stopped != 1 || errored != 1 {
		t.Fatalf("SessionStopped=%d RestoreErrored=%d, want original stop only and one restore error", stopped, errored)
	}
}

func TestRestoreAcceptsPreFrozenFullFingerprintFixture(t *testing.T) {
	store, _ := lifecycleStore(t)
	read := tool.NewDefinition("Read", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{fpTool{name: "Read"}}, nil
	})
	definition, err := loop.Define(
		loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("model")),
		loop.WithSystem("system"), loop.WithTools(read),
	)
	if err != nil {
		t.Fatal(err)
	}
	oldProvider := sessionruntime.FingerprintProvider(func(bound loop.BoundDefinition) event.ConfigFingerprint {
		return fingerprintWithTopology(bound, ConfigFingerprintFields{}, []loop.Definition{definition}, []string{"agent"}, "agent")
	})
	legacy, err := sessionruntime.NewTopologyLifecycle(sessionruntime.Topology{
		Definitions: []loop.Definition{definition}, Primers: []identity.AgentName{definition.Name()}, ActivePrimer: definition.Name(),
	}, store, sessionruntime.WithLifecycleFingerprintProvider(oldProvider))
	if err != nil {
		t.Fatal(err)
	}
	original, err := legacy.NewSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	id := original.SessionID()
	if err := original.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var persisted event.ConfigFingerprint
	for _, ev := range replayRigEvents(t, store, id) {
		if started, ok := ev.(event.SessionStarted); ok {
			persisted = started.Config
			break
		}
	}
	if persisted.ModelID == "" || persisted.SystemPromptRev == "" || persisted.ToolPolicyRev == "" || persisted.TopologyRev == "" {
		t.Fatalf("persisted full fingerprint = %+v", persisted)
	}

	current, err := Define(WithLoops(definition), WithPrimers("agent"), WithSessionStore(store))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := current.RestoreSession(context.Background(), id)
	if err != nil {
		t.Fatalf("restore pre-frozen full fingerprint fixture: %v", err)
	}
	if err := restored.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreAcceptsLegacyBoundFingerprintForFilesAndInjectedSubagent(t *testing.T) {
	store, _ := lifecycleStore(t)
	workspace, err := workspacestore.Open(memstore.New().Blobs)
	if err != nil {
		t.Fatal(err)
	}
	root, err := canonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	placement := sessionruntime.WorkspacePlacement{Mode: sessionruntime.PlacementShared, Store: workspace, Root: root}
	fields := ConfigFingerprintFields{WorkspaceRoot: placementFingerprint(placement, root)}

	var permissionBinds atomic.Int32
	primer, err := loop.Define(
		loop.WithName("primer"), loop.WithInference(&stubLLM{}, validModel("model")),
		loop.WithTools(harnesstools.Files(lifecycleReadGuard{})),
		loop.WithDelegates("delegate"),
		loop.WithPolicyRevision("legacy-files-delegate"),
		loop.WithPermissionFactory(func(context.Context, tool.Bindings) (loop.PermissionGate, error) {
			permissionBinds.Add(1)
			return lifecyclePermissionGate{}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	delegate, err := loop.Define(loop.WithName("delegate"), loop.WithInference(&stubLLM{}, validModel("delegate-model")))
	if err != nil {
		t.Fatal(err)
	}
	definitions := []loop.Definition{primer, delegate}
	primers := []string{"primer"}
	frozen := frozenFingerprint(fields, definitions, primers, "primer")
	if permissionBinds.Load() != 0 {
		t.Fatalf("frozen fingerprint invoked permission factory %d times, want 0", permissionBinds.Load())
	}

	var legacyBound event.ConfigFingerprint
	legacyProvider := sessionruntime.FingerprintProvider(func(bound loop.BoundDefinition) event.ConfigFingerprint {
		legacyBound = fingerprintWithTopology(bound, fields, definitions, primers, "primer")
		return legacyBound
	})
	legacy, err := sessionruntime.NewTopologyLifecycle(
		sessionruntime.Topology{Definitions: definitions, Primers: []identity.AgentName{"primer"}, ActivePrimer: "primer"},
		store,
		sessionruntime.WithLifecycleFingerprintProvider(legacyProvider),
		sessionruntime.WithLifecyclePlacement(placement),
		sessionruntime.WithLifecycleSnapshotPolicy(sessionruntime.SnapshotPolicy{Trigger: sessionruntime.SnapshotManual}),
	)
	if err != nil {
		t.Fatal(err)
	}
	original, err := legacy.NewSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	id := original.SessionID()
	if err := original.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !legacyBound.Equal(frozen) {
		t.Fatalf("legacy bound fingerprint = %+v, frozen = %+v", legacyBound, frozen)
	}

	current, err := Define(
		WithLoops(definitions...), WithPrimers("primer"), WithSessionStore(store),
		WithSharedWorkspace(workspace, root), WithSnapshots(SnapshotPolicy{Trigger: SnapshotManual}),
	)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := current.RestoreSession(context.Background(), id)
	if err != nil {
		t.Fatalf("restore legacy Files/delegate fixture: %v", err)
	}
	if err := restored.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreReconstructsPrimerAndDelegateInferenceBeforeAdmission(t *testing.T) {
	store, _ := lifecycleStore(t)
	define := func(name string, delegates ...identity.AgentName) loop.Definition {
		definition, err := loop.Define(
			loop.WithName(identity.AgentName(name)), loop.WithInference(&stubLLM{}, validModel(name+"-base")),
			loop.WithModes(
				loop.Mode{Name: "plan", Model: validModel(name + "-plan"), Effort: inference.EffortLow},
				loop.Mode{Name: "build", Model: validModel(name + "-build"), Effort: inference.EffortMedium},
			),
			loop.WithInitialMode("plan"),
			loop.WithDelegates(delegates...),
		)
		if err != nil {
			t.Fatal(err)
		}
		return definition
	}
	primer := define("primer", "delegate")
	delegate := define("delegate")
	r, err := Define(WithLoops(primer, delegate), WithPrimers("primer"), WithSessionStore(store))
	if err != nil {
		t.Fatal(err)
	}
	original, err := r.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	primerID := original.ActiveLoop().ID()
	spawner := original.(interface {
		NewLoop(loop.Provenance, loop.Definition) (uuid.UUID, error)
	})
	delegateID, err := spawner.NewLoop(loop.Provenance{LoopID: primerID}, delegate)
	if err != nil {
		t.Fatal(err)
	}
	primerController, ok := original.LoopController(primerID)
	if !ok {
		t.Fatal("missing primer controller")
	}
	delegateController, ok := original.LoopController(delegateID)
	if !ok {
		t.Fatal("missing delegate controller")
	}
	if err := primerController.SetMode(context.Background(), "build"); err != nil {
		t.Fatal(err)
	}
	if err := primerController.Change(context.Background(), loop.ChangeModel(validModel("primer-routed")), loop.ChangeEffort(inference.EffortHigh)); err != nil {
		t.Fatal(err)
	}
	if err := delegateController.SetMode(context.Background(), "build"); err != nil {
		t.Fatal(err)
	}
	if err := delegateController.Change(context.Background(), loop.ChangeModel(validModel("delegate-routed")), loop.ChangeEffort(inference.EffortMax)); err != nil {
		t.Fatal(err)
	}
	if err := original.SetActiveLoop(context.Background(), delegateID); err != nil {
		t.Fatal(err)
	}
	id := original.SessionID()
	if err := original.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	restored, err := r.RestoreSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Shutdown(context.Background()) }()
	assertInference := func(label string, id uuid.UUID, wantModel string, wantEffort inference.Effort) {
		t.Helper()
		handle, ok := restored.Loop(id)
		if !ok {
			t.Fatalf("%s loop missing", label)
		}
		model := handle.Model()
		if handle.Mode() != "build" || model.Name != wantModel || model.Sampling.Effort != wantEffort {
			t.Fatalf("%s restored mode/model/effort = %q/%q/%q", label, handle.Mode(), model.Name, model.Sampling.Effort)
		}
	}
	assertInference("primer", primerID, "primer-routed", inference.EffortHigh)
	assertInference("delegate", delegateID, "delegate-routed", inference.EffortMax)
	if restored.ActiveLoop().ID() != delegateID {
		t.Fatalf("active loop = %v, want delegate %v", restored.ActiveLoop().ID(), delegateID)
	}
}
