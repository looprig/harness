package sessionruntime

import (
	"context"
	"time"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
)

// ResidencyReleaseRefusedError reports that a nonterminal residency release was
// refused BEFORE any teardown ran. The session is untouched: still resident, still
// admitting work, still the caller's to Shutdown or to release again later. It is a
// distinct type from every teardown failure precisely so a host can tell "I could not
// release" apart from "I released badly".
type ResidencyReleaseRefusedError struct{ Cause error }

func (e *ResidencyReleaseRefusedError) Error() string {
	return "session: residency release refused"
}

func (e *ResidencyReleaseRefusedError) Unwrap() error { return e.Cause }

// ReleaseResidency gives up THIS process's resident runtime for the session —
// subscriptions, actors, supervised processes, leases, local contexts — while leaving
// the logical session restorable elsewhere. It is the nonterminal counterpart of
// Shutdown and satisfies session.Releaser.
//
// It deliberately does NOT call Shutdown. Shutdown's hub stop durably appends
// SessionStopped, which makes the logical session terminal; a registry loser that used
// it to hand a session over would have ended someone's session instead of releasing it.
// Everything else about the teardown IS Shutdown's: both drive the one sequence in
// Session.teardown, differing only at the plan seams below.
//
// The order is:
//
//  1. Admission. The session must not be faulted and must be whole-session idle. This
//     runs before the teardown owner is elected, so a refusal leaves the session
//     exactly as it was and returns *ResidencyReleaseRefusedError.
//  2. The shared teardown: latch closing, revoke collaboration origins, close hustle
//     admission, shut down and drain every loop, stop the checkpoint controller and the
//     offload GC, terminate the session resource registry.
//  3. Take and durably commit the anchoring workspace checkpoint, then append
//     SessionResidencyReleased carrying that checkpoint's sequence and the lease epoch.
//     A checkpoint that does not commit writes NO residency record — a record naming an
//     anchor that does not exist is worse than no record.
//  4. Close the hub LOCALLY, appending no SessionStopped.
//  5. Release the workspace root lease then the session lease, cancel the session
//     context, and abandon foreign delivery hooks.
//
// Done closes at the START of step 2, not at the end, so a supervisor learns the
// session is going away while teardown is still running.
//
// Concurrent and repeated calls — and a race with Shutdown — join ONE teardown owner
// and receive its result. Whoever is elected runs its own mode to completion; the
// stream therefore carries either a SessionResidencyReleased or a SessionStopped,
// never both.
//
// WHICH one it carries is not in this call's return value — a joined caller gets the
// owner's result, and nil means "teardown succeeded" under either mode. A caller that
// needs to know reads the session catalog, which projects the two apart on purpose
// (sessionstore.applyEvent): a release leaves Status/State untouched and sets
// Residency=cold, so the session reads cold AND restorable, while a stop sets
// Residency=cold together with Status=stopped/State=stopped. Residency alone never
// distinguishes them — no process is resident after either — so a caller deciding
// whether a session can still be restored must read the terminality axis, not
// residency.
//
// The idle admission is not a lock: a Submit to an already-registered loop may still be
// accepted between the admission check and the closing latch. It is not lost and it does
// not corrupt the anchor — step 2 sends every loop a Shutdown command and waits for the
// drain, so the checkpoint in step 3 is taken with no loop running.
func (s *Session) ReleaseResidency(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if hustleFinalizerOwnsSession(ctx, s) {
		return &HustleShutdownReentryError{}
	}
	owner, wait, err := s.beginTeardown(ctx, s.admitResidencyRelease)
	if err != nil {
		return err
	}
	if !owner {
		return shutdownResult(wait(), ctx.Err())
	}
	cleanupErr := s.teardown(nonterminalTeardown(s))
	s.finishTeardown(cleanupErr)
	return shutdownResult(cleanupErr, ctx.Err())
}

// admitResidencyRelease is the release-only precondition: a faulted session has no
// trustworthy durable log to anchor a release to, and a busy session must not have its
// work torn down by a release that could have waited. Both are checked with nothing
// latched, so a refusal is a no-op.
//
// WaitIdle is the whole-session quiescence wait, and on a workspace-backed session with
// a REQUIRED checkpoint policy it is also the required-checkpoint barrier: the hub does
// not report idle until that checkpoint's blob has committed, and a latched
// required-checkpoint fault reaches it as a sticky waiter failure. So this one wait
// carries both of the task's admission conditions.
func (s *Session) admitResidencyRelease(ctx context.Context) error {
	if err := s.faultIfFaulted(); err != nil {
		return &ResidencyReleaseRefusedError{Cause: err}
	}
	if err := s.WaitIdle(ctx); err != nil {
		return &ResidencyReleaseRefusedError{Cause: err}
	}
	return nil
}

// nonterminalTeardown is ReleaseResidency's plan over the shared teardown sequence.
func nonterminalTeardown(s *Session) teardownPlan {
	return newTeardownPlan(s.anchorAndRecordRelease, s.closeHubLocally)
}

// anchorAndRecordRelease takes the release's anchoring checkpoint and, only if it
// durably committed, appends the residency record. It runs at the teardown seam where
// the hub is still open and every producer of new work has already stopped.
//
// It runs on its OWN private deadline, like every other phase from the checkpoint
// stop onward, derived from the session's already-validated SnapshotPolicy.Timeout —
// the same budget the checkpoint CONTROLLER runs its boundary snapshots on. Without
// it the three blocking operations behind this seam (the workspace permit acquire,
// the snapshot, and the durable append) are unbounded, and a wedged blob backend
// would hold the teardown owner forever: Done is already closed, so a supervisor
// believes the session is going away, while the leases are never released and every
// joined caller — including a Shutdown fallback — blocks on cleanupDone. One stuck
// blob write would wedge the session in both the releasing and the restoring process.
//
// The bound is a CONTEXT deadline, so it depends on the provider honoring ctx —
// exactly the trust boundary stopHub already accepts for the hub transition. Going
// further and abandoning the snapshot on a goroutine (as stopSessionResources does)
// would be worse here, not better: an abandoned snapshot keeps reading the workspace
// after the root lease has been released to a successor.
func (s *Session) anchorAndRecordRelease(root context.Context, budget checkpointBudget) error {
	timeout := time.Duration(budget)
	ctx, cancel := cleanupContext(root, timeout)
	defer cancel()
	seq, err := s.checkpointForRelease(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return cleanupTimeoutError(ShutdownCleanupResidencyAnchor, timeout, ctx.Err())
		}
		return err
	}
	stamped, err := s.factory.Stamp(event.Header{Coordinates: identity.Coordinates{SessionID: s.sessionID}})
	if err != nil {
		return &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
	}
	if err := s.PublishEventChecked(ctx, event.SessionResidencyReleased{
		Header:        stamped,
		CheckpointSeq: seq,
		LeaseEpoch:    s.leaseEpoch(),
	}); err != nil {
		if ctx.Err() != nil {
			return cleanupTimeoutError(ShutdownCleanupResidencyAnchor, timeout, ctx.Err())
		}
		return err
	}
	return nil
}

// checkpointForRelease commits the workspace checkpoint the release is anchored to and
// returns its journal sequence. A session with no configured workspace has nothing to
// anchor and returns zero, which SessionResidencyReleased documents as "no workspace
// anchor" rather than "sequence zero".
//
// It goes through the controller-free direct path because the checkpoint controller has
// already been stopped by this point in teardown — and it must be, since a live
// controller could otherwise start a boundary checkpoint concurrently with this one.
// The publication is CHECKED so a failed append refuses the release instead of faulting
// the session and reporting success.
func (s *Session) checkpointForRelease(ctx context.Context) (uint64, error) {
	if s.ws == nil {
		return 0, nil
	}
	_, seq, err := s.snapshotWorkspaceDirect(ctx, true)
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// leaseEpoch reports the single-writer lease epoch this process holds, or zero when the
// session was not wired to a lease that reports one (a headless or no-persistence
// session, or one whose journal cannot deduplicate a redelivered append — see
// WithRuntimeCommands, which wires the same lease).
func (s *Session) leaseEpoch() uint64 {
	if s.runtimeCommandLease == nil {
		return 0
	}
	return s.runtimeCommandLease.Epoch()
}

// closeHubLocally is the NONTERMINAL hub close: it clears activity, forces the
// in-memory phase to stopped, wakes WaitIdle waiters and fails every subscription with
// hub.ErrResidencyReleased — and appends nothing, so the durable stream never says this
// session ended. It then waits for the hub's in-flight publications to drain, bounded
// by the same session-owned hub deadline the terminal stop uses.
func (s *Session) closeHubLocally(root context.Context, timeout time.Duration) error {
	return s.stopHubWith(root, timeout, func(ctx context.Context) {
		drained := s.hub.AbortSession(hub.ErrResidencyReleased)
		select {
		case <-drained:
		case <-ctx.Done():
		}
	})
}
