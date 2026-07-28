package rig

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/storage/memstore"
)

func TestWithHooksIsSingletonAndOwnsRegistrations(t *testing.T) {
	t.Parallel()
	firstGuard := hook.Guard{Operation: hook.OperationTurn, Check: func(context.Context, hook.Call) error { return nil }}
	secondGuard := hook.Guard{Operation: hook.OperationInference, Check: func(context.Context, hook.Call) error { return nil }}
	set := hook.Set{PolicyRevision: "guard-v1", Guards: []hook.Guard{firstGuard}}
	option := WithHooks(set)

	set.Guards[0] = secondGuard
	first := &definitionState{seen: make(map[singletonKey]bool)}
	if err := option(first); err != nil {
		t.Fatalf("first application: %v", err)
	}
	if got := first.hooks.Guards[0].Operation; got != hook.OperationTurn {
		t.Fatalf("captured guard operation = %v, want turn", got)
	}

	first.hooks.Guards[0] = secondGuard
	second := &definitionState{seen: make(map[singletonKey]bool)}
	if err := option(second); err != nil {
		t.Fatalf("second state application: %v", err)
	}
	if got := second.hooks.Guards[0].Operation; got != hook.OperationTurn {
		t.Fatalf("reused option guard operation = %v, want independently owned turn", got)
	}

	err := option(second)
	var definitionErr *DefinitionError
	if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionDuplicateOption {
		t.Fatalf("duplicate option error = %T %v, want duplicate option", err, err)
	}
}

func TestDefineRejectsInvalidHooks(t *testing.T) {
	t.Parallel()
	_, err := Define(WithHooks(hook.Set{
		PolicyRevision: "guard-v1",
		Guards:         []hook.Guard{{Operation: hook.OperationTurn}},
	}))
	var definitionErr *DefinitionError
	if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionInvalidHooks {
		t.Fatalf("Define error = %T %v, want invalid hooks", err, err)
	}
	if definitionErr.Cause == nil {
		t.Fatal("invalid hooks error has nil cause")
	}
	var configErr *hook.ConfigError
	if !errors.As(err, &configErr) || configErr.Kind != hook.ConfigNilGuard {
		t.Fatalf("wrapped cause = %T %v, want nil-guard ConfigError", definitionErr.Cause, definitionErr.Cause)
	}
}

func TestDefineCompilesAroundOnlyHooksWithoutPolicyRevision(t *testing.T) {
	t.Parallel()
	rig, _ := defineHookRig(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationTurn,
		Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			return ctx, nil
		},
	}}})
	if rig.hooks == nil {
		t.Fatal("Define did not retain compiled hooks for lifecycle handoff")
	}
}

func TestCompiledHooksReachNewAndRestoredSessionJournals(t *testing.T) {
	t.Parallel()

	var appends atomic.Int64
	var familiesMu sync.Mutex
	var families []hook.RecordFamily
	defined, _ := defineHookRig(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationJournalAppend,
		Begin: func(ctx context.Context, call hook.Call) (context.Context, hook.FinishFunc) {
			if call.JournalAppend == nil {
				t.Error("journal hook received nil payload")
			} else {
				familiesMu.Lock()
				families = append(families, call.JournalAppend.Family)
				familiesMu.Unlock()
			}
			appends.Add(1)
			return ctx, nil
		},
	}}})

	live, err := defined.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sessionID := live.SessionID()
	if appends.Load() == 0 {
		t.Fatal("NewSession journal did not receive compiled hooks")
	}
	familiesMu.Lock()
	if got := countHookFamily(families, hook.RecordFence); got != 1 {
		familiesMu.Unlock()
		t.Fatalf("NewSession opening fence hook count = %d, want 1", got)
	}
	familiesMu.Unlock()
	if err := live.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	beforeRestore := appends.Load()
	familiesMu.Lock()
	families = nil
	familiesMu.Unlock()
	restored, err := defined.RestoreSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	if appends.Load() <= beforeRestore {
		t.Fatal("RestoreSession journal did not receive compiled hooks")
	}
	familiesMu.Lock()
	if got := countHookFamily(families, hook.RecordFence); got != 1 {
		familiesMu.Unlock()
		t.Fatalf("RestoreSession opening fence hook count = %d, want 1", got)
	}
	families = nil
	familiesMu.Unlock()
	if _, err := restored.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "observe restored command"}}); err != nil {
		t.Fatalf("restored Submit: %v", err)
	}
	familiesMu.Lock()
	defer familiesMu.Unlock()
	for _, family := range families {
		if family == hook.RecordCommand {
			return
		}
	}
	t.Fatal("restored session command appender did not use the hooked journal")
}

func countHookFamily(families []hook.RecordFamily, want hook.RecordFamily) int {
	var count int
	for _, family := range families {
		if family == want {
			count++
		}
	}
	return count
}

func TestAroundCallbacksDoNotChangeManifest(t *testing.T) {
	t.Parallel()
	first, firstStore := defineHookRig(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationTurn,
		Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			return ctx, nil
		},
	}}})
	second, secondStore := defineHookRig(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationTurn,
		Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			return context.WithValue(ctx, hookTestContextKey{}, "changed"), nil
		},
	}}})

	firstManifest := hookRigManifest(t, first, firstStore)
	secondManifest := hookRigManifest(t, second, secondStore)
	if firstManifest.HookPolicyRev != "" || secondManifest.HookPolicyRev != "" {
		t.Fatalf("around-only revisions = %q, %q; want empty", firstManifest.HookPolicyRev, secondManifest.HookPolicyRev)
	}
	if firstManifest.Fingerprint() != secondManifest.Fingerprint() {
		t.Error("changing around callback changed manifest fingerprint")
	}
}

func TestGuardRevisionChangesManifestFingerprint(t *testing.T) {
	t.Parallel()
	guard := hook.Guard{Operation: hook.OperationTurn, Check: func(context.Context, hook.Call) error { return nil }}
	first, firstStore := defineHookRig(t, hook.Set{PolicyRevision: "guard-v1", Guards: []hook.Guard{guard}})
	second, secondStore := defineHookRig(t, hook.Set{PolicyRevision: "guard-v2", Guards: []hook.Guard{guard}})

	firstManifest := hookRigManifest(t, first, firstStore)
	secondManifest := hookRigManifest(t, second, secondStore)
	if firstManifest.HookPolicyRev != "guard-v1" || secondManifest.HookPolicyRev != "guard-v2" {
		t.Fatalf("hook revisions = %q, %q", firstManifest.HookPolicyRev, secondManifest.HookPolicyRev)
	}
	if firstManifest.Fingerprint() == secondManifest.Fingerprint() {
		t.Error("changing guard policy revision did not change manifest fingerprint")
	}
}

type hookTestContextKey struct{}

func defineHookRig(t *testing.T, set hook.Set) (*Rig, *sessionstore.Store) {
	t.Helper()
	definition, err := loop.Define(
		loop.WithName("agent"),
		loop.WithInference(&stubLLM{}, validModel("model")),
	)
	if err != nil {
		t.Fatalf("loop.Define: %v", err)
	}
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("sessionstore.Open: %v", err)
	}
	defined, err := Define(
		WithLoops(definition),
		WithPrimers("agent"),
		WithSessionStore(store),
		WithHooks(set),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	return defined, store
}

func hookRigManifest(t *testing.T, defined *Rig, store *sessionstore.Store) event.ConfigManifest {
	t.Helper()
	session, err := defined.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sessionID := session.SessionID()
	if err := session.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	replayer, err := store.OpenEventReplayer(sessionID, sessionstore.ReplayRequest{FromSeq: 0})
	if err != nil {
		t.Fatalf("OpenEventReplayer: %v", err)
	}
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{From: journal.Beginning()})
	if err != nil {
		t.Fatalf("Open replay: %v", err)
	}
	defer cursor.Close()
	for {
		record, _, nextErr := cursor.Next(context.Background())
		if errors.Is(nextErr, io.EOF) {
			t.Fatal("replay contained no SessionStarted")
		}
		if nextErr != nil {
			t.Fatalf("Replay next: %v", nextErr)
		}
		if started, ok := record.(event.SessionStarted); ok {
			return started.Manifest
		}
	}
}
