package sessionruntime

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
)

type persistedGateStream struct {
	sessionID  uuid.UUID
	rootLoopID uuid.UUID
	lease      journal.Lease
	gateID     gate.ID
}

func buildGateRestoreStream(t *testing.T, store *sessionstore.Store, cfg loop.Definition, prepared, opened, resolved bool) persistedGateStream {
	t.Helper()
	return buildGateRestoreStreamWithResolver(t, store, cfg, prepared, opened, resolved, gate.ResolverLoop)
}

func buildGateRestoreStreamWithResolver(t *testing.T, store *sessionstore.Store, cfg loop.Definition, prepared, opened, resolved bool, resolver gate.ResolverKind) persistedGateStream {
	t.Helper()
	sessionID := mustSessionID(t)
	rootLoopID := mustSessionID(t)
	turnID := mustSessionID(t)
	stepID := mustSessionID(t)
	gateID := gate.ID(mustSessionID(t))
	toolExecID := gate.ID(mustSessionID(t))

	lease := mustAcquireLease(t, store, sessionID)
	openCtx, openCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer openCancel()
	j, err := store.OpenJournal(openCtx, sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}

	var seq byte
	stamp := func(coords identity.Coordinates) event.Header {
		seq++
		return event.Header{
			Coordinates: coords,
			EventID:     uuid.UUID{0xD0, seq},
			CreatedAt:   time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC),
		}
	}
	appendEvent := func(ev event.Event) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := j.Append(ctx, journal.NewEventRecord(ev)); err != nil {
			t.Fatalf("append %T: %v", ev, err)
		}
	}
	appendRecord := func(rec journal.JournalRecord) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := j.Append(ctx, rec); err != nil {
			t.Fatalf("append %T: %v", rec, err)
		}
	}

	appendEvent(event.SessionStarted{
		Header: stamp(identity.Coordinates{SessionID: sessionID}),
		Config: fingerprintFromDefinition(cfg),
	})
	appendEvent(event.LoopStarted{
		Header: func() event.Header {
			h := stamp(identity.Coordinates{SessionID: sessionID, LoopID: rootLoopID})
			h.AgentName = "agent"
			return h
		}(),
		Runtime: runtimeFromFingerprint(fingerprintFromDefinition(cfg)),
	})

	coords := identity.Coordinates{SessionID: sessionID}
	if resolver == gate.ResolverLoop {
		coords = identity.Coordinates{SessionID: sessionID, LoopID: rootLoopID, TurnID: turnID, StepID: stepID}
	}
	g := gate.Gate{
		ID:          gateID,
		Kind:        gate.KindPermission,
		Resolver:    resolver,
		Blocks:      gate.BlocksToolCall,
		Effect:      gate.EffectResume,
		Criticality: gate.GateCritical,
		Subject: gate.Subject{
			ToolExecutionID: toolExecID,
			TurnID:          gate.ID(turnID),
			StepID:          gate.ID(stepID),
		},
		Prompt: gate.Prompt{
			Title: "Approve tool call",
			Body:  "echo ok",
			Controls: []gate.Control{
				{Action: "approve", Label: "Approve"},
				{Action: "deny", Label: "Deny"},
			},
		},
	}
	if resolver == gate.ResolverSession {
		g.Kind = gate.KindForm
		g.Blocks = gate.BlocksSession
		g.Subject = gate.Subject{}
		g.Prompt = gate.Prompt{
			Title: "Provide details",
			Controls: []gate.Control{
				{Action: gate.FormActionAccept, Label: "Submit"},
				{Action: gate.FormActionCancel, Label: "Cancel"},
			},
		}
	}
	if prepared {
		preparedEvent := event.GatePrepared{Header: stamp(coords), Gate: g}
		appendRecord(journal.NewGatePreparedRecord(preparedEvent, gate.OpenPayload{
			GateID:  gateID,
			Payload: gate.PermissionPayload{Request: typedGateRequest()},
		}))
	}
	if opened {
		appendEvent(event.GateOpened{Header: stamp(coords), Gate: g})
	}
	if resolved {
		appendEvent(event.GateResolved{Header: stamp(coords), GateID: gateID, Resolver: resolver, Reason: gate.CloseAnswered})
	}

	return persistedGateStream{sessionID: sessionID, rootLoopID: rootLoopID, lease: lease, gateID: gateID}
}

// buildRestorablePermissionGateStream is buildGateRestoreStreamWithResolver's
// counterpart for design §15/§22.4's restore contract: it persists an opened,
// unresolved KindPermission/ResolverLoop gate marked Restorable, exactly the
// shape gateRestoreHookSupported now accepts as restorable. It deliberately
// duplicates rather than parameterizing buildGateRestoreStreamWithResolver so
// every existing restore_gates_test.go case (none of which ever set
// Restorable) is provably unaffected by this task's restore change.
func buildRestorablePermissionGateStream(t *testing.T, store *sessionstore.Store, cfg loop.Definition) persistedGateStream {
	t.Helper()
	sessionID := mustSessionID(t)
	rootLoopID := mustSessionID(t)
	turnID := mustSessionID(t)
	stepID := mustSessionID(t)
	gateID := gate.ID(mustSessionID(t))
	toolExecID := gate.ID(mustSessionID(t))

	lease := mustAcquireLease(t, store, sessionID)
	openCtx, openCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer openCancel()
	j, err := store.OpenJournal(openCtx, sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}

	var seq byte
	stamp := func(coords identity.Coordinates) event.Header {
		seq++
		return event.Header{
			Coordinates: coords,
			EventID:     uuid.UUID{0xD1, seq},
			CreatedAt:   time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC),
		}
	}
	appendEvent := func(ev event.Event) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := j.Append(ctx, journal.NewEventRecord(ev)); err != nil {
			t.Fatalf("append %T: %v", ev, err)
		}
	}
	appendRecord := func(rec journal.JournalRecord) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := j.Append(ctx, rec); err != nil {
			t.Fatalf("append %T: %v", rec, err)
		}
	}

	appendEvent(event.SessionStarted{
		Header: stamp(identity.Coordinates{SessionID: sessionID}),
		Config: fingerprintFromDefinition(cfg),
	})
	appendEvent(event.LoopStarted{
		Header: func() event.Header {
			h := stamp(identity.Coordinates{SessionID: sessionID, LoopID: rootLoopID})
			h.AgentName = "agent"
			return h
		}(),
		Runtime: runtimeFromFingerprint(fingerprintFromDefinition(cfg)),
	})

	coords := identity.Coordinates{SessionID: sessionID, LoopID: rootLoopID, TurnID: turnID, StepID: stepID}
	g := gate.Gate{
		ID:          gateID,
		Kind:        gate.KindPermission,
		Resolver:    gate.ResolverLoop,
		Blocks:      gate.BlocksToolCall,
		Effect:      gate.EffectResume,
		Criticality: gate.GateCritical,
		Restorable:  true,
		Subject: gate.Subject{
			ToolExecutionID: toolExecID,
			TurnID:          gate.ID(turnID),
			StepID:          gate.ID(stepID),
		},
		Prompt: gate.Prompt{
			Title: "Approve tool call",
			Body:  "echo ok",
			Controls: []gate.Control{
				{Action: string(gate.ApprovalApprove), Label: "Approve"},
				{Action: string(gate.ApprovalDeny), Label: "Deny"},
			},
		},
	}
	preparedEvent := event.GatePrepared{Header: stamp(coords), Gate: g}
	appendRecord(journal.NewGatePreparedRecord(preparedEvent, gate.OpenPayload{
		GateID:  gateID,
		Payload: gate.PermissionPayload{Request: typedGateRequest()},
	}))
	appendEvent(event.GateOpened{Header: stamp(coords), Gate: g})

	return persistedGateStream{sessionID: sessionID, rootLoopID: rootLoopID, lease: lease, gateID: gateID}
}

// TestRestorePermissionGateRestoresOpenAndReviewFree proves design §15's
// "the permission gate restores normally... the gate remains answerable by a
// human" together with §22.4's restore-test list: a Restorable, opened,
// unresolved permission gate comes back OPEN (not CloseRestoreUnavailable),
// remains answerable by an ordinary human RespondGate, carries a zero
// reviewBasis (no auto-review replay: nothing was ever recorded against it
// after restore), and the session's cancellation-group bookkeeping is
// entirely empty (no stale live cancellation handle survived the restart).
func TestRestorePermissionGateRestoresOpenAndReviewFree(t *testing.T) {
	store := newRestoreStore(t)
	cfg := restoreCfg(&stubLLM{}, "model-x", "be helpful")
	orig := buildRestorablePermissionGateStream(t, store, cfg)
	handOver(t, orig.lease)

	s, err := restoreTestSession(context.Background(), cfg, orig.sessionID, store)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	open := s.ListGates(context.Background())
	if len(open) != 1 || open[0].ID != orig.gateID {
		t.Fatalf("ListGates() = %+v, want the restored gate open and answerable", open)
	}
	if got := countGateResolvedReason(restoredGateResolvedEvents(t, store, orig.sessionID), orig.gateID, gate.CloseRestoreUnavailable); got != 0 {
		t.Fatalf("restore_unavailable GateResolved count = %d, want 0 (a restorable permission gate must not be closed unavailable)", got)
	}

	s.gatesMu.Lock()
	entry, ok := s.gates[orig.gateID]
	s.gatesMu.Unlock()
	if !ok {
		t.Fatal("restored gate missing from directory")
	}
	if entry.reviewBasis != (gate.ReviewBasis{}) {
		t.Fatalf("restored gateEntry.reviewBasis = %+v, want zero (no auto-review replay from restored/guessed context)", entry.reviewBasis)
	}
	s.review.mu.Lock()
	cancellations := len(s.review.cancellations)
	s.review.mu.Unlock()
	if cancellations != 0 {
		t.Fatalf("restored session review.cancellations = %d, want 0 (no stale live cancellation handle survives restore)", cancellations)
	}

	// "remains answerable by a human": an ordinary RespondGate must succeed
	// exactly like it would for any other restored open gate.
	respondCtx, respondCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer respondCancel()
	if err := s.RespondGate(respondCtx, gate.GateResponse{
		GateID: orig.gateID,
		Action: string(gate.ApprovalDeny),
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	}); err != nil {
		t.Fatalf("RespondGate() on restored permission gate error = %v, want nil (human-answerable)", err)
	}
	if got := s.ListGates(context.Background()); len(got) != 0 {
		t.Fatalf("ListGates() after RespondGate = %+v, want 0 (gate resolved)", got)
	}
}

func TestRestoreUnavailableGateResolutionPreservesResolver(t *testing.T) {
	for _, resolver := range []gate.ResolverKind{gate.ResolverLoop, gate.ResolverSession} {
		t.Run(string(resolver), func(t *testing.T) {
			store := newRestoreStore(t)
			cfg := restoreCfg(&stubLLM{}, "model-x", "be helpful")
			orig := buildGateRestoreStreamWithResolver(t, store, cfg, false, true, false, resolver)
			handOver(t, orig.lease)

			s, err := restoreTestSession(context.Background(), cfg, orig.sessionID, store)
			if err != nil {
				t.Fatalf("Restore: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			var matchedResolver gate.ResolverKind
			matched := false
			for _, resolved := range restoredGateResolvedEvents(t, store, orig.sessionID) {
				if resolved.GateID == orig.gateID && resolved.Reason == gate.CloseRestoreUnavailable {
					matchedResolver = resolved.Resolver
					matched = true
					break
				}
			}
			if !matched {
				t.Fatalf("missing restore-unavailable GateResolved for gate %s", orig.gateID)
			}
			if matchedResolver != resolver {
				t.Fatalf("GateResolved.Resolver = %q, want opened gate resolver %q", matchedResolver, resolver)
			}
		})
	}
}

func restoredGateResolvedEvents(t *testing.T, store *sessionstore.Store, sessionID uuid.UUID) []event.GateResolved {
	t.Helper()
	r, err := store.OpenEventReplayer(sessionID, sessionstore.ReplayRequest{FromSeq: 0})
	if err != nil {
		t.Fatalf("OpenEventReplayer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cursor, err := r.Open(ctx, journal.ReplayRequest{Follow: false})
	if err != nil {
		t.Fatalf("replay Open: %v", err)
	}
	defer func() { _ = cursor.Close() }()

	var out []event.GateResolved
	for {
		ev, _, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("replay Next: %v", err)
		}
		if resolved, ok := ev.(event.GateResolved); ok {
			out = append(out, resolved)
		}
	}
}

func countGateResolvedReason(evs []event.GateResolved, gateID gate.ID, reason gate.CloseReason) int {
	n := 0
	for _, ev := range evs {
		if ev.GateID == gateID && ev.Reason == reason {
			n++
		}
	}
	return n
}

func TestRestoreGateOpenedWithoutResolutionClosesUnavailable(t *testing.T) {
	tests := []struct {
		name     string
		prepared bool
	}{
		{name: "prepared and opened nonrestorable gate", prepared: true},
		{name: "opened without prepared payload", prepared: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			store := newRestoreStore(t)
			cfg := restoreCfg(&stubLLM{}, "model-x", "be helpful")
			orig := buildGateRestoreStream(t, store, cfg, tt.prepared, true, false)
			handOver(t, orig.lease)

			s, err := restoreTestSession(context.Background(), cfg, orig.sessionID, store)
			if err != nil {
				t.Fatalf("Restore: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			if got := s.ListGates(context.Background()); len(got) != 0 {
				t.Fatalf("ListGates() returned %d gates, want 0", len(got))
			}
			err = s.RespondGate(context.Background(), gate.GateResponse{GateID: orig.gateID, Action: "deny"})
			var ge *GateError
			if !errors.As(err, &ge) || ge.Kind != GateNotFound {
				t.Fatalf("RespondGate() error = %v, want *GateError{GateNotFound}", err)
			}

			resolved := restoredGateResolvedEvents(t, store, orig.sessionID)
			if got := countGateResolvedReason(resolved, orig.gateID, gate.CloseRestoreUnavailable); got != 1 {
				t.Fatalf("restore_unavailable GateResolved count = %d, want 1; events=%#v", got, resolved)
			}
		})
	}
}

func TestRestoreWiresGateAppenderForNewGates(t *testing.T) {
	store := newRestoreStore(t)
	cfg := restoreCfg(&stubLLM{}, "model-x", "be helpful")
	orig := buildGateRestoreStream(t, store, cfg, false, false, false)
	handOver(t, orig.lease)

	s, err := restoreTestSession(context.Background(), cfg, orig.sessionID, store)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	gateID, err := s.PrepareGateOpen(context.Background(), orig.rootLoopID, permissionGate(), gate.PermissionPayload{Request: typedGateRequest()})
	if err != nil {
		t.Fatalf("PrepareGateOpen: %v", err)
	}
	if err := s.ActivateGate(context.Background(), gateID, gate.Route{GateID: gateID, LoopID: orig.rootLoopID}); err != nil {
		t.Fatalf("ActivateGate: %v", err)
	}

	replayer, err := store.OpenInternalRecordReplayer(orig.sessionID, sessionstore.ReplayRequest{FromSeq: 0})
	if err != nil {
		t.Fatalf("OpenInternalRecordReplayer: %v", err)
	}
	records, err := drainRecordReplay(context.Background(), replayer, journal.ReplayRequest{From: journal.Beginning()})
	if err != nil {
		t.Fatalf("drainRecordReplay: %v", err)
	}
	var prepared, opened bool
	for _, rec := range records {
		switch r := rec.(type) {
		case journal.GatePreparedRecord:
			if r.Prepared().Gate.ID == gateID {
				prepared = true
			}
		case journal.EventRecord:
			if ev, ok := r.Event().(event.GateOpened); ok && ev.Gate.ID == gateID {
				opened = true
			}
		}
	}
	if !prepared || !opened {
		t.Fatalf("restored session gate appender persisted prepared=%v opened=%v, want both true", prepared, opened)
	}
}

func TestRestoreGatePreparedWithoutOpenedIsInvisible(t *testing.T) {
	store := newRestoreStore(t)
	cfg := restoreCfg(&stubLLM{}, "model-x", "be helpful")
	orig := buildGateRestoreStream(t, store, cfg, true, false, false)
	handOver(t, orig.lease)

	s, err := restoreTestSession(context.Background(), cfg, orig.sessionID, store)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	if got := s.ListGates(context.Background()); len(got) != 0 {
		t.Fatalf("ListGates() returned %d gates, want 0", len(got))
	}
	if got := countGateResolvedReason(restoredGateResolvedEvents(t, store, orig.sessionID), orig.gateID, gate.CloseRestoreUnavailable); got != 0 {
		t.Fatalf("restore_unavailable GateResolved count = %d, want 0", got)
	}
}

func TestRestoreGateResolvedRemovesCandidate(t *testing.T) {
	store := newRestoreStore(t)
	cfg := restoreCfg(&stubLLM{}, "model-x", "be helpful")
	orig := buildGateRestoreStream(t, store, cfg, true, true, true)
	handOver(t, orig.lease)

	s, err := restoreTestSession(context.Background(), cfg, orig.sessionID, store)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	if got := s.ListGates(context.Background()); len(got) != 0 {
		t.Fatalf("ListGates() returned %d gates, want 0", len(got))
	}
	resolved := restoredGateResolvedEvents(t, store, orig.sessionID)
	if got := countGateResolvedReason(resolved, orig.gateID, gate.CloseAnswered); got != 1 {
		t.Fatalf("answered GateResolved count = %d, want original 1; events=%#v", got, resolved)
	}
	if got := countGateResolvedReason(resolved, orig.gateID, gate.CloseRestoreUnavailable); got != 0 {
		t.Fatalf("restore_unavailable GateResolved count = %d, want 0; events=%#v", got, resolved)
	}
}
