package rig

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/event"
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
		WithConfigFingerprintFields(ConfigFingerprintFields{AgentKind: "test-agent", RuntimeSkills: true}),
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
