package sessionstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/runtimecommand"
	durablestore "github.com/looprig/sessionstore"
	"github.com/looprig/storage/memstore"
)

// This file is the PRODUCTION COMPOSITION, end to end, with no transplant and no
// fake: an ORCHESTRATION store on its own backend, holding the catalog, the
// disposition inbox and the residency, configured with this package's exported
// reader; a HARNESS store on a different backend, holding the session's journal; and
// a settlement that crosses between them through the session's immutable binding
// alone.
//
// The separation is the design. The orchestration store never reads a journal
// itself: it hands its configured reader the binding and asks. That is what
// WithDispositionEvidence is for, and until this file existed it was the one seam in
// the whole protocol that nothing exercised.
//
// THE TWO IDENTITY SPACES ARE DELIBERATELY DIFFERENT HERE. The orchestration store
// knows the session by an opaque id that is NOT a UUID — the shape Factory admits a
// session under — while the journal is addressed by Binding.RuntimeSessionID, which
// is the rig's UUID. A composition that quietly required the two to agree would pass
// a test that used one id for both, and fail in production on the first session whose
// public identity was not a uuid rendering.

// composedSessionID is deliberately not a UUID. See the file header.
const composedSessionID coresessionwire.SessionID = "factory/session:composed-1"

// composedBindingID names the journal store this session is bound to. The reader
// does not route on it; see TestReaderDoesNotRouteOnStorageBindingID.
const composedBindingID = "harness-journals/east"

// composition is one orchestration store over its OWN backend, plus the Harness
// store and journal it settles against.
type composition struct {
	orchestration *durablestore.Store
	writer        *harnessWriter
	residency     *durablestore.ResidencyGrant
	binding       durablestore.SessionBinding
}

func newComposition(t *testing.T) *composition {
	t.Helper()
	ctx := context.Background()
	w := newHarnessWriter(t)
	binding := durablestore.SessionBinding{
		StorageBindingID: composedBindingID,
		BindingVersion:   "config-2026-09",
		// The journal-side identity. This is the ONLY place the rig's UUID enters the
		// orchestration store, and it is what the reader routes on.
		RuntimeSessionID: w.session.String(),
		ProtocolMode:     durablestore.ProtocolModeDisposition,
	}
	// A DIFFERENT backend, and the multi-tenant layout the disposition family
	// requires. The reader is this package's Store, configured directly — it
	// satisfies the released DispositionEvidenceReader, so no adapter stands between
	// the settlement and the journal store.
	orchestration, err := durablestore.Open(ctx, memstore.New(),
		durablestore.WithDispositionEvidence(w.store),
	)
	if err != nil {
		t.Fatalf("Open(orchestration): %v", err)
	}
	if _, _, err := orchestration.CreateCatalogEntry(ctx, durablestore.CreateCatalogEntryRequest{
		TenantID: harnessTenantID, SessionID: composedSessionID,
		AgentID: "agent-a", RuntimeCompatibilityID: "runtime-v1",
		CreatedAt: time.Now().UTC(), LastActiveAt: time.Now().UTC(),
		State: coresessionwire.SessionStateIdle, Residency: coresessionwire.SessionResidencyCold,
		DesiredPlacement: coresessionwire.HostPlacementPooled,
		IdempotencyKey:   "create-1", Binding: binding,
	}); err != nil {
		t.Fatalf("CreateCatalogEntry: %v", err)
	}
	grant, err := orchestration.AcquireResidency(ctx, durablestore.AcquireResidencyRequest{
		TenantID: harnessTenantID, SessionID: composedSessionID,
	})
	if err != nil {
		t.Fatalf("AcquireResidency: %v", err)
	}
	t.Cleanup(func() { _ = grant.Release(context.Background()) })
	return &composition{orchestration: orchestration, writer: w, residency: grant, binding: binding}
}

// dispatch drives one command through admit -> claim -> begin-attempt, which is the
// state a runtime is dispatched in, recording the JOURNAL grant the runtime actually
// holds. It returns the applying entry.
func (c *composition) dispatch(
	t *testing.T, commandID coresessionwire.CommandID, kind runtimecommand.Kind,
	runtimeID uuid.UUID, attemptID runtimecommand.AttemptID, journalEpoch uint64,
) durablestore.DispositionInboxEntry {
	t.Helper()
	ctx := context.Background()
	admitted, _, err := c.orchestration.AdmitDispositionCommand(ctx, durablestore.AdmitDispositionCommandRequest{
		TenantID: harnessTenantID, SessionID: composedSessionID, CommandID: commandID,
		Binding:                  c.binding,
		ProposedRuntimeCommandID: durablestore.RuntimeCommandID(runtimeID.String()),
		Kind:                     durablestore.CommandKind(kind),
		Payload:                  []byte(`{"p":1}`),
		AcceptedAt:               time.Now().UTC(),
		ApplyDeadline:            time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("AdmitDispositionCommand: %v", err)
	}
	claimed, _, err := c.orchestration.ClaimDispositionCommand(ctx, durablestore.ClaimDispositionCommandRequest{
		TenantID: harnessTenantID, SessionID: composedSessionID, CommandID: commandID,
		ExpectedRevision: admitted.Revision,
		Residency:        c.residency,
		ClaimExpiresAt:   time.Now().UTC().Add(roundTripClaimWindow),
	})
	if err != nil {
		t.Fatalf("ClaimDispositionCommand: %v", err)
	}
	// JournalEpoch is the grant the RUNTIME returned, never a copy of the residency.
	// The two domains are separate numbers and the protocol never compares them.
	applying, err := c.orchestration.BeginDispositionAttempt(ctx, durablestore.BeginDispositionAttemptRequest{
		TenantID: harnessTenantID, SessionID: composedSessionID, CommandID: commandID,
		ExpectedRevision: claimed.Revision,
		AttemptID:        durablestore.DispositionAttemptID(attemptID),
		JournalEpoch:     durablestore.JournalEpoch(journalEpoch),
		ResidencyEpoch:   c.residency.Epoch(),
		StartedAt:        time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("BeginDispositionAttempt: %v", err)
	}
	return applying
}

func (c *composition) settle(
	t *testing.T, commandID coresessionwire.CommandID, revision uint64,
) (durablestore.DispositionInboxEntry, bool, error) {
	t.Helper()
	return c.orchestration.SettleDispositionCommand(context.Background(), durablestore.SettleDispositionCommandRequest{
		TenantID: harnessTenantID, SessionID: composedSessionID, CommandID: commandID,
		ExpectedRevision: revision,
		ResidencyEpoch:   c.residency.Epoch(),
	})
}

// TestSettlementFromAHarnessJournalThroughTheBinding is the acceptance criterion for
// the whole release: an orchestration store on its own backend settles a command
// from evidence a Harness journal on a DIFFERENT backend holds, reached only through
// the session's immutable binding.
//
// Every decision is the released store's: its evidence-reader seam, its fence
// cross-check inside the delegated read, its verifier, and its terminal-state
// mapping. This package supplies the frames and the addressing, and nothing else.
//
// The terminal state is asserted per kind because the split is the protocol's rather
// than this writer's: a successful no-op is an APPLICATION — the runtime accepted the
// command and it had no effect — and settles applied, while a refusal settles
// rejected. Collapsing the two would be invisible in a decode test.
func TestSettlementFromAHarnessJournalThroughTheBinding(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		kind        runtimecommand.Kind
		disposition runtimecommand.DispositionKind
		want        durablestore.InboxState
	}{
		"input applied":     {runtimecommand.KindInput, runtimecommand.DispositionApplied, durablestore.InboxStateApplied},
		"interrupt applied": {runtimecommand.KindInterrupt, runtimecommand.DispositionApplied, durablestore.InboxStateApplied},
		"interrupt no_op":   {runtimecommand.KindInterrupt, runtimecommand.DispositionNoOp, durablestore.InboxStateApplied},
		"input refused":     {runtimecommand.KindInput, runtimecommand.DispositionRefused, durablestore.InboxStateRejected},
		"interrupt refused": {runtimecommand.KindInterrupt, runtimecommand.DispositionRefused, durablestore.InboxStateRejected},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			c := newComposition(t)
			commandID := coresessionwire.CommandID("public/command:" + name)
			runtimeID := newTestUUID(t)
			attemptID := runtimecommand.AttemptID("attempt/" + name)
			epoch := c.writer.lease.Epoch()
			applying := c.dispatch(t, commandID, row.kind, runtimeID, attemptID, epoch)

			// The runtime writes its disposition into ITS journal, on ITS backend. The
			// orchestration store learns nothing from this call.
			res, err := c.writer.log.AppendCommandDisposition(ctx, runtimecommand.CommandDisposition{
				CommandID:           runtimecommand.CommandID(commandID),
				RuntimeCommandID:    runtimeID,
				Kind:                row.kind,
				LeaseEpoch:          epoch,
				AttemptID:           attemptID,
				AttemptJournalEpoch: epoch,
				Disposition:         row.disposition,
			})
			if err != nil {
				t.Fatalf("AppendCommandDisposition: %v", err)
			}

			settled, ok, err := c.settle(t, commandID, applying.Revision)
			if err != nil || !ok {
				t.Fatalf("SettleDispositionCommand across the binding: ok=%v err=%v", ok, err)
			}
			if settled.Record.State != row.want {
				t.Fatalf("settled state = %q, want %q", settled.Record.State, row.want)
			}
			outcome := settled.Record.Outcome
			if outcome == nil {
				t.Fatalf("settled with no outcome")
			}
			if outcome.Kind != durablestore.DispositionOutcomeKind(row.disposition) {
				t.Errorf("outcome kind = %q, want %q", outcome.Kind, row.disposition)
			}
			if outcome.AttemptID != durablestore.DispositionAttemptID(attemptID) {
				t.Errorf("outcome attempt = %q, want %q", outcome.AttemptID, attemptID)
			}
			if outcome.DispositionSeq != res.Sequence {
				t.Errorf("outcome disposition seq = %d, want the sequence the journal append reported, %d",
					outcome.DispositionSeq, res.Sequence)
			}
			// The author grant the reader derived from the journal's opening fence,
			// crossing the store boundary intact.
			if uint64(outcome.AuthorJournalEpoch) != epoch || uint64(outcome.AttemptJournalEpoch) != epoch {
				t.Errorf("grants = author %d attempt %d, want %d for both",
					outcome.AuthorJournalEpoch, outcome.AttemptJournalEpoch, epoch)
			}
			// The residency rides through as settlement CONTEXT and is never compared
			// with a journal epoch. Asserting it here is what keeps the two domains
			// visibly separate in the composition that could most easily fuse them.
			if outcome.SettlingResidencyEpoch != c.residency.Epoch() {
				t.Errorf("settling residency = %d, want %d", outcome.SettlingResidencyEpoch, c.residency.Epoch())
			}
		})
	}
}

// TestRecoveryClosureSettlesAcrossTheBinding is the fourth kind through the same
// composition, and it needs a journal shape the table cannot produce: the closure is
// authored by a SUCCESSOR runtime, so a second lease is taken and a second journal
// opened, planting a strictly higher opening fence above the predecessor's records.
//
// It is the settlement that licenses a REJECTION over a command whose attempt is
// gone, so it is the one most worth proving end to end: the verifier requires the
// author grant to be strictly later than the attempt's AND the author's own fence
// sequence to precede the disposition, and only a real second fence supplies both.
func TestRecoveryClosureSettlesAcrossTheBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newComposition(t)
	commandID := coresessionwire.CommandID("public/command:closed")
	runtimeID := newTestUUID(t)
	attemptID := runtimecommand.AttemptID("attempt/closed")
	attemptEpoch := c.writer.lease.Epoch()
	applying := c.dispatch(t, commandID, runtimecommand.KindInput, runtimeID, attemptID, attemptEpoch)

	if err := c.writer.lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	successorLease, err := c.writer.store.AcquireLease(ctx, c.writer.session)
	if err != nil {
		t.Fatalf("AcquireLease(successor): %v", err)
	}
	if successorLease.Epoch() <= attemptEpoch {
		t.Fatalf("successor grant %d is not strictly later than %d", successorLease.Epoch(), attemptEpoch)
	}
	successorJournal, err := c.writer.store.OpenJournal(ctx, c.writer.session, successorLease)
	if err != nil {
		t.Fatalf("OpenJournal(successor): %v", err)
	}
	successorLog, err := c.writer.store.OpenRuntimeCommandLog(c.writer.session, successorJournal)
	if err != nil {
		t.Fatalf("OpenRuntimeCommandLog(successor): %v", err)
	}
	if _, err := successorLog.AppendCommandDisposition(ctx, runtimecommand.CommandDisposition{
		CommandID:           runtimecommand.CommandID(commandID),
		RuntimeCommandID:    runtimeID,
		Kind:                runtimecommand.KindInput,
		LeaseEpoch:          successorLease.Epoch(),
		AttemptID:           attemptID,
		AttemptJournalEpoch: attemptEpoch,
		Disposition:         runtimecommand.DispositionNotApplied,
	}); err != nil {
		t.Fatalf("AppendCommandDisposition(closure): %v", err)
	}

	settled, ok, err := c.settle(t, commandID, applying.Revision)
	if err != nil || !ok {
		t.Fatalf("SettleDispositionCommand(closure): ok=%v err=%v", ok, err)
	}
	if settled.Record.State != durablestore.InboxStateRejected {
		t.Fatalf("a recovery closure settled %q, want %q", settled.Record.State, durablestore.InboxStateRejected)
	}
	outcome := settled.Record.Outcome
	if outcome.Kind != durablestore.DispositionNotApplied {
		t.Errorf("outcome kind = %q, want not_applied", outcome.Kind)
	}
	if uint64(outcome.AuthorJournalEpoch) != successorLease.Epoch() {
		t.Errorf("author grant = %d, want the successor's %d", outcome.AuthorJournalEpoch, successorLease.Epoch())
	}
	if uint64(outcome.AttemptJournalEpoch) != attemptEpoch {
		t.Errorf("attempt grant = %d, want the predecessor's %d", outcome.AttemptJournalEpoch, attemptEpoch)
	}
	if outcome.AuthorFenceSeq == 0 || outcome.AuthorFenceSeq >= outcome.DispositionSeq {
		t.Errorf("author fence seq = %d, want a non-zero sequence below the disposition's %d",
			outcome.AuthorFenceSeq, outcome.DispositionSeq)
	}
}

// TestSettlementWithoutEvidenceLeavesTheCommandApplying is the negative half of the
// composition, and it is here to pin the CORRECT failure vocabulary — which is not
// the legacy one.
//
// With no disposition in the journal, settlement does not reach a wrong answer by any
// route: the reader refuses absence, settlement propagates the refusal, and the
// record stays applying. There is no `absent` and no `abandoned` here — those are the
// LEGACY correlation's outcomes, and two of them license a deadline reconciler to
// settle rejected over a durable effect. The disposition family has no such route,
// which is why a stalled command is strictly better than the hazard it replaces.
func TestSettlementWithoutEvidenceLeavesTheCommandApplying(t *testing.T) {
	t.Parallel()
	c := newComposition(t)
	commandID := coresessionwire.CommandID("public/command:no-evidence")
	applying := c.dispatch(t, commandID, runtimecommand.KindInput, newTestUUID(t),
		"attempt/no-evidence", c.writer.lease.Epoch())

	settled, ok, err := c.settle(t, commandID, applying.Revision)
	if err == nil {
		t.Fatalf("settlement succeeded with no evidence: %+v %v", settled, ok)
	}
	var inboxErr *durablestore.InboxError
	if !errors.As(err, &inboxErr) {
		t.Fatalf("err = %v, want an *InboxError", err)
	}
	if inboxErr.Code != durablestore.InboxErrorEvidence || inboxErr.Field != "disposition" {
		t.Errorf("err = %+v, want InboxErrorEvidence on the disposition field", inboxErr)
	}
	// And the record is untouched: still applying, at the same revision. A settlement
	// that failed must not have half-moved it.
	current, err := c.orchestration.GetDispositionCommand(context.Background(), durablestore.GetDispositionCommandRequest{
		TenantID: harnessTenantID, SessionID: composedSessionID, CommandID: commandID,
	})
	if err != nil {
		t.Fatalf("ReadDispositionCommand: %v", err)
	}
	if current.Record.State != durablestore.InboxStateApplying {
		t.Errorf("state = %q, want applying", current.Record.State)
	}
	if current.Revision != applying.Revision {
		t.Errorf("revision moved from %d to %d on a failed settlement", applying.Revision, current.Revision)
	}
}

// TestReaderRefusesABindingItCannotResolve covers the reader's own refusals. Each is
// reachable through the exported method — it is public API, so "the settlement would
// never build this request" is not a reason to leave it unguarded — and each must be
// an ERROR rather than an empty DispositionEvidence, because the released contract is
// explicit that a reader returning the zero value settles nothing while looking like
// an answer.
func TestReaderRefusesABindingItCannotResolve(t *testing.T) {
	t.Parallel()
	w := newHarnessWriter(t)
	base := durablestore.DispositionEvidenceRequest{
		TenantID:         harnessTenantID,
		SessionID:        composedSessionID,
		CommandID:        "public/command:x",
		Kind:             durablestore.CommandKind(runtimecommand.KindInput),
		RuntimeCommandID: durablestore.RuntimeCommandID(newTestUUID(t).String()),
		Binding: durablestore.SessionBinding{
			StorageBindingID: composedBindingID,
			BindingVersion:   "config-2026-09",
			RuntimeSessionID: w.session.String(),
			ProtocolMode:     durablestore.ProtocolModeDisposition,
		},
		Attempt: durablestore.DispositionAttempt{AttemptID: "attempt/x", JournalEpoch: 1},
	}
	for field, mutate := range map[string]func(*durablestore.DispositionEvidenceRequest){
		"TenantID": func(r *durablestore.DispositionEvidenceRequest) {
			r.TenantID = "another-tenant"
		},
		"Binding.ProtocolMode": func(r *durablestore.DispositionEvidenceRequest) {
			r.Binding.ProtocolMode = durablestore.ProtocolModeLegacy
		},
		"Binding.RuntimeSessionID": func(r *durablestore.DispositionEvidenceRequest) {
			// The opaque orchestration identity, which is exactly the value a
			// composition that confused the two identity spaces would put here.
			r.Binding.RuntimeSessionID = string(composedSessionID)
		},
	} {
		req := base
		mutate(&req)
		evidence, err := w.store.ReadDispositionEvidence(context.Background(), req)
		if err == nil {
			t.Errorf("%s: the reader answered a binding it cannot resolve", field)
			continue
		}
		if evidence != (durablestore.DispositionEvidence{}) {
			t.Errorf("%s: a refusal carried evidence %+v", field, evidence)
		}
		var bindingErr *DispositionBindingError
		if !errors.As(err, &bindingErr) {
			t.Errorf("%s: err = %v, want a *DispositionBindingError", field, err)
			continue
		}
		if bindingErr.Field != field {
			t.Errorf("%s: Field = %q, want %q", field, bindingErr.Field, field)
		}
	}
	// The NIL UUID is its own row because it is the one malformed binding that
	// PARSES. It renders a perfectly canonical legacy session id, so without an
	// explicit zero check it addresses a real-looking journal that never existed and
	// the refusal arrives from the journal as absent evidence — a settlement waiting
	// for a runtime to write into a session id nothing will ever hold.
	{
		req := base
		req.Binding.RuntimeSessionID = uuid.UUID{}.String()
		_, err := w.store.ReadDispositionEvidence(context.Background(), req)
		var bindingErr *DispositionBindingError
		if !errors.As(err, &bindingErr) {
			t.Errorf("nil uuid: err = %v, want a *DispositionBindingError", err)
		} else if bindingErr.Field != "Binding.RuntimeSessionID" {
			t.Errorf("nil uuid: Field = %q, want Binding.RuntimeSessionID", bindingErr.Field)
		}
	}
	// The unmutated baseline must reach the JOURNAL and fail there — on absent
	// evidence — rather than at the binding. Without this the rows above could all be
	// passing for a reason that has nothing to do with what they mutate.
	_, err := w.store.ReadDispositionEvidence(context.Background(), base)
	var bindingErr *DispositionBindingError
	if errors.As(err, &bindingErr) {
		t.Fatalf("the valid baseline was refused at the binding: %v", err)
	}
	var inboxErr *durablestore.InboxError
	if !errors.As(err, &inboxErr) || inboxErr.Code != durablestore.InboxErrorEvidence {
		t.Fatalf("the valid baseline failed with %v, want the journal's absent-evidence refusal", err)
	}
}

// TestReaderDoesNotRouteOnStorageBindingID pins an obligation that MOVES rather than
// disappearing: this reader answers for the keyspace it owns, and it cannot tell its
// own StorageBindingID from another's because a Store has no registry of the bindings
// it serves.
//
// So selecting which reader serves which StorageBindingID is the composition root's
// job, and a deployment with two journal stores needs a multiplexer keyed on that
// field in front of these readers. This test records the consequence so nobody
// assumes the routing already happens here; if a future change adds the check, this
// test fails and the multiplexer story must be revisited rather than silently
// duplicated.
func TestReaderDoesNotRouteOnStorageBindingID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newHarnessWriter(t)
	runtimeID := newTestUUID(t)
	epoch := w.lease.Epoch()
	d := runtimecommand.CommandDisposition{
		CommandID:           "public/command:binding-id",
		RuntimeCommandID:    runtimeID,
		Kind:                runtimecommand.KindInput,
		LeaseEpoch:          epoch,
		AttemptID:           "attempt/binding-id",
		AttemptJournalEpoch: epoch,
		Disposition:         runtimecommand.DispositionApplied,
	}
	if _, err := w.log.AppendCommandDisposition(ctx, d); err != nil {
		t.Fatalf("AppendCommandDisposition: %v", err)
	}
	req := durablestore.DispositionEvidenceRequest{
		TenantID:         harnessTenantID,
		SessionID:        composedSessionID,
		CommandID:        coresessionwire.CommandID(d.CommandID),
		Kind:             durablestore.CommandKind(d.Kind),
		RuntimeCommandID: durablestore.RuntimeCommandID(runtimeID.String()),
		Binding: durablestore.SessionBinding{
			StorageBindingID: "some-other-journal-store/west",
			BindingVersion:   "config-2026-09",
			RuntimeSessionID: w.session.String(),
			ProtocolMode:     durablestore.ProtocolModeDisposition,
		},
		Attempt: durablestore.DispositionAttempt{
			AttemptID: durablestore.DispositionAttemptID(d.AttemptID), JournalEpoch: durablestore.JournalEpoch(epoch),
		},
	}
	evidence, err := w.store.ReadDispositionEvidence(ctx, req)
	if err != nil {
		t.Fatalf("the reader refused a foreign StorageBindingID; the routing obligation has moved and the doc on ReadDispositionEvidence must be revisited: %v", err)
	}
	if evidence.Kind != durablestore.DispositionApplied {
		t.Fatalf("evidence kind = %q, want applied", evidence.Kind)
	}
}

// TestReaderWalksAFullSessionLifecycle retires a real risk rather than restating a
// covered one. Every other fixture reads a journal of two or three frames, and the
// walk the released reader performs is ALL-OR-NOTHING: a single fail-closed decode
// refusal stops it, so a frame shape nothing had put in front of it would take the
// whole settlement down rather than being stepped over.
//
// This journal is the shape a real session leaves by the time a command is settled:
// an opening fence, public enduring events, an INTERNAL event (framed as runtime
// control), an audit intent command record, a private gate-prepared record, an
// application prefix, an OFFLOADED over-threshold body (an object reference rather
// than an inline one, which is a different envelope shape on the same walk), and only
// then the disposition.
func TestReaderWalksAFullSessionLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newComposition(t)
	w := c.writer
	epoch := w.lease.Epoch()
	commandID := coresessionwire.CommandID("public/command:lifecycle")
	runtimeID := newTestUUID(t)
	attemptID := runtimecommand.AttemptID("attempt/lifecycle")
	applying := c.dispatch(t, commandID, runtimecommand.KindInput, runtimeID, attemptID, epoch)

	loopID := newTestUUID(t)
	appendLifecycleRecords(t, w, loopID, runtimeID)

	// The application prefix the applier writes before the effect.
	if _, err := w.log.AppendCommandApplication(ctx, runtimecommand.Application{
		CommandID:        runtimecommand.CommandID(commandID),
		RuntimeCommandID: runtimeID,
		LeaseEpoch:       epoch,
		Kind:             runtimecommand.KindInput,
	}); err != nil {
		t.Fatalf("AppendCommandApplication: %v", err)
	}
	// A body above the offload threshold, so the walk meets an object-referenced
	// frame and not only inline ones.
	appendOffloadedEvent(t, w, loopID)

	if _, err := w.log.AppendCommandDisposition(ctx, runtimecommand.CommandDisposition{
		CommandID:           runtimecommand.CommandID(commandID),
		RuntimeCommandID:    runtimeID,
		Kind:                runtimecommand.KindInput,
		LeaseEpoch:          epoch,
		AttemptID:           attemptID,
		AttemptJournalEpoch: epoch,
		Disposition:         runtimecommand.DispositionApplied,
	}); err != nil {
		t.Fatalf("AppendCommandDisposition: %v", err)
	}

	frames := harnessFrames(t, w.store, w.session)
	if len(frames) < 8 {
		t.Fatalf("the journal holds %d frames, too few to be a lifecycle", len(frames))
	}
	settled, ok, err := c.settle(t, commandID, applying.Revision)
	if err != nil || !ok {
		t.Fatalf("settlement over a %d-frame lifecycle journal: ok=%v err=%v", len(frames), ok, err)
	}
	if settled.Record.State != durablestore.InboxStateApplied {
		t.Fatalf("settled %q, want applied", settled.Record.State)
	}
	// The disposition is the LAST frame, so the walk reached the end rather than
	// stopping early on something it could not read.
	if settled.Record.Outcome.DispositionSeq != uint64(len(frames)) {
		t.Errorf("disposition seq = %d, want the final frame %d",
			settled.Record.Outcome.DispositionSeq, len(frames))
	}
}

func appendLifecycleRecords(t *testing.T, w *harnessWriter, loopID, runtimeID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	header := func() event.Header {
		return event.Header{
			Coordinates: identity.Coordinates{SessionID: w.session, LoopID: loopID},
			EventID:     newTestUUID(t),
			CreatedAt:   time.Now().UTC(),
		}
	}
	sessionStarted := event.SessionStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: w.session},
		EventID:     newTestUUID(t),
		CreatedAt:   time.Now().UTC(),
	}}
	loopStarted := event.LoopStarted{Header: header()}
	turnHeader := header()
	turnHeader.TurnID = newTestUUID(t)
	turnHeader.Cause = identity.Cause{CommandID: runtimeID, Agency: identity.AgencyUser}
	turnStarted := event.TurnStarted{Header: turnHeader, TurnIndex: 1}
	for _, ev := range []event.Event{sessionStarted, loopStarted, turnStarted} {
		if _, err := w.journal.Append(ctx, journal.NewEventRecord(ev)); err != nil {
			t.Fatalf("Append(%T): %v", ev, err)
		}
	}
	// An audit intent command record: a private runtime-control frame, which is the
	// shape a concurrent legacy Submit leaves in a real journal.
	if _, err := w.journal.Append(ctx, journal.NewCommandRecord(w.session, loopID, command.Interrupt{
		Header: command.Header{CommandID: newTestUUID(t)},
	})); err != nil {
		t.Fatalf("Append(command record): %v", err)
	}
}

// appendOffloadedEvent appends an INTERNAL event whose body exceeds the Store's
// offload threshold, so its frame is an EnvelopeKindRuntimeControl carrying an OBJECT
// REFERENCE rather than an inline body. That is two shapes in one record, and both
// are ones the other fixtures never put in front of the walk.
//
// The evidence reader resolves no bodies, which is exactly why this is worth
// appending: the claim under test is that it walks PAST an unresolved reference
// rather than trying to fetch it.
// bigUserMessage builds the message out of line because content.UserMessage carries
// Blocks on an embedded type, and a promoted-field literal needs a newer language
// version than this module declares.
func bigUserMessage(text string) *content.UserMessage {
	msg := &content.UserMessage{}
	msg.Blocks = []content.Block{&content.TextBlock{Text: text}}
	return msg
}

func appendOffloadedEvent(t *testing.T, w *harnessWriter, loopID uuid.UUID) {
	t.Helper()
	big := make([]byte, defaultOffloadThreshold+4096)
	for i := range big {
		big[i] = 'a' + byte(i%26)
	}
	header := event.Header{
		Coordinates: identity.Coordinates{SessionID: w.session, LoopID: loopID},
		EventID:     newTestUUID(t),
		CreatedAt:   time.Now().UTC(),
	}
	header.TurnID = newTestUUID(t)
	// Internal, so the frame is runtime control and no public projection is
	// attempted: the point here is the envelope shape, not the wire body.
	header.EventVisibility = event.Internal
	ev := event.TurnStarted{
		Header:    header,
		TurnIndex: 2,
		Message:   bigUserMessage(string(big)),
	}
	seq, err := w.journal.Append(context.Background(), journal.NewEventRecord(ev))
	if err != nil {
		t.Fatalf("Append(offloaded event): %v", err)
	}
	frames := harnessFrames(t, w.store, w.session)
	env, err := durablestore.DecodeEnvelope(frames[seq-1])
	if err != nil {
		t.Fatalf("DecodeEnvelope(offloaded): %v", err)
	}
	// Non-vacuity: if the body stayed inline the walk never met the shape this
	// helper exists to put in front of it.
	if env.Runtime.Reference == nil && env.Public.Reference == nil {
		t.Fatalf("the %d-byte body stayed inline; the offloaded shape was not exercised", len(big))
	}
}
