package sessionstore

import (
	"bytes"
	"context"
	"errors"
	"testing"

	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/fsstore"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	harnesssessionwire "github.com/looprig/harness/pkg/sessionwire"
	durablestore "github.com/looprig/sessionstore"
	"github.com/looprig/storage/memstore"
)

func TestPublicAppendStoresNativeRuntimeAndCanonicalPublicBodies(t *testing.T) {
	backend := memstore.New()
	store, err := Open(backend)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	sessionID := newTestUUID(t)
	eventID := newTestUUID(t)
	value := event.GateResolved{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: newTestUUID(t), TurnID: newTestUUID(t), StepID: newTestUUID(t)},
			EventID:     eventID,
		},
		GateID: gate.ID(newTestUUID(t)), Resolver: gate.ResolverLoop,
		Reason: gate.CloseAnswered, Action: gate.FormActionAccept,
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
		Audit:  gate.FormAudit{Values: map[string]string{"secret": "runtime-only-marker"}},
	}
	lease, err := store.AcquireLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	writer, err := store.OpenJournal(context.Background(), sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal() error = %v", err)
	}
	if _, err := writer.Append(context.Background(), journal.NewEventRecord(value)); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	cursor, err := backend.Ledger.Read(context.Background(), ledgerName(sessionID), 2)
	if err != nil {
		t.Fatalf("Ledger.Read() error = %v", err)
	}
	record, err := cursor.Next(context.Background())
	if err != nil {
		t.Fatalf("Cursor.Next() error = %v", err)
	}
	envelope, err := durablestore.DecodeEnvelope(record.Payload)
	if err != nil {
		t.Fatalf("DecodeEnvelope() error = %v", err)
	}
	wantRuntime, err := event.MarshalEvent(value)
	if err != nil {
		t.Fatalf("MarshalEvent() error = %v", err)
	}
	if !bytes.Equal(envelope.Runtime.Inline, wantRuntime) {
		t.Errorf("runtime body = %s, want native body %s", envelope.Runtime.Inline, wantRuntime)
	}
	if envelope.EventID != coresessionwire.EventID(eventID.String()) {
		t.Errorf("EventID = %q, want %q", envelope.EventID, eventID)
	}
	wantPublic, err := harnesssessionwire.Project(harnessTenantID, harnessSessionID(sessionID), value)
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	if !bytes.Equal(envelope.Public.Inline, wantPublic.Body) {
		t.Fatalf("public body = %s, want canonical body %s", envelope.Public.Inline, wantPublic.Body)
	}
	if !bytes.Contains(envelope.Runtime.Inline, []byte("runtime-only-marker")) || bytes.Contains(envelope.Public.Inline, []byte("runtime-only-marker")) {
		t.Fatalf("public/runtime redaction split = (%s, %s)", envelope.Public.Inline, envelope.Runtime.Inline)
	}
	page, err := store.durable.ReadPublicJournal(context.Background(), durablestore.ReadPublicJournalRequest{
		TenantID: harnessTenantID, SessionID: harnessSessionID(sessionID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("ReadPublicJournal() error = %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].JournalSeq != 2 || !bytes.Equal(page.Events[0].Body, wantPublic.Body) {
		t.Fatalf("public page = %+v, want one canonical event at sequence 2", page)
	}
}

func TestOpenRejectsFSStoreWithoutBoundedBlobReaderLifecycle(t *testing.T) {
	backend, err := fsstore.Open(fsstore.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("fsstore.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	store, err := Open(backend.Backend())
	if store != nil {
		t.Fatalf("Open() store = %v, want nil", store)
	}
	var invalid *durablestore.InvalidBackendError
	if !errors.As(err, &invalid) || invalid.Component != "BlobReaderLifecycle" {
		t.Fatalf("Open() error = %T %v, want BlobReaderLifecycle *sessionstore.InvalidBackendError", err, err)
	}
}

func TestPrivateAppendStoresRuntimeBodyWithoutPublicBody(t *testing.T) {
	backend := memstore.New()
	store, err := Open(backend)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	sessionID := newTestUUID(t)
	lease, err := store.AcquireLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	writer, err := store.OpenJournal(context.Background(), sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal() error = %v", err)
	}
	record := smallCommand(sessionID, newTestUUID(t))
	if _, err := writer.Append(context.Background(), record); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	cursor, err := backend.Ledger.Read(context.Background(), ledgerName(sessionID), 2)
	if err != nil {
		t.Fatalf("Ledger.Read() error = %v", err)
	}
	stored, err := cursor.Next(context.Background())
	if err != nil {
		t.Fatalf("Cursor.Next() error = %v", err)
	}
	envelope, err := durablestore.DecodeEnvelope(stored.Payload)
	if err != nil {
		t.Fatalf("DecodeEnvelope() error = %v", err)
	}
	if envelope.Kind != durablestore.EnvelopeKindRuntimeControl || envelope.Runtime.Inline == nil {
		t.Fatalf("private envelope = %+v, want runtime control with body", envelope)
	}
	if envelope.Public.Inline != nil || envelope.Public.Reference != nil || envelope.EventID != "" {
		t.Fatalf("private envelope exposes public fields: %+v", envelope)
	}
}

func TestPublicAppendSurfacesProjectionFailureWithoutWriting(t *testing.T) {
	backend := memstore.New()
	store, err := Open(backend)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	sessionID := newTestUUID(t)
	lease, err := store.AcquireLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	writer, err := store.OpenJournal(context.Background(), sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal() error = %v", err)
	}
	want := errors.New("projection failed")
	store.project = func(coresessionwire.TenantID, coresessionwire.SessionID, any) (harnesssessionwire.Projection, error) {
		return harnesssessionwire.Projection{}, want
	}
	writer.(*sessionJournal).project = store.project
	value := event.SessionStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sessionID},
		EventID:     newTestUUID(t),
	}}
	if _, err := writer.Append(context.Background(), journal.NewEventRecord(value)); !errors.Is(err, want) {
		t.Fatalf("Append() error = %v, want projection failure", err)
	}
	tip, err := backend.Ledger.Tip(context.Background(), ledgerName(sessionID))
	if err != nil {
		t.Fatalf("Tip() error = %v", err)
	}
	if tip != 1 {
		t.Fatalf("Tip() = %d, want opening fence only", tip)
	}
}
