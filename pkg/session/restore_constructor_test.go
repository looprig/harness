package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// failingNewID is an idGenerator seam that mints monotonically-distinct UUIDs but
// returns a hard error on the failOnCall-th call (1-based), succeeding on every other
// call. It drives a single restore-lifecycle id-mint failure at a chosen point while
// letting the surrounding mints (notably the RestoreErrored recorded by recordErrored)
// succeed, so the fail-secure exit can be observed end to end.
type failingNewID struct {
	n          int
	failOnCall int
}

// errMintFailed is the leaf cause an injected id-mint failure surfaces. A sentinel is
// permitted: it is a context-free leaf used only by this test seam.
var errMintFailed = errors.New("restore_constructor_test: injected id-mint failure")

func (f *failingNewID) next() (uuid.UUID, error) {
	f.n++
	if f.n == f.failOnCall {
		return uuid.UUID{}, errMintFailed
	}
	// A distinct, non-zero id per call so the journal's Nats-Msg-Id never collides.
	return uuid.UUID{0xD0, byte(f.n)}, nil
}

// fixedClock is a deterministic event.Clock for the restore-lifecycle stamps.
func fixedClock() time.Time { return time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) }

// TestRestoreSessionFailSecureExits drives restoreSession's post-journal failure exits
// directly through the injectable id-gen seam and asserts the fail-secure contract for
// each: a RestoreErrored is durably recorded AND the single-writer lease is released
// (a successor can re-acquire) AND (nil, *RestoreError{RestoreAppendFailed}) is
// returned — no Session ever comes up. The failure is forced at a restore-lifecycle
// id-mint (RestoreStarted, then RestoreDone) so it lands AFTER the journal exists,
// which is exactly where recordErrored is the single fail-secure exit.
func TestRestoreSessionFailSecureExits(t *testing.T) {
	tests := []struct {
		name string
		// failOnCall is the 1-based restore-lifecycle id-mint that fails. On a clean
		// (no open turn) stream the mints in order are: 1=RestoreStarted, 2=RestoreDone,
		// 3=RestoreErrored (in recordErrored). Failing on 1 routes to recordErrored whose
		// own mint is call 2 (succeeds); failing on 2 routes to recordErrored whose mint
		// is call 3 (succeeds) — so RestoreErrored is recorded in BOTH cases.
		failOnCall int
	}{
		{name: "RestoreStarted mint fails", failOnCall: 1},
		{name: "RestoreDone mint fails", failOnCall: 2},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newRestoreStore(t)
			fp := fingerprintFromDefinition(restoreCfg(&stubLLM{}, "model-x", "be helpful"))

			// A clean original run (ends on TurnDone, no open turn): the restore mints
			// exactly RestoreStarted, RestoreDone, then RestoreErrored on the failure path.
			orig := buildOriginalRun(t, store, fp, restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful"), 1)
			handOver(t, orig.lease)

			seam := &failingNewID{failOnCall: tt.failOnCall}
			s, err := restoreSession(
				context.Background(),
				restoreCfg(&stubLLM{}, "model-x", "be helpful"),
				orig.sessionID, store,
				seam.next, fixedClock,
			)

			// (a) No Session comes up.
			if s != nil {
				t.Fatalf("restoreSession returned a non-nil Session on a forced failure")
			}
			// (b) A typed *RestoreError classifying the append/mint failure is returned.
			var re *RestoreError
			if !errors.As(err, &re) {
				t.Fatalf("restoreSession err = %v, want *RestoreError", err)
			}
			if re.Kind != RestoreAppendFailed {
				t.Errorf("RestoreError.Kind = %q, want %q", re.Kind, RestoreAppendFailed)
			}
			// The injected mint failure chains through as the cause.
			if !errors.Is(err, errMintFailed) {
				t.Errorf("err does not chain the injected mint failure: %v", err)
			}

			// (c) A RestoreErrored is durably recorded (the failure is in the log, and no
			// RestoreDone followed it — the restore did not silently half-succeed).
			tail := restoreEventTail(t, store, orig.sessionID, orig.primaryLoopID)
			if !lastIs(tail, event.RestoreErrored{}) {
				t.Errorf("restore-event tail does not end with RestoreErrored: %v", tailTypes(tail))
			}
			for _, ev := range tail {
				if _, ok := ev.(event.RestoreDone); ok {
					t.Errorf("a RestoreDone is present on a failed restore: %v", tailTypes(tail))
				}
			}

			// (d) The lease was released: a successor can re-acquire it through the store (the
			// failed restore must not leave the session's single-writer lease held).
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			successorLease, acqErr := store.AcquireLease(ctx, orig.sessionID)
			if acqErr != nil {
				t.Fatalf("successor Acquire after failed restore = %v, want success (lease should have been released)", acqErr)
			}
			t.Cleanup(func() {
				rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer rcancel()
				_ = successorLease.Release(rctx)
			})
		})
	}
}
