package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// fixedForeignSID is the deterministic session id the fake foreign Builder returns, so
// a test can assert it is stamped verbatim onto the published LoopStarted.
const fixedForeignSID = "fixed-foreign-sid-0123456789"

// fakeBackend is a cooperative loop.Backend stand-in for a foreign loop: it serves its
// command channel from a goroutine, acks Shutdown/Interrupt, and closes Done on
// Shutdown so the session's Shutdown drains cleanly in test cleanup. Snapshot returns
// the seeded committed state so a restore test can assert the recovered thread.
type fakeBackend struct {
	cmds      chan command.Command
	done      chan struct{}
	msgs      content.AgenticMessages
	turnIndex event.TurnIndex
}

func newFakeBackend() *fakeBackend {
	fb := &fakeBackend{cmds: make(chan command.Command), done: make(chan struct{})}
	go fb.serve()
	return fb
}

func (f *fakeBackend) serve() {
	for cmd := range f.cmds {
		switch c := cmd.(type) {
		case command.Shutdown:
			c.Ack <- nil
			close(f.done)
			return
		case command.Interrupt:
			c.Ack <- false
		default:
			// Drop any other command while idle (no turn engine in the fake).
		}
	}
}

func (f *fakeBackend) CommandSink() chan<- command.Command { return f.cmds }
func (f *fakeBackend) DoneChan() <-chan struct{}           { return f.done }
func (f *fakeBackend) Snapshot(context.Context) (content.AgenticMessages, event.TurnIndex, error) {
	return f.msgs, f.turnIndex, nil
}

// fakeForeignBuilder records the wiring the session passes into the foreign Builder /
// RestoredBuilder seams and returns a pre-built fake backend (and, for the live
// builder, a fixed sid). A test asserts both the recorded call args and the returned
// values' downstream effects.
type fakeForeignBuilder struct {
	mu sync.Mutex

	sid     string       // returned by the live build seam
	backend loop.Backend // returned by both seams
	err     error        // forced construction failure, both seams

	calls       int
	calledSID   uuid.UUID
	calledLID   uuid.UUID
	restoreSeed foreign.RestoredForeign
	restoreCall int
	calledBound loop.BoundDefinition
}

func (b *fakeForeignBuilder) build(_ context.Context, sessionID, loopID uuid.UUID,
	_ loop.Provenance, _ foreign.EventPublisher, bound loop.BoundDefinition,
	_ func() (uuid.UUID, error), _ *event.Factory) (loop.Backend, string, error) {
	b.mu.Lock()
	b.calls++
	b.calledSID = sessionID
	b.calledLID = loopID
	b.calledBound = bound
	b.mu.Unlock()
	if b.err != nil {
		return nil, "", b.err
	}
	return b.backend, b.sid, nil
}

func TestForeignDelegateBuilderReceivesSelectedEffectiveMode(t *testing.T) {
	t.Parallel()
	rec := &recordingEventAppender{}
	builder := &fakeForeignBuilder{sid: fixedForeignSID, backend: newFakeBackend()}
	parent := delegateParent(loop.DelegationManaged, "child")
	child := mustDefine(
		loop.WithName("child"),
		loop.WithInference(&stubLLM{}, validModel("base-model")),
		loop.WithEngine(loop.EngineForeignClaude),
		loop.WithModes(
			loop.Mode{Name: "build", Model: validModel("build-model"), Instructions: "build instructions"},
			loop.Mode{Name: "review", Model: validModel("review-model"), Instructions: "review instructions"},
		),
		loop.WithInitialMode("build"),
	)
	s := newDelegationSession(t, parent, []Option{WithEventAppender(rec), WithForeignBuilders(builder.build, builder.buildRestored)}, child)
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	queued, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Mode: "review", Message: "inspect", Wait: false})
	if err != nil {
		t.Fatal(err)
	}
	if queued.DelegateID.IsZero() {
		t.Fatal("missing child id")
	}
	builder.mu.Lock()
	bound := builder.calledBound
	builder.mu.Unlock()
	if bound == nil {
		t.Fatal("builder received nil bound definition")
	}
	if bound.InitialMode() != "review" || bound.Model().Name != "review-model" || bound.Instructions() != "review instructions" {
		t.Fatalf("builder bound = mode %q model %q instructions %q, want selected review mode", bound.InitialMode(), bound.Model().Name, bound.Instructions())
	}
	var startedMode string
	for _, ev := range rec.snapshot() {
		switch e := ev.(type) {
		case event.LoopStarted:
			if e.LoopID == queued.DelegateID {
				startedMode = e.InitialMode
			}
		case event.LoopModeChanged:
			if e.LoopID == queued.DelegateID {
				t.Fatal("selected foreign start emitted synthetic LoopModeChanged")
			}
		}
	}
	if startedMode != "review" {
		t.Fatalf("LoopStarted.InitialMode = %q, want review", startedMode)
	}
}

func (b *fakeForeignBuilder) buildRestored(_ context.Context, sessionID, loopID uuid.UUID,
	_ loop.Provenance, _ foreign.EventPublisher, _ loop.BoundDefinition,
	_ func() (uuid.UUID, error), _ *event.Factory, seed foreign.RestoredForeign) (loop.Backend, error) {
	b.mu.Lock()
	b.restoreCall++
	b.calledSID = sessionID
	b.calledLID = loopID
	b.restoreSeed = seed
	b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	return b.backend, nil
}

// firstLoopStarted returns the first LoopStarted the recording durable tap captured.
// The session publishes the primary loop's LoopStarted through the hub at construction
// (before any subscriber can attach — the hub has no replay), so recording the REQUIRED
// durable tap (recordingEventAppender, shared from composition_options_test.go) is how a
// test observes that LoopStarted, including its ForeignSID.
func firstLoopStarted(r *recordingEventAppender) (event.LoopStarted, bool) {
	for _, ev := range r.snapshot() {
		if ls, ok := ev.(event.LoopStarted); ok {
			return ls, true
		}
	}
	return event.LoopStarted{}, false
}

// TestForeignNewLoop covers the newLoop Engine switch: a foreign definition routes
// construction through the wired Builder and stamps its sid onto the published
// LoopStarted; a foreign definition with NO wired Builder fails closed; a native definition is
// unaffected (built by loopruntime.New, LoopStarted.ForeignSID empty).
func TestForeignNewLoop(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		engine      loop.Engine
		wireBuilder bool
		wantErr     bool
		wantKind    SessionErrorKind
		wantSID     string
	}{
		{
			name:        "foreign primary stamps ForeignSID",
			engine:      loop.EngineForeignClaude,
			wireBuilder: true,
			wantSID:     fixedForeignSID,
		},
		{
			name:        "foreign engine without a builder fails closed",
			engine:      loop.EngineForeignClaude,
			wireBuilder: false,
			wantErr:     true,
			wantKind:    SessionForeignBuilderMissing,
		},
		{
			name:     "codex foreign engine without a builder fails closed",
			engine:   loop.EngineForeignCodex,
			wantErr:  true,
			wantKind: SessionForeignBuilderMissing,
		},
		{
			name:        "native engine is unaffected (empty ForeignSID)",
			engine:      loop.EngineNative,
			wireBuilder: false,
			wantSID:     "",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &recordingEventAppender{}
			builder := &fakeForeignBuilder{sid: fixedForeignSID, backend: newFakeBackend()}

			c := engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}, tt.engine, "x")

			opts := []Option{WithFingerprintProvider(testFingerprintProvider), WithEventAppender(rec)}
			if tt.wireBuilder {
				opts = append(opts, WithForeignBuilders(builder.build, builder.buildRestored))
			}

			s, err := newSession(context.Background(), c, uuid.New, time.Now, opts...)

			if tt.wantErr {
				var se *SessionError
				if !errors.As(err, &se) || se.Kind != tt.wantKind {
					t.Fatalf("newSession err = %v, want *SessionError{%v}", err, tt.wantKind)
				}
				if s != nil {
					t.Fatalf("newSession returned a non-nil Session on a fail-closed foreign engine")
				}
				return
			}

			if err != nil {
				t.Fatalf("newSession: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			ls, ok := firstLoopStarted(rec)
			if !ok {
				t.Fatal("no LoopStarted captured on the durable tap")
			}
			if ls.ForeignSID != tt.wantSID {
				t.Errorf("LoopStarted.ForeignSID = %q, want %q", ls.ForeignSID, tt.wantSID)
			}

			if tt.wireBuilder {
				builder.mu.Lock()
				calls, calledSID, calledLID := builder.calls, builder.calledSID, builder.calledLID
				builder.mu.Unlock()
				if calls != 1 {
					t.Errorf("foreign Builder invoked %d times, want exactly 1", calls)
				}
				if calledSID != s.SessionID() {
					t.Errorf("Builder sessionID = %v, want %v", calledSID, s.SessionID())
				}
				if calledLID != s.activeLoopID {
					t.Errorf("Builder loopID = %v, want rootLoopID %v", calledLID, s.activeLoopID)
				}
			}
		})
	}
}
