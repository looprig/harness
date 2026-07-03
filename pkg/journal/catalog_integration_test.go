//go:build integration

package journal_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/uuid"
	"github.com/nats-io/nats.go"
)

// catalogTestClock returns a clock fixed at t so LastActiveAt bumps are deterministic.
func catalogTestClock(t time.Time) journal.CatalogClock { return func() time.Time { return t } }

// newCatalog builds a Catalog over the embedded JetStream with a unique bucket (derived
// from the test name) so parallel tests never collide, an injected clock, and a
// replayer for repair. now defaults to time.Now when zero.
func newCatalog(t *testing.T, js nats.JetStreamContext, now time.Time) *journal.Catalog {
	t.Helper()
	opts := []journal.CatalogOption{
		journal.WithCatalogBucket("catalog_" + sanitizeBucket(t.Name())),
		journal.WithCatalogReplayer(journal.NewEventReplayer(js, nil)),
	}
	if !now.IsZero() {
		opts = append(opts, journal.WithCatalogClock(catalogTestClock(now)))
	}
	c, err := journal.NewCatalog(js, opts...)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	return c
}

// sanitizeBucket maps a test name to a valid KV bucket token (only [-/_=.a-zA-Z0-9]).
// '/' from subtests becomes '_'.
func sanitizeBucket(name string) string {
	b := []byte(name)
	for i, c := range b {
		if c == '/' || c == ' ' {
			b[i] = '_'
		}
	}
	return string(b)
}

// mustJournal binds a session journal (taking its lease) and returns it ready to append.
func mustJournal(t *testing.T, js nats.JetStreamContext, sid uuid.UUID) journal.SessionJournal {
	t.Helper()
	lease := mustAcquireLease(t, js, sid)
	j, err := journal.NewSessionJournal(js, sid, lease)
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}
	return j
}

// userMessage builds a single-text-block user message for a TurnStarted.
func userMessage(text string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{
		Role:   content.RoleUser,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

// TestCatalogUpdateOnEventIntegration drives the inline post-append catalog update over
// a real embedded JetStream: it appends a SessionStarted / first TurnStarted / LoopStarted
// / StepDone / RestoreDone / SessionStopped through the appender (which notifies the
// catalog best-effort after each successful append) and asserts the resulting SessionMeta
// in KV reflects the full event→field mapping.
func TestCatalogUpdateOnEventIntegration(t *testing.T) {
	_, js := newEmbeddedJS(t)
	sid := seedUUID(0x80)
	now := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	created := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	fp := event.ConfigFingerprint{AgentKind: "primary", ModelID: "model-x", SystemPromptRev: "rev1"}

	cat := newCatalog(t, js, now)
	j := mustJournal(t, js, sid)
	app := journal.NewJournalEventAppender(j, journal.WithCatalog(cat))

	lid := seedUUID(0x81)
	tid := seedUUID(0x82)
	tid2 := seedUUID(0x83)
	stepID := seedUUID(0x84)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	events := []event.Event{
		event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: seedUUID(0x01), CreatedAt: created}, Config: fp},
		event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid}, EventID: seedUUID(0x02)}},
		event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid}, EventID: seedUUID(0x03)}, Message: userMessage("Fix the login bug\nmore detail")},
		event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID}, EventID: seedUUID(0x04)}},
		event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid2}, EventID: seedUUID(0x05)}, Message: userMessage("Second turn message")},
		event.RestoreDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: seedUUID(0x06)}},
	}
	for i, ev := range events {
		if err := app.AppendEvent(ctx, ev); err != nil {
			t.Fatalf("AppendEvent #%d (%T): %v", i, ev, err)
		}
	}

	metas, err := cat.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("ListSessions returned %d metas, want 1", len(metas))
	}
	meta := metas[0]

	if meta.SessionID != sid {
		t.Errorf("SessionID = %v, want %v", meta.SessionID, sid)
	}
	if !meta.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", meta.CreatedAt, created)
	}
	if meta.Title != "Fix the login bug" {
		t.Errorf("Title = %q, want first turn's first line", meta.Title)
	}
	if !meta.LastActiveAt.Equal(now) {
		t.Errorf("LastActiveAt = %v, want %v (bumped by clock)", meta.LastActiveAt, now)
	}
	if meta.Status != journal.StatusActive {
		t.Errorf("Status = %q, want active", meta.Status)
	}
	if meta.AgentKind != "primary" {
		t.Errorf("AgentKind = %q, want primary (passthrough from fingerprint)", meta.AgentKind)
	}
	if meta.LoopCount != 2 {
		t.Errorf("LoopCount = %d, want 2 (primary + one LoopStarted)", meta.LoopCount)
	}
	if !meta.ConfigFingerprint.Equal(fp) {
		t.Errorf("ConfigFingerprint = %+v, want %+v", meta.ConfigFingerprint, fp)
	}

	// Now stop the session and assert Status flips.
	if err := app.AppendEvent(ctx, event.SessionStopped{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: seedUUID(0x07)}}); err != nil {
		t.Fatalf("AppendEvent SessionStopped: %v", err)
	}
	stopped, err := cat.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions after stop: %v", err)
	}
	if stopped[0].Status != journal.StatusStopped {
		t.Errorf("Status after SessionStopped = %q, want stopped", stopped[0].Status)
	}
}

// TestCatalogWriteFailureDoesNotFailAppend proves the catalog is the soft tail: even when
// the catalog KV is gone, AppendEvent returns nil AND the event is durably in the stream.
// The failure is forced by deleting the catalog's KV bucket out from under the bound
// handle, so its next read/write errors — while the journal's append (a separate stream)
// succeeds. The injected logger records the swallowed error.
func TestCatalogWriteFailureDoesNotFailAppend(t *testing.T) {
	_, js := newEmbeddedJS(t)
	sid := seedUUID(0x90)

	log := &recordingCatalogLogger{}
	cat, err := journal.NewCatalog(js,
		journal.WithCatalogBucket("catalog_fail_"+sanitizeBucket(t.Name())),
		journal.WithCatalogLogger(log),
	)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}

	j := mustJournal(t, js, sid)
	app := journal.NewJournalEventAppender(j, journal.WithCatalog(cat))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Delete the catalog's KV bucket so its next Put fails — proving the swallow path.
	if err := js.DeleteKeyValue("catalog_fail_" + sanitizeBucket(t.Name())); err != nil {
		t.Fatalf("DeleteKeyValue: %v", err)
	}

	ev := event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: seedUUID(0x01)}}
	if err := app.AppendEvent(ctx, ev); err != nil {
		t.Fatalf("AppendEvent = %v, want nil despite catalog write failure", err)
	}

	// The append must have landed durably in the stream: a replay sees it.
	if !streamHasEvent(t, js, sid, seedUUID(0x01)) {
		t.Error("event not found in stream; the durable append must succeed even when the catalog write fails")
	}
	// And the catalog failure must have been logged (swallowed), never returned.
	if len(log.errs) == 0 {
		t.Error("expected the catalog write failure to be logged (best-effort swallow)")
	}
}

// recordingCatalogLogger captures swallowed best-effort catalog failures.
type recordingCatalogLogger struct{ errs []error }

func (l *recordingCatalogLogger) CatalogUpdateFailed(err error) { l.errs = append(l.errs, err) }

// streamHasEvent reports whether the session's stream carries an event with the given id
// by replaying its events (the authoritative source-of-truth read).
func streamHasEvent(t *testing.T, js nats.JetStreamContext, sid, eventID uuid.UUID) bool {
	t.Helper()
	r := journal.NewEventReplayer(js, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cur, err := r.Open(ctx, journal.ReplayRequest{SessionID: sid, From: journal.Beginning()})
	if err != nil {
		t.Fatalf("replay Open: %v", err)
	}
	defer func() { _ = cur.Close() }()
	for {
		ev, _, nerr := cur.Next(ctx)
		if errors.Is(nerr, io.EOF) {
			return false
		}
		if nerr != nil {
			t.Fatalf("replay Next: %v", nerr)
		}
		if ev.EventHeader().EventID == eventID {
			return true
		}
	}
}

// TestListSessionsReadsKVOnly proves ListSessions reads purely from the KV bucket: it
// opens NO stream consumer (the per-session stream's consumer count is unchanged across
// the call) and returns the catalog even though no replay ever ran.
func TestListSessionsReadsKVOnly(t *testing.T) {
	_, js := newEmbeddedJS(t)
	sid := seedUUID(0xA0)
	now := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)

	cat := newCatalog(t, js, now)
	j := mustJournal(t, js, sid)
	app := journal.NewJournalEventAppender(j, journal.WithCatalog(cat))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.AppendEvent(ctx, event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: seedUUID(0x01)}, Config: event.ConfigFingerprint{ModelID: "m"}}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	// Snapshot the stream's consumer count before listing.
	before := streamConsumerCount(t, js, sid)

	metas, err := cat.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(metas) != 1 || metas[0].SessionID != sid {
		t.Fatalf("ListSessions = %+v, want one entry for %v", metas, sid)
	}

	after := streamConsumerCount(t, js, sid)
	if after != before {
		t.Errorf("stream consumer count changed %d -> %d; ListSessions must open NO consumer", before, after)
	}
}

// streamConsumerCount returns the number of consumers currently bound to a session's
// stream — the proof surface for "no consumer opened" assertions.
func streamConsumerCount(t *testing.T, js nats.JetStreamContext, sid uuid.UUID) int {
	t.Helper()
	info, err := js.StreamInfo(journal.StreamName(sid))
	if err != nil {
		t.Fatalf("StreamInfo: %v", err)
	}
	return info.State.Consumers
}

// TestRepairCatalogRebuildsFromStream proves the catalog is a rebuildable cache: after a
// session is indexed and its KV entry is DELETED, RepairCatalog reconstructs an identical
// SessionMeta by scanning the session's stream (the source of truth) — CreatedAt, Title,
// Status, LoopCount, ConfigFingerprint all match the original.
func TestRepairCatalogRebuildsFromStream(t *testing.T) {
	_, js := newEmbeddedJS(t)
	sid := seedUUID(0xB0)
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	created := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	fp := event.ConfigFingerprint{AgentKind: "primary", ModelID: "model-z"}

	cat := newCatalog(t, js, now)
	j := mustJournal(t, js, sid)
	app := journal.NewJournalEventAppender(j, journal.WithCatalog(cat))

	lid := seedUUID(0xB1)
	tid := seedUUID(0xB2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	events := []event.Event{
		event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: seedUUID(0x01), CreatedAt: created}, Config: fp},
		event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid}, EventID: seedUUID(0x02)}},
		event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid}, EventID: seedUUID(0x03)}, Message: userMessage("Repair me")},
		event.SessionStopped{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: seedUUID(0x04)}},
	}
	for i, ev := range events {
		if err := app.AppendEvent(ctx, ev); err != nil {
			t.Fatalf("AppendEvent #%d: %v", i, ev)
		}
	}

	before, err := cat.ListSessions(ctx)
	if err != nil || len(before) != 1 {
		t.Fatalf("ListSessions before delete = %+v, %v; want one entry", before, err)
	}
	orig := before[0]

	// Wipe the catalog entry so the catalog is now missing for this session.
	if err := deleteCatalogEntry(t, js, "catalog_"+sanitizeBucket(t.Name()), sid); err != nil {
		t.Fatalf("delete catalog entry: %v", err)
	}
	gone, err := cat.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions after delete: %v", err)
	}
	if len(gone) != 0 {
		t.Fatalf("ListSessions after delete = %d entries, want 0", len(gone))
	}

	// Repair rebuilds it from the stream.
	repaired, err := cat.RepairCatalog(ctx, sid)
	if err != nil {
		t.Fatalf("RepairCatalog: %v", err)
	}
	if repaired.SessionID != orig.SessionID {
		t.Errorf("repaired SessionID = %v, want %v", repaired.SessionID, orig.SessionID)
	}
	if !repaired.CreatedAt.Equal(orig.CreatedAt) {
		t.Errorf("repaired CreatedAt = %v, want %v", repaired.CreatedAt, orig.CreatedAt)
	}
	if repaired.Title != orig.Title {
		t.Errorf("repaired Title = %q, want %q", repaired.Title, orig.Title)
	}
	if repaired.Status != orig.Status {
		t.Errorf("repaired Status = %q, want %q", repaired.Status, orig.Status)
	}
	if repaired.LoopCount != orig.LoopCount {
		t.Errorf("repaired LoopCount = %d, want %d", repaired.LoopCount, orig.LoopCount)
	}
	if !repaired.ConfigFingerprint.Equal(orig.ConfigFingerprint) {
		t.Errorf("repaired ConfigFingerprint = %+v, want %+v", repaired.ConfigFingerprint, orig.ConfigFingerprint)
	}

	// And it is persisted: a fresh list shows the repaired entry.
	after, err := cat.ListSessions(ctx)
	if err != nil || len(after) != 1 {
		t.Fatalf("ListSessions after repair = %+v, %v; want one entry", after, err)
	}
}

// deleteCatalogEntry removes a session's catalog KV entry directly (simulating a lost or
// wiped cache entry) so RepairCatalog has something to rebuild.
func deleteCatalogEntry(t *testing.T, js nats.JetStreamContext, bucket string, sid uuid.UUID) error {
	t.Helper()
	kv, err := js.KeyValue(bucket)
	if err != nil {
		return err
	}
	return kv.Delete(sid.String())
}

// TestRepairCatalogEmptySession proves repairing a session whose stream carries no
// SessionStarted yields a typed *EmptySessionError (nothing to index).
func TestRepairCatalogEmptySession(t *testing.T) {
	_, js := newEmbeddedJS(t)
	sid := seedUUID(0xC0)

	cat := newCatalog(t, js, time.Time{})
	// Bind a journal so the stream exists (it carries only the opening LeaseFence — no
	// SessionStarted event), then repair.
	_ = mustJournal(t, js, sid)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := cat.RepairCatalog(ctx, sid)
	if err == nil {
		t.Fatal("RepairCatalog on a session with no SessionStarted = nil, want *EmptySessionError")
	}
	var ese *journal.EmptySessionError
	if !errors.As(err, &ese) {
		t.Fatalf("error %v is not *EmptySessionError", err)
	}
}
