package sessionstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/runtimecommand"
	durablestore "github.com/looprig/sessionstore"

	"github.com/looprig/storage/memstore"
)

// hostTenant is the tenant a counterparty (Host) admitted a session under. It is
// deliberately NOT "local": the whole point of these tests is that the tenant is
// an input rather than a constant, so a fixture reusing the default would prove
// nothing about the input.
const hostTenant coresessionwire.TenantID = "tenant-a"

// publicEventFor builds a public (projectable) event for session id. It is the
// EFFECT half of the correlation: the released reader reports `committed` only
// when a correlated prefix is immediately followed by an EnvelopeKindPublicEvent,
// so a private record here would leave the outcome `unresolved` and the test
// would pass for the wrong reason.
func publicEventFor(t *testing.T, sessionID uuid.UUID) event.Event {
	t.Helper()
	return event.GateResolved{
		Header: event.Header{
			Coordinates: identity.Coordinates{
				SessionID: sessionID,
				LoopID:    newTestUUID(t),
				TurnID:    newTestUUID(t),
				StepID:    newTestUUID(t),
			},
			EventID: newTestUUID(t),
		},
		GateID:   gate.ID(newTestUUID(t)),
		Resolver: gate.ResolverLoop,
		Reason:   gate.CloseAnswered,
		Action:   gate.FormActionAccept,
		Source:   gate.ResponseSource{Kind: gate.ResponseFromUser},
	}
}

// TestHarnessAndAHostStoreShareOneBackendAndCorrelateAnApplicationPrefix is the
// property this whole change exists for, and it is ONE test rather than two
// because the two halves are only interesting together.
//
// Half one: a released store opened by a counterparty under (tenant-a, <uuid>)
// and a Harness store opened over the SAME backend must both open. Before
// WithTenant, Harness hard-coded the legacy tenant "local", so the two layout
// markers differed and whichever store opened second was refused with
// KeyspaceLayoutMismatch — Host and the runtime it launches could not share a
// backend at all.
//
// Half two — the one that does not go away by giving them separate backends —
// is that the counterparty's FindCommandApplication must FIND the application
// prefix Harness's journal wrote. That query derives the journal name from
// (TenantID, SessionID); a prefix written in a different scope reads as absent,
// and absent is the single outcome that licenses a deadline reconciler to settle
// `rejected` over a durable effect.
//
// The assertion is on the OUTCOME and the correlated sequences, not merely on
// "not absent": `unresolved` and `abandoned` are also not-absent and both are
// wrong answers here, and `abandoned` is admitted by provesNoEffect.
func TestHarnessAndAHostStoreShareOneBackendAndCorrelateAnApplicationPrefix(t *testing.T) {
	ctx := context.Background()
	backend := memstore.New()
	sessionID := newTestUUID(t)
	runtimeCommandID := newTestUUID(t)
	commandID := coresessionwire.CommandID("v1:command-a")

	counterparty, err := durablestore.Open(ctx, backend, durablestore.WithLegacySingleTenant(hostTenant))
	if err != nil {
		t.Fatalf("open the counterparty store: %v", err)
	}
	t.Cleanup(func() { _ = counterparty.Close(context.WithoutCancel(ctx)) })

	store, err := Open(backend, WithTenant(hostTenant))
	if err != nil {
		t.Fatalf("Open() over a backend the counterparty initialized: %v", err)
	}

	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	if _, _, err := counterparty.AdmitCommand(ctx, durablestore.AdmitCommandRequest{
		TenantID:                 hostTenant,
		SessionID:                harnessSessionID(sessionID),
		CommandID:                commandID,
		ProposedRuntimeCommandID: durablestore.RuntimeCommandID(runtimeCommandID.String()),
		Kind:                     durablestore.CommandKind(runtimecommand.KindInput),
		Payload:                  []byte(`{"blocks":[]}`),
		AcceptedAt:               now,
		ApplyDeadline:            now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("AdmitCommand() error = %v", err)
	}

	lease, err := store.AcquireLease(ctx, sessionID)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	t.Cleanup(func() { _ = lease.Release(context.WithoutCancel(ctx)) })
	writer, err := store.OpenJournal(ctx, sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal() error = %v", err)
	}

	prefixSeq, err := writer.Append(ctx, journal.NewCommandApplicationRecord(runtimecommand.Application{
		CommandID:        runtimecommand.CommandID(commandID),
		RuntimeCommandID: runtimeCommandID,
		LeaseEpoch:       lease.Epoch(),
		Kind:             runtimecommand.KindInput,
	}))
	if err != nil {
		t.Fatalf("Append(application prefix) error = %v", err)
	}
	effectSeq, err := writer.Append(ctx, journal.NewEventRecord(publicEventFor(t, sessionID)))
	if err != nil {
		t.Fatalf("Append(public effect) error = %v", err)
	}

	found, err := counterparty.FindCommandApplication(ctx, durablestore.FindCommandApplicationRequest{
		TenantID:  hostTenant,
		SessionID: harnessSessionID(sessionID),
		CommandID: commandID,
	})
	if err != nil {
		t.Fatalf("FindCommandApplication() error = %v", err)
	}
	if found.Outcome != durablestore.CommandApplicationCommitted {
		t.Fatalf("outcome = %q, want %q", found.Outcome, durablestore.CommandApplicationCommitted)
	}
	if found.PrefixSeq != prefixSeq {
		t.Errorf("PrefixSeq = %d, want the sequence Harness appended the prefix at (%d)", found.PrefixSeq, prefixSeq)
	}
	if found.EffectSeq != effectSeq {
		t.Errorf("EffectSeq = %d, want the sequence Harness appended the effect at (%d)", found.EffectSeq, effectSeq)
	}
	if found.PrefixEpoch != lease.Epoch() {
		t.Errorf("PrefixEpoch = %d, want the Harness lease epoch %d", found.PrefixEpoch, lease.Epoch())
	}
}

// TestHarnessOpenRefusesABackendFiledUnderADifferentTenant is the migration
// answer, and it is measured rather than asserted from the option's doc comment.
//
// An existing single-tenant journal is filed under ("local", <uuid>) and its
// layout marker CARRIES the tenant bytes, which the released store compares for
// byte equality on every Open. So the three outcomes are exhaustive and the
// third is impossible by construction: the same tenant opens and reads its own
// records, a different tenant is refused before any session I/O, and there is no
// path on which records filed under one tenant are read under another.
//
// The refusal direction is asserted on the CODE, not on err != nil, because an
// unrelated *InvalidBackendError would satisfy a bare non-nil check and would
// keep satisfying it if the layouts ever silently became compatible.
func TestHarnessOpenRefusesABackendFiledUnderADifferentTenant(t *testing.T) {
	ctx := context.Background()
	backend := memstore.New()
	sessionID := newTestUUID(t)

	legacy, err := Open(backend)
	if err != nil {
		t.Fatalf("Open() with the default tenant: %v", err)
	}
	lease, err := legacy.AcquireLease(ctx, sessionID)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	writer, err := legacy.OpenJournal(ctx, sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal() error = %v", err)
	}
	if _, err := writer.Append(ctx, journal.NewEventRecord(publicEventFor(t, sessionID))); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	// Refused: the tenant is part of the marker.
	_, err = Open(backend, WithTenant(hostTenant))
	var keyspaceErr *durablestore.KeyspaceError
	if !errors.As(err, &keyspaceErr) {
		t.Fatalf("Open(WithTenant(%q)) over a %q backend = %v, want a *KeyspaceError", hostTenant, harnessTenantID, err)
	}
	if keyspaceErr.Code != durablestore.KeyspaceLayoutMismatch {
		t.Fatalf("keyspace code = %q, want %q", keyspaceErr.Code, durablestore.KeyspaceLayoutMismatch)
	}

	// Read-compatible: the same tenant reopens and replays what it wrote. The
	// record count is the reader — a refusal-only test would pass over a store
	// that opened and then found nothing.
	reopened, err := Open(backend, WithTenant(harnessTenantID))
	if err != nil {
		t.Fatalf("Open(WithTenant(%q)) over its own backend: %v", harnessTenantID, err)
	}
	replayer, err := reopened.OpenInternalRecordReplayer(sessionID, ReplayRequest{FromSeq: 1})
	if err != nil {
		t.Fatalf("OpenInternalRecordReplayer() error = %v", err)
	}
	replayed, _ := drainRecords(t, replayer, journal.ReplayRequest{})
	if got := len(replayed); got != 2 {
		t.Fatalf("replayed %d records after reopening under the same tenant, want the fence and the event", got)
	}
}

// TestHarnessSessionIDIsAlwaysACanonicalLegacySessionID gives the UUID→SessionID
// derivation a reader, over a space DERIVED from the mechanism rather than one
// picked example.
//
// The released legacy layout admits a SessionID only if it is exactly 36 bytes
// of lowercase hex with hyphens at 8/13/18/23 (isCanonicalLegacySessionID), and
// refuses anything else with KeyspaceLegacySession. That is the constraint that
// makes the derivation load-bearing: Harness keys every Store method by
// uuid.UUID, and the rendering is what makes those keys addressable by a
// counterparty at all. The space is the two lexical extremes plus random draws,
// because the failure this guards against is a rendering change (uppercase, no
// hyphens, braces), which shows up on every value or none.
func TestHarnessSessionIDIsAlwaysACanonicalLegacySessionID(t *testing.T) {
	ctx := context.Background()
	store, err := durablestore.Open(ctx, memstore.New(), durablestore.WithLegacySingleTenant(hostTenant))
	if err != nil {
		t.Fatalf("open the released store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.WithoutCancel(ctx)) })

	ids := []uuid.UUID{{}, {
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	}}
	for i := 0; i < 64; i++ {
		ids = append(ids, newTestUUID(t))
	}
	for _, id := range ids {
		// ReadRuntimeJournal derives the session scope and nothing else that
		// could fail for an unwritten session, so a KeyspaceLegacySession here
		// is exactly the rendering refusal and cannot be anything else.
		if _, err := store.ReadRuntimeJournal(ctx, durablestore.ReadRuntimeJournalRequest{
			TenantID: hostTenant, SessionID: harnessSessionID(id), Limit: 1,
		}); err != nil {
			var keyspaceErr *durablestore.KeyspaceError
			if errors.As(err, &keyspaceErr) && keyspaceErr.Code == durablestore.KeyspaceLegacySession {
				t.Fatalf("harnessSessionID(%v) = %q is not a canonical legacy session id", id, harnessSessionID(id))
			}
			t.Fatalf("ReadRuntimeJournal(%q) error = %v", harnessSessionID(id), err)
		}
	}
}

// TestOffloadedBodiesAreFiledUnderTheStoresTenant gives the OBJECT half of the
// tenant its reader, which the correlation test above cannot: a small record is
// inlined in the ledger frame and never reaches the released object API at all.
//
// The reader is not a round-trip coincidence. Under the legacy layout an object's
// physical blob prefix carries no tenant, so a put and a get that were BOTH wrong
// in the same way would still find each other. What makes this load-bearing is
// that deriveTenantScope refuses any tenant other than the backend's committed
// one with KeyspaceLegacyTenant, before provider I/O — so a body offloaded under
// the historical "local" constant while the Store is filing for "tenant-a" fails
// the append outright rather than landing somewhere quiet.
func TestOffloadedBodiesAreFiledUnderTheStoresTenant(t *testing.T) {
	ctx := context.Background()
	backend := memstore.New()
	sessionID := newTestUUID(t)

	// One byte forces every body out of line, so the object path is exercised by
	// the ordinary append rather than by a fixture built to be large.
	store, err := Open(backend, WithTenant(hostTenant), WithOffloadThreshold(1))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	lease, err := store.AcquireLease(ctx, sessionID)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	t.Cleanup(func() { _ = lease.Release(context.WithoutCancel(ctx)) })
	writer, err := store.OpenJournal(ctx, sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal() error = %v", err)
	}
	want := publicEventFor(t, sessionID)
	if _, err := writer.Append(ctx, journal.NewEventRecord(want)); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	// FOUR readers, because there are four independently-constructed object
	// readers behind them and a test that exercised one would leave the others
	// unread. Each resolves the offloaded body through the released object API,
	// so each names a tenant of its own. The product-facing replayer is not a
	// duplicate of the privileged one: they are two separate constructions of
	// the same struct, and mutating only the product-facing one survived every
	// other test in this package.
	publicReplay, err := store.OpenEventReplayer(sessionID, ReplayRequest{FromSeq: 1})
	if err != nil {
		t.Fatalf("OpenEventReplayer() error = %v", err)
	}
	if publicEvents, _ := drainEvents(t, publicReplay, journal.ReplayRequest{}); len(publicEvents) != 1 {
		t.Fatalf("public replay returned %d events, want the one offloaded event", len(publicEvents))
	}

	eventReplay, err := store.OpenInternalEventReplayer(sessionID, ReplayRequest{FromSeq: 1})
	if err != nil {
		t.Fatalf("OpenInternalEventReplayer() error = %v", err)
	}
	events, _ := drainEvents(t, eventReplay, journal.ReplayRequest{})
	if len(events) != 1 {
		t.Fatalf("replayed %d events, want the one offloaded event", len(events))
	}
	if got := events[0].EventHeader().EventID; got != want.EventHeader().EventID {
		t.Fatalf("replayed event id = %v, want %v", got, want.EventHeader().EventID)
	}

	recordReplay, err := store.OpenInternalRecordReplayer(sessionID, ReplayRequest{FromSeq: 1})
	if err != nil {
		t.Fatalf("OpenInternalRecordReplayer() error = %v", err)
	}
	if records, _ := drainRecords(t, recordReplay, journal.ReplayRequest{}); len(records) != 2 {
		t.Fatalf("replayed %d records, want the fence and the offloaded event", len(records))
	}

	// Reopening hydrates the idempotency index by walking the whole ledger,
	// which resolves the offloaded body through a cursor built in OpenJournal
	// rather than in either replayer — a third construction, and the one that
	// belongs to neither replay test.
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	reopenLease, err := store.AcquireLease(ctx, sessionID)
	if err != nil {
		t.Fatalf("AcquireLease() on reopen error = %v", err)
	}
	t.Cleanup(func() { _ = reopenLease.Release(context.WithoutCancel(ctx)) })
	if _, err := store.OpenJournal(ctx, sessionID, reopenLease); err != nil {
		t.Fatalf("reopen OpenJournal() error = %v", err)
	}
}

// TestWithTenantRefusesAnInvalidTenantRatherThanRevertingToTheDefault is the
// reader for the last sentence of WithTenant's doc comment. That sentence is a
// claim about WHERE the value ends up, not about the validator: the released
// store already refuses an invalid tenant, so a mutation that defaults inside
// Open dies against tests that do not exist here. What has no reader without
// this test is the option itself silently falling back — `if tenant != "" {
// o.TenantID = tenant }` — which leaves Open succeeding under "local" and files
// the caller's records in a scope its counterparty never addresses.
//
// The refusal is asserted on the released *InvalidOptionError rather than on
// err != nil, because Harness's own *InvalidBackendError satisfies a bare
// non-nil check and would keep satisfying it if the tenant stopped reaching the
// released Open at all.
//
// The space is DERIVED from core's validateID (ids.go): empty, over MaxIDBytes,
// and not valid UTF-8 are the three ways an opaque TenantID can be invalid. A
// fallback written against any one of them would be caught by the others.
// Nothing here asserts a SHAPE — "  ", "Tenant/A" and "TENANT-A" are all valid
// tenants and simply different keyspaces, and a shape rule in this package
// would be a second authority beside core's.
func TestWithTenantRefusesAnInvalidTenantRatherThanRevertingToTheDefault(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tenant coresessionwire.TenantID
	}{
		{name: "empty", tenant: ""},
		{name: "too long", tenant: coresessionwire.TenantID(strings.Repeat("x", coresessionwire.MaxIDBytes+1))},
		{name: "invalid utf8", tenant: coresessionwire.TenantID("\xff")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := memstore.New()

			_, err := Open(backend, WithTenant(tc.tenant))
			if err == nil {
				t.Fatalf("Open(WithTenant(%q)) succeeded, want a refusal rather than a fallback to %q", tc.tenant, harnessTenantID)
			}
			var optionErr *durablestore.InvalidOptionError
			if !errors.As(err, &optionErr) {
				t.Fatalf("Open(WithTenant(%q)) error = %v, want a *durablestore.InvalidOptionError", tc.tenant, err)
			}

			// The backend must also be untouched. A fallback that persisted the
			// default layout marker and then failed for some later reason would
			// satisfy the assertion above while having already committed the
			// wrong tenant; opening under a DIFFERENT tenant afterwards can only
			// succeed if no marker was written.
			if _, err := Open(backend, WithTenant(hostTenant)); err != nil {
				t.Fatalf("Open(WithTenant(%q)) after the refusal: %v, want a backend no marker was committed to", hostTenant, err)
			}
		})
	}
}
