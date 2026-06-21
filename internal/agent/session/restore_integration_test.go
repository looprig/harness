//go:build integration

package session

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/agent/session/hub"
	"github.com/inventivepotter/urvi/internal/agent/session/journal"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// --- embedded-server + journal test wiring (local to package session) -------------

// newEmbeddedJS starts an in-process JetStream server (no TCP) and returns a connected
// client, torn down via t.Cleanup. It mirrors the journal package's helper (kept local
// because that one lives in package journal_test).
func newEmbeddedJS(t *testing.T) nats.JetStreamContext {
	t.Helper()
	srv, err := server.NewServer(&server.Options{
		JetStream:  true,
		StoreDir:   t.TempDir(),
		DontListen: true,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	return js
}

func mustSessionID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return id
}

func mustLeaseManager(t *testing.T, js nats.JetStreamContext) *journal.LeaseManager {
	t.Helper()
	lm, err := journal.NewLeaseManager(js)
	if err != nil {
		t.Fatalf("NewLeaseManager: %v", err)
	}
	return lm
}

// mustAcquireLease acquires a lease for sid (released on cleanup).
func mustAcquireLease(t *testing.T, js nats.JetStreamContext, sid uuid.UUID) journal.Lease {
	t.Helper()
	lm := mustLeaseManager(t, js)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := lm.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("Acquire lease for %v: %v", sid, err)
	}
	return lease
}

// handOver releases the original run's lease so Restore can acquire single-writer
// ownership (the handover boundary). A failed release fails the test loudly.
func handOver(t *testing.T, lease journal.Lease) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("release original lease (handover): %v", err)
	}
}

// mustObjectStore binds the per-session object store the journal created.
func mustObjectStore(t *testing.T, js nats.JetStreamContext, sid uuid.UUID) nats.ObjectStore {
	t.Helper()
	store, err := js.ObjectStore(journal.SessionObjectBucket(sid))
	if err != nil {
		t.Fatalf("ObjectStore: %v", err)
	}
	return store
}

// testFactory mints deterministic, monotonically increasing EventIDs and a fixed
// CreatedAt so persisted events get stable, non-zero ids/times for journal dedup.
func testFactory() *event.Factory {
	var n byte = 0x90
	ts := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	return event.NewFactory(func() (uuid.UUID, error) {
		n++
		return uuid.UUID{n}, nil
	}, func() time.Time { return ts })
}

// eventStamper mints a fresh, distinct EventID for each directly-published event so the
// journal's Nats-Msg-Id (the EventID) never collides — a zero EventID on every event
// would dedup them all to one. The hub does NOT stamp a TRIGGERING event (only its
// derived session events), so a direct publisher must stamp them itself, exactly as the
// real loop's eventFactory does for the events it emits.
type eventStamper struct{ n byte }

// stamp sets a fresh EventID + CreatedAt on ev's Header and publishes it through the
// journal-backed hub, failing the test on a publish error.
func (es *eventStamper) stamp(t *testing.T, ctx context.Context, h *hub.Hub, ev event.Event) {
	t.Helper()
	es.n++
	hdr := ev.EventHeader()
	hdr.EventID = uuid.UUID{0xE0, es.n}
	hdr.CreatedAt = time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	stamped := setHeader(t, ev, hdr)
	if err := h.PublishEvent(ctx, stamped); err != nil {
		t.Fatalf("PublishEvent(%T): %v", stamped, err)
	}
}

// setHeader returns a copy of a directly-published original-run event with hdr
// substituted. The set is exactly the events the original-run builders publish.
func setHeader(t *testing.T, ev event.Event, hdr event.Header) event.Event {
	t.Helper()
	switch e := ev.(type) {
	case event.SessionStarted:
		e.Header = hdr
		return e
	case event.LoopStarted:
		e.Header = hdr
		return e
	case event.TurnStarted:
		e.Header = hdr
		return e
	case event.StepDone:
		e.Header = hdr
		return e
	case event.TurnDone:
		e.Header = hdr
		return e
	case event.LoopIdle:
		e.Header = hdr
		return e
	default:
		t.Fatalf("setHeader: unexpected event %T", ev)
		return nil
	}
}

// restoreCfg is the loop.Config both the original run AND the restore use. A System
// prompt + model id make the config fingerprint non-empty, so match/mismatch is real.
func restoreCfg(client llm.LLM, model, system string) loop.Config {
	return loop.Config{
		Client:       client,
		Model:        llm.ModelSpec{Model: model, System: system},
		DrainTimeout: 200 * time.Millisecond,
	}
}

// --- original-run builders ---------------------------------------------------------

// persistedStream is the durable record of an ORIGINAL session run plus the facts the
// restore assertions need: the session/loop ids, the still-held lease (released for the
// handover), and the committed state the original ended with.
type persistedStream struct {
	sessionID     uuid.UUID
	primaryLoopID uuid.UUID
	lease         journal.Lease
	committedMsgs content.AgenticMessages
	committedTurn event.TurnIndex
}

// newOriginalHub wires a journal-backed hub for an original run (the durable-tap
// wiring): a real SessionJournal over a freshly-acquired lease, a JournalEventAppender
// as the hub's required durable tap, and a deterministic Factory. It returns the hub,
// the session/loop ids, the held lease, and the stamper used for direct publishes.
func newOriginalHub(t *testing.T, js nats.JetStreamContext, fp event.ConfigFingerprint) (*hub.Hub, uuid.UUID, uuid.UUID, journal.Lease, *eventStamper) {
	t.Helper()
	sessionID := mustSessionID(t)
	primaryLoopID := mustSessionID(t)
	lease := mustAcquireLease(t, js, sessionID)
	j, err := journal.NewSessionJournal(js, sessionID, lease)
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}
	h := hub.New(sessionID, hub.WithAppender(journal.NewJournalEventAppender(j)), hub.WithFactory(testFactory()))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	es := &eventStamper{}
	// The session records its start (carrying the config fingerprint) + the root loop —
	// exactly what newSession/NewLoop publish on a fresh run.
	es.stamp(t, ctx, h, event.SessionStarted{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
		Config: fp,
	})
	es.stamp(t, ctx, h, event.LoopStarted{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: primaryLoopID}},
	})
	return h, sessionID, primaryLoopID, lease, es
}

// buildOriginalRun drives `turns` COMPLETE turns through a REAL loop whose events persist
// via the journal-backed hub, then snapshots the committed state and stops the loop. The
// lease is left held for the caller to release (handover). This is the faithful
// "drive a few turns" path.
func buildOriginalRun(t *testing.T, js nats.JetStreamContext, fp event.ConfigFingerprint, cfg loop.Config, turns int) persistedStream {
	t.Helper()
	h, sessionID, primaryLoopID, lease, _ := newOriginalHub(t, js, fp)

	// Subscribe so we can drain each turn to its terminal deterministically.
	sub, err := h.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer func() { _ = sub.Close() }()

	loopCtx, loopCancel := context.WithCancel(context.Background())
	l, err := loop.New(loopCtx, sessionID, primaryLoopID, loop.Provenance{}, h, cfg)
	if err != nil {
		t.Fatalf("loop.New: %v", err)
	}

	var want content.AgenticMessages
	for i := 0; i < turns; i++ {
		l.Commands <- command.UserInput{Header: command.Header{CommandID: mustSessionID(t)}, Blocks: []content.Block{&content.TextBlock{Text: "turn input"}}}
		drainSubToTerminal(t, sub)
		want = append(want, foldUserMsg("turn input"), aiMessage("reply"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msgs, idx, err := l.Snapshot(ctx)
	if err != nil {
		t.Fatalf("original Snapshot: %v", err)
	}
	if !reflect.DeepEqual(msgs, want) {
		t.Fatalf("original committed msgs =\n  %#v\nwant\n  %#v", msgs, want)
	}
	loopCancel()
	<-l.Done

	return persistedStream{
		sessionID:     sessionID,
		primaryLoopID: primaryLoopID,
		lease:         lease,
		committedMsgs: msgs,
		committedTurn: idx,
	}
}

// buildCrashedRun produces a durable stream ending on an OPEN turn (the crash seam): one
// complete turn, then a TurnStarted + a single StepDone with NO terminal. It publishes
// these directly through the journal-backed hub (deterministic — no goroutine race), so
// the fold sees exactly user + completed step and OpenTurn=true. The committed msgs it
// reports are what the fold must reconstruct.
func buildCrashedRun(t *testing.T, js nats.JetStreamContext, fp event.ConfigFingerprint) persistedStream {
	t.Helper()
	h, sessionID, primaryLoopID, lease, es := newOriginalHub(t, js, fp)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	turn1 := mustSessionID(t)
	openTurn := mustSessionID(t)
	step1 := mustSessionID(t)
	step2 := mustSessionID(t)

	turnCoord := func(turnID uuid.UUID) identity.Coordinates {
		return identity.Coordinates{SessionID: sessionID, LoopID: primaryLoopID, TurnID: turnID}
	}
	// StepDone is step-scoped: it requires SessionID+LoopID+TurnID+StepID, exactly as a
	// real loop's StepDone carries (the replay validates on decode).
	stepCoord := func(turnID, stepID uuid.UUID) identity.Coordinates {
		return identity.Coordinates{SessionID: sessionID, LoopID: primaryLoopID, TurnID: turnID, StepID: stepID}
	}

	// Turn 1: complete (user + step + terminal).
	es.stamp(t, ctx, h, event.TurnStarted{Header: event.Header{Coordinates: turnCoord(turn1)}, TurnIndex: 1, Message: foldUserMsg("first")})
	es.stamp(t, ctx, h, event.StepDone{Header: event.Header{Coordinates: stepCoord(turn1, step1)}, Messages: content.AgenticMessages{aiMessage("answer one")}})
	es.stamp(t, ctx, h, event.TurnDone{Header: event.Header{Coordinates: turnCoord(turn1)}, TurnIndex: 1, Message: aiMessage("answer one")})
	es.stamp(t, ctx, h, event.LoopIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: primaryLoopID}}})

	// Turn 2: OPEN — committed TurnStarted + one StepDone, then a "crash" (no terminal).
	es.stamp(t, ctx, h, event.TurnStarted{Header: event.Header{Coordinates: turnCoord(openTurn)}, TurnIndex: 2, Message: foldUserMsg("crashed mid-turn")})
	es.stamp(t, ctx, h, event.StepDone{Header: event.Header{Coordinates: stepCoord(openTurn, step2)}, Messages: content.AgenticMessages{aiMessage("calling tool")}})

	return persistedStream{
		sessionID:     sessionID,
		primaryLoopID: primaryLoopID,
		lease:         lease,
		committedMsgs: content.AgenticMessages{
			foldUserMsg("first"),
			aiMessage("answer one"),
			foldUserMsg("crashed mid-turn"),
			aiMessage("calling tool"),
		},
		committedTurn: 2,
	}
}

// --- restore assertions ------------------------------------------------------------

// restoredSnapshot reads the restored primary loop's committed msgs + turnIndex through
// the actor-served Snapshot (the same accessor the loop tests use), so the read never
// races the actor.
func restoredSnapshot(t *testing.T, s *Session) (content.AgenticMessages, event.TurnIndex) {
	t.Helper()
	l, ok := s.loopFor(s.PrimaryLoopID())
	if !ok {
		t.Fatal("restored session has no primary loop registered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msgs, idx, err := l.Snapshot(ctx)
	if err != nil {
		t.Fatalf("restored Snapshot: %v", err)
	}
	return msgs, idx
}

// restoreEventTail replays the stream scoped to the primary loop (session events + that
// loop's events, in stream order) and returns only the restore-lifecycle events
// (RestoreStarted/RestoreDone/RestoreErrored and any TurnInterrupted that closed an open
// turn) — the tail the assertions check.
func restoreEventTail(t *testing.T, js nats.JetStreamContext, sessionID, primaryLoopID uuid.UUID) []event.Event {
	t.Helper()
	r := journal.NewEventReplayer(js, mustObjectStore(t, js, sessionID))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cursor, err := r.Open(ctx, journal.ReplayRequest{SessionID: sessionID, LoopID: primaryLoopID, From: journal.Beginning(), Follow: false})
	if err != nil {
		t.Fatalf("replay Open: %v", err)
	}
	defer func() { _ = cursor.Close() }()

	var tail []event.Event
	for {
		ev, _, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("replay Next: %v", err)
		}
		switch ev.(type) {
		case event.RestoreStarted, event.RestoreDone, event.RestoreErrored, event.TurnInterrupted:
			tail = append(tail, ev)
		}
	}
	return tail
}

// assertTail fails unless got's concrete types match want's, in order.
func assertTail(t *testing.T, got, want []event.Event) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("restore-event tail = %v, want %v", tailTypes(got), tailTypes(want))
	}
	for i := range want {
		if reflect.TypeOf(got[i]) != reflect.TypeOf(want[i]) {
			t.Errorf("restore-event tail[%d] = %T, want %T", i, got[i], want[i])
		}
	}
}

func lastIs(got []event.Event, want event.Event) bool {
	if len(got) == 0 {
		return false
	}
	return reflect.TypeOf(got[len(got)-1]) == reflect.TypeOf(want)
}

func tailTypes(evs []event.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = reflect.TypeOf(e).String()
	}
	return out
}

// drainSubToTerminal drains a subscription until a turn terminal
// (TurnDone/TurnFailed/TurnInterrupted) arrives, failing on timeout.
func drainSubToTerminal(t *testing.T, sub event.Subscription) {
	t.Helper()
	timeout := time.After(10 * time.Second)
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

// submitAndDrain submits input to the restored session and drains a fresh subscription
// to the resulting turn terminal — deterministic, unlike WaitIdle which can race the
// fire-and-forget submit (it may observe the still-idle session before TurnStarted
// mutates quiescence). The subscription is created BEFORE the submit so the terminal is
// never missed (the hub has no replay).
func submitAndDrain(t *testing.T, s *Session, input []content.Block) {
	t.Helper()
	sub, err := s.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer func() { _ = sub.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.Submit(ctx, input); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	drainSubToTerminal(t, sub)
}

// --- the headline tests ------------------------------------------------------------

// TestRestoreRoundTrip is the headline: an original run persists, the lease hands over,
// and Restore brings the primary loop back up IDLE with byte-for-byte msgs + turnIndex,
// stable identity, the expected restore-event tail, and a working new Submit that numbers
// from the restored turnIndex.
func TestRestoreRoundTrip(t *testing.T) {
	js := newEmbeddedJS(t)
	fp := FingerprintFrom(restoreCfg(&stubLLM{}, "model-x", "be helpful"))

	orig := buildOriginalRun(t, js, fp, restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful"), 2)
	handOver(t, orig.lease)

	objStore := mustObjectStore(t, js, orig.sessionID)
	leases := mustLeaseManager(t, js)

	restoreClient := &stubLLM{chunks: []content.Chunk{textChunk("after restore")}}
	s, err := Restore(context.Background(), restoreCfg(restoreClient, "model-x", "be helpful"),
		orig.sessionID, js, objStore, leases)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Identity is stable.
	if s.SessionID != orig.sessionID {
		t.Errorf("restored SessionID = %v, want %v", s.SessionID, orig.sessionID)
	}
	if s.PrimaryLoopID() != orig.primaryLoopID {
		t.Errorf("restored primaryLoopID = %v, want %v", s.PrimaryLoopID(), orig.primaryLoopID)
	}

	// Restored committed state deep-equals the original (byte-for-byte msgs + turnIndex).
	msgs, idx := restoredSnapshot(t, s)
	if !reflect.DeepEqual(msgs, orig.committedMsgs) {
		t.Errorf("restored msgs =\n  %#v\nwant\n  %#v", msgs, orig.committedMsgs)
	}
	if idx != orig.committedTurn {
		t.Errorf("restored turnIndex = %d, want %d", idx, orig.committedTurn)
	}

	// The restore-event tail (no open turn): RestoreStarted → RestoreDone.
	assertTail(t, restoreEventTail(t, js, orig.sessionID, orig.primaryLoopID),
		[]event.Event{event.RestoreStarted{}, event.RestoreDone{}})

	// A new Submit is accepted; the next turn numbers from the restored turnIndex.
	submitAndDrain(t, s, []content.Block{&content.TextBlock{Text: "continue"}})
	msgs2, idx2 := restoredSnapshot(t, s)
	if idx2 != orig.committedTurn+1 {
		t.Errorf("post-restore turnIndex = %d, want %d", idx2, orig.committedTurn+1)
	}
	if len(msgs2) != len(orig.committedMsgs)+2 {
		t.Errorf("post-restore msgs len = %d, want %d (restored + new user + new ai)", len(msgs2), len(orig.committedMsgs)+2)
	}
}

// TestRestoreConfigMismatch proves the fail-secure config check: a mismatch rejects with
// *ConfigMismatchError, the session does not come up, and a RestoreErrored is recorded —
// unless WithAllowConfigMismatch.
func TestRestoreConfigMismatch(t *testing.T) {
	js := newEmbeddedJS(t)
	fp := FingerprintFrom(restoreCfg(&stubLLM{}, "model-x", "be helpful"))

	orig := buildOriginalRun(t, js, fp, restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful"), 1)
	handOver(t, orig.lease)

	objStore := mustObjectStore(t, js, orig.sessionID)

	// Mismatch (different model) rejects by default; the session does not come up.
	_, err := Restore(context.Background(), restoreCfg(&stubLLM{}, "model-DIFFERENT", "be helpful"),
		orig.sessionID, js, objStore, mustLeaseManager(t, js))
	var cme *ConfigMismatchError
	if !errors.As(err, &cme) {
		t.Fatalf("Restore err = %v, want *ConfigMismatchError", err)
	}

	// A RestoreErrored is recorded (no RestoreDone followed).
	tail := restoreEventTail(t, js, orig.sessionID, orig.primaryLoopID)
	if !lastIs(tail, event.RestoreErrored{}) {
		t.Errorf("restore-event tail does not end with RestoreErrored: %v", tailTypes(tail))
	}

	// The override proceeds despite the mismatch (the rejected attempt released its lease).
	s, err := Restore(context.Background(), restoreCfg(&stubLLM{}, "model-DIFFERENT", "be helpful"),
		orig.sessionID, js, objStore, mustLeaseManager(t, js), WithAllowConfigMismatch())
	if err != nil {
		t.Fatalf("Restore with WithAllowConfigMismatch: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	if s.SessionID != orig.sessionID {
		t.Errorf("override restore SessionID = %v, want %v", s.SessionID, orig.sessionID)
	}
}

// TestRestoreCrashMidTurn proves the crash seam: a stream ending on an open turn restores
// user + completed steps (no partial), appends a TurnInterrupted, and comes up idle.
func TestRestoreCrashMidTurn(t *testing.T) {
	js := newEmbeddedJS(t)
	fp := FingerprintFrom(restoreCfg(&stubLLM{}, "model-x", "be helpful"))

	orig := buildCrashedRun(t, js, fp)
	handOver(t, orig.lease)

	objStore := mustObjectStore(t, js, orig.sessionID)

	s, err := Restore(context.Background(), restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("recovered")}}, "model-x", "be helpful"),
		orig.sessionID, js, objStore, mustLeaseManager(t, js))
	if err != nil {
		t.Fatalf("Restore (crash mid-turn): %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Folded history = user + completed step (NO partial), turnIndex counts the open turn.
	msgs, idx := restoredSnapshot(t, s)
	if !reflect.DeepEqual(msgs, orig.committedMsgs) {
		t.Errorf("restored (crash) msgs =\n  %#v\nwant\n  %#v", msgs, orig.committedMsgs)
	}
	if idx != orig.committedTurn {
		t.Errorf("restored (crash) turnIndex = %d, want %d", idx, orig.committedTurn)
	}

	// A TurnInterrupted closed the open turn: tail is RestoreStarted → TurnInterrupted → RestoreDone.
	assertTail(t, restoreEventTail(t, js, orig.sessionID, orig.primaryLoopID),
		[]event.Event{event.RestoreStarted{}, event.TurnInterrupted{}, event.RestoreDone{}})

	// Comes up idle: a new Submit numbers from the restored index.
	submitAndDrain(t, s, []content.Block{&content.TextBlock{Text: "carry on"}})
	if _, idx2 := restoredSnapshot(t, s); idx2 != orig.committedTurn+1 {
		t.Errorf("post-crash-restore turnIndex = %d, want %d", idx2, orig.committedTurn+1)
	}
}
