package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/sessionstore"
)

// TestRunnerCompile proves Compile binds cfg+store into a reusable Runner and fails
// closed with a typed *NilStoreError when handed a nil store (the durable backend is a
// required dependency).
func TestRunnerCompile(t *testing.T) {
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
			r, err := Compile(cfg(&stubLLM{}), store)
			if tt.wantErr {
				var nse *NilStoreError
				if !errors.As(err, &nse) {
					t.Fatalf("Compile err = %v, want *NilStoreError", err)
				}
				if r != nil {
					t.Fatalf("Compile returned a non-nil Runner on error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if r == nil {
				t.Fatal("Compile returned a nil Runner without error")
			}
		})
	}
}

// TestRunnerRun proves Run mints a fresh, non-zero session id, brings up a live session
// whose SubscribeEvents works, and mints DISTINCT ids across successive runs of the same
// compiled Runner.
func TestRunnerRun(t *testing.T) {
	t.Parallel()

	t.Run("mints non-zero id and a live session", func(t *testing.T) {
		t.Parallel()
		r, err := Compile(cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}), newRestoreStore(t))
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		sid, s, err := r.Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
		if sid.IsZero() {
			t.Fatal("Run returned a zero session id")
		}
		if s == nil {
			t.Fatal("Run returned a nil session")
		}
		if s.SessionID != sid {
			t.Errorf("session.SessionID = %v, want the returned id %v", s.SessionID, sid)
		}
		sub, err := s.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
		if err != nil {
			t.Fatalf("SubscribeEvents: %v", err)
		}
		_ = sub.Close()
	})

	t.Run("distinct ids across runs", func(t *testing.T) {
		t.Parallel()
		r, err := Compile(cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}), newRestoreStore(t))
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		sid1, s1, err := r.Run(context.Background())
		if err != nil {
			t.Fatalf("Run #1: %v", err)
		}
		t.Cleanup(func() { _ = s1.Shutdown(context.Background()) })
		sid2, s2, err := r.Run(context.Background())
		if err != nil {
			t.Fatalf("Run #2: %v", err)
		}
		t.Cleanup(func() { _ = s2.Shutdown(context.Background()) })
		if sid1 == sid2 {
			t.Fatalf("two runs minted the same session id %v, want distinct", sid1)
		}
	})

	t.Run("ceiling factory minted once per run", func(t *testing.T) {
		t.Parallel()
		var mints int64
		factory := func() *ceiling.State {
			atomic.AddInt64(&mints, 1)
			return ceiling.New()
		}
		r, err := Compile(cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}), newRestoreStore(t),
			WithCompileCeilingFactory(factory))
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		_, s1, err := r.Run(context.Background())
		if err != nil {
			t.Fatalf("Run #1: %v", err)
		}
		t.Cleanup(func() { _ = s1.Shutdown(context.Background()) })
		_, s2, err := r.Run(context.Background())
		if err != nil {
			t.Fatalf("Run #2: %v", err)
		}
		t.Cleanup(func() { _ = s2.Shutdown(context.Background()) })
		if got := atomic.LoadInt64(&mints); got != 2 {
			t.Errorf("ceiling factory minted %d times, want 2 (one per Run)", got)
		}
	})
}

// TestRunnerRestore proves Restore rebuilds a live session from its journal (happy path),
// surfaces a typed discovery error for an unknown/never-run session id (no panic), and
// enforces the config fingerprint captured at Compile — refusing a mismatch unless
// WithCompileAllowConfigMismatch was compiled in.
func TestRunnerRestore(t *testing.T) {
	t.Parallel()

	t.Run("happy path rebuilds a live session", func(t *testing.T) {
		t.Parallel()
		store := newRestoreStore(t)
		r, err := Compile(restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful"), store)
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		sid, s, err := r.Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		// Append an event through the live session, then let the turn commit and go idle.
		if _, err := s.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "hi"}}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := s.WaitIdle(waitCtx); err != nil {
			t.Fatalf("WaitIdle: %v", err)
		}
		waitCancel()
		// Clean shutdown releases the single-writer lease so Restore can re-acquire.
		if err := s.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}

		restored, err := r.Restore(context.Background(), sid)
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
		if restored.SessionID != sid {
			t.Errorf("restored SessionID = %v, want %v", restored.SessionID, sid)
		}
		if restored.PrimaryLoopID().IsZero() {
			t.Error("restored session has a zero primary loop id")
		}
	})

	t.Run("unknown session id surfaces a typed discovery error", func(t *testing.T) {
		t.Parallel()
		r, err := Compile(restoreCfg(&stubLLM{}, "model-x", "be helpful"), newRestoreStore(t))
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		// No prior Run: the session has no journal history at all.
		s, err := r.Restore(context.Background(), mustSessionID(t))
		if s != nil {
			t.Fatalf("Restore returned a non-nil session for an unknown id")
		}
		var de *RestoreDiscoveryError
		if !errors.As(err, &de) {
			t.Fatalf("Restore err = %v, want *RestoreDiscoveryError", err)
		}
	})

	t.Run("config fingerprint mismatch rejects unless allowed", func(t *testing.T) {
		t.Parallel()
		store := newRestoreStore(t)
		// Original run under model-x.
		orig, err := Compile(restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful"), store)
		if err != nil {
			t.Fatalf("Compile orig: %v", err)
		}
		sid, s, err := orig.Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if err := s.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}

		// A Runner compiled under a DIFFERENT model fingerprint rejects the restore.
		mismatch, err := Compile(restoreCfg(&stubLLM{}, "model-DIFFERENT", "be helpful"), store)
		if err != nil {
			t.Fatalf("Compile mismatch: %v", err)
		}
		restored, err := mismatch.Restore(context.Background(), sid)
		if restored != nil {
			t.Fatalf("Restore returned a non-nil session on a config mismatch")
		}
		var cme *ConfigMismatchError
		if !errors.As(err, &cme) {
			t.Fatalf("Restore err = %v, want *ConfigMismatchError", err)
		}

		// The same mismatch with WithCompileAllowConfigMismatch proceeds.
		override, err := Compile(restoreCfg(&stubLLM{}, "model-DIFFERENT", "be helpful"), store,
			WithCompileAllowConfigMismatch())
		if err != nil {
			t.Fatalf("Compile override: %v", err)
		}
		s2, err := override.Restore(context.Background(), sid)
		if err != nil {
			t.Fatalf("Restore with WithCompileAllowConfigMismatch err = %v, want success", err)
		}
		t.Cleanup(func() { _ = s2.Shutdown(context.Background()) })
	})
}
