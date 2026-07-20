package sessionruntime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// fakeSessionJournal is a no-op journal.SessionJournal whose Append always succeeds. It
// lets a non-integration test build the restored session's journal-backed hub without an
// embedded JetStream server: the restore branch under test is the Engine switch + sid
// recovery, NOT the durable tap (the round-trip integration tests cover the real tap).
type fakeSessionJournal struct{}

func (fakeSessionJournal) Append(context.Context, journal.JournalRecord) (uint64, error) {
	return 1, nil
}

func TestFindForeignSIDPrefersLoopStarted(t *testing.T) {
	t.Parallel()
	got := findForeignSID([]event.Event{
		event.LoopStarted{ForeignSID: "from-loop-started"},
		event.ForeignSessionBound{ForeignSID: "from-bound"},
	})
	if got != "from-loop-started" {
		t.Fatalf("sid = %q, want LoopStarted sid", got)
	}
}

func TestFindForeignSIDFallsBackToForeignSessionBound(t *testing.T) {
	t.Parallel()
	got := findForeignSID([]event.Event{
		event.LoopStarted{},
		event.ForeignSessionBound{ForeignSID: "from-bound"},
	})
	if got != "from-bound" {
		t.Fatalf("sid = %q, want bound sid", got)
	}
}

func TestCodexForeignRestoreRecoversSIDFromForeignSessionBound(t *testing.T) {
	t.Parallel()

	foldedEvents := []event.Event{
		event.LoopStarted{},
		event.TurnStarted{Message: foldUserMsg("hello")},
		event.ForeignSessionBound{ForeignSID: "codex-thread-restored-1"},
		foldStepGroup(aiMessage("hi")),
		event.TurnDone{Message: aiMessage("hi")},
	}
	folded := foldLoop(foldedEvents)
	foreignSID := findForeignSID(foldedEvents)

	builder := &fakeForeignBuilder{}
	fb := newFakeBackend()
	fb.msgs = folded.Msgs
	fb.turnIndex = folded.TurnIndex
	builder.backend = fb

	sessionID := mustUUID()
	rootLoopID := mustUUID()
	c := bindCfg(engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}, loop.EngineForeignCodex, "be helpful"), sessionID, rootLoopID)
	fac := event.NewFactory(uuid.New, time.Now)
	restoreCtx, restoreCancel := context.WithCancel(context.Background())
	t.Cleanup(restoreCancel)

	s, err := buildRestoredSession(restoreCtx, restoreCancel, c, tool.Bindings{SessionID: sessionID, LoopID: rootLoopID}, sessionID, rootLoopID,
		foreignSID, 0, folded, restoredInference{}, nil, fakeSessionJournal{}, fac, uuid.New, time.Now,
		WithForeignBuilders(builder.build, builder.buildRestored))
	if err != nil {
		t.Fatalf("buildRestoredSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	builder.mu.Lock()
	calls := builder.restoreCall
	seed := builder.restoreSeed
	calledSID := builder.calledSID
	calledLID := builder.calledLID
	builder.mu.Unlock()
	if calls != 1 {
		t.Errorf("restored Builder invoked %d times, want exactly 1", calls)
	}
	if seed.ForeignSID != "codex-thread-restored-1" {
		t.Errorf("restored seed.ForeignSID = %q, want %q", seed.ForeignSID, "codex-thread-restored-1")
	}
	if seed.TurnIndex != folded.TurnIndex {
		t.Errorf("restored seed.TurnIndex = %d, want %d", seed.TurnIndex, folded.TurnIndex)
	}
	if !reflect.DeepEqual(seed.Msgs, folded.Msgs) {
		t.Errorf("restored seed.Msgs =\n  %#v\nwant\n  %#v", seed.Msgs, folded.Msgs)
	}
	if calledSID != sessionID {
		t.Errorf("restored Builder sessionID = %v, want %v", calledSID, sessionID)
	}
	if calledLID != rootLoopID {
		t.Errorf("restored Builder loopID = %v, want %v", calledLID, rootLoopID)
	}
}

func TestCodexForeignRestoreFailsClosedWithoutSIDSource(t *testing.T) {
	t.Parallel()

	foldedEvents := []event.Event{
		event.LoopStarted{},
		event.TurnStarted{Message: foldUserMsg("hello")},
		foldStepGroup(aiMessage("hi")),
		event.TurnDone{Message: aiMessage("hi")},
	}
	folded := foldLoop(foldedEvents)
	foreignSID := findForeignSID(foldedEvents)
	if foreignSID != "" {
		t.Fatalf("findForeignSID = %q, want empty with no Codex SID source", foreignSID)
	}

	builder := &fakeForeignBuilder{}
	sessionID := mustUUID()
	rootLoopID := mustUUID()
	c := bindCfg(engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}, loop.EngineForeignCodex, "be helpful"), sessionID, rootLoopID)
	restoreCtx, restoreCancel := context.WithCancel(context.Background())
	t.Cleanup(restoreCancel)

	s, err := buildRestoredSession(restoreCtx, restoreCancel, c, tool.Bindings{SessionID: sessionID, LoopID: rootLoopID}, sessionID, rootLoopID,
		foreignSID, 0, folded, restoredInference{}, nil, fakeSessionJournal{}, event.NewFactory(uuid.New, time.Now), uuid.New, time.Now,
		WithForeignBuilders(builder.build, builder.buildRestored))
	if s != nil {
		t.Fatalf("buildRestoredSession returned a non-nil Session on a fail-closed Codex restore")
	}
	var re *RestoreError
	if !errors.As(err, &re) || re.Kind != RestoreForeignSIDMissing {
		t.Fatalf("err = %v, want *RestoreError{%v}", err, RestoreForeignSIDMissing)
	}

	builder.mu.Lock()
	calls := builder.restoreCall
	builder.mu.Unlock()
	if calls != 0 {
		t.Errorf("restored Builder invoked %d times, want 0 when Codex SID is missing", calls)
	}
}

// TestForeignRestore covers buildRestoredSession's Engine switch: a foreign definition
// reconstructs the primary loop through the wired RestoredBuilder, carrying the recovered
// foreign sid into the seed; an empty recovered sid (or a missing restored builder) fails
// closed; a native definition restores through loopruntime.NewRestored unchanged.
func TestForeignRestore(t *testing.T) {
	t.Parallel()

	// A small clean primary history the fold reconstructs: user + one step + TurnDone →
	// Msgs = [user "hello", ai "hi"], TurnIndex = 1, no open turn.
	foldedEvents := []event.Event{
		event.TurnStarted{Message: foldUserMsg("hello")},
		foldStepGroup(aiMessage("hi")),
		event.TurnDone{Message: aiMessage("hi")},
	}

	const recoveredSID = "recovered-foreign-sid-abc"

	tests := []struct {
		name        string
		engine      loop.Engine
		foreignSID  string
		wireBuilder bool
		wantErr     bool
		wantKind    RestoreErrorKind
	}{
		{
			name:        "foreign restore recovers the sid into the restored builder",
			engine:      loop.EngineForeignClaude,
			foreignSID:  recoveredSID,
			wireBuilder: true,
		},
		{
			name:        "foreign restore with empty ForeignSID fails closed",
			engine:      loop.EngineForeignClaude,
			foreignSID:  "",
			wireBuilder: true,
			wantErr:     true,
			wantKind:    RestoreForeignSIDMissing,
		},
		{
			name:        "foreign restore without a restored builder fails closed",
			engine:      loop.EngineForeignClaude,
			foreignSID:  recoveredSID,
			wireBuilder: false,
			wantErr:     true,
			wantKind:    RestoreForeignBuilderMissing,
		},
		{
			name:        "codex foreign restore without a restored builder fails closed",
			engine:      loop.EngineForeignCodex,
			foreignSID:  recoveredSID,
			wireBuilder: false,
			wantErr:     true,
			wantKind:    RestoreForeignBuilderMissing,
		},
		{
			name:        "native restore is unaffected",
			engine:      loop.EngineNative,
			foreignSID:  "", // irrelevant for native — the branch never reads it
			wireBuilder: false,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			folded := foldLoop(foldedEvents)

			builder := &fakeForeignBuilder{}
			// Only the happy foreign path actually invokes the restored builder, so only it
			// needs a (goroutine-backed) fake backend; the fail-closed rows return before the
			// builder is ever called.
			if tt.wireBuilder && !tt.wantErr {
				fb := newFakeBackend()
				fb.msgs = folded.Msgs
				fb.turnIndex = folded.TurnIndex
				builder.backend = fb
			}

			sessionID := mustUUID()
			rootLoopID := mustUUID()
			c := bindCfg(engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}, tt.engine, "be helpful"), sessionID, rootLoopID)
			fac := event.NewFactory(uuid.New, time.Now)
			restoreCtx, restoreCancel := context.WithCancel(context.Background())
			t.Cleanup(restoreCancel)

			var opts []Option
			if tt.wireBuilder {
				opts = append(opts, WithForeignBuilders(builder.build, builder.buildRestored))
			}

			s, err := buildRestoredSession(restoreCtx, restoreCancel, c, tool.Bindings{SessionID: sessionID, LoopID: rootLoopID}, sessionID, rootLoopID,
				tt.foreignSID, 0, folded, restoredInference{}, nil, fakeSessionJournal{}, fac, uuid.New, time.Now, opts...)

			if tt.wantErr {
				if s != nil {
					t.Fatalf("buildRestoredSession returned a non-nil Session on a fail-closed restore")
				}
				var re *RestoreError
				if !errors.As(err, &re) || re.Kind != tt.wantKind {
					t.Fatalf("err = %v, want *RestoreError{%v}", err, tt.wantKind)
				}
				return
			}

			if err != nil {
				t.Fatalf("buildRestoredSession: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			// Identity is stable: the recovered session keeps its sessionID + primary loop id.
			if s.SessionID() != sessionID {
				t.Errorf("restored SessionID = %v, want %v", s.SessionID(), sessionID)
			}
			if s.ActiveLoopID() != rootLoopID {
				t.Errorf("restored rootLoopID = %v, want %v", s.ActiveLoopID(), rootLoopID)
			}

			// The primary loop comes up registered (idle) and its Snapshot returns the folded
			// committed thread at the recovered turn index.
			l, ok := s.loopFor(rootLoopID)
			if !ok {
				t.Fatal("restored session has no primary loop registered")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			msgs, idx, err := l.Snapshot(ctx)
			if err != nil {
				t.Fatalf("restored Snapshot: %v", err)
			}
			if !reflect.DeepEqual(msgs, folded.Msgs) {
				t.Errorf("restored msgs =\n  %#v\nwant\n  %#v", msgs, folded.Msgs)
			}
			if idx != folded.TurnIndex {
				t.Errorf("restored turnIndex = %d, want %d", idx, folded.TurnIndex)
			}

			if tt.engine == loop.EngineForeignClaude {
				// The RESTORED builder was invoked once with the recovered seed: the journal's
				// ForeignSID plus the folded committed state — the heart of F3.
				builder.mu.Lock()
				calls := builder.restoreCall
				seed := builder.restoreSeed
				calledSID := builder.calledSID
				calledLID := builder.calledLID
				builder.mu.Unlock()
				if calls != 1 {
					t.Errorf("restored Builder invoked %d times, want exactly 1", calls)
				}
				if seed.ForeignSID != tt.foreignSID {
					t.Errorf("restored seed.ForeignSID = %q, want %q", seed.ForeignSID, tt.foreignSID)
				}
				if seed.TurnIndex != folded.TurnIndex {
					t.Errorf("restored seed.TurnIndex = %d, want %d", seed.TurnIndex, folded.TurnIndex)
				}
				if !reflect.DeepEqual(seed.Msgs, folded.Msgs) {
					t.Errorf("restored seed.Msgs =\n  %#v\nwant\n  %#v", seed.Msgs, folded.Msgs)
				}
				if calledSID != sessionID {
					t.Errorf("restored Builder sessionID = %v, want %v", calledSID, sessionID)
				}
				if calledLID != rootLoopID {
					t.Errorf("restored Builder loopID = %v, want %v", calledLID, rootLoopID)
				}
			}
		})
	}
}
