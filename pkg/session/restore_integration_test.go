//go:build integration

package session

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/command"
	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/hub"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/journal"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/uuid"
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
	case event.TurnFoldedInto:
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

// restoreCfgNamed is restoreCfg with an AgentName set, so a restore can validate the
// configured primary's attribution name against the persisted root loop's stamped name.
func restoreCfgNamed(client llm.LLM, model, system string, agent identity.AgentName) loop.Config {
	c := restoreCfg(client, model, system)
	c.AgentName = agent
	return c
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

// newOriginalHub wires a journal-backed hub for an original run with an UNNAMED root
// loop (the common case). It is newOriginalHubNamed with an empty AgentName.
func newOriginalHub(t *testing.T, js nats.JetStreamContext, fp event.ConfigFingerprint) (*hub.Hub, uuid.UUID, uuid.UUID, journal.Lease, *eventStamper) {
	t.Helper()
	return newOriginalHubNamed(t, js, fp, "")
}

// newOriginalHubNamed wires a journal-backed hub for an original run (the durable-tap
// wiring): a real SessionJournal over a freshly-acquired lease, a JournalEventAppender as
// the hub's required durable tap, and a deterministic Factory. It stamps the root
// LoopStarted with agentName — exactly what NewLoop does from cfg.AgentName on a fresh run
// — so a restore can validate the persisted root name. It returns the hub, the session/loop
// ids, the held lease, and the stamper used for direct publishes.
func newOriginalHubNamed(t *testing.T, js nats.JetStreamContext, fp event.ConfigFingerprint, agentName identity.AgentName) (*hub.Hub, uuid.UUID, uuid.UUID, journal.Lease, *eventStamper) {
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
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: primaryLoopID},
			AgentName:   agentName,
		},
	})
	return h, sessionID, primaryLoopID, lease, es
}

// buildOriginalRun drives `turns` complete turns through a REAL loop with an UNNAMED root
// loop (the common case). It is buildOriginalRunNamed with an empty AgentName.
func buildOriginalRun(t *testing.T, js nats.JetStreamContext, fp event.ConfigFingerprint, cfg loop.Config, turns int) persistedStream {
	t.Helper()
	return buildOriginalRunNamed(t, js, fp, "", cfg, turns)
}

// buildOriginalRunNamed drives `turns` COMPLETE turns through a REAL loop whose events
// persist via the journal-backed hub, stamping the root loop with agentName, then snapshots
// the committed state and stops the loop. The lease is left held for the caller to release
// (handover). This is the faithful "drive a few turns" path.
func buildOriginalRunNamed(t *testing.T, js nats.JetStreamContext, fp event.ConfigFingerprint, agentName identity.AgentName, cfg loop.Config, turns int) persistedStream {
	t.Helper()
	h, sessionID, primaryLoopID, lease, _ := newOriginalHubNamed(t, js, fp, agentName)

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

// buildComplexShapesRun produces a durable stream that exercises the structurally-
// interesting commit shapes the simpler builders omit, all inside ONE cleanly-closed
// turn (so the restore tail is the plain RestoreStarted -> RestoreDone, no interrupt):
//
//   - a multi-message StepDone group: an AIMessage carrying a tool-use reply FOLLOWED
//     by two ToolResultMessages (the exact shape foldPrimaryLoop's unit test asserts),
//     committed as a single StepDone.Messages slice, then
//   - a TurnFoldedInto user message landing MID-TURN (after that step), then
//   - a second single-AIMessage StepDone, then the TurnDone terminal.
//
// It direct-publishes through the journal-backed hub (deterministic, no goroutine
// race), so the fold sees exactly these committed bytes. The committedMsgs it reports
// is the independently-built slice the restored Snapshot must deep-equal — proving the
// journaled BYTES of a multi-message StepDone group and a TurnFoldedInto rehydrate into
// the identical AgenticMessages, not merely that the pure fold is correct.
func buildComplexShapesRun(t *testing.T, js nats.JetStreamContext, fp event.ConfigFingerprint) persistedStream {
	t.Helper()
	h, sessionID, primaryLoopID, lease, es := newOriginalHub(t, js, fp)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	turn1 := mustSessionID(t)
	step1 := mustSessionID(t)
	step2 := mustSessionID(t)

	// TurnFoldedInto is turn-scoped (SessionID+LoopID+TurnID, no StepID); StepDone is
	// step-scoped (adds StepID). Stamp each with the coordinates the replay validates.
	turnCoord := identity.Coordinates{SessionID: sessionID, LoopID: primaryLoopID, TurnID: turn1}
	stepCoord := func(stepID uuid.UUID) identity.Coordinates {
		return identity.Coordinates{SessionID: sessionID, LoopID: primaryLoopID, TurnID: turn1, StepID: stepID}
	}

	// One turn: user -> (AI tool-use + two tool results) -> folded user -> AI -> TurnDone.
	es.stamp(t, ctx, h, event.TurnStarted{Header: event.Header{Coordinates: turnCoord}, TurnIndex: 1, Message: foldUserMsg("use a tool")})
	es.stamp(t, ctx, h, event.StepDone{Header: event.Header{Coordinates: stepCoord(step1)}, Messages: content.AgenticMessages{
		aiMessage("calling tool"),
		foldToolResult("t1", "result a"),
		foldToolResult("t2", "result b"),
	}})
	es.stamp(t, ctx, h, event.TurnFoldedInto{Header: event.Header{Coordinates: turnCoord}, TurnIndex: 1, Message: foldUserMsg("folded follow-up")})
	es.stamp(t, ctx, h, event.StepDone{Header: event.Header{Coordinates: stepCoord(step2)}, Messages: content.AgenticMessages{aiMessage("final answer")}})
	es.stamp(t, ctx, h, event.TurnDone{Header: event.Header{Coordinates: turnCoord}, TurnIndex: 1, Message: aiMessage("final answer")})

	return persistedStream{
		sessionID:     sessionID,
		primaryLoopID: primaryLoopID,
		lease:         lease,
		committedMsgs: content.AgenticMessages{
			foldUserMsg("use a tool"),
			aiMessage("calling tool"),
			foldToolResult("t1", "result a"),
			foldToolResult("t2", "result b"),
			foldUserMsg("folded follow-up"),
			aiMessage("final answer"),
		},
		committedTurn: 1,
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

// TestRestoreReleasesLeaseOnShutdown proves the Phase-10 lease-release-on-teardown wiring
// for a RESTORED session: Restore holds the single-writer lease for the session lifetime,
// and a clean Shutdown releases it so a SECOND Restore can re-acquire single-writer
// ownership without waiting out the TTL. Without the release, the second Restore would
// fail *LeaseHeldError until the lease expired.
func TestRestoreReleasesLeaseOnShutdown(t *testing.T) {
	js := newEmbeddedJS(t)
	fp := FingerprintFrom(restoreCfg(&stubLLM{}, "model-x", "be helpful"))

	orig := buildOriginalRun(t, js, fp, restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful"), 1)
	handOver(t, orig.lease)

	objStore := mustObjectStore(t, js, orig.sessionID)

	// First restore acquires + holds the lease.
	s1, err := Restore(context.Background(), restoreCfg(&stubLLM{}, "model-x", "be helpful"),
		orig.sessionID, js, objStore, mustLeaseManager(t, js))
	if err != nil {
		t.Fatalf("Restore #1: %v", err)
	}
	// Clean Shutdown must release the lease.
	if err := s1.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown #1: %v", err)
	}

	// Second restore re-acquires immediately (no TTL wait) — proving the lease was released.
	s2, err := Restore(context.Background(), restoreCfg(&stubLLM{}, "model-x", "be helpful"),
		orig.sessionID, js, objStore, mustLeaseManager(t, js))
	if err != nil {
		t.Fatalf("Restore #2 (lease not released on Shutdown #1?): %v", err)
	}
	t.Cleanup(func() { _ = s2.Shutdown(context.Background()) })
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

// TestRestoreSwarmFingerprintMismatch proves the fail-secure config check end to end for
// the injected swarm-level fingerprint fields (AgentKind/RuntimeSkills/WorkspaceRoot): a
// session persisted under one set of these fields rejects a restore that injects a
// DIFFERENT value via WithConfigFingerprintFields — even when the loop.Config (model,
// system, tools) is identical. A mismatch in any one field rejects with
// *ConfigMismatchError and records a RestoreErrored, unless WithAllowConfigMismatch. This
// is what stops a session silently resuming under a different skill-trust mode or repo.
func TestRestoreSwarmFingerprintMismatch(t *testing.T) {
	persistedFields := ConfigFingerprintFields{
		AgentKind:                 "swe:orchestrator",
		RuntimeSkills:             true,
		WorkspaceRoot:             "/home/user/repo",
		NativePermissionPolicyRev: "policyrev-aaa",
	}
	diffKind := persistedFields
	diffKind.AgentKind = "swe:operator"
	diffSkills := persistedFields
	diffSkills.RuntimeSkills = false
	diffRoot := persistedFields
	diffRoot.WorkspaceRoot = "/home/user/OTHER"
	diffPolicyRev := persistedFields
	diffPolicyRev.NativePermissionPolicyRev = "policyrev-bbb"

	tests := []struct {
		name      string
		liveField ConfigFingerprintFields
	}{
		{"AgentKind differs", diffKind},
		{"RuntimeSkills differs", diffSkills},
		{"WorkspaceRoot differs", diffRoot},
		{"NativePermissionPolicyRev differs", diffPolicyRev},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			js := newEmbeddedJS(t)
			cfg := restoreCfg(&stubLLM{}, "model-x", "be helpful")
			// Persist the fingerprint the original ran under: loop-derived + the swarm fields.
			persistedFP := fingerprintWith(cfg, persistedFields)

			orig := buildOriginalRun(t, js, persistedFP,
				restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful"), 1)
			handOver(t, orig.lease)
			objStore := mustObjectStore(t, js, orig.sessionID)

			// Restore with the SAME loop.Config but a DIFFERENT injected swarm field → reject.
			s, err := Restore(context.Background(), cfg, orig.sessionID, js, objStore, mustLeaseManager(t, js),
				WithConfigFingerprintFields(tt.liveField))
			if s != nil {
				t.Fatalf("Restore returned a non-nil Session on a swarm fingerprint mismatch")
			}
			var cme *ConfigMismatchError
			if !errors.As(err, &cme) {
				t.Fatalf("Restore err = %v, want *ConfigMismatchError", err)
			}
			if !cme.Persisted.Equal(persistedFP) {
				t.Errorf("ConfigMismatchError.Persisted = %+v, want %+v", cme.Persisted, persistedFP)
			}

			// A RestoreErrored is recorded — fail-secure (no RestoreDone followed).
			tail := restoreEventTail(t, js, orig.sessionID, orig.primaryLoopID)
			if !lastIs(tail, event.RestoreErrored{}) {
				t.Errorf("restore-event tail does not end with RestoreErrored: %v", tailTypes(tail))
			}

			// The override proceeds despite the mismatch (the rejected attempt released its lease).
			s2, err := Restore(context.Background(), cfg, orig.sessionID, js, objStore, mustLeaseManager(t, js),
				WithConfigFingerprintFields(tt.liveField), WithAllowConfigMismatch())
			if err != nil {
				t.Fatalf("Restore with WithAllowConfigMismatch (swarm field) err = %v, want success", err)
			}
			// Shutdown releases the lease Restore installed, so a successor can re-acquire.
			if err := s2.Shutdown(context.Background()); err != nil {
				t.Fatalf("Shutdown override session: %v", err)
			}

			// A MATCHING injected field set restores cleanly (the agreement/compatibility path).
			s3, err := Restore(context.Background(), cfg, orig.sessionID, js, objStore, mustLeaseManager(t, js),
				WithConfigFingerprintFields(persistedFields))
			if err != nil {
				t.Fatalf("Restore with matching swarm fields err = %v, want success", err)
			}
			t.Cleanup(func() { _ = s3.Shutdown(context.Background()) })
		})
	}
}

// TestRestoreAgentNameMismatch proves the fail-secure root-loop AgentName check end to end:
// the persisted root LoopStarted's stamped name must match the configured primary's
// AgentName. A different name — AND an empty (legacy/pre-AgentName) persisted name vs a
// configured one — rejects with *AgentNameMismatchError and records a RestoreErrored (no
// session comes up), unless WithAllowConfigMismatch. A matching name restores cleanly.
func TestRestoreAgentNameMismatch(t *testing.T) {
	tests := []struct {
		name            string
		persistedAgent  identity.AgentName
		configuredAgent identity.AgentName
		wantMismatch    bool
	}{
		{name: "different name rejects", persistedAgent: "operator", configuredAgent: "reviewer", wantMismatch: true},
		{name: "empty legacy persisted vs configured rejects (not silently accepted)", persistedAgent: "", configuredAgent: "operator", wantMismatch: true},
		{name: "matching name restores cleanly", persistedAgent: "operator", configuredAgent: "operator", wantMismatch: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			js := newEmbeddedJS(t)
			fp := FingerprintFrom(restoreCfg(&stubLLM{}, "model-x", "be helpful"))

			orig := buildOriginalRunNamed(t, js, fp, tt.persistedAgent,
				restoreCfgNamed(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful", tt.persistedAgent), 1)
			handOver(t, orig.lease)

			objStore := mustObjectStore(t, js, orig.sessionID)

			cfg := restoreCfgNamed(&stubLLM{}, "model-x", "be helpful", tt.configuredAgent)
			s, err := Restore(context.Background(), cfg, orig.sessionID, js, objStore, mustLeaseManager(t, js))

			if !tt.wantMismatch {
				// Matching name (model/system/tools unchanged): the session comes up.
				if err != nil {
					t.Fatalf("Restore (matching agent name) err = %v, want success", err)
				}
				t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
				if s.SessionID != orig.sessionID {
					t.Errorf("restored SessionID = %v, want %v", s.SessionID, orig.sessionID)
				}
				return
			}

			// Mismatch rejects with the typed error; no session comes up.
			if s != nil {
				t.Fatalf("Restore returned a non-nil Session on an agent-name mismatch")
			}
			var ame *AgentNameMismatchError
			if !errors.As(err, &ame) {
				t.Fatalf("Restore err = %v, want *AgentNameMismatchError", err)
			}
			if ame.Persisted != tt.persistedAgent || ame.Configured != tt.configuredAgent {
				t.Errorf("AgentNameMismatchError carried persisted=%q configured=%q, want %q / %q",
					ame.Persisted, ame.Configured, tt.persistedAgent, tt.configuredAgent)
			}

			// A RestoreErrored is recorded (no RestoreDone followed) — fail-secure.
			tail := restoreEventTail(t, js, orig.sessionID, orig.primaryLoopID)
			if !lastIs(tail, event.RestoreErrored{}) {
				t.Errorf("restore-event tail does not end with RestoreErrored: %v", tailTypes(tail))
			}

			// The override proceeds despite the mismatch (the rejected attempt released its lease).
			s2, err := Restore(context.Background(), cfg, orig.sessionID, js, objStore, mustLeaseManager(t, js), WithAllowConfigMismatch())
			if err != nil {
				t.Fatalf("Restore with WithAllowConfigMismatch (agent name) err = %v, want success", err)
			}
			t.Cleanup(func() { _ = s2.Shutdown(context.Background()) })
		})
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

// TestRestoreComplexShapesRoundTrip is the end-to-end round-trip of the structurally-
// interesting commit shapes the other round-trips omit: a multi-message StepDone group
// (an AIMessage carrying a tool-use reply FOLLOWED by two ToolResultMessages) and a
// TurnFoldedInto user message landing MID-TURN. It direct-publishes them through the
// REAL journal, then Restore rehydrates via the EventReplayer -> fold -> loop seed, and
// the restored loop's actor-served Snapshot is asserted to deep-equal the independently-
// built expected AgenticMessages (and turnIndex). This proves the journaled BYTES of a
// multi-message StepDone group and a TurnFoldedInto fold into the IDENTICAL slice — not
// merely that the pure foldPrimaryLoop is correct.
func TestRestoreComplexShapesRoundTrip(t *testing.T) {
	js := newEmbeddedJS(t)
	fp := FingerprintFrom(restoreCfg(&stubLLM{}, "model-x", "be helpful"))

	orig := buildComplexShapesRun(t, js, fp)
	handOver(t, orig.lease)

	objStore := mustObjectStore(t, js, orig.sessionID)

	s, err := Restore(context.Background(), restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("after restore")}}, "model-x", "be helpful"),
		orig.sessionID, js, objStore, mustLeaseManager(t, js))
	if err != nil {
		t.Fatalf("Restore (complex shapes): %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// The folded history deep-equals the independently-built slice: the multi-message
	// StepDone group (AI + two tool results) and the mid-turn TurnFoldedInto user message
	// rehydrate into the exact AgenticMessages, in order — byte-for-byte.
	msgs, idx := restoredSnapshot(t, s)
	if !reflect.DeepEqual(msgs, orig.committedMsgs) {
		t.Errorf("restored (complex shapes) msgs =\n  %#v\nwant\n  %#v", msgs, orig.committedMsgs)
	}
	if idx != orig.committedTurn {
		t.Errorf("restored (complex shapes) turnIndex = %d, want %d", idx, orig.committedTurn)
	}

	// The turn closed cleanly (TurnDone), so no interrupt: tail is RestoreStarted -> RestoreDone.
	assertTail(t, restoreEventTail(t, js, orig.sessionID, orig.primaryLoopID),
		[]event.Event{event.RestoreStarted{}, event.RestoreDone{}})

	// Comes up idle: a new Submit numbers from the restored index.
	submitAndDrain(t, s, []content.Block{&content.TextBlock{Text: "keep going"}})
	if _, idx2 := restoredSnapshot(t, s); idx2 != orig.committedTurn+1 {
		t.Errorf("post-restore turnIndex = %d, want %d", idx2, orig.committedTurn+1)
	}
}

// buildRunWithSubagents drives one complete primary turn AND stamps `subagents` durable
// NON-ROOT LoopStarted events (each carrying a non-zero Header.Cause = a subagent spawn),
// then snapshots and stops the loop. It models a crashed session that had spawned
// `subagents` sub-loops over its lifetime — the durable record restore must recount to
// re-seed the cumulative spawn quota. The lease is left held for the caller to release.
func buildRunWithSubagents(t *testing.T, js nats.JetStreamContext, fp event.ConfigFingerprint, cfg loop.Config, subagents int) persistedStream {
	t.Helper()
	h, sessionID, primaryLoopID, lease, es := newOriginalHub(t, js, fp)

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

	// One complete primary turn so the stream is a realistic run.
	l.Commands <- command.UserInput{Header: command.Header{CommandID: mustSessionID(t)}, Blocks: []content.Block{&content.TextBlock{Text: "turn input"}}}
	drainSubToTerminal(t, sub)
	want := content.AgenticMessages{foldUserMsg("turn input"), aiMessage("reply")}

	// Stamp `subagents` NON-ROOT LoopStarted events directly through the journal-backed hub.
	// Each carries a non-zero Cause (parent = the primary loop), exactly what NewLoop stamps
	// on a real subagent spawn — so countSpawnedLoops counts them on restore.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := 0; i < subagents; i++ {
		subLoopID := mustSessionID(t)
		es.stamp(t, ctx, h, event.LoopStarted{Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: subLoopID},
			Cause:       identity.Cause{Coordinates: identity.Coordinates{LoopID: primaryLoopID}, Agency: identity.AgencyMachine},
		}})
	}

	snapCtx, snapCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer snapCancel()
	msgs, idx, err := l.Snapshot(snapCtx)
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

// TestRestoreRecountsSpawnQuota proves the quota SURVIVES restore: an original run that
// spawned K subagents (K durable non-root LoopStarted events) restores with spawned == K,
// so the restored session enforces the quota against the durable history — a restart never
// grants a fresh budget. Concretely: with Quota == K+1, exactly ONE more spawn is allowed
// post-restore and the next is refused with SessionLoopQuotaExceeded.
func TestRestoreRecountsSpawnQuota(t *testing.T) {
	const k = 3
	js := newEmbeddedJS(t)
	fp := FingerprintFrom(restoreCfg(&stubLLM{}, "model-x", "be helpful"))

	orig := buildRunWithSubagents(t, js, fp,
		restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful"), k)
	handOver(t, orig.lease)

	objStore := mustObjectStore(t, js, orig.sessionID)
	leases := mustLeaseManager(t, js)

	// Restore with Quota == k+1: the durable recount must set spawned == k, leaving room
	// for exactly one more spawn.
	s, err := Restore(context.Background(),
		restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("after restore")}}, "model-x", "be helpful"),
		orig.sessionID, js, objStore, leases,
		WithLimits(Limits{Depth: 10, Quota: k + 1}))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// The counter was re-seeded from the durable non-root LoopStarted events.
	s.loopsMu.RLock()
	gotSpawned := s.spawned
	s.loopsMu.RUnlock()
	if gotSpawned != k {
		t.Fatalf("restored spawned = %d, want %d (recounted from durable LoopStarted)", gotSpawned, k)
	}

	// Exactly one more spawn fits within Quota == k+1.
	if _, err := s.NewLoop(loop.Provenance{LoopID: s.PrimaryLoopID()}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("ok")}})); err != nil {
		t.Fatalf("spawn within restored quota (k+1) err = %v, want success", err)
	}

	// The NEXT spawn exceeds Quota-k (== 1 post-restore) and is refused.
	_, err = s.NewLoop(loop.Provenance{LoopID: s.PrimaryLoopID()}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("over")}}))
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionLoopQuotaExceeded {
		t.Fatalf("spawn past restored quota err = %v, want *SessionError{SessionLoopQuotaExceeded}", err)
	}
}
