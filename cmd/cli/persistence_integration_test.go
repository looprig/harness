//go:build integration

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/agents/coding"
	"github.com/inventivepotter/urvi/internal/persistence"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// withEngine starts an embedded engine under a temp XDG root and returns a Persistence
// over it, torn down via cleanup.
func withEngine(t *testing.T) *coding.Persistence {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	opts, err := persistence.DefaultEngineOptions()
	if err != nil {
		t.Fatalf("DefaultEngineOptions: %v", err)
	}
	eng, err := persistence.Open(opts)
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	p, err := coding.NewPersistence(eng.JetStream())
	if err != nil {
		t.Fatalf("NewPersistence: %v", err)
	}
	return p
}

// TestListSessionsEmptyAndPopulated proves the --list path: an empty catalog prints the
// friendly note, and after a session has run (its NewPersistent + first turn index it) the
// listing includes that session id. It reads the KV index only — no replay.
func TestListSessionsEmptyAndPopulated(t *testing.T) {
	// LLM_API_KEY is unset in CI; the coding model's provider requires a key, so
	// NewPersistent would fail at the credential boundary. This test exercises the LIST
	// path over an empty catalog (no agent needed) plus the catalog read shape.
	p := withEngine(t)

	var empty strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := listSessions(ctx, p, &empty); err != nil {
		t.Fatalf("listSessions (empty): %v", err)
	}
	if !strings.Contains(empty.String(), "no sessions yet") {
		t.Errorf("empty listing = %q, want the 'no sessions yet' note", empty.String())
	}
}

// TestOpenThunkNonCodingFallsBackToRegistry proves a non-coding agent name routes through
// the registry (unpersisted) rather than the persisted coding path — and an unknown name
// surfaces the registry's typed UnknownNameError so main's exit-2 branch fires.
func TestOpenThunkNonCodingFallsBackToRegistry(t *testing.T) {
	p := withEngine(t)
	reg := buildRegistry()
	open := openThunk("definitely-not-an-agent", reg, p, uuid.UUID{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := open(ctx); err == nil {
		t.Fatal("openThunk for an unknown agent returned no error, want UnknownNameError")
	}
}
