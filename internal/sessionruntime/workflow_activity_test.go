package sessionruntime

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/storage/memstore"
)

func validWorkflowActivityMetadata(sessionID uuid.UUID) tool.WorkflowActivityMetadata {
	return tool.WorkflowActivityMetadata{
		EventID:         uuid.UUID{11},
		SessionID:       sessionID,
		RunID:           uuid.UUID{12},
		WorkflowName:    "source_document_extract",
		WorkflowVersion: "v1",
		Kind:            string(event.WorkflowActivityRunStarted),
		Status:          string(event.WorkflowRunStatusRunning),
		TotalVertices:   2,
		OccurredAt:      time.Date(2026, time.August, 9, 14, 0, 0, 0, time.UTC),
		Message:         "workflow started",
	}
}

func TestWorkflowActivityPublisherRejectsInvalidDependencies(t *testing.T) {
	t.Parallel()

	sessionID := uuid.UUID{1}
	h := hub.New(sessionID)
	if _, err := newCheckedWorkflowActivityPublisher(uuid.UUID{}, h, nil); err == nil {
		t.Fatal("zero session id constructor error = nil")
	}
	if _, err := newCheckedWorkflowActivityPublisher(sessionID, nil, nil); err == nil {
		t.Fatal("nil hub constructor error = nil")
	}
}

func TestWorkflowActivityPublisherPublishesValidatedSessionEvent(t *testing.T) {
	t.Parallel()

	sessionID := uuid.UUID{21}
	app := &dedupAppender{}
	h := hub.New(sessionID, hub.WithAppender(app))
	sub := processSubscribeAll(t, h)
	defer sub.Close()

	now := time.Date(2026, time.August, 9, 15, 0, 0, 0, time.UTC)
	pub, err := newCheckedWorkflowActivityPublisher(sessionID, h, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newCheckedWorkflowActivityPublisher() = %v", err)
	}
	metadata := validWorkflowActivityMetadata(sessionID)
	if err := pub.PublishWorkflowActivity(context.Background(), metadata); err != nil {
		t.Fatalf("PublishWorkflowActivity() = %v", err)
	}

	delivered := recvProcessEvent(t, sub)
	got, ok := delivered.(event.WorkflowActivity)
	if !ok {
		t.Fatalf("published event = %T, want WorkflowActivity", delivered)
	}
	if got.EventID != metadata.EventID || got.RunID != metadata.RunID || got.WorkflowName != metadata.WorkflowName || got.Kind != event.WorkflowActivityKind(metadata.Kind) {
		t.Fatalf("published activity = %#v, want metadata-preserving event", got)
	}
	if !got.CreatedAt.Equal(metadata.OccurredAt) {
		t.Fatalf("CreatedAt = %v, want stable OccurredAt %v", got.CreatedAt, metadata.OccurredAt)
	}
	if got.EventVisibility != event.Public || got.Scope() != event.ScopeSession || got.Class() != event.Enduring {
		t.Fatalf("published contract = visibility:%v scope:%v class:%v", got.EventVisibility, got.Scope(), got.Class())
	}
}

func TestWorkflowActivityPublisherRejectsForeignSessionAndInvalidBody(t *testing.T) {
	t.Parallel()

	sessionID := uuid.UUID{31}
	app := &dedupAppender{}
	h := hub.New(sessionID, hub.WithAppender(app))
	pub, err := newCheckedWorkflowActivityPublisher(sessionID, h, nil)
	if err != nil {
		t.Fatalf("newCheckedWorkflowActivityPublisher() = %v", err)
	}

	foreign := validWorkflowActivityMetadata(uuid.UUID{32})
	var ownerErr *WorkflowActivityOwnerMismatchError
	if err := pub.PublishWorkflowActivity(context.Background(), foreign); !errors.As(err, &ownerErr) {
		t.Fatalf("foreign PublishWorkflowActivity() = %T %v, want owner mismatch", err, err)
	}
	if app.callCount() != 0 {
		t.Fatalf("appender calls after foreign publish = %d, want 0", app.callCount())
	}

	invalid := validWorkflowActivityMetadata(sessionID)
	invalid.RunID = uuid.UUID{}
	if err := pub.PublishWorkflowActivity(context.Background(), invalid); err == nil {
		t.Fatal("invalid PublishWorkflowActivity() error = nil")
	}
	if app.callCount() != 0 {
		t.Fatalf("appender calls after invalid publish = %d, want 0", app.callCount())
	}
}

func TestWorkflowActivityPublisherDeduplicatesLiveDelivery(t *testing.T) {
	t.Parallel()

	sessionID := uuid.UUID{41}
	app := &dedupAppender{decisions: []bool{true, false}}
	h := hub.New(sessionID, hub.WithAppender(app))
	sub := processSubscribeAll(t, h)
	defer sub.Close()
	pub, err := newCheckedWorkflowActivityPublisher(sessionID, h, nil)
	if err != nil {
		t.Fatalf("newCheckedWorkflowActivityPublisher() = %v", err)
	}
	metadata := validWorkflowActivityMetadata(sessionID)
	if err := pub.PublishWorkflowActivity(context.Background(), metadata); err != nil {
		t.Fatalf("first PublishWorkflowActivity() = %v", err)
	}
	if _, ok := recvProcessEvent(t, sub).(event.WorkflowActivity); !ok {
		t.Fatal("first publication was not WorkflowActivity")
	}
	if err := pub.PublishWorkflowActivity(context.Background(), metadata); err != nil {
		t.Fatalf("duplicate PublishWorkflowActivity() = %v", err)
	}
	expectNoProcessEvent(t, sub)
	if got := app.callCount(); got != 2 {
		t.Fatalf("appender calls = %d, want 2", got)
	}
}

func TestWorkflowActivityPublisherRetriesWithStableDurableEnvelope(t *testing.T) {
	t.Parallel()

	backend := memstore.New()
	store, err := sessionstore.Open(backend)
	if err != nil {
		t.Fatalf("sessionstore.Open() = %v", err)
	}
	sessionID := uuid.UUID{61}
	lease, err := store.AcquireLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireLease() = %v", err)
	}
	t.Cleanup(func() { _ = lease.Release(context.Background()) })
	j, err := store.OpenJournal(context.Background(), sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal() = %v", err)
	}
	h := hub.New(sessionID, hub.WithAppender(journal.NewJournalEventAppender(j)))
	sub := processSubscribeAll(t, h)
	defer sub.Close()

	clock := time.Date(2026, time.August, 9, 16, 0, 0, 0, time.UTC)
	pub, err := newCheckedWorkflowActivityPublisher(sessionID, h, func() time.Time { return clock })
	if err != nil {
		t.Fatalf("newCheckedWorkflowActivityPublisher() = %v", err)
	}
	metadata := validWorkflowActivityMetadata(sessionID)
	if err := pub.PublishWorkflowActivity(context.Background(), metadata); err != nil {
		t.Fatalf("first PublishWorkflowActivity() = %v", err)
	}
	first, ok := recvProcessEvent(t, sub).(event.WorkflowActivity)
	if !ok {
		t.Fatal("first publication was not WorkflowActivity")
	}
	if !first.CreatedAt.Equal(metadata.OccurredAt) {
		t.Fatalf("first CreatedAt = %v, want %v", first.CreatedAt, metadata.OccurredAt)
	}

	clock = clock.Add(time.Hour)
	if err := pub.PublishWorkflowActivity(context.Background(), metadata); err != nil {
		t.Fatalf("identical retry PublishWorkflowActivity() = %v", err)
	}
	expectNoProcessEvent(t, sub)

	collision := metadata
	collision.Message = "different semantic activity"
	if err := pub.PublishWorkflowActivity(context.Background(), collision); err == nil {
		t.Fatal("different-body retry error = nil, want journal idempotency collision")
	} else {
		var collisionErr *journal.IdempotencyCollisionError
		if !errors.As(err, &collisionErr) {
			t.Fatalf("different-body retry error = %T %v, want *journal.IdempotencyCollisionError", err, err)
		}
	}
}

func TestWorkflowActivityReplaysWithOriginalSequenceAndDoesNotFoldIntoContext(t *testing.T) {
	t.Parallel()

	backend := memstore.New()
	store1, err := sessionstore.Open(backend)
	if err != nil {
		t.Fatalf("first sessionstore.Open() = %v", err)
	}
	sessionID := uuid.UUID{71}
	lease1, err := store1.AcquireLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("first AcquireLease() = %v", err)
	}
	j1, err := store1.OpenJournal(context.Background(), sessionID, lease1)
	if err != nil {
		t.Fatalf("first OpenJournal() = %v", err)
	}
	h1 := hub.New(sessionID, hub.WithAppender(journal.NewJournalEventAppender(j1)))
	sub1 := processSubscribeAll(t, h1)
	defer sub1.Close()
	pub1, err := newCheckedWorkflowActivityPublisher(sessionID, h1, func() time.Time {
		return time.Date(2026, time.August, 9, 18, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatalf("first workflow publisher = %v", err)
	}
	metadata := validWorkflowActivityMetadata(sessionID)
	if err := pub1.PublishWorkflowActivity(context.Background(), metadata); err != nil {
		t.Fatalf("first PublishWorkflowActivity() = %v", err)
	}
	var first event.Delivery
	select {
	case first = <-sub1.Events():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first workflow activity")
	}
	firstActivity, ok := first.Event.(event.WorkflowActivity)
	if !ok {
		t.Fatalf("first event = %T, want WorkflowActivity", first.Event)
	}
	if first.JournalSeq == 0 {
		t.Fatal("first workflow activity has zero journal sequence")
	}
	if err := lease1.Release(context.Background()); err != nil {
		t.Fatalf("first lease Release() = %v", err)
	}

	store2, err := sessionstore.Open(backend)
	if err != nil {
		t.Fatalf("reopened sessionstore.Open() = %v", err)
	}
	lease2, err := store2.AcquireLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("reopened AcquireLease() = %v", err)
	}
	t.Cleanup(func() { _ = lease2.Release(context.Background()) })
	j2, err := store2.OpenJournal(context.Background(), sessionID, lease2)
	if err != nil {
		t.Fatalf("reopened OpenJournal() = %v", err)
	}
	h2 := hub.New(sessionID, hub.WithAppender(journal.NewJournalEventAppender(j2)))
	sub2 := processSubscribeAll(t, h2)
	defer sub2.Close()
	pub2, err := newCheckedWorkflowActivityPublisher(sessionID, h2, func() time.Time {
		return time.Date(2026, time.August, 9, 19, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatalf("reopened workflow publisher = %v", err)
	}
	if err := pub2.PublishWorkflowActivity(context.Background(), metadata); err != nil {
		t.Fatalf("replayed identical PublishWorkflowActivity() = %v", err)
	}
	expectNoProcessEvent(t, sub2)

	replayer, err := store2.OpenEventReplayer(sessionID, sessionstore.ReplayRequest{FromSeq: first.JournalSeq})
	if err != nil {
		t.Fatalf("OpenEventReplayer() = %v", err)
	}
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{SessionID: sessionID, From: journal.FromSeq(first.JournalSeq)})
	if err != nil {
		t.Fatalf("EventReplayer.Open() = %v", err)
	}
	defer cursor.Close()
	replayed, seq, err := cursor.Next(context.Background())
	if err != nil {
		t.Fatalf("replay Next() = %v", err)
	}
	if seq != first.JournalSeq {
		t.Fatalf("replayed sequence = %d, want original %d", seq, first.JournalSeq)
	}
	if got, ok := replayed.(event.WorkflowActivity); !ok || !reflect.DeepEqual(got, firstActivity) {
		t.Fatalf("replayed event = %#v, want original activity %#v", replayed, firstActivity)
	}
	if _, _, err := cursor.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("replay after activity = %v, want io.EOF", err)
	}

	folded := foldLoop([]event.Event{replayed})
	if folded.Err != nil || len(folded.Msgs) != 0 || folded.HasContext || folded.HasBasis || folded.OpenTurn {
		t.Fatalf("replayed activity entered model context: fold=%+v", folded)
	}
}

func TestWorkflowActivityPublisherFailsAfterSessionAbort(t *testing.T) {
	t.Parallel()

	sessionID := uuid.UUID{51}
	h := hub.New(sessionID)
	pub, err := newCheckedWorkflowActivityPublisher(sessionID, h, nil)
	if err != nil {
		t.Fatalf("newCheckedWorkflowActivityPublisher() = %v", err)
	}
	h.AbortSession(errors.New("session shutdown"))
	if err := pub.PublishWorkflowActivity(context.Background(), validWorkflowActivityMetadata(sessionID)); err == nil {
		t.Fatal("PublishWorkflowActivity() after session abort error = nil")
	}
}
