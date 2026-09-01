package sessionstore

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"github.com/looprig/harness/pkg/gate"
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

// gatePreparedRecordWithEncodedSize builds a GatePreparedRecord whose encoded
// runtime body is exactly size bytes, padding the ask-user question (a plain JSON
// string, so one pad rune is one wire byte). The base is measured with a
// ONE-character question because an empty one is omitted from the wire entirely,
// which would make the padding non-linear. It asserts the achieved size rather
// than assuming it, so a codec change fails the fixture instead of silently
// moving the boundary the test claims to drive.
func gatePreparedRecordWithEncodedSize(t *testing.T, sessionID uuid.UUID, size int) journal.GatePreparedRecord {
	t.Helper()
	gateID := newTestUUID(t)
	coords := identity.Coordinates{
		SessionID: sessionID, LoopID: newTestUUID(t), TurnID: newTestUUID(t), StepID: newTestUUID(t),
	}
	g := gate.Gate{
		ID:       gateID,
		Kind:     gate.KindAskUser,
		Resolver: gate.ResolverLoop,
		Blocks:   gate.BlocksToolCall,
		Effect:   gate.EffectResume,
		Prompt:   gate.Prompt{Title: "Ask", Body: "question"},
	}
	prepared := event.GatePrepared{Header: event.Header{Coordinates: coords, EventID: newTestUUID(t)}, Gate: g}
	makeRecord := func(question string) journal.GatePreparedRecord {
		return journal.NewGatePreparedRecord(prepared, gate.OpenPayload{
			GateID: gateID, Payload: gate.AskUserPayload{Question: question},
		})
	}
	baseBody, err := journal.MarshalGatePreparedRecord(makeRecord("x"))
	if err != nil {
		t.Fatalf("MarshalGatePreparedRecord(base) error = %v", err)
	}
	if size < len(baseBody) {
		t.Fatalf("requested gate-prepared body size %d is below base size %d", size, len(baseBody))
	}
	record := makeRecord(strings.Repeat("x", 1+size-len(baseBody)))
	body, err := journal.MarshalGatePreparedRecord(record)
	if err != nil {
		t.Fatalf("MarshalGatePreparedRecord(sized) error = %v", err)
	}
	if len(body) != size {
		t.Fatalf("sized gate-prepared body = %d bytes, want %d", len(body), size)
	}
	return record
}

// TestAppendRefusesRuntimeBodyAboveReplayCeiling drives the append-time runtime
// body ceiling at exactly maxRuntimeBodyBytes and one byte either side, for both
// record kinds whose write-side codec does NOT cap its own output:
// command.MarshalCommand and journal.MarshalGatePreparedRecord. (A gate-prepared
// body is an event-capped "prepared" half plus an UNCAPPED payload half plus JSON
// framing, so it exceeds the ceiling by construction; a command body simply has no
// marshal-side cap at all.)
//
// Without the guard both kinds could be offloaded and appended successfully and
// then be permanently unreachable through replay's declared-size ceiling — a
// fail-closed but UNRECOVERABLE restore. Only strictly above the ceiling may the
// append be refused: the at- and below-ceiling arms are what a `>` weakened to
// `>=` fails. Round-tripping a legal record through the same path is
// TestAppendBelowReplayCeilingStillRoundTrips.
func TestAppendRefusesRuntimeBodyAboveReplayCeiling(t *testing.T) {
	kinds := map[string]func(*testing.T, uuid.UUID, int) journal.JournalRecord{
		"command": func(t *testing.T, id uuid.UUID, size int) journal.JournalRecord {
			return commandRecordWithEncodedSize(t, id, size)
		},
		"gate-prepared": func(t *testing.T, id uuid.UUID, size int) journal.JournalRecord {
			return gatePreparedRecordWithEncodedSize(t, id, size)
		},
	}
	for kindName, build := range kinds {
		for _, delta := range []int{-1, 0, 1} {
			t.Run(kindName+"/"+strconv.Itoa(delta), func(t *testing.T) {
				backend := memstore.New()
				store, err := Open(backend)
				if err != nil {
					t.Fatalf("Open() error = %v", err)
				}
				id := newTestUUID(t)
				record := build(t, id, maxRuntimeBodyBytes+delta)
				lease, err := store.AcquireLease(context.Background(), id)
				if err != nil {
					t.Fatalf("AcquireLease() error = %v", err)
				}
				writer, err := store.OpenJournal(context.Background(), id, lease)
				if err != nil {
					t.Fatalf("OpenJournal() error = %v", err)
				}
				_, appendErr := writer.Append(context.Background(), record)

				if delta <= 0 {
					if appendErr != nil {
						t.Fatalf("Append() at %d bytes error = %v, want success at or below the %d byte ceiling",
							maxRuntimeBodyBytes+delta, appendErr, maxRuntimeBodyBytes)
					}
					return
				}
				var tooLarge *journal.RecordTooLargeError
				if !errors.As(appendErr, &tooLarge) {
					t.Fatalf("Append() error = %T %v, want *journal.RecordTooLargeError above the ceiling", appendErr, appendErr)
				}
				if !errors.Is(appendErr, errRuntimeBodyAboveReplayCeiling) {
					t.Fatalf("Append() error = %v, want the replay-ceiling cause", appendErr)
				}
				if tooLarge.Length != maxRuntimeBodyBytes+delta || tooLarge.MsgID != record.IdempotencyID() {
					t.Fatalf("RecordTooLargeError = %+v, want length %d and msg %q",
						tooLarge, maxRuntimeBodyBytes+delta, record.IdempotencyID())
				}
				tip, err := backend.Ledger.Tip(context.Background(), ledgerName(id))
				if err != nil {
					t.Fatalf("Ledger.Tip() error = %v", err)
				}
				if tip != 1 {
					t.Fatalf("ledger tip = %d, want 1 (opening fence only; the refused record must not be durable)", tip)
				}
				keys, err := backend.Blobs.List(context.Background(), ledgerName(id)+blobsInfix)
				if err != nil {
					t.Fatalf("Blobs.List() error = %v", err)
				}
				if len(keys) != 0 {
					t.Fatalf("blobs after refusal = %v, want none (refusal precedes object publication)", keys)
				}
			})
		}
	}
}

// TestAppendBelowReplayCeilingStillRoundTrips is the legal-shape half of the
// ceiling guard: an offloaded record of each guarded kind, comfortably under every
// codec cap, still appends and replays back with its identity intact. A guard that
// closed the oversized write path by rejecting ordinary records would fail here.
func TestAppendBelowReplayCeilingStillRoundTrips(t *testing.T) {
	kinds := map[string]func(*testing.T, uuid.UUID) journal.JournalRecord{
		"command": func(t *testing.T, id uuid.UUID) journal.JournalRecord {
			return commandRecordWithEncodedSize(t, id, 1<<20)
		},
		"gate-prepared": func(t *testing.T, id uuid.UUID) journal.JournalRecord {
			return gatePreparedRecordWithEncodedSize(t, id, 1<<20)
		},
	}
	for kindName, build := range kinds {
		t.Run(kindName, func(t *testing.T) {
			backend := memstore.New()
			store, err := Open(backend)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			id := newTestUUID(t)
			record := build(t, id)
			lease, err := store.AcquireLease(context.Background(), id)
			if err != nil {
				t.Fatalf("AcquireLease() error = %v", err)
			}
			writer, err := store.OpenJournal(context.Background(), id, lease)
			if err != nil {
				t.Fatalf("OpenJournal() error = %v", err)
			}
			seq, err := writer.Append(context.Background(), record)
			if err != nil {
				t.Fatalf("Append() error = %v", err)
			}
			replayer, err := store.OpenInternalRecordReplayer(id, ReplayRequest{FromSeq: seq})
			if err != nil {
				t.Fatalf("OpenInternalRecordReplayer() error = %v", err)
			}
			cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{})
			if err != nil {
				t.Fatalf("RecordReplayer.Open() error = %v", err)
			}
			t.Cleanup(func() { _ = cursor.Close() })
			got, gotSeq, err := cursor.Next(context.Background())
			if err != nil || gotSeq != seq {
				t.Fatalf("replay = (seq %d, err %v), want seq %d success", gotSeq, err, seq)
			}
			if got.IdempotencyID() != record.IdempotencyID() {
				t.Fatalf("replayed record id = %q, want %q", got.IdempotencyID(), record.IdempotencyID())
			}
		})
	}
}

// TestCanonicalPublicBodyOffloadsAboveThreshold covers the object-backed
// canonical public body: any public projection above the effective threshold is
// persisted through the released object API and referenced from the envelope.
// It is a live production path (a public projection over 512 KiB) that the
// inline-only public tests cannot reach — they sit the public body AT the
// threshold, and the combined-boundary test offloads the RUNTIME body instead.
// It asserts the object is published exactly once, is readable back under the
// public object kind, and that the small runtime body stays inline.
func TestCanonicalPublicBodyOffloadsAboveThreshold(t *testing.T) {
	backend := memstore.New()
	store, err := Open(backend)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	id := newTestUUID(t)
	value := publicEventWithEncodedSize(t, id, 1024) // small runtime body: stays inline
	publicBody := canonicalBodyWithSize(t, defaultOffloadThreshold+1)
	store.project = func(coresessionwire.TenantID, coresessionwire.SessionID, any) (harnesssessionwire.Projection, error) {
		return harnesssessionwire.Projection{
			EventID: coresessionwire.EventID(value.EventHeader().EventID.String()),
			Body:    bytes.Clone(publicBody),
		}, nil
	}
	envelope := appendAndReadDurableEnvelope(t, store, backend, id, journal.NewEventRecord(value))

	if envelope.Public.Reference == nil || envelope.Public.Inline != nil {
		t.Fatalf("public slot = %+v, want an object reference and no inline body", envelope.Public)
	}
	if len(envelope.Runtime.Inline) == 0 || envelope.Runtime.Reference != nil {
		t.Fatalf("runtime slot = %+v, want the small native body inline", envelope.Runtime)
	}
	if envelope.Public.Reference.SizeBytes != uint64(len(publicBody)) {
		t.Fatalf("public reference size = %d, want %d", envelope.Public.Reference.SizeBytes, len(publicBody))
	}
	if got := sha256.Sum256(publicBody); envelope.Public.Reference.SHA256 != got {
		t.Fatalf("public reference digest does not name the projected body")
	}

	// Exactly one object was published for this append: the canonical public body.
	keys, err := backend.Blobs.List(context.Background(), ledgerName(id)+blobsInfix)
	if err != nil {
		t.Fatalf("Blobs.List() error = %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("published objects = %v, want exactly one canonical public body", keys)
	}

	// The referenced object is readable back through the released verified API
	// under the PUBLIC object kind, byte-for-byte.
	metadata, err := envelope.Public.Reference.ObjectMetadata()
	if err != nil {
		t.Fatalf("ObjectMetadata() error = %v", err)
	}
	reader, err := store.durable.GetObject(context.Background(), durablestore.GetObjectRequest{
		TenantID: harnessTenantID, SessionID: harnessSessionID(id),
		ExpectedKind: durablestore.ObjectKindJournalPublic, Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("GetObject() error = %v", err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read offloaded public body: read %v, close %v", readErr, closeErr)
	}
	if !bytes.Equal(got, publicBody) {
		t.Fatalf("offloaded public body = %d bytes, want the %d projected bytes", len(got), len(publicBody))
	}
}
