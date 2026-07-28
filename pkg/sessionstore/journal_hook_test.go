package sessionstore

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

func TestOpenJournalWithOpeningAppendObservesCommittedFenceExactlyOnce(t *testing.T) {
	t.Parallel()

	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sessionID := newTestUUID(t)
	lease, _ := leaseFor(7, sessionID)
	var mu sync.Mutex
	var calls []hook.Call
	var results []hook.Result
	runner, err := hook.Compile(hook.Set{Around: []hook.Around{{
		Operation: hook.OperationJournalAppend,
		Begin: func(ctx context.Context, call hook.Call) (context.Context, hook.FinishFunc) {
			mu.Lock()
			calls = append(calls, call)
			mu.Unlock()
			return ctx, func(result hook.Result) {
				mu.Lock()
				results = append(results, result)
				mu.Unlock()
			}
		},
	}}})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}

	opened, err := store.OpenJournalWithOpeningAppend(
		context.Background(),
		sessionID,
		lease,
		journal.HookMiddleware(runner, sessionID),
	)
	if err != nil || opened == nil {
		t.Fatalf("OpenJournalWithOpeningAppend = (%T, %v), want ready journal", opened, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || len(results) != 1 {
		t.Fatalf("opening observations = %d begins/%d finishes, want 1/1", len(calls), len(results))
	}
	call := calls[0]
	if call.Operation != hook.OperationJournalAppend ||
		call.JournalAppend == nil ||
		call.JournalAppend.Family != hook.RecordFence ||
		call.JournalAppend.RecordID != "7" {
		t.Fatalf("opening call = %#v, want fence epoch 7", call)
	}
	if results[0].Outcome != hook.OutcomeCompleted || results[0].Err != nil {
		t.Fatalf("opening result = %#v, want completed", results[0])
	}
}

func TestOpenJournalWithOpeningAppendFailureFinishesAndDoesNotExposeJournal(t *testing.T) {
	t.Parallel()

	appendFailure := errors.New("opening fence append failed")
	mem := memstore.New()
	failing := &openingFailLedger{Ledger: mem.Ledger, err: appendFailure}
	composite := &storage.Composite{
		Ledger: failing,
		Leaser: mem.Leaser,
		KV:     mem.KV,
		Blobs:  mem.Blobs,
	}
	store, err := Open(composite)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sessionID := newTestUUID(t)
	lease, _ := leaseFor(8, sessionID)
	var results []hook.Result
	runner, err := hook.Compile(hook.Set{Around: []hook.Around{{
		Operation: hook.OperationJournalAppend,
		Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			return ctx, func(result hook.Result) { results = append(results, result) }
		},
	}}})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}

	opened, err := store.OpenJournalWithOpeningAppend(
		context.Background(),
		sessionID,
		lease,
		journal.HookMiddleware(runner, sessionID),
	)
	if opened != nil {
		t.Fatalf("OpenJournalWithOpeningAppend returned %T after failed fence", opened)
	}
	if !errors.Is(err, appendFailure) {
		t.Fatalf("error = %v, want opening failure", err)
	}
	if len(results) != 1 || results[0].Outcome != hook.OutcomeFailed || !errors.Is(results[0].Err, appendFailure) {
		t.Fatalf("opening results = %#v, want one failed terminal", results)
	}
}

func TestOpenJournalWithOpeningAppendRequiresExactlyOneFenceDelegation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		wrap    journal.AppendMiddleware
		wantTip uint64
	}{
		{
			name: "returns nil append function",
			wrap: func(journal.AppendFunc) journal.AppendFunc {
				return nil
			},
			wantTip: 0,
		},
		{
			name: "skips next",
			wrap: func(journal.AppendFunc) journal.AppendFunc {
				return func(context.Context, journal.JournalRecord) (uint64, error) {
					return 99, nil
				}
			},
			wantTip: 0,
		},
		{
			name: "calls next twice",
			wrap: func(next journal.AppendFunc) journal.AppendFunc {
				return func(ctx context.Context, record journal.JournalRecord) (uint64, error) {
					seq, err := next(ctx, record)
					_, _ = next(ctx, record)
					return seq, err
				}
			},
			wantTip: 1,
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			store, err := Open(memstore.New())
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			sessionID := newTestUUID(t)
			lease, _ := leaseFor(9, sessionID)

			opened, err := store.OpenJournalWithOpeningAppend(
				context.Background(),
				sessionID,
				lease,
				testCase.wrap,
			)
			if err == nil || opened != nil {
				t.Fatalf("OpenJournalWithOpeningAppend = (%T, %v), want fail-closed middleware error", opened, err)
			}
			tip, tipErr := store.backend.Ledger.Tip(context.Background(), ledgerName(sessionID))
			if tipErr != nil {
				t.Fatalf("Tip: %v", tipErr)
			}
			if tip != testCase.wantTip {
				t.Fatalf("Tip = %d, want %d (no duplicate fence)", tip, testCase.wantTip)
			}
		})
	}
}

func TestOpenJournalWithOpeningAppendCannotRewriteFenceResult(t *testing.T) {
	t.Parallel()

	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sessionID := newTestUUID(t)
	lease, _ := leaseFor(10, sessionID)
	fabricated := errors.New("fabricated middleware result")
	rewrite := func(next journal.AppendFunc) journal.AppendFunc {
		return func(ctx context.Context, record journal.JournalRecord) (uint64, error) {
			_, _ = next(ctx, record)
			return 99, fabricated
		}
	}

	opened, err := store.OpenJournalWithOpeningAppend(
		context.Background(),
		sessionID,
		lease,
		rewrite,
	)
	if err != nil || opened == nil {
		t.Fatalf("OpenJournalWithOpeningAppend = (%T, %v), want committed fence result", opened, err)
	}
	seq, err := opened.Append(
		context.Background(),
		journal.NewFenceRecord(sessionID, journal.LeaseFence{Epoch: 11}),
	)
	if err != nil || seq != 2 {
		t.Fatalf("ready Append = (%d, %v), want (2, nil)", seq, err)
	}
}

type openingFailLedger struct {
	storage.Ledger
	err error
}

func (l *openingFailLedger) Append(context.Context, string, uint64, []byte) error {
	return l.err
}
