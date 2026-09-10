package sessionruntime

import (
	"context"
	"sync/atomic"
	"testing"
)

// revocableLease is a leaseEpochSource whose validity a test can flip, which
// fixedEpochLease (always valid) cannot express. It keeps reporting its epoch after
// revocation ON PURPOSE: the storage lease does exactly that — journal.Lease's Release
// "marks it no longer held" and no pinned provider zeroes Epoch — so a LeaseEpoch that
// returned (0, false) after revocation by reading a zeroed epoch rather than by
// consulting Valid would pass against a fake that zeroes it. The fake is therefore
// LOOSER than the real lease in the one direction that matters here.
type revocableLease struct {
	epoch uint64
	valid atomic.Bool
}

func newRevocableLease(epoch uint64) *revocableLease {
	l := &revocableLease{epoch: epoch}
	l.valid.Store(true)
	return l
}

func (l *revocableLease) Epoch() uint64 { return l.epoch }
func (l *revocableLease) Valid() bool   { return l.valid.Load() }
func (l *revocableLease) revoke()       { l.valid.Store(false) }

// TestLeaseEpochReportsTheHeldEpoch is the positive claim: a session wired to a lease
// reports THAT lease's epoch, not a hardcoded constant and not the zero value. The
// fixture's epoch is 11 rather than 1, so a report that always answered "the first
// epoch" would be visible.
func TestLeaseEpochReportsTheHeldEpoch(t *testing.T) {
	t.Parallel()
	f := newIdleReleaseFixture(t)
	t.Cleanup(func() { _ = f.session.Shutdown(context.Background()) })
	epoch, held := f.session.LeaseEpoch()
	if !held {
		t.Fatal("LeaseEpoch held = false on a session wired with a lease; the capability is unreachable")
	}
	if epoch != releaseFixtureEpoch {
		t.Errorf("LeaseEpoch epoch = %d, want %d (the epoch the fixture's lease reports)", epoch, releaseFixtureEpoch)
	}
}

// TestLeaseEpochAbsentWithoutALease is the ambiguity the two-result form exists to
// resolve. A session composed WITHOUT a single-writer lease — headless, no persistence,
// or simply not wired for durable commands — must be distinguishable from one holding
// epoch 0, so held must be false and not merely the epoch zero.
func TestLeaseEpochAbsentWithoutALease(t *testing.T) {
	t.Parallel()
	s, err := newTestSession(context.Background(), cfg(&stubLLM{}))
	if err != nil {
		t.Fatalf("newTestSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	epoch, held := s.LeaseEpoch()
	if held {
		t.Errorf("LeaseEpoch held = true on a session with no lease (epoch %d); a consumer cannot tell it from a real grant", epoch)
	}
	if epoch != 0 {
		t.Errorf("LeaseEpoch epoch = %d with held false, want 0: the epoch is meaningless and must not carry a number", epoch)
	}
}

// TestLeaseEpochAbsentOnceTheLeaseIsLost pins the fail-secure half. The lease still
// REPORTS its epoch after revocation (see revocableLease), so the only way to answer
// (0, false) here is to consult Valid — a report that read the epoch alone would answer
// (7, true) and hand a consumer a number a live fencing check is guaranteed to reject.
//
// The pre-revocation read is a positive control: without it "held is false" would pass
// against a session that never reported an epoch at all.
func TestLeaseEpochAbsentOnceTheLeaseIsLost(t *testing.T) {
	t.Parallel()
	lease := newRevocableLease(7)
	s, err := newTestSession(context.Background(), cfg(&stubLLM{}),
		WithRuntimeCommands(unusedRuntimeCommandLog{}, lease))
	if err != nil {
		t.Fatalf("newTestSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	if epoch, held := s.LeaseEpoch(); !held || epoch != 7 {
		t.Fatalf("LeaseEpoch before revocation = (%d, %v), want (7, true); the revocation assertion below would be vacuous", epoch, held)
	}
	lease.revoke()
	epoch, held := s.LeaseEpoch()
	if held {
		t.Errorf("LeaseEpoch held = true after the lease was lost (epoch %d); a stale epoch was handed to a caller", epoch)
	}
	if epoch != 0 {
		t.Errorf("LeaseEpoch epoch = %d after the lease was lost, want 0", epoch)
	}
}

// TestLiveLeaseEpochMatchesTheReleasedRecord ties the new LIVE read to the surface the
// same number was already published on. The two are produced by different code paths —
// this one gates on Valid, the release record's does not — and a consumer that joins a
// live stamp to a durable residency record needs them to be the same number, so the
// agreement is measured rather than assumed.
func TestLiveLeaseEpochMatchesTheReleasedRecord(t *testing.T) {
	t.Parallel()
	f := newIdleReleaseFixture(t)
	live, held := f.session.LeaseEpoch()
	if !held {
		t.Fatal("LeaseEpoch held = false before release")
	}
	if err := f.session.ReleaseResidency(context.Background()); err != nil {
		t.Fatalf("ReleaseResidency: %v", err)
	}
	released, _ := countResidencyEvents(f.recorder.snapshot())
	if len(released) != 1 {
		t.Fatalf("SessionResidencyReleased count = %d, want exactly 1", len(released))
	}
	if released[0].LeaseEpoch != live {
		t.Errorf("live LeaseEpoch = %d but SessionResidencyReleased.LeaseEpoch = %d; the two surfaces disagree about the same grant", live, released[0].LeaseEpoch)
	}
}
