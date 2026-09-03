package sessionstore

import (
	"testing"
	"time"

	"github.com/looprig/harness/pkg/event"
)

// assertRestorable asserts the two clauses that together mean "restorable": the
// session is not terminal on EITHER of the catalog's back-compat Status axis or its
// richer State axis. It is deliberately separate from the residency clause below, so
// a case that disagrees about residency still gets both terminality clauses checked.
func assertRestorable(t *testing.T, meta SessionMeta) {
	t.Helper()
	if meta.Status == StatusStopped {
		t.Errorf("Status = %q, want a non-terminal status: a released session is restorable, not stopped", meta.Status)
	}
	if meta.State == StateStopped {
		t.Errorf("State = %q, want a non-terminal state: a released session is restorable, not stopped", meta.State)
	}
}

func assertResidency(t *testing.T, meta SessionMeta, want SessionResidency) {
	t.Helper()
	if meta.Residency != want {
		t.Errorf("Residency = %q, want %q", meta.Residency, want)
	}
}

// TestResidencyReleaseProjectsColdAndRestorable is the catalog half of the
// nonterminal-release contract. Residency and execution state are INDEPENDENT axes
// (Core's SessionResidency vs SessionState): releasing residency must move the
// session to cold WITHOUT moving it toward a terminal status or state, and without
// disturbing the execution state it was left in.
func TestResidencyReleaseProjectsColdAndRestorable(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x01)
	clock := fixedClock(time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC))

	meta, _ := mustApplyEvent(t, SessionMeta{}, event.SessionStarted{Header: hdr(sid)}, 1, clock)
	assertResidency(t, meta, ResidencyResident)
	assertRestorable(t, meta)

	released, changed := mustApplyEvent(t, meta, event.SessionResidencyReleased{
		Header: hdr(sid), CheckpointSeq: 7, LeaseEpoch: 3,
	}, 9, clock)
	if !changed {
		t.Fatal("applyEvent(SessionResidencyReleased) reported changed=false, so nothing is upserted and the catalog never goes cold")
	}
	assertResidency(t, released, ResidencyCold)
	assertRestorable(t, released)
	if released.State != meta.State {
		t.Errorf("State = %q, want it unchanged at %q: residency is independent of execution state", released.State, meta.State)
	}
	if released.LastJournalSeq != 9 {
		t.Errorf("LastJournalSeq = %d, want 9", released.LastJournalSeq)
	}
}

// TestStopProjectsColdAndTerminal pins the contrast the release event exists to
// draw. A stop is ALSO cold — no process is resident after either — so residency
// alone cannot tell them apart; terminality is what differs.
func TestStopProjectsColdAndTerminal(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x01)
	clock := fixedClock(time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC))

	meta, _ := mustApplyEvent(t, SessionMeta{}, event.SessionStarted{Header: hdr(sid)}, 1, clock)
	stopped, changed := mustApplyEvent(t, meta, event.SessionStopped{Header: hdr(sid)}, 5, clock)
	if !changed {
		t.Fatal("applyEvent(SessionStopped) reported changed=false")
	}
	assertResidency(t, stopped, ResidencyCold)
	if stopped.Status != StatusStopped || stopped.State != StateStopped {
		t.Errorf("Status/State = %q/%q, want stopped/stopped", stopped.Status, stopped.State)
	}
}

// TestRestoreAfterReleaseProjectsResidentAgain proves the cold projection is not a
// one-way door: a successor that restores the released session is resident, so the
// catalog reports it as resident again.
func TestRestoreAfterReleaseProjectsResidentAgain(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x01)
	clock := fixedClock(time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC))

	meta, _ := mustApplyEvent(t, SessionMeta{}, event.SessionStarted{Header: hdr(sid)}, 1, clock)
	released, _ := mustApplyEvent(t, meta, event.SessionResidencyReleased{Header: hdr(sid)}, 9, clock)
	assertResidency(t, released, ResidencyCold)

	restored, changed := mustApplyEvent(t, released, event.RestoreDone{Header: hdr(sid)}, 12, clock)
	if !changed {
		t.Fatal("applyEvent(RestoreDone) reported changed=false")
	}
	assertResidency(t, restored, ResidencyResident)
	assertRestorable(t, restored)
}
