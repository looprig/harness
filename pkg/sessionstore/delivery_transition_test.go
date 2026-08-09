package sessionstore

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/storage/memstore"
)

func TestSessionJournalPersistsDelegateFallbackAsDistinctTransition(t *testing.T) {
	t.Parallel()
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sessionID, loopID, requestID := newTestUUID(t), newTestUUID(t), newTestUUID(t)
	lease, _ := leaseFor(1, sessionID)
	j, err := store.OpenJournal(context.Background(), sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	intent, fallback := deliveryRecords(sessionID, loopID, requestID, "payload", "payload")
	intentSeq, err := j.Append(context.Background(), intent)
	if err != nil {
		t.Fatalf("append intent: %v", err)
	}
	fallbackSeq, err := j.Append(context.Background(), fallback)
	if err != nil {
		t.Fatalf("append fallback: %v", err)
	}
	if fallbackSeq != intentSeq+1 {
		t.Fatalf("fallback seq = %d, want intent seq + 1 = %d", fallbackSeq, intentSeq+1)
	}
	if got := readEnvelope(t, store, sessionID, fallbackSeq).ID; got != fallback.IdempotencyID() {
		t.Fatalf("fallback envelope id = %q, want phase-aware id %q", got, fallback.IdempotencyID())
	}

	idempotent, ok := j.(journal.IdempotentJournal)
	if !ok {
		t.Fatal("journal does not implement IdempotentJournal")
	}
	retry, err := idempotent.AppendIdempotent(context.Background(), fallback)
	if err != nil {
		t.Fatalf("retry fallback: %v", err)
	}
	if retry.Appended || retry.Sequence != fallbackSeq {
		t.Fatalf("retry = %+v, want duplicate at seq %d", retry, fallbackSeq)
	}
}

func TestSessionJournalRejectsDelegateFallbackBeforeIntent(t *testing.T) {
	t.Parallel()
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sessionID, loopID, requestID := newTestUUID(t), newTestUUID(t), newTestUUID(t)
	lease, _ := leaseFor(1, sessionID)
	j, err := store.OpenJournal(context.Background(), sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	_, fallback := deliveryRecords(sessionID, loopID, requestID, "payload", "payload")
	if _, err := j.Append(context.Background(), fallback); err == nil {
		t.Fatal("fallback-before-intent append succeeded")
	} else {
		var transition *journal.DeliveryTransitionError
		if !errors.As(err, &transition) {
			t.Fatalf("fallback-before-intent error = %T %v, want typed transition error", err, err)
		}
	}
}

func TestSessionJournalRejectsPhasedRouteMismatch(t *testing.T) {
	t.Parallel()
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sessionID, loopID, requestID := newTestUUID(t), newTestUUID(t), newTestUUID(t)
	wrongLoopID := newTestUUID(t)
	lease, _ := leaseFor(1, sessionID)
	j, err := store.OpenJournal(context.Background(), sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	intent, _ := deliveryRecords(sessionID, loopID, requestID, "payload", "payload")
	wrongRoute := journal.NewCommandRecord(sessionID, wrongLoopID, intent.Command())
	if _, err := j.Append(context.Background(), wrongRoute); err == nil {
		t.Fatal("phased route mismatch append succeeded")
	} else {
		var mismatch *journal.CommandRouteMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("phased route mismatch error = %T %v, want typed route mismatch", err, err)
		}
	}
}

func TestSessionJournalDelegateTransitionChangedPayloadCollides(t *testing.T) {
	t.Parallel()
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sessionID, loopID, requestID := newTestUUID(t), newTestUUID(t), newTestUUID(t)
	lease, _ := leaseFor(1, sessionID)
	j, err := store.OpenJournal(context.Background(), sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	intent, fallback := deliveryRecords(sessionID, loopID, requestID, "payload", "payload")
	if _, err := j.Append(context.Background(), intent); err != nil {
		t.Fatalf("append intent: %v", err)
	}
	if _, err := j.Append(context.Background(), fallback); err != nil {
		t.Fatalf("append fallback: %v", err)
	}
	_, changed := deliveryRecords(sessionID, loopID, requestID, "payload", "changed")
	if _, err := j.Append(context.Background(), changed); err == nil {
		t.Fatal("changed fallback payload append succeeded")
	} else {
		var collision *journal.IdempotencyCollisionError
		if !errors.As(err, &collision) {
			t.Fatalf("changed fallback error = %T %v, want idempotency collision", err, err)
		}
	}
}

func TestSessionJournalDelegateFallbackConcurrentIdenticalRetryAppendsOnce(t *testing.T) {
	t.Parallel()
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sessionID, loopID, requestID := newTestUUID(t), newTestUUID(t), newTestUUID(t)
	lease, _ := leaseFor(1, sessionID)
	j, err := store.OpenJournal(context.Background(), sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	intent, fallback := deliveryRecords(sessionID, loopID, requestID, "payload", "payload")
	if _, err := j.Append(context.Background(), intent); err != nil {
		t.Fatalf("append intent: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	appended := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, appendErr := j.(journal.IdempotentJournal).AppendIdempotent(context.Background(), fallback)
			if appendErr != nil {
				t.Errorf("concurrent fallback: %v", appendErr)
				return
			}
			if result.Appended {
				mu.Lock()
				appended++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if appended != 1 {
		t.Fatalf("concurrent fallback physical appends = %d, want 1", appended)
	}
}

func TestSessionJournalDelegateFallbackConcurrentConflictsFailClosed(t *testing.T) {
	t.Parallel()
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sessionID, loopID, requestID := newTestUUID(t), newTestUUID(t), newTestUUID(t)
	lease, _ := leaseFor(1, sessionID)
	j, err := store.OpenJournal(context.Background(), sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	intent, fallback := deliveryRecords(sessionID, loopID, requestID, "payload", "payload")
	_, err = j.Append(context.Background(), intent)
	if err != nil {
		t.Fatalf("append intent: %v", err)
	}
	_, changed := deliveryRecords(sessionID, loopID, requestID, "payload", "changed")
	type outcome struct {
		result journal.AppendResult
		err    error
	}
	results := make(chan outcome, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			record := fallback
			if i%2 == 1 {
				record = changed
			}
			result, appendErr := j.(journal.IdempotentJournal).AppendIdempotent(context.Background(), record)
			results <- outcome{result: result, err: appendErr}
		}(i)
	}
	wg.Wait()
	close(results)
	appended, failures := 0, 0
	for result := range results {
		if result.err != nil {
			failures++
			var collision *journal.IdempotencyCollisionError
			var transition *journal.DeliveryTransitionError
			if !errors.As(result.err, &collision) && !errors.As(result.err, &transition) {
				t.Fatalf("conflicting fallback error = %T %v, want collision or transition error", result.err, result.err)
			}
			continue
		}
		if result.result.Appended {
			appended++
		}
	}
	if appended != 1 {
		t.Fatalf("concurrent conflicting fallback physical appends = %d, want 1", appended)
	}
	if failures == 0 {
		t.Fatal("concurrent conflicting fallbacks all succeeded")
	}
}

func TestSessionJournalReopenHydratesDelegateIntentForFallback(t *testing.T) {
	t.Parallel()
	backend := memstore.New()
	store1, err := Open(backend)
	if err != nil {
		t.Fatalf("Open first store: %v", err)
	}
	sessionID, loopID, requestID := newTestUUID(t), newTestUUID(t), newTestUUID(t)
	lease1, err := store1.AcquireLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireLease first: %v", err)
	}
	j1, err := store1.OpenJournal(context.Background(), sessionID, lease1)
	if err != nil {
		t.Fatalf("OpenJournal first: %v", err)
	}
	intent, fallback := deliveryRecords(sessionID, loopID, requestID, "payload", "payload")
	intentSeq, err := j1.Append(context.Background(), intent)
	if err != nil {
		t.Fatalf("append intent: %v", err)
	}
	if err := lease1.Release(context.Background()); err != nil {
		t.Fatalf("release first lease: %v", err)
	}

	store2, err := Open(backend)
	if err != nil {
		t.Fatalf("Open second store: %v", err)
	}
	lease2, err := store2.AcquireLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireLease second: %v", err)
	}
	j2, err := store2.OpenJournal(context.Background(), sessionID, lease2)
	if err != nil {
		t.Fatalf("OpenJournal second: %v", err)
	}
	fallbackSeq, err := j2.Append(context.Background(), fallback)
	if err != nil {
		t.Fatalf("append fallback after reopen: %v", err)
	}
	if fallbackSeq != intentSeq+2 {
		// The second opening fence is inserted between the two command frames.
		t.Fatalf("fallback seq = %d, want intent seq + 2 = %d", fallbackSeq, intentSeq+2)
	}
}

func deliveryRecords(sessionID, loopID, requestID uuid.UUID, intentText, fallbackText string) (journal.CommandRecord, journal.CommandRecord) {
	base := command.UserInput{
		Header:       command.Header{CommandID: requestID, Agency: identity.AgencyMachine},
		Blocks:       []content.Block{&content.TextBlock{Text: intentText}},
		TargetLoopID: loopID,
	}
	intent := base
	intent.DelegateDeliveryPhase = command.DelegateDeliveryPhaseIntent
	fallback := base
	fallback.Blocks = []content.Block{&content.TextBlock{Text: fallbackText}}
	fallback.DelegateDeliveryPhase = command.DelegateDeliveryPhaseFallbackQueued
	return journal.NewCommandRecord(sessionID, loopID, intent), journal.NewCommandRecord(sessionID, loopID, fallback)
}
