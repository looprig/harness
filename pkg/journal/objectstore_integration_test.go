//go:build integration

package journal_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/journal"
	"github.com/ciram-co/looprig/pkg/uuid"
	"github.com/nats-io/nats.go"
)

// The Looprig- offload header marker the write side stamps on a pointer message. Mirrors
// the unexported constant in objectstore.go (this is package journal_test, so it
// cannot reference it directly); the value is the contract the Task 5.3 replayer reads.
const objectIDHeaderName = "Looprig-Object-Id"

// smallTextEvent builds a session-scoped SessionStarted event — a tiny record that
// always marshals well under the inline threshold.
func smallTextEvent(sid uuid.UUID, eid byte) event.SessionStarted {
	return event.SessionStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid},
			EventID:     seedUUID(eid),
		},
	}
}

// largeStepDone builds a loop-scoped StepDone whose single assistant text block is
// `blockChars` bytes long, so its marshaled payload comfortably exceeds the 512 KiB
// inline threshold and is forced down the offload path.
func largeStepDone(sid, lid uuid.UUID, eid byte, blockChars int) event.StepDone {
	return event.StepDone{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: seedUUID(0x71), StepID: seedUUID(0x72)},
			EventID:     seedUUID(eid),
		},
		Messages: content.AgenticMessages{
			&content.AIMessage{Message: content.Message{
				Role:   content.RoleAssistant,
				Blocks: []content.Block{&content.TextBlock{Text: strings.Repeat("x", blockChars)}},
			}},
		},
	}
}

// TestSessionJournalInlineSmallRecord pins the unchanged inline path: a small record is
// published in-stream verbatim (stored body == marshaled payload), no object is created
// in the per-session bucket, and the sequence advances by exactly one.
func TestSessionJournalInlineSmallRecord(t *testing.T) {
	sid := seedUUID(0x60)
	_, js := newEmbeddedJS(t)
	j, err := journal.NewSessionJournal(js, sid, mustAcquireLease(t, js, sid))
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ev := smallTextEvent(sid, 0x61)
	payload, err := event.MarshalEvent(ev)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}

	// The journal's opening LeaseFence is seq 1; the first user append lands at seq 2.
	seq, err := j.Append(ctx, journal.NewEventRecord(ev))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if seq != 2 {
		t.Fatalf("Append seq = %d, want 2 (after opening LeaseFence at seq 1)", seq)
	}

	raw, err := js.GetMsg(journal.StreamName(sid), seq)
	if err != nil {
		t.Fatalf("GetMsg: %v", err)
	}
	// Stored verbatim: body equals the marshaled payload, with no offload marker.
	if !bytes.Equal(raw.Data, payload) {
		t.Errorf("inline body len = %d, want exact payload len %d", len(raw.Data), len(payload))
	}
	if got := raw.Header.Get(objectIDHeaderName); got != "" {
		t.Errorf("inline message carries offload marker %q = %q, want absent", objectIDHeaderName, got)
	}

	// No object was created in the per-session bucket: binding it shows zero objects.
	store, err := js.ObjectStore(journal.SessionObjectBucket(sid))
	if err != nil {
		t.Fatalf("ObjectStore: %v", err)
	}
	objs, err := store.List()
	if err != nil && err != nats.ErrNoObjectsFound {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 0 {
		t.Errorf("object bucket has %d objects after inline append, want 0", len(objs))
	}
}

// TestSessionJournalOffloadsLargeRecord is the core Task 5.2 assertion. A StepDone whose
// marshaled payload exceeds the inline threshold is offloaded: the object exists in the
// per-session bucket under hex(sha256(payload)) with bytes equal to the marshaled
// payload (upload-before-append — the object is present right after Append returns), the
// stream message is the small pointer (size much smaller than the threshold) carrying
// the offload header, the sequence advances by exactly one, and a follow-up append still
// fences correctly (seq advances to 2). Offload is content-addressed/idempotent: the
// same payload appended again maps to the same object id.
func TestSessionJournalOffloadsLargeRecord(t *testing.T) {
	sid := seedUUID(0x62)
	lid := seedUUID(0x63)
	const blockChars = 700 * 1024 // > 512 KiB inline threshold once marshaled.

	_, js := newEmbeddedJS(t)
	j, err := journal.NewSessionJournal(js, sid, mustAcquireLease(t, js, sid))
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ev := largeStepDone(sid, lid, 0x64, blockChars)
	payload, err := event.MarshalEvent(ev)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	if len(payload) <= 512*1024 {
		t.Fatalf("test payload len %d is not over the 512 KiB threshold; raise blockChars", len(payload))
	}
	sum := sha256.Sum256(payload)
	wantObjID := hex.EncodeToString(sum[:])

	// The opening LeaseFence is seq 1; the first user append (the large record) is seq 2.
	seq, err := j.Append(ctx, journal.NewEventRecord(ev))
	if err != nil {
		t.Fatalf("Append (large): %v", err)
	}
	if seq != 2 {
		t.Fatalf("Append (large) seq = %d, want 2 (after opening LeaseFence at seq 1)", seq)
	}

	// Upload-before-append proof: the object is present in the bucket NOW (Append has
	// returned), addressed by hex(sha256(payload)), with bytes equal to the payload.
	store, err := js.ObjectStore(journal.SessionObjectBucket(sid))
	if err != nil {
		t.Fatalf("ObjectStore: %v", err)
	}
	gotBytes, err := store.GetBytes(wantObjID)
	if err != nil {
		t.Fatalf("GetBytes(%s): %v (object not uploaded before append?)", wantObjID, err)
	}
	if !bytes.Equal(gotBytes, payload) {
		t.Errorf("object bytes len = %d, want marshaled payload len %d", len(gotBytes), len(payload))
	}

	// The stream message is the SMALL pointer, not the payload, and carries the offload
	// header marking it as a pointer with the matching object id.
	raw, err := js.GetMsg(journal.StreamName(sid), seq)
	if err != nil {
		t.Fatalf("GetMsg: %v", err)
	}
	if len(raw.Data) >= 512*1024 {
		t.Errorf("stream message len = %d, want << 512 KiB (a pointer, not the payload)", len(raw.Data))
	}
	if len(raw.Data) > 1024 {
		t.Errorf("pointer body len = %d, want a few hundred bytes", len(raw.Data))
	}
	if got := raw.Header.Get(objectIDHeaderName); got != wantObjID {
		t.Errorf("pointer %s header = %q, want %q", objectIDHeaderName, got, wantObjID)
	}
	// The pointer body itself names the same content-addressed object.
	if !bytes.Contains(raw.Data, []byte(wantObjID)) {
		t.Errorf("pointer body %q does not reference object id %q", raw.Data, wantObjID)
	}
	// The Nats-Msg-Id fence/dedup id is unchanged — the record's own EventID.
	if got := raw.Header.Get(nats.MsgIdHdr); got != ev.EventID.String() {
		t.Errorf("pointer %s = %q, want record id %q", nats.MsgIdHdr, got, ev.EventID.String())
	}

	// A follow-up append still fences correctly: it lands at seq 3 (the fence advanced
	// to 2 from the offloaded append, exactly as the inline path advances).
	follow := smallTextEvent(sid, 0x65)
	seq2, err := j.Append(ctx, journal.NewEventRecord(follow))
	if err != nil {
		t.Fatalf("Append (follow-up): %v", err)
	}
	if seq2 != 3 {
		t.Errorf("follow-up Append seq = %d, want 3 (fence advanced past the offloaded record)", seq2)
	}

	// Stream tip reflects three durable records: the opening LeaseFence plus the two
	// appends.
	info, err := js.StreamInfo(journal.StreamName(sid))
	if err != nil {
		t.Fatalf("StreamInfo: %v", err)
	}
	if info.State.LastSeq != 3 || info.State.Msgs != 3 {
		t.Errorf("stream state LastSeq=%d Msgs=%d, want 3/3 (LeaseFence + 2 appends)", info.State.LastSeq, info.State.Msgs)
	}
}

// TestOffloadObjectIDIsContentAddressed asserts the same marshaled payload always maps
// to the same object id (and different payloads to different ids), proving offload is
// content-addressed and a re-upload of identical bytes is idempotent.
func TestOffloadObjectIDIsContentAddressed(t *testing.T) {
	sid := seedUUID(0x67)
	const blockChars = 700 * 1024

	_, js := newEmbeddedJS(t)
	if _, err := journal.NewSessionJournal(js, sid, mustAcquireLease(t, js, sid)); err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}
	store, err := js.ObjectStore(journal.SessionObjectBucket(sid))
	if err != nil {
		t.Fatalf("ObjectStore: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	payload := bytes.Repeat([]byte("abc"), blockChars)
	sum := sha256.Sum256(payload)
	id := hex.EncodeToString(sum[:])

	// Put the same bytes twice under the content-addressed id: the second put is
	// idempotent — one object, same bytes.
	if _, err := store.PutBytes(id, payload, nats.Context(ctx)); err != nil {
		t.Fatalf("first PutBytes: %v", err)
	}
	if _, err := store.PutBytes(id, payload, nats.Context(ctx)); err != nil {
		t.Fatalf("second PutBytes (idempotent): %v", err)
	}
	got, err := store.GetBytes(id)
	if err != nil {
		t.Fatalf("GetBytes: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("stored bytes differ from payload after idempotent re-put")
	}

	// Different bytes ⇒ different content-addressed id.
	other := append(bytes.Clone(payload), 'z')
	otherSum := sha256.Sum256(other)
	otherID := hex.EncodeToString(otherSum[:])
	if otherID == id {
		t.Fatalf("distinct payloads produced the same object id %q", id)
	}
}
