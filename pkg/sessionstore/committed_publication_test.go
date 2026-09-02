package sessionstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	durablestore "github.com/looprig/sessionstore"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// committedRig is one live session's write path assembled exactly as the composition
// root assembles it — real Store, real single-writer journal, real event appender,
// real hub — plus the two consumer streams, so a test can compare what a subscriber
// receives against what the durable store actually holds.
type committedRig struct {
	store     *Store
	backend   *storage.Composite
	sessionID uuid.UUID
	hub       *hub.Hub
	compat    *hub.EventSubscription
	committed *hub.EventSubscription
}

func newCommittedRig(t *testing.T, opts ...Option) *committedRig {
	t.Helper()
	backend := memstore.New()
	store, err := Open(backend, opts...)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	sessionID := newTestUUID(t)
	lease, err := store.AcquireLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	writer, err := store.OpenJournal(context.Background(), sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal() error = %v", err)
	}
	appender, err := journal.NewJournalEventAppenderChecked(writer)
	if err != nil {
		t.Fatalf("NewJournalEventAppenderChecked() error = %v", err)
	}
	if !appender.SupportsCommittedPublicBodies() {
		t.Fatalf("the released session journal does not report committed public bodies")
	}
	h := hub.New(sessionID, hub.WithAppender(appender))
	compat, err := h.SubscribeEvents(allEventsFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents() error = %v", err)
	}
	t.Cleanup(func() { _ = compat.Close() })
	committed, err := h.SubscribeCommittedPublicEvents(allEventsFilter())
	if err != nil {
		t.Fatalf("SubscribeCommittedPublicEvents() error = %v", err)
	}
	t.Cleanup(func() { _ = committed.Close() })
	return &committedRig{store: store, backend: backend, sessionID: sessionID, hub: h, compat: compat, committed: committed}
}

func allEventsFilter() event.EventFilter {
	return event.EventFilter{Ephemeral: event.LoopScope{All: true}, Enduring: event.LoopScope{All: true}}
}

// publicPage reads what the durable store would serve a PUBLIC reader: the canonical
// bodies and sequences, with every private record absent.
func (r *committedRig) publicPage(t *testing.T) []coresessionwire.JournalEvent {
	t.Helper()
	page, err := r.store.durable.ReadPublicJournal(context.Background(), durablestore.ReadPublicJournalRequest{
		TenantID: harnessTenantID, SessionID: harnessSessionID(r.sessionID), Limit: 1000,
	})
	if err != nil {
		t.Fatalf("ReadPublicJournal() error = %v", err)
	}
	return page.Events
}

func (r *committedRig) publicEvent(t *testing.T, seq uint64) coresessionwire.JournalEvent {
	t.Helper()
	for _, stored := range r.publicPage(t) {
		if stored.JournalSeq == seq {
			return stored
		}
	}
	t.Fatalf("public read has no event at sequence %d", seq)
	return coresessionwire.JournalEvent{}
}

func recvWithin(t *testing.T, sub *hub.EventSubscription) event.Delivery {
	t.Helper()
	select {
	case d, open := <-sub.Events():
		if !open {
			t.Fatalf("subscription closed: %v", sub.Err())
		}
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery within the deadline")
		return event.Delivery{}
	}
}

func (r *committedRig) sessionStarted(t *testing.T) event.SessionStarted {
	t.Helper()
	return event.SessionStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: r.sessionID},
		EventID:     newTestUUID(t),
		CreatedAt:   time.Now().UTC(),
	}}
}

// TestPersistedPublicBodyEqualsLivePublicationBody is the byte-identity contract. The
// SessionStore public read, the compatibility Hub delivery, and the segregated
// Host-facing committed capability must all carry the SAME bytes under the SAME
// EventID/JournalSeq/CoveredThrough for one event — because a client that joins a
// durable tail to a live stream dedupes on (sequence, event id) and would otherwise
// render two different things for one event with no error raised anywhere.
func TestPersistedPublicBodyEqualsLivePublicationBody(t *testing.T) {
	t.Run("public enduring event", func(t *testing.T) {
		r := newCommittedRig(t)
		ev := r.sessionStarted(t)
		if err := r.hub.PublishEvent(context.Background(), ev); err != nil {
			t.Fatalf("PublishEvent() error = %v", err)
		}
		live := recvWithin(t, r.compat)
		host := recvWithin(t, r.committed)
		stored := r.publicEvent(t, live.JournalSeq)

		if !bytes.Equal(live.PublicBody, stored.Body) {
			t.Errorf("live body = %s, want the stored body %s", live.PublicBody, stored.Body)
		}
		if !bytes.Equal(host.PublicBody, stored.Body) {
			t.Errorf("host body = %s, want the stored body %s", host.PublicBody, stored.Body)
		}
		if live.EventID != string(stored.EventID) || host.EventID != string(stored.EventID) {
			t.Errorf("event ids = live:%q host:%q, want stored %q", live.EventID, host.EventID, stored.EventID)
		}
		if live.JournalSeq != stored.JournalSeq || host.JournalSeq != stored.JournalSeq {
			t.Errorf("sequences = live:%d host:%d, want stored %d", live.JournalSeq, host.JournalSeq, stored.JournalSeq)
		}
		if live.CoveredThrough != stored.JournalSeq || host.CoveredThrough != stored.JournalSeq {
			t.Errorf("coverage = live:%d host:%d, want %d", live.CoveredThrough, host.CoveredThrough, stored.JournalSeq)
		}
		if live.EventID != ev.EventID.String() {
			t.Errorf("committed EventID = %q, want the event's own id %q", live.EventID, ev.EventID)
		}
	})

	// The byte-identity assertions above would be unfalsifiable on an event whose
	// Harness-native replay body and canonical public body happen to be identical, so
	// this case uses one where they provably differ: GateResolved carries the raw
	// answer values in its runtime body and must never carry them in its public one.
	t.Run("runtime and public bodies differ", func(t *testing.T) {
		r := newCommittedRig(t)
		ev := event.GateResolved{
			Header: event.Header{
				Coordinates: identity.Coordinates{
					SessionID: r.sessionID, LoopID: newTestUUID(t),
					TurnID: newTestUUID(t), StepID: newTestUUID(t),
				},
				EventID:   newTestUUID(t),
				CreatedAt: time.Now().UTC(),
				Cause:     identity.Cause{CommandID: newTestUUID(t)},
			},
			GateID:   gate.ID(newTestUUID(t)),
			Resolver: gate.ResolverLoop,
			Reason:   gate.CloseAnswered,
			Action:   gate.FormActionAccept,
			Source:   gate.ResponseSource{Kind: gate.ResponseFromUser},
			Audit:    gate.FormAudit{Values: map[string]string{"secret": "runtime-only-marker"}},
		}
		if err := r.hub.PublishEvent(context.Background(), ev); err != nil {
			t.Fatalf("PublishEvent() error = %v", err)
		}
		host := recvWithin(t, r.committed)
		runtimeBody, err := event.MarshalEvent(ev)
		if err != nil {
			t.Fatalf("MarshalEvent() error = %v", err)
		}
		if bytes.Equal(host.PublicBody, runtimeBody) {
			t.Fatalf("delivered body equals the Harness-native runtime body; this case cannot detect a swap")
		}
		if bytes.Contains(host.PublicBody, []byte("runtime-only-marker")) {
			t.Errorf("delivered body leaks the runtime-only answer value: %s", host.PublicBody)
		}
		stored := r.publicEvent(t, host.JournalSeq)
		if !bytes.Equal(host.PublicBody, stored.Body) {
			t.Errorf("delivered body = %s, want the stored public body %s", host.PublicBody, stored.Body)
		}
	})

	// NOTE on swap-sensitivity: this case cannot detect a runtime-for-public body
	// swap on its own, and no rearrangement of it could. Only an event that REDACTS
	// distinguishes the two encodings, and the hub derives a session edge only from
	// TurnStarted/LoopIdle/TurnFoldedInto/InputCancelled (see hub.activeMutation) —
	// none of which redact, and neither do SessionActive/SessionIdle themselves. All
	// four encode byte-identically in both forms, verified directly. The swap guard
	// is therefore the "runtime and public bodies differ" case above, which is
	// sufficient because the projection that could be swapped is shared code
	// (sessionJournal.frame), not per-path. What THIS case falsifies is per-path
	// misrouting: a derived event carrying another append's sequence, id, or bytes.
	t.Run("derived session event", func(t *testing.T) {
		r := newCommittedRig(t)
		loopID, turnID := newTestUUID(t), newTestUUID(t)
		start := event.TurnStarted{
			Header: event.Header{
				Coordinates: identity.Coordinates{SessionID: r.sessionID, LoopID: loopID, TurnID: turnID},
				EventID:     newTestUUID(t),
				CreatedAt:   time.Now().UTC(),
				Cause:       identity.Cause{CommandID: newTestUUID(t), Agency: identity.AgencyUser},
			},
			TurnIndex: 1,
		}
		if err := r.hub.PublishEvent(context.Background(), start); err != nil {
			t.Fatalf("PublishEvent() error = %v", err)
		}
		trigger := recvWithin(t, r.committed)
		derived := recvWithin(t, r.committed)
		if _, ok := derived.Event.(event.SessionActive); !ok {
			t.Fatalf("second committed delivery = %T, want event.SessionActive", derived.Event)
		}
		for name, d := range map[string]event.Delivery{"trigger": trigger, "derived": derived} {
			stored := r.publicEvent(t, d.JournalSeq)
			if !bytes.Equal(d.PublicBody, stored.Body) {
				t.Errorf("%s body = %s, want the stored body %s", name, d.PublicBody, stored.Body)
			}
			if d.EventID != string(stored.EventID) {
				t.Errorf("%s EventID = %q, want stored %q", name, d.EventID, stored.EventID)
			}
			if d.CoveredThrough != d.JournalSeq {
				t.Errorf("%s CoveredThrough = %d, want %d", name, d.CoveredThrough, d.JournalSeq)
			}
		}
		if derived.JournalSeq <= trigger.JournalSeq {
			t.Errorf("derived sequence %d must follow the trigger's %d", derived.JournalSeq, trigger.JournalSeq)
		}
	})

	t.Run("private internal event closes the gap without revealing it", func(t *testing.T) {
		r := newCommittedRig(t)
		for range 2 {
			private := replayHustleStarted(t, r.sessionID)
			if err := r.hub.PublishInternalEventChecked(context.Background(), private); err != nil {
				t.Fatalf("PublishInternalEventChecked() error = %v", err)
			}
		}
		if got := len(r.compat.Events()); got != 0 {
			t.Fatalf("compat stream buffered %d deliveries for private events, want 0", got)
		}
		if got := len(r.committed.Events()); got != 0 {
			t.Fatalf("committed stream buffered %d deliveries for private events, want 0", got)
		}
		if got := r.publicPage(t); len(got) != 0 {
			t.Fatalf("public read returned %d events for a private-only log, want 0", len(got))
		}

		if err := r.hub.PublishEvent(context.Background(), r.sessionStarted(t)); err != nil {
			t.Fatalf("PublishEvent() error = %v", err)
		}
		host := recvWithin(t, r.committed)
		// The two private records occupy the sequences immediately before this one.
		// The watermark closes them — a public reader is caught up through its own
		// committed sequence — and does so without the delivery carrying anything
		// about what those records were.
		if host.JournalSeq <= 2 {
			t.Fatalf("public event landed at sequence %d, want it to follow the fence and both private records", host.JournalSeq)
		}
		switch {
		case host.CoveredThrough < host.JournalSeq:
			t.Errorf("CoveredThrough = %d, want %d: a lower watermark leaves the private sequences permanently unclosed",
				host.CoveredThrough, host.JournalSeq)
		case host.CoveredThrough > host.JournalSeq:
			t.Errorf("CoveredThrough = %d, want %d: a watermark past this append claims coverage it did not earn",
				host.CoveredThrough, host.JournalSeq)
		}
		if page := r.publicPage(t); len(page) != 1 || page[0].JournalSeq != host.JournalSeq {
			t.Errorf("public read = %+v, want only the one public event at sequence %d", page, host.JournalSeq)
		}
		if bytes.Contains(host.PublicBody, []byte("private.audit")) || bytes.Contains(host.PublicBody, []byte("hustle")) {
			t.Errorf("public body leaks withheld private record content: %s", host.PublicBody)
		}
	})

	t.Run("ephemeral event", func(t *testing.T) {
		r := newCommittedRig(t)
		ephemeral := event.TokenDelta{
			Header: event.Header{
				Coordinates: identity.Coordinates{SessionID: r.sessionID, LoopID: newTestUUID(t), TurnID: newTestUUID(t)},
				EventID:     newTestUUID(t),
				CreatedAt:   time.Now().UTC(),
			},
			Chunk: &content.TextChunk{Text: "hello"},
		}
		if err := r.hub.PublishEvent(context.Background(), ephemeral); err != nil {
			t.Fatalf("PublishEvent(ephemeral) error = %v", err)
		}
		if err := r.hub.PublishEvent(context.Background(), r.sessionStarted(t)); err != nil {
			t.Fatalf("PublishEvent(enduring) error = %v", err)
		}
		live := recvWithin(t, r.compat)
		if _, ok := live.Event.(event.TokenDelta); !ok {
			t.Fatalf("compat first delivery = %T, want event.TokenDelta", live.Event)
		}
		if live.JournalSeq != 0 || live.EventID != "" || live.PublicBody != nil || live.CoveredThrough != 0 {
			t.Errorf("ephemeral delivery = %+v, want no sequence, id, body, or coverage", live)
		}
		host := recvWithin(t, r.committed)
		if _, ok := host.Event.(event.SessionStarted); !ok {
			t.Fatalf("committed first delivery = %T, want the enduring event (ephemerals are not on this stream)", host.Event)
		}
		if !host.Committed() {
			t.Errorf("committed delivery = %+v, want committed bytes", host)
		}
	})

	t.Run("deduplicated append", func(t *testing.T) {
		r := newCommittedRig(t)
		ev := r.sessionStarted(t)
		if err := r.hub.PublishEvent(context.Background(), ev); err != nil {
			t.Fatalf("first PublishEvent() error = %v", err)
		}
		first := recvWithin(t, r.committed)
		firstBody := bytes.Clone(first.PublicBody)
		if err := r.hub.PublishEvent(context.Background(), ev); err != nil {
			t.Fatalf("redelivered PublishEvent() error = %v", err)
		}
		if got := len(r.committed.Events()); got != 0 {
			t.Errorf("committed stream buffered %d deliveries for a deduplicated retry, want 0", got)
		}
		if got := len(r.compat.Events()); got != 1 {
			t.Errorf("compat stream buffered %d deliveries, want the single original", got)
		}
		page := r.publicPage(t)
		if len(page) != 1 {
			t.Fatalf("public read returned %d events after a deduplicated retry, want 1", len(page))
		}
		if !bytes.Equal(page[0].Body, firstBody) {
			t.Errorf("stored body = %s after the retry, want the original %s", page[0].Body, firstBody)
		}
		if page[0].JournalSeq != first.JournalSeq {
			t.Errorf("stored sequence = %d, want the original %d", page[0].JournalSeq, first.JournalSeq)
		}
	})

	t.Run("mutation isolation", func(t *testing.T) {
		r := newCommittedRig(t)
		if err := r.hub.PublishEvent(context.Background(), r.sessionStarted(t)); err != nil {
			t.Fatalf("PublishEvent() error = %v", err)
		}
		live := recvWithin(t, r.compat)
		host := recvWithin(t, r.committed)
		want := bytes.Clone(host.PublicBody)
		if len(live.PublicBody) == 0 {
			t.Fatal("compat delivery carried no public body")
		}
		for i := range live.PublicBody {
			live.PublicBody[i] = 'X'
		}
		if !bytes.Equal(host.PublicBody, want) {
			t.Errorf("host body = %s after a peer's in-place edit, want %s", host.PublicBody, want)
		}
		if stored := r.publicEvent(t, host.JournalSeq); !bytes.Equal(stored.Body, want) {
			t.Errorf("stored body = %s after a subscriber's in-place edit, want %s", stored.Body, want)
		}
	})

	// The offload leg: a public body larger than the configured threshold is stored
	// as a verified immutable object rather than inline, and the bytes handed to the
	// live delivery must still be the bytes the public read serves. The threshold is
	// lowered because at the DEFAULT threshold this leg is not exercisable — see the
	// caveat on event.Delivery.PublicBody.
	t.Run("offloaded public body", func(t *testing.T) {
		r := newCommittedRig(t, WithOffloadThreshold(64<<10))
		if got := len(blobBodies(t, r)); got != 0 {
			t.Fatalf("session already had %d offloaded objects before the publish", got)
		}
		ev := event.StepDone{
			Header: event.Header{
				Coordinates: identity.Coordinates{
					SessionID: r.sessionID, LoopID: newTestUUID(t),
					TurnID: newTestUUID(t), StepID: newTestUUID(t),
				},
				EventID:   newTestUUID(t),
				CreatedAt: time.Now().UTC(),
			},
			Messages: content.AgenticMessages{
				&content.AIMessage{Message: content.Message{
					Role:   content.RoleAssistant,
					Blocks: []content.Block{&content.TextBlock{Text: strings.Repeat("x", 100*1024)}},
				}},
			},
		}
		if err := r.hub.PublishEvent(context.Background(), ev); err != nil {
			t.Fatalf("PublishEvent() error = %v", err)
		}
		host := recvWithin(t, r.committed)
		live := recvWithin(t, r.compat)

		// Prove the public body actually offloaded, and that the bytes delivered live
		// are the ones sitting in the object store. Counting objects alone would not
		// do it: this event's RUNTIME body is over the threshold too, so the append
		// publishes two objects, and an earlier draft of this assertion expected one
		// and reported two — which is why it checks contents, not arithmetic.
		blobs := blobBodies(t, r)
		if len(blobs) != 2 {
			t.Fatalf("published %d objects, want 2 (the offloaded public and runtime bodies)", len(blobs))
		}
		var offloadedPublic int
		for _, blob := range blobs {
			if bytes.Equal(blob, host.PublicBody) {
				offloadedPublic++
			}
		}
		// At least one, not exactly one: StepDone does not redact, so its public and
		// runtime bodies are byte-identical here and BOTH objects match. (An earlier
		// draft asserted "exactly 1" and reported 2 — twice, for two different
		// reasons. The discriminating assertion is the public read below, which
		// resolves the stored reference through the object store.)
		if offloadedPublic == 0 {
			t.Fatalf("none of the %d offloaded objects hold the delivered public body", len(blobs))
		}
		if len(host.PublicBody) <= 64<<10 {
			t.Fatalf("public body is %d bytes, at or below the threshold: nothing was offloaded", len(host.PublicBody))
		}
		stored := r.publicEvent(t, host.JournalSeq)
		if !bytes.Equal(host.PublicBody, stored.Body) {
			t.Errorf("host body (%d bytes) differs from the offloaded stored body (%d bytes)",
				len(host.PublicBody), len(stored.Body))
		}
		if !bytes.Equal(live.PublicBody, stored.Body) {
			t.Errorf("live body (%d bytes) differs from the offloaded stored body (%d bytes)",
				len(live.PublicBody), len(stored.Body))
		}
		if host.CoveredThrough != host.JournalSeq {
			t.Errorf("CoveredThrough = %d, want %d", host.CoveredThrough, host.JournalSeq)
		}
	})

	t.Run("overflow", func(t *testing.T) {
		r := newCommittedRig(t)
		for range 260 {
			if err := r.hub.PublishEvent(context.Background(), r.sessionStarted(t)); err != nil {
				t.Fatalf("PublishEvent() error = %v", err)
			}
		}
		var loss *hub.SubscriptionLossError
		if !errors.As(r.committed.Err(), &loss) {
			t.Fatalf("committed Err() = %T %v, want *hub.SubscriptionLossError", r.committed.Err(), r.committed.Err())
		}
		if loss.DroppedClass != event.Enduring {
			t.Errorf("DroppedClass = %v, want Enduring", loss.DroppedClass)
		}
	})
}

// blobBodies reads every offloaded object this session has published. A test uses it
// to prove an offload actually happened AND that a specific body is what landed in
// the object store, rather than inferring either from a size.
func blobBodies(t *testing.T, r *committedRig) [][]byte {
	t.Helper()
	names, err := r.backend.Blobs.List(context.Background(), ledgerName(r.sessionID)+blobsInfix)
	if err != nil {
		t.Fatalf("Blobs.List() error = %v", err)
	}
	bodies := make([][]byte, 0, len(names))
	for _, name := range names {
		reader, err := r.backend.Blobs.Get(context.Background(), name)
		if err != nil {
			t.Fatalf("Blobs.Get(%q) error = %v", name, err)
		}
		body, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatalf("read blob %q: %v", name, err)
		}
		bodies = append(bodies, body)
	}
	return bodies
}
