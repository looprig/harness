package sessionstore

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/runtimecommand"
	durablestore "github.com/looprig/sessionstore"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// This file is the ACCEPTANCE evidence for the disposition writer: bytes Harness
// frames and appends are read back, verified and SETTLED by the released
// sessionstore, through its own production evidence reader and its own verifier.
// Nothing that decides anything here is a fake.
//
// It is deliberately more than a decode assertion, because the halves that can
// silently disagree are not in the codec. The reader derives the author grant from
// the NEAREST PRECEDING OPENING FENCE and refuses a record whose own LeaseEpoch
// disagrees — harness bypasses the store's JournalWriter, so no field is
// store-stamped and every one is a CLAIM — and the verifier then compares the
// record's attempt grant against the store's own immutable attempt. A writer can
// satisfy EncodeEnvelope and still fail both.
//
// WHY THERE ARE TWO ROUND TRIPS RATHER THAN ONE, and it is a finding rather than a
// convenience. A disposition-family session must be created with
// ProtocolModeDisposition, and sessionstore's bindProtocolMode REFUSES that mode on
// the legacy single-tenant layout outright. harness/pkg/sessionstore opens the
// released store with WithLegacySingleTenant and derives its ledger name itself as
// "sessions/<uuid>", so a Harness journal is always a legacy-layout journal and can
// never BE the bound journal of a disposition session. That is a placement gap in
// the module graph, not in this writer: the frames are right, the ledger they land
// in is the wrong one. See TestHarnessJournalScopeCannotHostADispositionSession,
// which pins the refusal so the gap cannot close silently.
//
// So the coverage is split. TestHarnessDispositionFramesAreAcceptedByTheReleasedReader
// runs the REAL production reader over the REAL Harness journal, which is what
// exercises the fence derivation and the lease-epoch cross-check against bytes this
// module actually placed. TestHarnessDispositionBytesSettleThroughTheReleasedStore
// transplants those EXACT frame bytes into a disposition session and runs the whole
// settlement, which is what exercises the verifier and the terminal-state mapping.
// Between them every decision the store makes about a Harness-written disposition is
// covered; what is not covered is the placement, because it is not currently possible.

const roundTripClaimWindow = time.Minute

func dispositionBinding() durablestore.SessionBinding {
	return durablestore.SessionBinding{
		StorageBindingID: "agent-pool/east",
		BindingVersion:   "config-2026-09",
		RuntimeSessionID: "runtime/session-a",
		ProtocolMode:     durablestore.ProtocolModeDisposition,
	}
}

// harnessWriter is one Harness journal over its own store, plus the released store
// reading the same backend on the same legacy layout.
type harnessWriter struct {
	store   *Store
	reader  *durablestore.Store
	session uuid.UUID
	wireID  coresessionwire.SessionID
	lease   journal.Lease
	journal journal.SessionJournal
	log     *RuntimeCommandLog
}

func newHarnessWriter(t *testing.T) *harnessWriter {
	t.Helper()
	ctx := context.Background()
	backend := memstore.New()
	hs, err := Open(backend)
	if err != nil {
		t.Fatalf("Open(harness): %v", err)
	}
	// The SAME backend, opened the only way a counterparty can share a Harness
	// scope: the legacy layout under the same tenant.
	reader, err := durablestore.Open(ctx, backend,
		durablestore.WithLegacySingleTenant(harnessTenantID),
		durablestore.WithJournalDispositionEvidence(),
	)
	if err != nil {
		t.Fatalf("Open(durable): %v", err)
	}
	w := &harnessWriter{store: hs, reader: reader, session: newTestUUID(t)}
	w.wireID = harnessSessionID(w.session)
	w.lease, err = hs.AcquireLease(ctx, w.session)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	w.journal, err = hs.OpenJournal(ctx, w.session, w.lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	w.log, err = hs.OpenRuntimeCommandLog(w.session, w.journal)
	if err != nil {
		t.Fatalf("OpenRuntimeCommandLog: %v", err)
	}
	return w
}

func (w *harnessWriter) evidenceRequest(
	commandID coresessionwire.CommandID, kind runtimecommand.Kind, runtimeID uuid.UUID,
	attemptID runtimecommand.AttemptID, attemptEpoch uint64,
) durablestore.DispositionEvidenceRequest {
	return durablestore.DispositionEvidenceRequest{
		TenantID:         harnessTenantID,
		SessionID:        w.wireID,
		CommandID:        commandID,
		Kind:             durablestore.CommandKind(kind),
		RuntimeCommandID: durablestore.RuntimeCommandID(runtimeID.String()),
		Binding:          dispositionBinding(),
		Attempt: durablestore.DispositionAttempt{
			AttemptID:    durablestore.DispositionAttemptID(attemptID),
			JournalEpoch: durablestore.JournalEpoch(attemptEpoch),
		},
	}
}

// TestHarnessDispositionFramesAreAcceptedByTheReleasedReader runs the released
// PRODUCTION evidence reader over a real Harness journal. Every refusal the reader
// documents is a refusal these frames must not earn: an unknown envelope kind, a
// mismatched command/runtime/kind correlation, a disposition ahead of every opening
// fence, and — the one no fake would catch — a LeaseEpoch that disagrees with the
// nearest preceding fence.
func TestHarnessDispositionFramesAreAcceptedByTheReleasedReader(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		kind        runtimecommand.Kind
		disposition runtimecommand.DispositionKind
	}{
		"input applied":     {runtimecommand.KindInput, runtimecommand.DispositionApplied},
		"input refused":     {runtimecommand.KindInput, runtimecommand.DispositionRefused},
		"interrupt applied": {runtimecommand.KindInterrupt, runtimecommand.DispositionApplied},
		"interrupt no_op":   {runtimecommand.KindInterrupt, runtimecommand.DispositionNoOp},
		"interrupt refused": {runtimecommand.KindInterrupt, runtimecommand.DispositionRefused},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			w := newHarnessWriter(t)
			commandID := coresessionwire.CommandID("public/command:" + name)
			runtimeID := newTestUUID(t)
			attemptID := runtimecommand.AttemptID("attempt/" + name)
			epoch := w.lease.Epoch()

			res, err := w.log.AppendCommandDisposition(ctx, runtimecommand.CommandDisposition{
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
			if !res.Appended || res.Sequence == 0 {
				t.Fatalf("AppendCommandDisposition = %+v, want a new durable frame", res)
			}

			evidence, err := w.reader.ReadDispositionEvidence(ctx,
				w.evidenceRequest(commandID, row.kind, runtimeID, attemptID, epoch))
			if err != nil {
				t.Fatalf("the released reader refused a Harness-written disposition: %v", err)
			}
			if evidence.Kind != durablestore.DispositionOutcomeKind(row.disposition) {
				t.Errorf("evidence kind = %q, want %q", evidence.Kind, row.disposition)
			}
			if evidence.AttemptID != durablestore.DispositionAttemptID(attemptID) {
				t.Errorf("evidence attempt = %q, want %q", evidence.AttemptID, attemptID)
			}
			if evidence.DispositionSeq != res.Sequence {
				t.Errorf("evidence seq = %d, want %d", evidence.DispositionSeq, res.Sequence)
			}
			// AuthorJournalEpoch is the FENCE's epoch, taken by the reader from the
			// journal and never from the record. It equals the claim only because the
			// claim is right.
			if uint64(evidence.AuthorJournalEpoch) != epoch {
				t.Errorf("author grant = %d, want the fence's %d", evidence.AuthorJournalEpoch, epoch)
			}
			if uint64(evidence.AttemptJournalEpoch) != epoch {
				t.Errorf("attempt grant = %d, want %d", evidence.AttemptJournalEpoch, epoch)
			}
			// Only a recovery closure names an author fence; the verifier refuses one
			// of these three kinds that does.
			if evidence.AuthorFenceSeq != 0 {
				t.Errorf("a %s disposition named author fence %d; it must name none",
					row.disposition, evidence.AuthorFenceSeq)
			}
			if evidence.EventID != "" || evidence.EventSeq != 0 {
				t.Errorf("the disposition named event %q at %d; a disposition names no event",
					evidence.EventID, evidence.EventSeq)
			}
		})
	}
}

// TestHarnessRecoveryClosureIsReadUnderTheSuccessorFence is the fourth kind, and it
// needs a genuinely different journal shape rather than another table row: the
// closure is authored by a SUCCESSOR, so a second lease is taken and a second
// journal opened, which plants a strictly higher opening fence above the
// predecessor's records.
//
// The fence ordering is the whole point. The reader attributes a disposition to the
// NEAREST PRECEDING fence, so a closure written before the successor's own fence
// would be attributed to the predecessor's grant and refused with lease_epoch — the
// same code a forged epoch earns.
func TestHarnessRecoveryClosureIsReadUnderTheSuccessorFence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newHarnessWriter(t)
	commandID := coresessionwire.CommandID("public/command:closed")
	runtimeID := newTestUUID(t)
	attemptID := runtimecommand.AttemptID("attempt/closed")
	attemptEpoch := w.lease.Epoch()

	if err := w.lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	successorLease, err := w.store.AcquireLease(ctx, w.session)
	if err != nil {
		t.Fatalf("AcquireLease(successor): %v", err)
	}
	if successorLease.Epoch() <= attemptEpoch {
		t.Fatalf("successor grant %d is not strictly later than %d", successorLease.Epoch(), attemptEpoch)
	}
	successorJournal, err := w.store.OpenJournal(ctx, w.session, successorLease)
	if err != nil {
		t.Fatalf("OpenJournal(successor): %v", err)
	}
	successorLog, err := w.store.OpenRuntimeCommandLog(w.session, successorJournal)
	if err != nil {
		t.Fatalf("OpenRuntimeCommandLog(successor): %v", err)
	}
	res, err := successorLog.AppendCommandDisposition(ctx, runtimecommand.CommandDisposition{
		CommandID:           runtimecommand.CommandID(commandID),
		RuntimeCommandID:    runtimeID,
		Kind:                runtimecommand.KindInput,
		LeaseEpoch:          successorLease.Epoch(),
		AttemptID:           attemptID,
		AttemptJournalEpoch: attemptEpoch,
		Disposition:         runtimecommand.DispositionNotApplied,
	})
	if err != nil {
		t.Fatalf("AppendCommandDisposition(closure): %v", err)
	}

	evidence, err := w.reader.ReadDispositionEvidence(ctx,
		w.evidenceRequest(commandID, runtimecommand.KindInput, runtimeID, attemptID, attemptEpoch))
	if err != nil {
		t.Fatalf("the released reader refused a Harness-written closure: %v", err)
	}
	if evidence.Kind != durablestore.DispositionNotApplied {
		t.Errorf("evidence kind = %q, want not_applied", evidence.Kind)
	}
	if uint64(evidence.AuthorJournalEpoch) != successorLease.Epoch() {
		t.Errorf("author grant = %d, want the successor's %d", evidence.AuthorJournalEpoch, successorLease.Epoch())
	}
	if uint64(evidence.AttemptJournalEpoch) != attemptEpoch {
		t.Errorf("attempt grant = %d, want the predecessor's %d", evidence.AttemptJournalEpoch, attemptEpoch)
	}
	if evidence.AuthorFenceSeq == 0 || evidence.AuthorFenceSeq >= evidence.DispositionSeq {
		t.Errorf("author fence seq = %d, want a non-zero sequence below the disposition's %d",
			evidence.AuthorFenceSeq, evidence.DispositionSeq)
	}
	_ = res
}

// namingLedger records every ledger name the wrapped provider is asked about. It
// exists for one reason: sessionstore derives a non-legacy session's journal name
// from a keyed digest and exposes no accessor for it, so a test that must place
// bytes in that journal has to learn the name from the store's own I/O rather than
// recompute it — recomputing would be a second implementation of the keyspace and
// would pass while disagreeing with the real one.
type namingLedger struct {
	storage.Ledger
	mu    sync.Mutex
	names map[string]struct{}
}

func newNamingLedger(inner storage.Ledger) *namingLedger {
	return &namingLedger{Ledger: inner, names: map[string]struct{}{}}
}

func (l *namingLedger) note(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names[name] = struct{}{}
}

func (l *namingLedger) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.names))
	for name := range l.names {
		out = append(out, name)
	}
	return out
}

func (l *namingLedger) Append(ctx context.Context, name string, expected uint64, payload []byte) error {
	l.note(name)
	return l.Ledger.Append(ctx, name, expected, payload)
}

func (l *namingLedger) Read(ctx context.Context, name string, from uint64) (storage.Cursor, error) {
	l.note(name)
	return l.Ledger.Read(ctx, name, from)
}

func (l *namingLedger) Tip(ctx context.Context, name string) (uint64, error) {
	l.note(name)
	return l.Ledger.Tip(ctx, name)
}

func (l *namingLedger) Delete(ctx context.Context, name string) error {
	l.note(name)
	return l.Ledger.Delete(ctx, name)
}

// harnessFrames reads every raw frame Harness wrote to its own ledger, in order.
// They are the exact bytes EncodeEnvelope produced — not a re-encoding.
func harnessFrames(t *testing.T, s *Store, id uuid.UUID) [][]byte {
	t.Helper()
	ctx := context.Background()
	cursor, err := s.backend.Ledger.Read(ctx, ledgerName(id), 1)
	if err != nil {
		t.Fatalf("Ledger.Read: %v", err)
	}
	defer func() { _ = cursor.Close() }()
	var frames [][]byte
	for {
		rec, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		frames = append(frames, append([]byte(nil), rec.Payload...))
	}
}

// TestHarnessDispositionBytesSettleThroughTheReleasedStore is the other half of the
// acceptance: the EXACT frame bytes a Harness journal produced are verified and
// settled by the released store's whole settlement path — evidence reader, fence
// cross-check, verifier and terminal-state mapping.
//
// The bytes are transplanted rather than written in place, and the transplant is the
// gap this file's header names: a disposition session's journal is a non-legacy
// ledger, and a Harness journal is always a legacy one. Nothing about the FRAMES is
// simulated — they are read back out of Harness's own ledger, opaque, and appended
// unmodified, fence included, so the reader resolves the author grant from Harness's
// own fence rather than from one this test wrote.
//
// The terminal state is asserted per kind because the split is the protocol's: a
// successful no-op is an APPLICATION and settles applied, while a refusal settles
// rejected. Collapsing the two would be invisible in a decode test.
func TestHarnessDispositionBytesSettleThroughTheReleasedStore(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		kind        runtimecommand.Kind
		disposition runtimecommand.DispositionKind
		want        durablestore.InboxState
	}{
		"input applied":     {runtimecommand.KindInput, runtimecommand.DispositionApplied, durablestore.InboxStateApplied},
		"interrupt no_op":   {runtimecommand.KindInterrupt, runtimecommand.DispositionNoOp, durablestore.InboxStateApplied},
		"input refused":     {runtimecommand.KindInput, runtimecommand.DispositionRefused, durablestore.InboxStateRejected},
		"interrupt refused": {runtimecommand.KindInterrupt, runtimecommand.DispositionRefused, durablestore.InboxStateRejected},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			const tenant coresessionwire.TenantID = "acme"
			session := coresessionwire.SessionID("session/" + name)
			commandID := coresessionwire.CommandID("public/command:" + name)
			runtimeID := newTestUUID(t)
			attemptID := runtimecommand.AttemptID("attempt/" + name)

			// 1. Harness writes the frames into its own journal.
			w := newHarnessWriter(t)
			epoch := w.lease.Epoch()
			if _, err := w.log.AppendCommandDisposition(ctx, runtimecommand.CommandDisposition{
				CommandID:           runtimecommand.CommandID(commandID),
				RuntimeCommandID:    runtimeID,
				Kind:                row.kind,
				LeaseEpoch:          epoch,
				AttemptID:           attemptID,
				AttemptJournalEpoch: epoch,
				Disposition:         row.disposition,
			}); err != nil {
				t.Fatalf("AppendCommandDisposition: %v", err)
			}
			frames := harnessFrames(t, w.store, w.session)
			if len(frames) != 2 {
				t.Fatalf("Harness wrote %d frames, want the opening fence and the disposition", len(frames))
			}

			// 2. A disposition session, on the layout the protocol requires.
			backend := memstore.New()
			spy := newNamingLedger(backend.Ledger)
			backend.Ledger = spy
			// No WithLegacySingleTenant: the multi-tenant layout is the default, and
			// it is the ONLY layout a disposition session can live on.
			store, err := durablestore.Open(ctx, backend,
				durablestore.WithJournalDispositionEvidence(),
			)
			if err != nil {
				t.Fatalf("Open(disposition store): %v", err)
			}
			if _, _, err := store.CreateCatalogEntry(ctx, durablestore.CreateCatalogEntryRequest{
				TenantID: tenant, SessionID: session,
				AgentID: "agent-a", RuntimeCompatibilityID: "runtime-v1",
				CreatedAt: time.Now().UTC(), LastActiveAt: time.Now().UTC(),
				State: coresessionwire.SessionStateIdle, Residency: coresessionwire.SessionResidencyCold,
				DesiredPlacement: coresessionwire.HostPlacementPooled,
				IdempotencyKey:   "create-1", Binding: dispositionBinding(),
			}); err != nil {
				t.Fatalf("CreateCatalogEntry: %v", err)
			}
			admitted, _, err := store.AdmitDispositionCommand(ctx, durablestore.AdmitDispositionCommandRequest{
				TenantID: tenant, SessionID: session, CommandID: commandID,
				Binding:                  dispositionBinding(),
				ProposedRuntimeCommandID: durablestore.RuntimeCommandID(runtimeID.String()),
				Kind:                     durablestore.CommandKind(row.kind),
				Payload:                  []byte(`{"p":1}`),
				AcceptedAt:               time.Now().UTC(),
				ApplyDeadline:            time.Now().UTC().Add(time.Hour),
			})
			if err != nil {
				t.Fatalf("AdmitDispositionCommand: %v", err)
			}
			residency, err := store.AcquireResidency(ctx, durablestore.AcquireResidencyRequest{
				TenantID: tenant, SessionID: session,
			})
			if err != nil {
				t.Fatalf("AcquireResidency: %v", err)
			}
			t.Cleanup(func() { _ = residency.Release(context.Background()) })
			claimed, _, err := store.ClaimDispositionCommand(ctx, durablestore.ClaimDispositionCommandRequest{
				TenantID: tenant, SessionID: session, CommandID: commandID,
				ExpectedRevision: admitted.Revision,
				Residency:        residency,
				ClaimExpiresAt:   time.Now().UTC().Add(roundTripClaimWindow),
			})
			if err != nil {
				t.Fatalf("ClaimDispositionCommand: %v", err)
			}
			applying, err := store.BeginDispositionAttempt(ctx, durablestore.BeginDispositionAttemptRequest{
				TenantID: tenant, SessionID: session, CommandID: commandID,
				ExpectedRevision: claimed.Revision,
				AttemptID:        durablestore.DispositionAttemptID(attemptID),
				JournalEpoch:     durablestore.JournalEpoch(epoch),
				ResidencyEpoch:   residency.Epoch(),
				StartedAt:        time.Now().UTC(),
			})
			if err != nil {
				t.Fatalf("BeginDispositionAttempt: %v", err)
			}

			// 3. Learn the bound journal's name from the store's own read, then place
			//    Harness's frames in it verbatim.
			before := spy.seen()
			if _, err := store.ReadDispositionEvidence(ctx, durablestore.DispositionEvidenceRequest{
				TenantID: tenant, SessionID: session, CommandID: commandID,
				Kind:             durablestore.CommandKind(row.kind),
				RuntimeCommandID: durablestore.RuntimeCommandID(runtimeID.String()),
				Binding:          dispositionBinding(),
				Attempt:          *applying.Record.Attempt,
			}); err == nil {
				t.Fatalf("an empty journal produced evidence; absence must be refused")
			}
			journalName := newlySeen(t, before, spy.seen())
			tip, err := backend.Ledger.Tip(ctx, journalName)
			if err != nil {
				t.Fatalf("Tip(%q): %v", journalName, err)
			}
			for _, frame := range frames {
				if err := storage.AppendDefinite(ctx, backend.Ledger, journalName, tip, frame); err != nil {
					t.Fatalf("AppendDefinite: %v", err)
				}
				tip++
			}

			// 4. The released store settles from those bytes alone.
			settled, ok, err := store.SettleDispositionCommand(ctx, durablestore.SettleDispositionCommandRequest{
				TenantID: tenant, SessionID: session, CommandID: commandID,
				ExpectedRevision: applying.Revision,
				ResidencyEpoch:   residency.Epoch(),
			})
			if err != nil || !ok {
				t.Fatalf("SettleDispositionCommand over Harness-written bytes: ok=%v err=%v", ok, err)
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
			if uint64(outcome.AuthorJournalEpoch) != epoch || uint64(outcome.AttemptJournalEpoch) != epoch {
				t.Errorf("grants = author %d attempt %d, want %d for both",
					outcome.AuthorJournalEpoch, outcome.AttemptJournalEpoch, epoch)
			}
		})
	}
}

func newlySeen(t *testing.T, before, after []string) string {
	t.Helper()
	seen := map[string]struct{}{}
	for _, name := range before {
		seen[name] = struct{}{}
	}
	var fresh []string
	for _, name := range after {
		if _, ok := seen[name]; !ok {
			fresh = append(fresh, name)
		}
	}
	if len(fresh) != 1 {
		t.Fatalf("the evidence read touched %d new ledgers (%v); the bound journal must be exactly one", len(fresh), fresh)
	}
	return fresh[0]
}

// TestHarnessJournalScopeCannotHostADispositionSession pins the placement gap this
// file's header describes, so it fails the day it closes rather than being
// rediscovered. A Harness journal is a legacy-layout journal by construction, and
// sessionstore refuses ProtocolModeDisposition on that layout.
//
// It is a finding and not a defect in this writer: the frames are accepted and
// settled on both sides of it. What it blocks is a composed Host reading a Harness
// journal AS a disposition session's bound journal.
func TestHarnessJournalScopeCannotHostADispositionSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newHarnessWriter(t)
	_, _, err := w.reader.CreateCatalogEntry(ctx, durablestore.CreateCatalogEntryRequest{
		TenantID: harnessTenantID, SessionID: w.wireID,
		AgentID: "agent-a", RuntimeCompatibilityID: "runtime-v1",
		CreatedAt: time.Now().UTC(), LastActiveAt: time.Now().UTC(),
		State: coresessionwire.SessionStateIdle, Residency: coresessionwire.SessionResidencyCold,
		DesiredPlacement: coresessionwire.HostPlacementPooled,
		IdempotencyKey:   "create-1", Binding: dispositionBinding(),
	})
	if err == nil {
		t.Fatalf("a disposition session was created on the Harness legacy scope; the placement gap has closed and this test and its callers must be revisited")
	}
	var catalogErr *durablestore.CatalogError
	if !errors.As(err, &catalogErr) || catalogErr.Field != "binding.protocol_mode" {
		t.Fatalf("refusal = %v, want a catalog error on binding.protocol_mode", err)
	}
	// The legacy mode is accepted on the same scope, so the refusal is about the
	// MODE and not about the session, the tenant or the backend.
	legacy := dispositionBinding()
	legacy.ProtocolMode = durablestore.ProtocolModeLegacy
	if _, _, err := w.reader.CreateCatalogEntry(ctx, durablestore.CreateCatalogEntryRequest{
		TenantID: harnessTenantID, SessionID: w.wireID,
		AgentID: "agent-a", RuntimeCompatibilityID: "runtime-v1",
		CreatedAt: time.Now().UTC(), LastActiveAt: time.Now().UTC(),
		State: coresessionwire.SessionStateIdle, Residency: coresessionwire.SessionResidencyCold,
		DesiredPlacement: coresessionwire.HostPlacementPooled,
		IdempotencyKey:   "create-1", Binding: legacy,
	}); err != nil {
		t.Fatalf("the same scope refused the legacy mode too, so the row above proves nothing: %v", err)
	}
}

// TestHarnessDispositionFrameIsPrivateAndBodiless holds the two shape properties the
// released verifier depends on and that no settlement outcome would reveal: the
// frame carries kind 5 with every correlation field populated, and it names NO
// event. A writer that put its effect's identity in the frame would be refused with
// the same "event" code a forged record earns, which reads as an attack rather than
// as a writer bug.
func TestHarnessDispositionFrameIsPrivateAndBodiless(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newHarnessWriter(t)
	runtimeID := newTestUUID(t)
	epoch := w.lease.Epoch()
	d := runtimecommand.CommandDisposition{
		CommandID:           "public/command:shape",
		RuntimeCommandID:    runtimeID,
		Kind:                runtimecommand.KindInterrupt,
		LeaseEpoch:          epoch,
		AttemptID:           "attempt/shape",
		AttemptJournalEpoch: epoch,
		Disposition:         runtimecommand.DispositionNoOp,
	}
	res, err := w.log.AppendCommandDisposition(ctx, d)
	if err != nil {
		t.Fatalf("AppendCommandDisposition: %v", err)
	}
	frames := harnessFrames(t, w.store, w.session)
	if uint64(len(frames)) != res.Sequence {
		t.Fatalf("read %d frames, want %d", len(frames), res.Sequence)
	}
	env, err := durablestore.DecodeEnvelope(frames[res.Sequence-1])
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if env.Kind != durablestore.EnvelopeKindCommandDisposition {
		t.Fatalf("envelope kind = %d, want %d (command disposition)",
			env.Kind, durablestore.EnvelopeKindCommandDisposition)
	}
	if env.Public.Inline != nil || env.Public.Reference != nil ||
		env.Runtime.Inline != nil || env.Runtime.Reference != nil {
		t.Errorf("the disposition frame carries a body: public=%+v runtime=%+v", env.Public, env.Runtime)
	}
	if env.EventID != "" {
		t.Errorf("the disposition frame names event %q; a disposition names no event", env.EventID)
	}
	if env.RecordID != "" {
		t.Errorf("the frame carries a generic record id %q, so it was framed as runtime control", env.RecordID)
	}
	want := durablestore.Envelope{
		Kind:                durablestore.EnvelopeKindCommandDisposition,
		CommandID:           coresessionwire.CommandID(d.CommandID),
		RuntimeCommandID:    d.RuntimeCommandID,
		CommandKind:         string(d.Kind),
		LeaseEpoch:          d.LeaseEpoch,
		AttemptID:           string(d.AttemptID),
		AttemptJournalEpoch: d.AttemptJournalEpoch,
		DispositionKind:     string(d.Disposition),
	}
	// Compared field by field because an Envelope holds two body slots and is not
	// comparable; the slots are asserted empty above, which is the same claim.
	if env.CommandID != want.CommandID || env.RuntimeCommandID != want.RuntimeCommandID ||
		env.CommandKind != want.CommandKind || env.LeaseEpoch != want.LeaseEpoch ||
		env.AttemptID != want.AttemptID || env.AttemptJournalEpoch != want.AttemptJournalEpoch ||
		env.DispositionKind != want.DispositionKind {
		t.Errorf("framed envelope = %+v, want %+v", env, want)
	}
}

// TestHarnessDispositionSurvivesJournalHydration is the replay half. The idempotency
// index is rebuilt by RE-ENCODING every replayed record and comparing its
// fingerprint, so a replay arm that reconstructs a disposition differently from the
// write path turns every redelivered append into a collision — or refuses to open
// the journal at all.
func TestHarnessDispositionSurvivesJournalHydration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newHarnessWriter(t)
	runtimeID := newTestUUID(t)
	epoch := w.lease.Epoch()
	d := runtimecommand.CommandDisposition{
		CommandID:           "public/command:hydrate",
		RuntimeCommandID:    runtimeID,
		Kind:                runtimecommand.KindInput,
		LeaseEpoch:          epoch,
		AttemptID:           "attempt/hydrate",
		AttemptJournalEpoch: epoch,
		Disposition:         runtimecommand.DispositionApplied,
	}
	first, err := w.log.AppendCommandDisposition(ctx, d)
	if err != nil {
		t.Fatalf("AppendCommandDisposition: %v", err)
	}

	if err := w.lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	lease, err := w.store.AcquireLease(ctx, w.session)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	// The hydration walk runs inside this Open. Before the replay arm existed it
	// failed here, which is the fail-closed direction but bricks every session that
	// ever held a disposition.
	reopened, err := w.store.OpenJournal(ctx, w.session, lease)
	if err != nil {
		t.Fatalf("OpenJournal after a disposition was written: %v", err)
	}
	log, err := w.store.OpenRuntimeCommandLog(w.session, reopened)
	if err != nil {
		t.Fatalf("OpenRuntimeCommandLog: %v", err)
	}
	again, err := log.AppendCommandDisposition(ctx, d)
	if err != nil {
		t.Fatalf("redelivered AppendCommandDisposition: %v", err)
	}
	if again.Appended {
		t.Errorf("a redelivered disposition appended a second frame at seq %d", again.Sequence)
	}
	if again.Sequence != first.Sequence {
		t.Errorf("redelivery reported seq %d, want the original %d", again.Sequence, first.Sequence)
	}

	// A DIFFERENT statement about the same attempt collides. This is the guard that
	// stops a successor's recovery closure from overwriting a durable application,
	// and it works only because the hydration above restored the original's
	// fingerprint.
	closure := d
	closure.Disposition = runtimecommand.DispositionNotApplied
	closure.LeaseEpoch = lease.Epoch()
	if _, err := log.AppendCommandDisposition(ctx, closure); err == nil {
		t.Errorf("a not_applied closure overwrote a durable applied disposition")
	} else {
		var collision *journal.IdempotencyCollisionError
		if !errors.As(err, &collision) {
			t.Errorf("collision error %v is not an *IdempotencyCollisionError", err)
		}
	}
}

// TestFrameCarriesTheRecordsClaimedGrantNotTheLiveLease pins that framing COPIES the
// disposition's own LeaseEpoch rather than substituting the journal's live grant.
//
// The two agree on every path a caller can reach today — the applier stamps the held
// grant and the closer stamps its own — so this looks equivalent, and it is not.
// Substituting the live lease would LAUNDER a wrong claim into a right one, and the
// released reader's fence cross-check exists precisely because the claim is the
// writer's and may be wrong. A writer that silently corrected itself would make that
// check unfalsifiable for every Harness-written record.
//
// The second half is the proof that the check is real: the forged claim written here
// is refused by the released reader with lease_epoch.
func TestFrameCarriesTheRecordsClaimedGrantNotTheLiveLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newHarnessWriter(t)
	live := w.lease.Epoch()
	forged := live + 7
	d := runtimecommand.CommandDisposition{
		CommandID:           "public/command:forged",
		RuntimeCommandID:    newTestUUID(t),
		Kind:                runtimecommand.KindInput,
		LeaseEpoch:          forged,
		AttemptID:           "attempt/forged",
		AttemptJournalEpoch: forged,
		Disposition:         runtimecommand.DispositionApplied,
	}
	// Appended through the journal directly: the record IS the claim, and the writer
	// is not consulted about whether it likes it.
	seq, err := w.journal.Append(ctx, journal.NewCommandDispositionRecord(d))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	frames := harnessFrames(t, w.store, w.session)
	env, err := durablestore.DecodeEnvelope(frames[seq-1])
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if env.LeaseEpoch != forged {
		t.Fatalf("framed LeaseEpoch = %d, want the record's claimed %d (the live grant is %d)",
			env.LeaseEpoch, forged, live)
	}
	if _, err := w.reader.ReadDispositionEvidence(ctx,
		w.evidenceRequest(coresessionwire.CommandID(d.CommandID), d.Kind, d.RuntimeCommandID, d.AttemptID, forged)); err == nil {
		t.Fatalf("the released reader accepted a claim that disagrees with the opening fence")
	}
}

// TestRecoveryClosureSurvivesJournalHydration is the hydration test for the ONE kind
// whose two epochs differ. Every other disposition has LeaseEpoch ==
// AttemptJournalEpoch, so a replay arm that reconstructed the attempt grant from the
// author grant would agree with all of them and disagree only here — turning a
// redelivered closure into a collision, which is the same error a successor earns for
// trying to tombstone an applied command.
func TestRecoveryClosureSurvivesJournalHydration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newHarnessWriter(t)
	attemptEpoch := w.lease.Epoch()
	if err := w.lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	successorLease, err := w.store.AcquireLease(ctx, w.session)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	successorJournal, err := w.store.OpenJournal(ctx, w.session, successorLease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	successorLog, err := w.store.OpenRuntimeCommandLog(w.session, successorJournal)
	if err != nil {
		t.Fatalf("OpenRuntimeCommandLog: %v", err)
	}
	closure := runtimecommand.CommandDisposition{
		CommandID:           "public/command:closed-hydrate",
		RuntimeCommandID:    newTestUUID(t),
		Kind:                runtimecommand.KindInput,
		LeaseEpoch:          successorLease.Epoch(),
		AttemptID:           "attempt/closed-hydrate",
		AttemptJournalEpoch: attemptEpoch,
		Disposition:         runtimecommand.DispositionNotApplied,
	}
	if closure.LeaseEpoch == closure.AttemptJournalEpoch {
		t.Fatalf("the two grants are equal, so this test cannot distinguish them")
	}
	first, err := successorLog.AppendCommandDisposition(ctx, closure)
	if err != nil {
		t.Fatalf("AppendCommandDisposition(closure): %v", err)
	}

	// A third grant reopens the journal, which hydrates the index from the frames.
	if err := successorLease.Release(ctx); err != nil {
		t.Fatalf("Release(successor): %v", err)
	}
	thirdLease, err := w.store.AcquireLease(ctx, w.session)
	if err != nil {
		t.Fatalf("AcquireLease(third): %v", err)
	}
	thirdJournal, err := w.store.OpenJournal(ctx, w.session, thirdLease)
	if err != nil {
		t.Fatalf("OpenJournal(third) after a closure was written: %v", err)
	}
	thirdLog, err := w.store.OpenRuntimeCommandLog(w.session, thirdJournal)
	if err != nil {
		t.Fatalf("OpenRuntimeCommandLog(third): %v", err)
	}
	again, err := thirdLog.AppendCommandDisposition(ctx, closure)
	if err != nil {
		t.Fatalf("a redelivered closure was refused after hydration: %v", err)
	}
	if again.Appended || again.Sequence != first.Sequence {
		t.Fatalf("redelivered closure = %+v, want a dedup to the original seq %d", again, first.Sequence)
	}
}
