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
		Ledger:       failing,
		Leaser:       mem.Leaser,
		KV:           mem.KV,
		OrderedIndex: mem.OrderedIndex,
		Blobs:        mem.Blobs,
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

// TestOpeningFenceConflictNeverRereadsTip is the mechanical half of the no-rebase
// rule, measured at the ledger seam rather than through the returned error: across
// a whole Open whose opening fence conflicts, the writer must issue exactly ONE
// Tip read for its stream and exactly ONE CAS, at the expected tip that read
// returned. The removed retry loop issued a second Tip and a second CAS at the
// refreshed value — that is the rebase, and it is invisible to an error-only
// assertion. The middleware seam is exercised at the same time: it observes the
// single append and its terminal carries the raw CAS failure, while Open's own
// result is the typed *OpeningFenceConflictError telling the caller to acquire a
// fresh lease.
func TestOpeningFenceConflictNeverRereadsTip(t *testing.T) {
	t.Parallel()

	backend := memstore.New()
	sessionID := newTestUUID(t)
	name := ledgerName(sessionID)

	seeded, err := Open(backend)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	first, err := seeded.AcquireLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if _, err := seeded.OpenJournal(context.Background(), sessionID, first); err != nil {
		t.Fatalf("seed OpenJournal: %v", err)
	}
	if err := first.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}

	counting := &openingConflictLedger{Ledger: backend.Ledger, name: name}
	composite := &storage.Composite{
		Ledger:       counting,
		Leaser:       backend.Leaser,
		KV:           backend.KV,
		OrderedIndex: backend.OrderedIndex,
		Blobs:        backend.Blobs,
	}
	store, err := Open(composite)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	lease, err := store.AcquireLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	counting.racer = fenceFrame(t, lease.Epoch()+1)

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
	// Deliberately Errorf, not Fatalf: a reintroduced rebase makes Open SUCCEED, and
	// stopping here would hide the two measurements that name the defect mechanically
	// (the second Tip read and the second CAS) behind a generic "returned a journal".
	if opened != nil {
		t.Errorf("OpenJournalWithOpeningAppend returned %T after a conflicting fence", opened)
	}
	var conflict *OpeningFenceConflictError
	if !errors.As(err, &conflict) {
		t.Errorf("error = %v, want *OpeningFenceConflictError", err)
	}

	tips, expected := counting.observed()
	if tips != 1 {
		t.Errorf("Tip reads = %d, want 1 (a second read is a rebase of the expected tip)", tips)
	}
	if len(expected) != 1 || expected[0] != 1 {
		t.Errorf("CAS expected values = %v, want exactly [1] (one attempt, on the tip first read)", expected)
	}
	if len(results) != 1 || results[0].Outcome != hook.OutcomeFailed {
		t.Fatalf("opening results = %#v, want one failed terminal", results)
	}
	var appendErr *journal.AppendError
	if !errors.As(results[0].Err, &appendErr) {
		t.Fatalf("opening terminal err = %v, want a *journal.AppendError", results[0].Err)
	}
}

// openingConflictLedger injects one foreign record immediately before the first
// Append on its bound stream — making that Append's CAS conflict deterministically
// — and records every Tip read and every CAS expected value the writer issues for
// that stream, so a test can prove the writer neither rereads nor rebases its tip.
// The injected write goes through the inner ledger and is deliberately not counted.
type openingConflictLedger struct {
	storage.Ledger
	name     string
	racer    []byte
	once     sync.Once
	mu       sync.Mutex
	tips     int
	expected []uint64
}

func (l *openingConflictLedger) Tip(ctx context.Context, name string) (uint64, error) {
	if name == l.name {
		l.mu.Lock()
		l.tips++
		l.mu.Unlock()
	}
	return l.Ledger.Tip(ctx, name)
}

func (l *openingConflictLedger) Append(ctx context.Context, name string, expected uint64, payload []byte) error {
	if name == l.name {
		l.once.Do(func() {
			if err := l.Ledger.Append(context.Background(), name, expected, l.racer); err != nil {
				panic(err)
			}
		})
		l.mu.Lock()
		l.expected = append(l.expected, expected)
		l.mu.Unlock()
	}
	return l.Ledger.Append(ctx, name, expected, payload)
}

func (l *openingConflictLedger) observed() (int, []uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tips, append([]uint64(nil), l.expected...)
}

type openingFailLedger struct {
	storage.Ledger
	err error
}

func (l *openingFailLedger) Append(context.Context, string, uint64, []byte) error {
	return l.err
}
