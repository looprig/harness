//go:build integration

package swe

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/agents/orchestrator"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/agent/session/journal"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/persistence"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// textChunk wraps s as a streamed text chunk for the fake LLM. (The fake_test fakeLLM is
// shared with the non-tagged unit tests; this helper is only used by the persisted
// integration tests, which need to drive a turn to a terminal.)
func textChunk(s string) content.Chunk { return &content.TextChunk{Text: s} }

// openEngine starts an embedded engine on dir (created under a temp XDG root) and tears it
// down via cleanup. It is the CLI-shaped composition: a real embedded server over a
// persistent on-disk StoreDir, exactly as cmd/swe wires it.
func openEngine(t *testing.T, dir string) *persistence.Engine {
	t.Helper()
	eng, err := persistence.Open(persistence.EngineOptions{DataDir: dir, SyncInterval: 2 * time.Second})
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

// drainTurn submits input through the persisted agent and drains a fresh subscription to
// the turn terminal — deterministic (unlike a WaitIdle that can race the fire-and-forget
// submit). The subscription is created BEFORE the submit so the terminal is never missed.
func drainTurn(t *testing.T, a *sessionAgent, text string) {
	t.Helper()
	sub, err := a.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := a.Submit(ctx, []content.Block{&content.TextBlock{Text: text}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before a terminal")
			}
			switch ev.(type) {
			case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
				return
			}
		case <-timeout:
			t.Fatal("no terminal within deadline")
		}
	}
}

// TestPersistenceNewSessionBasics proves Open with a ZERO selector builds a NEW persisted
// session: it has a non-zero SessionID (the composition root minted + injected it) and,
// being a NEW (not restored) session, ReplayBacklog returns nil so the TUI skips the
// cold-restore repaint. Orchestrator-as-primary for the persisted path is asserted in the
// round-trip test via the replayed LoopStarted's attribution name.
func TestPersistenceNewSessionBasics(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	dir := filepath.Join(root, "urvi", "jetstream")

	eng := openEngine(t, dir)
	p, err := NewPersistence(eng.JetStream())
	if err != nil {
		t.Fatalf("NewPersistence: %v", err)
	}

	a, err := p.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("first reply")}}, newModelFactory("test-key"), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("openWithClient (new): %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if a.SessionID().IsZero() {
		t.Fatal("new persisted session has a zero SessionID")
	}
	// A NEW session is not a restore: ReplayBacklog returns nil (the TUI skips the repaint).
	if backlog, err := a.ReplayBacklog(context.Background()); err != nil {
		t.Fatalf("new-session ReplayBacklog: %v", err)
	} else if len(backlog) != 0 {
		t.Errorf("new-session ReplayBacklog returned %d events, want 0", len(backlog))
	}
}

// primaryLoopAgentName returns the AgentName the LoopStarted for loopID carries in evs, or
// the empty string if no such LoopStarted is present. The restored backlog includes the
// primary loop's LoopStarted (replayed from the beginning of the durable log), whose
// Header.AgentName is the attribution name the loop ran under.
func primaryLoopAgentName(evs []event.Event, loopID uuid.UUID) identity.AgentName {
	for _, e := range evs {
		ls, ok := e.(event.LoopStarted)
		if !ok {
			continue
		}
		if ls.EventHeader().LoopID == loopID {
			return ls.EventHeader().AgentName
		}
	}
	return ""
}

// TestPersistenceRoundTrip is the headline CLI-shaped wiring smoke: a NEW persisted
// session (built through the real composition wiring — embedded engine + journal + lease +
// appenders) runs a turn that persists, the agent is Closed (releasing the lease), the
// embedded server is RESTARTED on the SAME StoreDir, and the SAME session is RESUMED via
// the Restore path. The restored session's ReplayBacklog reproduces the committed Enduring
// events and a fresh turn continues — proving the durable log survived a full
// process-restart cycle. It mirrors the prior coding agent's TestPersistenceWiringRoundTrip.
func TestPersistenceRoundTrip(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	dir := filepath.Join(root, "urvi", "jetstream")

	// --- original run: a NEW persisted session ---
	eng := openEngine(t, dir)
	p, err := NewPersistence(eng.JetStream())
	if err != nil {
		t.Fatalf("NewPersistence: %v", err)
	}

	a, err := p.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("first reply")}}, newModelFactory("test-key"), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("openWithClient (new): %v", err)
	}
	sessionID := a.SessionID()
	if sessionID.IsZero() {
		t.Fatal("new persisted session has a zero SessionID")
	}

	drainTurn(t, a, "hello")

	// Clean shutdown releases the lease so a successor (the restored session) can
	// re-acquire without waiting out the TTL.
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close (original): %v", err)
	}

	// --- simulate restart: shut the server, re-open a fresh engine on the SAME StoreDir ---
	if err := eng.Close(); err != nil {
		t.Fatalf("engine Close (restart): %v", err)
	}
	eng2 := openEngine(t, dir)
	p2, err := NewPersistence(eng2.JetStream())
	if err != nil {
		t.Fatalf("NewPersistence (restart): %v", err)
	}

	// --- resume the same session (the Restore path) ---
	a2, err := p2.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("after restore")}}, newModelFactory("test-key"), SessionSelector{Resume: sessionID}, Config{})
	if err != nil {
		t.Fatalf("openWithClient (resume): %v", err)
	}
	t.Cleanup(func() { _ = a2.Close(context.Background()) })

	// Identity is stable across the restart.
	if a2.SessionID() != sessionID {
		t.Errorf("resumed SessionID = %v, want %v", a2.SessionID(), sessionID)
	}

	// The restored session's ReplayBacklog reproduces the committed Enduring history: it
	// MUST contain the user turn's TurnStarted and the bracketing RestoreDone. The headline
	// property: the durable projection survived.
	backlog, err := a2.ReplayBacklog(context.Background())
	if err != nil {
		t.Fatalf("resumed ReplayBacklog: %v", err)
	}
	if !hasType(backlog, event.TurnStarted{}) {
		t.Errorf("resumed backlog missing TurnStarted: %v", typeNames(backlog))
	}
	if !hasType(backlog, event.RestoreDone{}) {
		t.Errorf("resumed backlog missing RestoreDone (restore was not bracketed): %v", typeNames(backlog))
	}

	// Orchestrator-as-primary survived the persist/restore round-trip: the replayed
	// primary-loop LoopStarted is attributed to the orchestrator (the persisted path reused
	// orchestratorConfig, so the journaled primary ran AS the orchestrator).
	if got := primaryLoopAgentName(backlog, a2.PrimaryLoopID()); got != orchestrator.Name {
		t.Errorf("restored primary-loop LoopStarted AgentName = %q, want %q (orchestrator-as-primary)", got, orchestrator.Name)
	}

	// The session continues: a fresh turn is accepted and reaches a terminal.
	drainTurn(t, a2, "continue")
}

// TestPersistenceListAndResumeSeams covers the --list / --resume building blocks at the
// swarm layer: a NEW persisted session appears in the catalog listing (replay-free), and
// the lease is released on Close so a successor Resume can re-acquire it. It mirrors
// the prior coding agent's TestPersistenceListAndResumeSeams.
func TestPersistenceListAndResumeSeams(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	dir := filepath.Join(root, "urvi", "jetstream")

	eng := openEngine(t, dir)
	p, err := NewPersistence(eng.JetStream())
	if err != nil {
		t.Fatalf("NewPersistence: %v", err)
	}

	// An empty catalog lists nothing (the --list-with-no-sessions path).
	if metas, err := p.List(context.Background()); err != nil {
		t.Fatalf("List (empty): %v", err)
	} else if len(metas) != 0 {
		t.Errorf("List on a fresh engine = %d sessions, want 0", len(metas))
	}

	a, err := p.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("reply")}}, newModelFactory("test-key"), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("openWithClient: %v", err)
	}
	sessionID := a.SessionID()
	drainTurn(t, a, "list me")

	// The listing reads the KV catalog only (no replay) and includes the running session.
	metas, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !containsSession(metas, sessionID) {
		t.Errorf("List did not include %v", sessionID)
	}

	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After a clean Close the lease is released: a successor Restore re-acquires it.
	a2, err := p.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("resumed")}}, newModelFactory("test-key"), SessionSelector{Resume: sessionID}, Config{})
	if err != nil {
		t.Fatalf("resume after Close (lease not released?): %v", err)
	}
	_ = a2.Close(context.Background())
}

// hasType reports whether evs contains an event of the same concrete type as want.
func hasType(evs []event.Event, want event.Event) bool {
	wt := reflect.TypeOf(want)
	for _, e := range evs {
		if reflect.TypeOf(e) == wt {
			return true
		}
	}
	return false
}

func typeNames(evs []event.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = reflect.TypeOf(e).String()
	}
	return out
}

func containsSession(metas []journal.SessionMeta, id uuid.UUID) bool {
	for _, m := range metas {
		if m.SessionID == id {
			return true
		}
	}
	return false
}
