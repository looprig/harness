package sessionstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	harnesssessionwire "github.com/looprig/harness/pkg/sessionwire"
	durablestore "github.com/looprig/sessionstore"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

func commandRecordWithEncodedSize(t *testing.T, sessionID uuid.UUID, size int) journal.JournalRecord {
	t.Helper()
	commandID := newTestUUID(t)
	makeRecord := func(text string) journal.JournalRecord {
		return journal.NewCommandRecord(sessionID, sessionID, command.UserInput{
			Header: command.Header{CommandID: commandID},
			Blocks: []content.Block{&content.TextBlock{Text: text}},
		})
	}
	base := makeRecord("")
	baseBody, err := command.MarshalCommand(base.(journal.CommandRecord).Command())
	if err != nil {
		t.Fatalf("MarshalCommand(base) error = %v", err)
	}
	if size < len(baseBody) {
		t.Fatalf("requested command body size %d is below base size %d", size, len(baseBody))
	}
	record := makeRecord(strings.Repeat("x", size-len(baseBody)))
	body, err := command.MarshalCommand(record.(journal.CommandRecord).Command())
	if err != nil {
		t.Fatalf("MarshalCommand(sized) error = %v", err)
	}
	if len(body) != size {
		t.Fatalf("sized command body = %d bytes, want %d", len(body), size)
	}
	return record
}

func publicEventWithEncodedSize(t *testing.T, sessionID uuid.UUID, size int) event.Event {
	t.Helper()
	eventID := newTestUUID(t)
	loopID, turnID, stepID := newTestUUID(t), newTestUUID(t), newTestUUID(t)
	makeEvent := func(pad string) event.Event {
		return event.StepDone{
			Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID, TurnID: turnID, StepID: stepID}, EventID: eventID},
			Messages: content.AgenticMessages{&content.AIMessage{Message: content.Message{
				Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: pad}},
			}}},
		}
	}
	base := makeEvent("")
	baseBody, err := event.MarshalEvent(base)
	if err != nil {
		t.Fatalf("MarshalEvent(base) error = %v", err)
	}
	if size < len(baseBody) {
		t.Fatalf("requested event body size %d is below base size %d", size, len(baseBody))
	}
	value := makeEvent(strings.Repeat("x", size-len(baseBody)))
	body, err := event.MarshalEvent(value)
	if err != nil {
		t.Fatalf("MarshalEvent(sized) error = %v", err)
	}
	if len(body) != size {
		t.Fatalf("sized event body = %d bytes, want %d", len(body), size)
	}
	return value
}

func canonicalBodyWithSize(t *testing.T, size int) []byte {
	t.Helper()
	const empty = `{"pad":""}`
	if size < len(empty) {
		t.Fatalf("requested canonical body size %d is below base size %d", size, len(empty))
	}
	return []byte(`{"pad":"` + strings.Repeat("x", size-len(empty)) + `"}`)
}

func appendAndReadDurableEnvelope(t *testing.T, store *Store, backend *storage.Composite, id uuid.UUID, record journal.JournalRecord) durablestore.Envelope {
	t.Helper()
	lease, err := store.AcquireLease(context.Background(), id)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	writer, err := store.OpenJournal(context.Background(), id, lease)
	if err != nil {
		t.Fatalf("OpenJournal() error = %v", err)
	}
	if _, err := writer.Append(context.Background(), record); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	cursor, err := backend.Ledger.Read(context.Background(), ledgerName(id), 2)
	if err != nil {
		t.Fatalf("Ledger.Read() error = %v", err)
	}
	t.Cleanup(func() { _ = cursor.Close() })
	stored, err := cursor.Next(context.Background())
	if err != nil {
		t.Fatalf("Cursor.Next() error = %v", err)
	}
	envelope, err := durablestore.DecodeEnvelope(stored.Payload)
	if err != nil {
		t.Fatalf("DecodeEnvelope() error = %v", err)
	}
	replayer, err := store.OpenInternalRecordReplayer(id, ReplayRequest{FromSeq: 2})
	if err != nil {
		t.Fatalf("OpenInternalRecordReplayer() error = %v", err)
	}
	replayCursor, err := replayer.Open(context.Background(), journal.ReplayRequest{})
	if err != nil {
		t.Fatalf("RecordReplayer.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = replayCursor.Close() })
	if _, seq, err := replayCursor.Next(context.Background()); err != nil || seq != 2 {
		t.Fatalf("replay boundary record = (seq %d, err %v), want seq 2 success", seq, err)
	}
	return envelope
}

func TestEffectiveOffloadHonorsReleasedInlineBodyBoundary(t *testing.T) {
	for _, size := range []int{durablestore.MaxInlineBodyBytes, durablestore.MaxInlineBodyBytes + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			backend := memstore.New()
			store, err := Open(backend, WithOffloadThreshold(durablestore.MaxEnvelopeBytes))
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			id := newTestUUID(t)
			record := commandRecordWithEncodedSize(t, id, size)
			envelope := appendAndReadDurableEnvelope(t, store, backend, id, record)
			if size == durablestore.MaxInlineBodyBytes {
				if len(envelope.Runtime.Inline) != size || envelope.Runtime.Reference != nil {
					t.Fatalf("runtime slot at limit = %+v, want %d inline bytes", envelope.Runtime, size)
				}
			} else if envelope.Runtime.Reference == nil || envelope.Runtime.Inline != nil {
				t.Fatalf("runtime slot above limit = %+v, want object reference", envelope.Runtime)
			}
		})
	}
}

func TestEffectiveOffloadHonorsReleasedCombinedEnvelopeBoundary(t *testing.T) {
	for _, delta := range []int{0, 1} {
		t.Run(strconv.Itoa(delta), func(t *testing.T) {
			backend := memstore.New()
			store, err := Open(backend, WithOffloadThreshold(durablestore.MaxEnvelopeBytes))
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			id := newTestUUID(t)
			value := publicEventWithEncodedSize(t, id, durablestore.MaxInlineBodyBytes)
			probe, err := durablestore.EncodeEnvelope(durablestore.Envelope{
				Kind: durablestore.EnvelopeKindPublicEvent, EventID: coresessionwire.EventID(value.EventHeader().EventID.String()),
				Public: durablestore.BodySlot{Inline: []byte(`{}`)}, Runtime: durablestore.BodySlot{Inline: []byte{}},
			})
			if err != nil {
				t.Fatalf("EncodeEnvelope(empty-body probe) error = %v", err)
			}
			publicAtLimit := durablestore.MaxEnvelopeBytes - (len(probe) - len(`{}`)) - durablestore.MaxInlineBodyBytes
			publicBody := canonicalBodyWithSize(t, publicAtLimit+delta)
			store.project = func(coresessionwire.TenantID, coresessionwire.SessionID, any) (harnesssessionwire.Projection, error) {
				return harnesssessionwire.Projection{EventID: coresessionwire.EventID(value.EventHeader().EventID.String()), Body: bytes.Clone(publicBody)}, nil
			}
			envelope := appendAndReadDurableEnvelope(t, store, backend, id, journal.NewEventRecord(value))
			if delta == 0 {
				if len(envelope.Public.Inline) != len(publicBody) || len(envelope.Runtime.Inline) != durablestore.MaxInlineBodyBytes {
					t.Fatalf("combined exact-limit slots = (public %+v, runtime %+v), want both inline", envelope.Public, envelope.Runtime)
				}
			} else if envelope.Runtime.Reference == nil || envelope.Runtime.Inline != nil || len(envelope.Public.Inline) != len(publicBody) {
				t.Fatalf("combined above-limit slots = (public %+v, runtime %+v), want larger runtime body offloaded", envelope.Public, envelope.Runtime)
			}
		})
	}
}

func TestEffectiveOffloadCombinedTieKeepsCanonicalPublicBodyInline(t *testing.T) {
	backend := memstore.New()
	store, err := Open(backend, WithOffloadThreshold(durablestore.MaxEnvelopeBytes))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	id := newTestUUID(t)
	value := publicEventWithEncodedSize(t, id, durablestore.MaxInlineBodyBytes)
	publicBody := canonicalBodyWithSize(t, durablestore.MaxInlineBodyBytes)
	store.project = func(coresessionwire.TenantID, coresessionwire.SessionID, any) (harnesssessionwire.Projection, error) {
		return harnesssessionwire.Projection{EventID: coresessionwire.EventID(value.EventHeader().EventID.String()), Body: bytes.Clone(publicBody)}, nil
	}
	envelope := appendAndReadDurableEnvelope(t, store, backend, id, journal.NewEventRecord(value))
	if len(envelope.Public.Inline) != len(publicBody) || envelope.Public.Reference != nil || envelope.Runtime.Reference == nil || envelope.Runtime.Inline != nil {
		t.Fatalf("equal-size combined slots = (public inline %d/ref %v, runtime inline %d/ref %v), want canonical public inline and runtime offloaded",
			len(envelope.Public.Inline), envelope.Public.Reference != nil, len(envelope.Runtime.Inline), envelope.Runtime.Reference != nil)
	}
}

var errSensitiveBlobPut = errors.New("postgres://operator:secret@example.invalid/session")

type putFailBlobs struct{ storage.Blobs }

func (putFailBlobs) Put(context.Context, string, io.Reader) error { return errSensitiveBlobPut }
func (b putFailBlobs) BlobReaderCloseBound() time.Duration {
	return b.Blobs.(storage.BlobReaderLifecycle).BlobReaderCloseBound()
}

func TestDurableOffloadFailureRetainsLegacyRecordTooLargeClassification(t *testing.T) {
	for _, public := range []bool{false, true} {
		t.Run(map[bool]string{false: "private", true: "public"}[public], func(t *testing.T) {
			base := memstore.New()
			backend, err := storage.NewCompositeWithOrderedIndex(base.Ledger, base.Leaser, base.KV, putFailBlobs{Blobs: base.Blobs}, base.OrderedIndex)
			if err != nil {
				t.Fatalf("NewCompositeWithOrderedIndex() error = %v", err)
			}
			store, err := Open(backend, WithOffloadThreshold(1))
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			id := newTestUUID(t)
			var record journal.JournalRecord = commandRecordWithEncodedSize(t, id, 1024)
			if public {
				value := publicEventWithEncodedSize(t, id, 1024)
				store.project = func(coresessionwire.TenantID, coresessionwire.SessionID, any) (harnesssessionwire.Projection, error) {
					return harnesssessionwire.Projection{EventID: coresessionwire.EventID(value.EventHeader().EventID.String()), Body: []byte(`{"ok":true}`)}, nil
				}
				record = journal.NewEventRecord(value)
			}
			lease, err := store.AcquireLease(context.Background(), id)
			if err != nil {
				t.Fatalf("AcquireLease() error = %v", err)
			}
			writer, err := store.OpenJournal(context.Background(), id, lease)
			if err != nil {
				t.Fatalf("OpenJournal() error = %v", err)
			}
			_, err = writer.Append(context.Background(), record)
			var tooLarge *journal.RecordTooLargeError
			if !errors.As(err, &tooLarge) {
				t.Fatalf("Append() error = %T %v, want *journal.RecordTooLargeError", err, err)
			}
			if !errors.Is(err, errSensitiveBlobPut) {
				t.Fatalf("Append() error does not preserve backend cause: %v", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("Append() error leaked backend detail: %v", err)
			}
			if tooLarge.Subject != ledgerName(id) || tooLarge.MsgID != record.IdempotencyID() || tooLarge.Length <= 1 {
				t.Fatalf("RecordTooLargeError = %+v, want legacy subject/msg/length", tooLarge)
			}
		})
	}
}
