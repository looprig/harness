package rig

import (
	"context"
	"errors"
	"testing"

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
	r, err := Define(WithLoops(definition), WithPrimers("agent"), WithSessionStore(store))
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
