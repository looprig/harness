package personalassistant

import (
	"context"
	"errors"
	"testing"
)

// These tests use t.Setenv, which forbids t.Parallel(); they run serially. They
// read the package-level `model` (chutes, key-required) but do not mutate it.

func TestNewMissingKey(t *testing.T) {
	t.Setenv("LLM_API_KEY", "")
	a, err := New(context.Background())
	if a != nil {
		_ = a.Close(context.Background())
		t.Fatalf("New() returned non-nil assistant, want nil")
	}
	var me *MissingEnvError
	if !errors.As(err, &me) || me.Var != "LLM_API_KEY" {
		t.Fatalf("err = %v, want *MissingEnvError{Var: LLM_API_KEY}", err)
	}
}

func TestNewWhitespaceKey(t *testing.T) {
	t.Setenv("LLM_API_KEY", "   ")
	a, err := New(context.Background())
	if a != nil {
		_ = a.Close(context.Background())
		t.Fatalf("New() returned non-nil assistant, want nil")
	}
	var me *MissingEnvError
	if !errors.As(err, &me) {
		t.Fatalf("err = %v, want *MissingEnvError", err)
	}
}

func TestNewHappy(t *testing.T) {
	t.Setenv("LLM_API_KEY", "test-key-not-used-offline")
	a, err := New(context.Background())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if a == nil {
		t.Fatal("New() returned nil assistant")
	}
	// Construction performs no network I/O; Close stops the real session actor.
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}
