package journal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"

	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/nats-io/nats.go"
)

// inlineThreshold is the per-record marshaled-size limit for staying in-stream. A
// record whose payload is at or below this size is published inline exactly as before;
// a record above it is offloaded to the per-session object store and replaced by a
// small pointer record. It is set conservatively UNDER the NATS default max_payload
// (1 MiB) so that anything which cannot be inlined is ALWAYS offloaded — there is no
// size band where a record is too big to inline yet not offloaded.
const inlineThreshold = 512 * 1024 // 512 KiB

// streamInlineCeiling is the stream's MaxMsgSize: the hard inline ceiling enforced by
// JetStream as defense-in-depth. It sits at the NATS default max_payload (1 MiB), well
// above inlineThreshold, so a correctly-offloaded pointer record (a few hundred bytes)
// always fits while a stray oversized inline publish is rejected by the server.
const streamInlineCeiling = 1 << 20 // 1 MiB

// objectBucketSuffix is appended to the per-session prefix to form the object-store
// bucket name. Keeping the bucket per-session (urvi_session_<sid>_obj) scopes Phase-6
// orphan-GC and future deletion to one session: dropping a session drops its bucket.
const objectBucketSuffix = "_obj"

// pointerCodecVersion is the version stamped into every pointer record body. It lets a
// future codec change be detected at restore (Task 5.3) rather than silently
// misinterpreting an older pointer; v1 is the whole-record offload scheme.
const pointerCodecVersion uint32 = 1

// objectIDHeader is the stream-message header that MARKS a message as an offload
// pointer (its presence, not its absence, is the detection signal the Task 5.3
// replayer keys on) and carries the content-addressed object id for a fast,
// body-free check. The body still carries the authoritative pointerRecord; the
// header just lets the replayer route without decoding. The Urvi- prefix avoids the
// JetStream-reserved Nats-* namespace.
const objectIDHeader = "Urvi-Object-Id"

// objectLenHeader carries the offloaded object's byte length alongside the id header,
// so an observer can size the object without reading the body. It mirrors the body's
// Length field; the body remains authoritative.
const objectLenHeader = "Urvi-Object-Len"

// codecVersionHeader carries the pointer codec version alongside the id header. It
// mirrors the body's CodecVersion; the body remains authoritative.
const codecVersionHeader = "Urvi-Codec-Version"

// SessionObjectBucket returns the per-session object-store bucket name:
// "urvi_session_<sid>_obj". It reuses the stream's session-scoped prefix and uuid
// (whose dashes/underscores are legal in a NATS bucket name, ^[a-zA-Z0-9_-]+$) so the
// bucket is scoped to exactly one session and never collides with the stream name.
func SessionObjectBucket(sessionID uuid.UUID) string {
	return streamPrefix + sessionID.String() + objectBucketSuffix
}

// objectID is the content-addressed object name for a record's marshaled bytes: the
// lowercase hex sha256 of the payload. Content addressing makes PutBytes idempotent —
// the same bytes always land under the same name, so a re-upload (e.g. an ambiguous-ack
// retry) dedups to the identical object — and lets the Task 5.3 replayer verify the
// fetched object by re-hashing it.
func objectID(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// pointerRecord is the small in-stream record that REPLACES an over-threshold record's
// body: it names the content-addressed object holding the real marshaled bytes, the
// byte length (for sizing and a cheap integrity cross-check), and the codec version. It
// is self-describing so the Task 5.3 replayer can rehydrate by fetching ObjectID,
// verifying sha256(bytes) == ObjectID, and decoding the bytes with the original codec.
type pointerRecord struct {
	ObjectID     string `json:"object_id"`
	Length       uint64 `json:"length"`
	CodecVersion uint32 `json:"codec_version"`
}

// PointerEncodeError wraps a failure to marshal a pointer record to JSON. A pointer is
// two strings and two integers, so this is effectively unreachable, but the codec
// returns a typed error rather than dropping json.Marshal's error to satisfy the
// errors-are-typed contract.
type PointerEncodeError struct{ Cause error }

func (e *PointerEncodeError) Error() string {
	return "journal: encode offload pointer: " + e.Cause.Error()
}
func (e *PointerEncodeError) Unwrap() error { return e.Cause }

// PointerDecodeError reports a pointer-record body that does not decode to a valid
// pointer at the untrusted restore boundary: malformed JSON, a non-object, an unknown
// field, trailing data, a missing/non-hex/wrong-length object id, or a zero length. The
// codec fails closed with this typed error so the Task 5.3 replayer never fetches an
// object from a malformed pointer.
type PointerDecodeError struct {
	Reason string
	Cause  error
}

func (e *PointerDecodeError) Error() string {
	if e.Cause == nil {
		return "journal: decode offload pointer: " + e.Reason
	}
	return "journal: decode offload pointer: " + e.Reason + ": " + e.Cause.Error()
}
func (e *PointerDecodeError) Unwrap() error { return e.Cause }

// marshalPointer encodes a pointerRecord as its compact JSON object. It is the body of
// the stream message that stands in for an offloaded record.
func marshalPointer(p pointerRecord) ([]byte, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, &PointerEncodeError{Cause: err}
	}
	return data, nil
}

// unmarshalPointer decodes bytes produced by marshalPointer and validates the pointer's
// invariants. It rejects empty bytes, non-object JSON, unknown fields, trailing bytes,
// an object id that is not exactly a lowercase hex sha256, and a zero length — failing
// closed with a *PointerDecodeError so a malformed pointer never drives an object fetch.
func unmarshalPointer(data []byte) (pointerRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p pointerRecord
	if err := dec.Decode(&p); err != nil {
		return pointerRecord{}, &PointerDecodeError{Reason: "invalid json", Cause: err}
	}
	if _, err := dec.Token(); err != io.EOF {
		return pointerRecord{}, &PointerDecodeError{Reason: "trailing data after object"}
	}
	if !isHexSHA256(p.ObjectID) {
		return pointerRecord{}, &PointerDecodeError{Reason: "object_id is not a lowercase hex sha256"}
	}
	if p.Length == 0 {
		return pointerRecord{}, &PointerDecodeError{Reason: "length must be non-zero"}
	}
	return p, nil
}

// isHexSHA256 reports whether s is exactly 64 lowercase hex characters — the shape of a
// sha256 object id. It is the syntactic gate at the pointer-decode boundary; the Task
// 5.3 replayer additionally re-hashes the fetched bytes against it (semantic check).
func isHexSHA256(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// objectPutter is the narrow object-store surface the journal's offload path depends on
// (Interface Segregation): upload bytes under a name. The vendored nats.ObjectStore
// satisfies it; the journal never depends on the full store interface. ctx-free because
// PutBytes carries its context via a nats.ContextOpt; the caller passes nats.Context.
type objectPutter interface {
	PutBytes(name string, data []byte, opts ...nats.ObjectOpt) (*nats.ObjectInfo, error)
}

// RecordTooLargeError reports a record whose marshaled payload exceeds inlineThreshold
// but could NOT be offloaded to the object store (the bucket is unavailable or PutBytes
// failed). The journal fails closed with this typed error rather than silently
// inlining an over-threshold — possibly over-max_payload — record. It carries the
// destination subject, the record's Nats-Msg-Id, the payload length, and the underlying
// object-store cause.
type RecordTooLargeError struct {
	Subject string
	MsgID   string
	Length  int
	Cause   error
}

func (e *RecordTooLargeError) Error() string {
	return "journal: record too large to inline and offload failed for " + strconv.Quote(e.Subject) +
		" (msg-id " + strconv.Quote(e.MsgID) +
		", len " + strconv.Itoa(e.Length) + "): " + e.Cause.Error()
}
func (e *RecordTooLargeError) Unwrap() error { return e.Cause }

// ObjectStoreSetupError wraps a failure to create or bind the per-session object-store
// bucket in NewSessionJournal. It carries the bucket name and unwraps to the underlying
// NATS error, so a caller can errors.As both this and the cause. It is the object-store
// analogue of StreamSetupError.
type ObjectStoreSetupError struct {
	Bucket string
	Cause  error
}

func (e *ObjectStoreSetupError) Error() string {
	return "journal: object store setup for " + strconv.Quote(e.Bucket) + ": " + e.Cause.Error()
}
func (e *ObjectStoreSetupError) Unwrap() error { return e.Cause }

// ensureObjectStore creates (or binds, if already provisioned) the per-session object
// bucket idempotently, mirroring ensureStream. CreateObjectStore on a fresh bucket
// creates it; on an existing bucket it returns ErrStreamNameAlreadyInUse (the bucket is
// backed by a JetStream stream), which is NOT a failure — we bind the existing store.
// Any other error fails closed with a typed *ObjectStoreSetupError.
func ensureObjectStore(js nats.JetStreamContext, sessionID uuid.UUID) (nats.ObjectStore, error) {
	bucket := SessionObjectBucket(sessionID)
	store, err := js.CreateObjectStore(&nats.ObjectStoreConfig{Bucket: bucket})
	if err == nil {
		return store, nil
	}
	if errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
		bound, bindErr := js.ObjectStore(bucket)
		if bindErr != nil {
			return nil, &ObjectStoreSetupError{Bucket: bucket, Cause: bindErr}
		}
		return bound, nil
	}
	return nil, &ObjectStoreSetupError{Bucket: bucket, Cause: err}
}

// buildOffloadMessage uploads payload to the object store under its content-addressed id
// (BEFORE the caller appends, so a dangling pointer is impossible) and returns a stream
// message whose body is the small pointer record and whose headers carry the same
// Nats-Msg-Id as the original record (so the fence/dedup path is identical — only the
// body changes) plus the Urvi- offload markers. On any upload failure it returns a
// *RecordTooLargeError, failing closed rather than inlining an over-threshold record.
//
// The returned message is otherwise wired exactly like the inline message in Append
// (same Subject, same Nats-Msg-Id), so the serializer's fence, dedup, and ambiguous-ack
// reconciliation reason about it identically.
func buildOffloadMessage(ctx context.Context, store objectPutter, rec JournalRecord, payload []byte) (*nats.Msg, error) {
	id := objectID(payload)

	// Upload-before-append: the object must be durable before any pointer references it.
	if _, err := store.PutBytes(id, payload, nats.Context(ctx)); err != nil {
		return nil, &RecordTooLargeError{Subject: rec.Subject(), MsgID: rec.IdempotencyID(), Length: len(payload), Cause: err}
	}

	body, err := marshalPointer(pointerRecord{ObjectID: id, Length: uint64(len(payload)), CodecVersion: pointerCodecVersion})
	if err != nil {
		return nil, &RecordTooLargeError{Subject: rec.Subject(), MsgID: rec.IdempotencyID(), Length: len(payload), Cause: err}
	}

	return &nats.Msg{
		Subject: rec.Subject(),
		Header: nats.Header{
			nats.MsgIdHdr:      []string{rec.IdempotencyID()},
			objectIDHeader:     []string{id},
			objectLenHeader:    []string{strconv.Itoa(len(payload))},
			codecVersionHeader: []string{strconv.FormatUint(uint64(pointerCodecVersion), 10)},
		},
		Data: body,
	}, nil
}
