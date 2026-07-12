package rig

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreignloop"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/storage/memstore"
)

func TestDefineRequiresSessionStore(t *testing.T) {
	t.Parallel()
	_, err := Define()
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionMissingSessionStore {
		t.Fatalf("Define() error = %T %v, want missing-session-store DefinitionError", err, err)
	}
}

// TestWithLoopsAndPrimersAccumulateAcrossCalls proves the non-singleton options append
// rather than replace: two separate WithLoops calls (and two separate WithPrimers calls)
// leave the definition seeing every loop/primer from BOTH calls, in call order. A
// replacing (last-write-wins) implementation would drop the first call's contribution and
// silently narrow the topology.
func TestWithLoopsAndPrimersAccumulateAcrossCalls(t *testing.T) {
	t.Parallel()
	defineLoop := func(name string) loop.Definition {
		d, err := loop.Define(loop.WithName(identity.AgentName(name)), loop.WithInference(&stubLLM{}, validModel(name)))
		if err != nil {
			t.Fatalf("loop.Define(%q): %v", name, err)
		}
		return d
	}
	planner, builder := defineLoop("planner"), defineLoop("builder")

	state := &definitionState{seen: make(map[singletonKey]bool)}
	for _, opt := range []Option{
		WithLoops(planner),
		WithLoops(builder),
		WithPrimers("planner"),
		WithPrimers("builder"),
	} {
		if err := opt(state); err != nil {
			t.Fatalf("option: %v", err)
		}
	}

	if len(state.loops) != 2 {
		t.Fatalf("loops len = %d, want 2 (both WithLoops calls accumulate)", len(state.loops))
	}
	if state.loops[0].Name() != "planner" || state.loops[1].Name() != "builder" {
		t.Fatalf("loops = %q,%q, want planner,builder in call order", state.loops[0].Name(), state.loops[1].Name())
	}
	if len(state.primers) != 2 {
		t.Fatalf("primers len = %d, want 2 (both WithPrimers calls accumulate)", len(state.primers))
	}
	if state.primers[0] != "planner" || state.primers[1] != "builder" {
		t.Fatalf("primers = %v, want [planner builder] in call order", state.primers)
	}

	// End-to-end: the accumulated loops + primers validate as a real topology, so split
	// calls are indistinguishable from a single call listing all of them.
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("sessionstore.Open: %v", err)
	}
	if _, err := Define(
		WithLoops(planner),
		WithLoops(builder),
		WithPrimers("planner"),
		WithPrimers("builder"),
		WithActivePrimer("planner"),
		WithSessionStore(store),
	); err != nil {
		t.Fatalf("Define with split WithLoops/WithPrimers calls: %v", err)
	}
}

func TestRigNewShutdownRestore(t *testing.T) {
	t.Parallel()
	definition, err := loop.Define(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("model")))
	if err != nil {
		t.Fatalf("loop.Define: %v", err)
	}
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("sessionstore.Open: %v", err)
	}
	r, err := Define(
		WithLoops(definition),
		WithPrimers("agent"),
		WithSessionStore(store),
		WithFingerprintFields(ConfigFingerprintFields{AgentKind: "test-agent", RuntimeSkills: true}),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	controller, err := r.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	id := controller.SessionID()
	if id.IsZero() {
		t.Fatal("NewSession returned zero ID")
	}
	if err := controller.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	restored, err := r.RestoreSession(context.Background(), id)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	if restored.SessionID() != id {
		t.Fatalf("restored SessionID = %v, want %v", restored.SessionID(), id)
	}
	if err := restored.Shutdown(context.Background()); err != nil {
		t.Fatalf("restored Shutdown: %v", err)
	}
}

func TestWithSessionStoreRejectsTypedNil(t *testing.T) {
	t.Parallel()
	_, err := Define(WithSessionStore((*sessionstore.Store)(nil)))
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionInvalidSessionStore {
		t.Fatalf("Define() error = %T %v, want invalid-session-store DefinitionError", err, err)
	}
}

func TestWithCeilingFactoryRejectsNil(t *testing.T) {
	t.Parallel()
	_, err := Define(WithCeilingFactory(nil))
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionInvalidCeilingFactory {
		t.Fatalf("Define() error = %T %v, want invalid-ceiling-factory DefinitionError", err, err)
	}
}

func TestDefineRejectsInvalidFinalLifecycleOptions(t *testing.T) {
	t.Parallel()
	goodLive := foreignloop.Builder(func(context.Context, uuid.UUID, uuid.UUID, loop.Provenance, foreignloop.EventPublisher, loop.BoundDefinition, func() (uuid.UUID, error), *event.Factory) (loop.Backend, string, error) {
		return nil, "", nil
	})
	goodRestored := foreignloop.RestoredBuilder(func(context.Context, uuid.UUID, uuid.UUID, loop.Provenance, foreignloop.EventPublisher, loop.BoundDefinition, func() (uuid.UUID, error), *event.Factory, foreignloop.RestoredForeign) (loop.Backend, error) {
		return nil, nil
	})
	tests := []struct {
		name string
		opt  Option
		kind DefinitionErrorKind
	}{
		{name: "foreign builders both nil", opt: WithForeignBuilders(nil, nil), kind: DefinitionInvalidForeignBuilders},
		{name: "foreign live builder missing", opt: WithForeignBuilders(nil, goodRestored), kind: DefinitionInvalidForeignBuilders},
		{name: "foreign restore builder missing", opt: WithForeignBuilders(goodLive, nil), kind: DefinitionInvalidForeignBuilders},
		{name: "negative delegation depth", opt: WithDelegationLimits(DelegationLimits{Depth: -1}), kind: DefinitionInvalidDelegationLimits},
		{name: "negative delegation quota", opt: WithDelegationLimits(DelegationLimits{Quota: -1}), kind: DefinitionInvalidDelegationLimits},
		{name: "negative gate max open", opt: WithGateCaps(GateCaps{MaxOpen: -1}), kind: DefinitionInvalidGateCaps},
		{name: "negative gate timeout", opt: WithGateCaps(GateCaps{MaxTimeout: -time.Second}), kind: DefinitionInvalidGateCaps},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Define(tt.opt)
			var target *DefinitionError
			if !errors.As(err, &target) || target.Kind != tt.kind {
				t.Fatalf("Define error = %T %v, want kind %q", err, err, tt.kind)
			}
		})
	}
}

func TestDefineRejectsEveryDuplicateSingletonOption(t *testing.T) {
	t.Parallel()
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatal(err)
	}
	foreignLive := foreignloop.Builder(func(context.Context, uuid.UUID, uuid.UUID, loop.Provenance, foreignloop.EventPublisher, loop.BoundDefinition, func() (uuid.UUID, error), *event.Factory) (loop.Backend, string, error) {
		return nil, "", nil
	})
	foreignRestored := foreignloop.RestoredBuilder(func(context.Context, uuid.UUID, uuid.UUID, loop.Provenance, foreignloop.EventPublisher, loop.BoundDefinition, func() (uuid.UUID, error), *event.Factory, foreignloop.RestoredForeign) (loop.Backend, error) {
		return nil, nil
	})
	tests := []struct {
		name string
		opt  Option
	}{
		{name: "active primer", opt: WithActivePrimer("agent")},
		{name: "session store", opt: WithSessionStore(store)},
		{name: "delegation limits", opt: WithDelegationLimits(DelegationLimits{})},
		{name: "fingerprint fields", opt: WithFingerprintFields(ConfigFingerprintFields{AgentKind: "test"})},
		{name: "foreign builders", opt: WithForeignBuilders(foreignLive, foreignRestored)},
		{name: "gate caps", opt: WithGateCaps(GateCaps{})},
		{name: "allow config mismatch", opt: WithAllowConfigMismatch()},
		{name: "ceiling factory", opt: WithCeilingFactory(func() *ceiling.State { return ceiling.New() })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &definitionState{seen: make(map[singletonKey]bool)}
			if err := tt.opt(state); err != nil {
				t.Fatalf("first option: %v", err)
			}
			err := tt.opt(state)
			var target *DefinitionError
			if !errors.As(err, &target) || target.Kind != DefinitionDuplicateOption {
				t.Fatalf("second option error = %T %v, want duplicate", err, err)
			}
		})
	}
}

func TestDefineFreezesAdditiveOptionInputs(t *testing.T) {
	t.Parallel()
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatal(err)
	}
	planner, err := loop.Define(loop.WithName("planner"), loop.WithInference(&stubLLM{}, validModel("planner")))
	if err != nil {
		t.Fatal(err)
	}
	definitions := []loop.Definition{planner}
	primers := []string{"planner"}
	loopsOption := WithLoops(definitions...)
	primersOption := WithPrimers(primers...)
	definitions[0] = loop.Definition{}
	primers[0] = "mutated"

	r, err := Define(loopsOption, primersOption, WithSessionStore(store))
	if err != nil {
		t.Fatalf("Define observed caller mutation: %v", err)
	}
	s, err := r.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Shutdown(context.Background()) }()
	if got := s.ActiveLoop().Model().Name; got != "planner" {
		t.Fatalf("active model = %q, want frozen planner definition", got)
	}
}

func TestCeilingFactoryInvokedPerConcurrentSession(t *testing.T) {
	t.Parallel()
	definition, err := loop.Define(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("model")))
	if err != nil {
		t.Fatalf("loop.Define: %v", err)
	}
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("sessionstore.Open: %v", err)
	}
	var calls atomic.Int32
	r, err := Define(
		WithLoops(definition),
		WithPrimers("agent"),
		WithSessionStore(store),
		WithCeilingFactory(func() *ceiling.State {
			calls.Add(1)
			return ceiling.New()
		}),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	const sessions = 8
	errorsCh := make(chan error, sessions)
	var group sync.WaitGroup
	for range sessions {
		group.Add(1)
		go func() {
			defer group.Done()
			controller, err := r.NewSession(context.Background())
			if err == nil {
				err = controller.Shutdown(context.Background())
			}
			errorsCh <- err
		}()
	}
	group.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent NewSession: %v", err)
		}
	}
	if got := calls.Load(); got != sessions {
		t.Fatalf("ceiling factory calls = %d, want %d", got, sessions)
	}
}

func TestDefinePrimerTopology(t *testing.T) {
	t.Parallel()
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatal(err)
	}
	defineLoop := func(name string) loop.Definition {
		d, defineErr := loop.Define(loop.WithName(identity.AgentName(name)), loop.WithInference(&stubLLM{}, validModel(name)))
		if defineErr != nil {
			t.Fatal(defineErr)
		}
		return d
	}
	planner, builder := defineLoop("planner"), defineLoop("builder")

	tests := []struct {
		name string
		opts []Option
		kind DefinitionErrorKind
	}{
		{"duplicate loop", []Option{WithLoops(planner, planner), WithPrimers("planner"), WithSessionStore(store)}, DefinitionDuplicateLoop},
		{"unknown primer", []Option{WithLoops(planner), WithPrimers("missing"), WithSessionStore(store)}, DefinitionInvalidPrimer},
		{"unreferenced loop", []Option{WithLoops(planner, builder), WithPrimers("planner"), WithSessionStore(store)}, DefinitionInvalidLoop},
		{"multiple needs active", []Option{WithLoops(planner, builder), WithPrimers("planner", "builder"), WithSessionStore(store)}, DefinitionInvalidActivePrimer},
		{"active must be primer", []Option{WithLoops(planner, builder), WithPrimers("planner"), WithActivePrimer("builder"), WithSessionStore(store)}, DefinitionInvalidActivePrimer},
		{"duplicate primer", []Option{WithLoops(planner), WithPrimers("planner", "planner"), WithActivePrimer("planner"), WithSessionStore(store)}, DefinitionInvalidPrimer},
		{"explicit empty active", []Option{WithLoops(planner), WithPrimers("planner"), WithActivePrimer(""), WithSessionStore(store)}, DefinitionInvalidActivePrimer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, gotErr := Define(tt.opts...)
			var target *DefinitionError
			if !errors.As(gotErr, &target) || target.Kind != tt.kind {
				t.Fatalf("Define error = %T %v, want kind %q", gotErr, gotErr, tt.kind)
			}
		})
	}

	if _, err := Define(WithLoops(planner, builder), WithPrimers("planner", "builder"), WithActivePrimer("builder"), WithSessionStore(store)); err != nil {
		t.Fatalf("valid topology: %v", err)
	}
}

func TestDefineRequiresGraphReachabilityFromPrimers(t *testing.T) {
	store, _ := sessionstore.Open(memstore.New())
	makeLoop := func(name string, delegates ...identity.AgentName) loop.Definition {
		d, err := loop.Define(loop.WithName(identity.AgentName(name)), loop.WithInference(&stubLLM{}, validModel(name)), loop.WithDelegates(delegates...))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	t.Run("unreachable cycle", func(t *testing.T) {
		_, err := Define(WithLoops(makeLoop("root"), makeLoop("a", "b"), makeLoop("b", "a")), WithPrimers("root"), WithSessionStore(store))
		var target *DefinitionError
		if !errors.As(err, &target) || target.Kind != DefinitionInvalidLoop {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("reachable cycle and diamond", func(t *testing.T) {
		_, err := Define(
			WithLoops(makeLoop("root", "a", "b"), makeLoop("a", "c"), makeLoop("b", "c"), makeLoop("c", "a")),
			WithPrimers("root"), WithSessionStore(store),
		)
		if err != nil {
			t.Fatalf("Define: %v", err)
		}
	})
}

func TestRigPrimersActiveLoopAndRestore(t *testing.T) {
	t.Parallel()
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatal(err)
	}
	defineLoop := func(name string) loop.Definition {
		d, defineErr := loop.Define(loop.WithName(identity.AgentName(name)), loop.WithInference(&stubLLM{}, validModel(name)))
		if defineErr != nil {
			t.Fatal(defineErr)
		}
		return d
	}
	r, err := Define(
		WithLoops(defineLoop("planner"), defineLoop("builder")),
		WithPrimers("planner", "builder"),
		WithActivePrimer("planner"),
		WithSessionStore(store),
	)
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	planner := s.ActiveLoop()
	if got := planner.Model().Name; got != "planner" {
		t.Fatalf("active model = %q, want planner", got)
	}
	readRoots := func() map[string]uuid.UUID {
		replayer, replayErr := store.OpenEventReplayer(s.SessionID(), sessionstore.ReplayRequest{FromSeq: 0})
		if replayErr != nil {
			t.Fatal(replayErr)
		}
		cursor, openErr := replayer.Open(context.Background(), journal.ReplayRequest{From: journal.Beginning()})
		if openErr != nil {
			t.Fatal(openErr)
		}
		defer cursor.Close()
		rootIDs := map[string]uuid.UUID{}
		for {
			ev, _, nextErr := cursor.Next(context.Background())
			if errors.Is(nextErr, io.EOF) {
				return rootIDs
			}
			if nextErr != nil {
				t.Fatal(nextErr)
			}
			if started, ok := ev.(event.LoopStarted); ok && started.Cause.Coordinates.LoopID.IsZero() {
				rootIDs[string(started.AgentName)] = started.LoopID
			}
		}
	}
	rootIDs := readRoots()
	if len(rootIDs) != 2 {
		t.Fatalf("durable root loops = %v, want planner and builder", rootIDs)
	}
	if err := s.SetActiveLoop(context.Background(), rootIDs["builder"]); err != nil {
		t.Fatal(err)
	}
	if s.ActiveLoop().Model().Name != "builder" {
		t.Fatal("fresh session did not switch active loop")
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, err := r.RestoreSession(context.Background(), s.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.ActiveLoop().Model().Name; got != "builder" {
		t.Fatalf("restored active model = %q, want builder", got)
	}
	builder, ok := restored.Loop(rootIDs["builder"])
	if !ok || builder.Model().Name != "builder" {
		t.Fatal("restored topology did not expose builder primer")
	}
	if err := restored.SetActiveLoop(context.Background(), rootIDs["planner"]); err != nil {
		t.Fatal(err)
	}
	if restored.ActiveLoop().ID() != rootIDs["planner"] {
		t.Fatal("active loop change was not visible")
	}
	if err := restored.SetActiveLoop(context.Background(), uuid.UUID{0xff}); err == nil || restored.ActiveLoop().ID() != rootIDs["planner"] {
		t.Fatal("invalid active-loop target changed selection")
	}
}

// TestWithOffloadGCValidatesPolicy proves WithOffloadGC accepts a positive policy and
// rejects a non-positive interval or timeout with the dedicated typed errors, and that it is
// an at-most-once singleton.
func TestWithOffloadGCValidatesPolicy(t *testing.T) {
	t.Parallel()

	t.Run("rejects non-positive fields", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name        string
			policy      OffloadGCPolicy
			wantInterva bool
			wantTimeout bool
		}{
			{name: "zero interval", policy: OffloadGCPolicy{Interval: 0, Timeout: time.Second}, wantInterva: true},
			{name: "negative interval", policy: OffloadGCPolicy{Interval: -time.Second, Timeout: time.Second}, wantInterva: true},
			{name: "zero timeout", policy: OffloadGCPolicy{Interval: time.Minute, Timeout: 0}, wantTimeout: true},
			{name: "negative timeout", policy: OffloadGCPolicy{Interval: time.Minute, Timeout: -time.Second}, wantTimeout: true},
		}
		for _, tt := range tests {
			tt := tt
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				err := WithOffloadGC(tt.policy)(&definitionState{seen: make(map[singletonKey]bool)})
				var iErr *InvalidOffloadGCIntervalError
				var toErr *InvalidOffloadGCTimeoutError
				if tt.wantInterva && !errors.As(err, &iErr) {
					t.Fatalf("WithOffloadGC error = %T %v, want *InvalidOffloadGCIntervalError", err, err)
				}
				if tt.wantTimeout && !errors.As(err, &toErr) {
					t.Fatalf("WithOffloadGC error = %T %v, want *InvalidOffloadGCTimeoutError", err, err)
				}
			})
		}
	})

	t.Run("accepts a valid policy", func(t *testing.T) {
		t.Parallel()
		state := &definitionState{seen: make(map[singletonKey]bool)}
		if err := WithOffloadGC(OffloadGCPolicy{Interval: time.Minute, Timeout: 10 * time.Second})(state); err != nil {
			t.Fatalf("WithOffloadGC(valid) error = %v", err)
		}
	})

	t.Run("is an at-most-once singleton", func(t *testing.T) {
		t.Parallel()
		state := &definitionState{seen: make(map[singletonKey]bool)}
		opt := WithOffloadGC(OffloadGCPolicy{Interval: time.Minute, Timeout: 10 * time.Second})
		if err := opt(state); err != nil {
			t.Fatalf("first WithOffloadGC: %v", err)
		}
		var target *DefinitionError
		if err := opt(state); !errors.As(err, &target) || target.Kind != DefinitionDuplicateOption {
			t.Fatalf("second WithOffloadGC error = %T %v, want duplicate", err, err)
		}
	})
}
