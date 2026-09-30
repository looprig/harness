package sessionruntime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	gatedomain "github.com/looprig/harness/pkg/gate"
	sessionapi "github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
)

// These tests pin the teardown's release contract: a journal lease or workspace root
// lease that could not be given back is REPORTED by the call that tore the session
// down, never logged and swallowed. No pinned backend expires either grant, so a
// swallowed failure locks a same-process successor out for the life of the process
// while the caller believes the residency is gone.

var errReleaseRefused = errors.New("injected release refusal")

// parkedGateSession runs a fresh session to an open ask_user gate and returns it with
// its store; the caller tears it down.
func parkedGateSession(t *testing.T) (*sessionstore.Store, *Session) {
	t.Helper()
	store := newRestoreStore(t)
	lifecycle, err := newTestLifecycle(askDefinition(&resumeScriptLLM{toolName: "Ask"}, newAskTool(true)), store)
	if err != nil {
		t.Fatal(err)
	}
	s, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "go"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	awaitOpenGate(t, s, gatedomain.KindAskUser)
	// The gate is listed in memory before its request record is durable; measure the
	// journal only once that record has landed, so the teardown is the only writer left.
	deadline := time.Now().Add(10 * time.Second)
	for countEvents[event.UserInputRequested](replayAllSessionEvents(t, store, s.SessionID())) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the open gate's request record never became durable")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return store, s
}

// assertNothingJournaledByTeardown checks the crash-equivalent guarantees survive a
// release failure: the journal is exactly as the live runtime last wrote it, and the
// parked gate was never resolved.
func assertNothingJournaledByTeardown(t *testing.T, store *sessionstore.Store, sessionID uuid.UUID, before int) {
	t.Helper()
	events := replayAllSessionEvents(t, store, sessionID)
	if len(events) != before {
		for _, ev := range events[before:] {
			t.Logf("journaled by teardown: %T", ev)
		}
		t.Fatalf("abandon journaled %d record(s), want 0", len(events)-before)
	}
	for _, ev := range events {
		switch ev.(type) {
		case event.GateResolved, event.SessionStopped, event.SessionResidencyReleased:
			t.Fatalf("abandon journaled %T", ev)
		}
	}
}

func TestAbandonResidencyReportsAJournalLeaseItCouldNotRelease(t *testing.T) {
	t.Parallel()
	store, s := parkedGateSession(t)
	realRelease := s.leaseRelease
	if realRelease == nil {
		t.Fatal("a store-backed session has no journal lease release hook")
	}
	s.leaseRelease = func(context.Context) error { return errReleaseRefused }
	var rootReleased atomic.Bool
	s.wsRootRelease = func(context.Context) error { rootReleased.Store(true); return nil }
	before := len(replayAllSessionEvents(t, store, s.SessionID()))

	err := s.AbandonResidency(context.Background())

	var releaseErr *sessionapi.LeaseReleaseError
	if !errors.As(err, &releaseErr) {
		t.Fatalf("AbandonResidency = %v, want a *LeaseReleaseError", err)
	}
	if !errors.Is(releaseErr.Lease, errReleaseRefused) || releaseErr.Workspace != nil {
		t.Fatalf("LeaseReleaseError = {Lease: %v, Workspace: %v}, want only the journal lease failure", releaseErr.Lease, releaseErr.Workspace)
	}
	if !errors.Is(err, errReleaseRefused) {
		t.Errorf("the release cause is not reachable with errors.Is: %v", err)
	}
	if !rootReleased.Load() {
		t.Error("the workspace root was not released after the journal lease failed")
	}
	select {
	case <-s.Done():
	default:
		t.Error("Done still open after AbandonResidency")
	}
	// A joined caller receives the same result rather than a spurious success.
	if again := s.AbandonResidency(context.Background()); !errors.As(again, &releaseErr) {
		t.Fatalf("repeated AbandonResidency = %v, want the owner's *LeaseReleaseError", again)
	}
	assertNothingJournaledByTeardown(t, store, s.SessionID(), before)

	// The consequence the error warns about: the grant is still held.
	lifecycle, err := newTestLifecycle(askDefinition(&resumeScriptLLM{toolName: "Ask", continueOnly: true}, newAskTool(true)), store)
	if err != nil {
		t.Fatal(err)
	}
	if restored, err := lifecycle.RestoreSession(context.Background(), s.SessionID()); err == nil {
		_ = restored.Shutdown(context.Background())
		t.Fatal("a successor restored while the journal lease was still held")
	}
	// Once the grant is actually given back the session restores with its gate open.
	if err := realRelease(context.Background()); err != nil {
		t.Fatalf("real release: %v", err)
	}
	restored, err := lifecycle.RestoreSession(context.Background(), s.SessionID())
	if err != nil {
		t.Fatalf("RestoreSession after the release: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	if open := restored.ListGates(context.Background()); len(open) != 1 || open[0].Kind != gatedomain.KindAskUser {
		t.Fatalf("restored gates = %+v, want the parked ask_user gate still open", open)
	}
}

func TestAbandonResidencyReportsAWorkspaceRootItCouldNotRelease(t *testing.T) {
	t.Parallel()
	store, s := parkedGateSession(t)
	s.wsRootRelease = func(context.Context) error { return errReleaseRefused }
	before := len(replayAllSessionEvents(t, store, s.SessionID()))

	err := s.AbandonResidency(context.Background())

	var releaseErr *sessionapi.LeaseReleaseError
	if !errors.As(err, &releaseErr) {
		t.Fatalf("AbandonResidency = %v, want a *LeaseReleaseError", err)
	}
	if !errors.Is(releaseErr.Workspace, errReleaseRefused) || releaseErr.Lease != nil {
		t.Fatalf("LeaseReleaseError = {Lease: %v, Workspace: %v}, want only the workspace failure", releaseErr.Lease, releaseErr.Workspace)
	}
	assertNothingJournaledByTeardown(t, store, s.SessionID(), before)
	// The journal lease was still given back: a successor restores with the gate open.
	lifecycle, err := newTestLifecycle(askDefinition(&resumeScriptLLM{toolName: "Ask", continueOnly: true}, newAskTool(true)), store)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := lifecycle.RestoreSession(context.Background(), s.SessionID())
	if err != nil {
		t.Fatalf("RestoreSession after a workspace-only release failure: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	if open := restored.ListGates(context.Background()); len(open) != 1 {
		t.Fatalf("restored gates = %+v, want the parked gate open", open)
	}
}

func TestAbandonResidencyBoundsAHungRelease(t *testing.T) {
	t.Parallel()
	_, s := parkedGateSession(t)
	s.shutdownTimeouts.leaseRelease = 50 * time.Millisecond
	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	// A provider that ignores its context entirely.
	s.leaseRelease = func(context.Context) error { <-unblock; return nil }

	returned := make(chan error, 1)
	go func() { returned <- s.AbandonResidency(context.Background()) }()
	var err error
	select {
	case err = <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("AbandonResidency blocked on a release that ignores its context")
	}
	var releaseErr *sessionapi.LeaseReleaseError
	if !errors.As(err, &releaseErr) || releaseErr.Lease == nil {
		t.Fatalf("AbandonResidency = %v, want a *LeaseReleaseError naming the journal lease", err)
	}
	var timeout *ShutdownCleanupTimeoutError
	if !errors.As(releaseErr.Lease, &timeout) || timeout.Phase != ShutdownCleanupLeaseRelease {
		t.Fatalf("journal lease failure = %v, want a %s cleanup timeout", releaseErr.Lease, ShutdownCleanupLeaseRelease)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a timed-out release does not chain context.DeadlineExceeded: %v", err)
	}
}

func TestAbandonResidencyHappyPathReportsNoReleaseError(t *testing.T) {
	t.Parallel()
	_, s := parkedGateSession(t)
	var rootReleased atomic.Bool
	s.wsRootRelease = func(context.Context) error { rootReleased.Store(true); return nil }
	if err := s.AbandonResidency(context.Background()); err != nil {
		t.Fatalf("AbandonResidency: %v", err)
	}
	if !rootReleased.Load() {
		t.Error("the workspace root was not released")
	}
}

// The terminal and nonterminal teardowns share the release step, so they must not
// swallow it either.
func TestShutdownAndReleaseResidencyReportALeaseTheyCouldNotRelease(t *testing.T) {
	t.Parallel()
	for name, teardown := range map[string]func(*Session) error{
		"Shutdown":         func(s *Session) error { return s.Shutdown(context.Background()) },
		"ReleaseResidency": func(s *Session) error { return s.ReleaseResidency(context.Background()) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := newRestoreStore(t)
			lifecycle, err := newTestLifecycle(cfg(&stubLLM{chunks: []content.Chunk{textChunk("ok")}}), store)
			if err != nil {
				t.Fatal(err)
			}
			s, err := lifecycle.NewSession(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			realRelease := s.leaseRelease
			t.Cleanup(func() { _ = realRelease(context.Background()) })
			s.leaseRelease = func(context.Context) error { return errReleaseRefused }
			err = teardown(s)
			var releaseErr *sessionapi.LeaseReleaseError
			if !errors.As(err, &releaseErr) || !errors.Is(releaseErr.Lease, errReleaseRefused) {
				t.Fatalf("%s = %v, want a *LeaseReleaseError naming the journal lease", name, err)
			}
		})
	}
}
