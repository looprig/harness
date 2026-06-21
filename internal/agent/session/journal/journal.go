package journal

import (
	"context"
	"sync"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/nats-io/nats.go"
)

// SessionJournal is the single serialized writer for one session's durable
// stream. Append encodes a JournalRecord's payload, publishes it to the record's
// JetStream subject under stream-level expected-sequence fencing, and returns the
// assigned stream sequence. It is the only thing that writes a session's stream;
// callers funnel every event, command, and fence through it so the stream stays a
// totally-ordered, gap-free log.
//
// The interface is intentionally narrow (one method): a caller that only needs to
// persist a record must not depend on stream-management surface. The concrete
// *streamJournal in this package satisfies it; NewSessionJournal is the
// composition-time constructor.
type SessionJournal interface {
	// Append serializes rec, publishes it under the next expected sequence, and
	// returns the assigned stream sequence. ctx bounds the caller's willingness to
	// wait; the publish additionally carries a per-append deadline independent of
	// ctx so one stuck call cannot wedge the serialized writer forever. Appends are
	// totally ordered: the returned sequences are strictly monotonic across calls.
	Append(ctx context.Context, rec JournalRecord) (seq uint64, err error)
}

// defaultAppendTimeout bounds a single Append's publish round-trip independent of
// the caller's context. The journal holds its mutex across the publish, so an
// unbounded publish would wedge every queued Append behind it; this per-append
// deadline fails the one stuck call fast and keeps the serialized writer live.
const defaultAppendTimeout = 5 * time.Second

// streamJournal is the concrete single-writer serializer. It binds one session's
// stream and serializes every Append behind mu, advancing expectedSeq from each
// publish ack so the next publish fences on the exact prior sequence. A foreign
// goroutine cannot race the sequence: mu guards the whole publish-and-advance.
type streamJournal struct {
	js     nats.JetStreamContext
	stream string // the bound stream name (StreamName(sessionID))

	// appendTimeout is the per-append publish deadline (defaultAppendTimeout unless
	// overridden via WithAppendTimeout).
	appendTimeout time.Duration

	// mu serializes Append and guards expectedSeq. The serializer is single-writer
	// by contract; the mutex makes that safe even if a caller fans Append across
	// goroutines.
	mu sync.Mutex
	// expectedSeq is the stream sequence the next publish must fence on: the last
	// successfully published sequence (0 for a fresh stream). Publishing with
	// Nats-Expected-Last-Sequence == expectedSeq rejects any write that does not
	// extend exactly this tip, so a stale writer (Task 4.4) cannot interleave.
	expectedSeq uint64
}

// Append marshals rec via its payload codec, publishes it to rec.Subject() with the
// record's IdempotencyID() as the Nats-Msg-Id and expectedSeq as the
// stream-level Nats-Expected-Last-Sequence fence, then advances expectedSeq to the
// assigned sequence on success. The whole operation holds mu so the publish and the
// expectedSeq advance are one atomic step; the publish carries its own deadline.
func (j *streamJournal) Append(ctx context.Context, rec JournalRecord) (uint64, error) {
	payload, err := marshalRecord(rec)
	if err != nil {
		return 0, err
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	// Per-append deadline independent of the session context: a derived child of
	// ctx so the caller can still cancel earlier, but capped so one stuck publish
	// cannot hold mu (and thus every queued Append) indefinitely.
	pubCtx, cancel := context.WithTimeout(ctx, j.appendTimeout)
	defer cancel()

	msg := &nats.Msg{
		Subject: rec.Subject(),
		Header:  nats.Header{nats.MsgIdHdr: []string{rec.IdempotencyID()}},
		Data:    payload,
	}
	ack, err := j.js.PublishMsg(msg,
		nats.Context(pubCtx),
		nats.ExpectLastSequence(j.expectedSeq),
	)
	if err != nil {
		// TODO(4.5): resolve ambiguous ack — a deadline/transport error here is
		// ambiguous (the server may have committed the record before the ack was
		// lost). 4.5 will re-fetch the tip by Nats-Msg-Id to decide commit vs retry;
		// for now fail closed with a typed error and leave expectedSeq unadvanced so
		// the next Append re-fences on the same tip.
		return 0, &AppendError{Subject: rec.Subject(), MsgID: rec.IdempotencyID(), Expected: j.expectedSeq, Cause: err}
	}
	j.expectedSeq = ack.Sequence
	return ack.Sequence, nil
}

// marshalRecord encodes a record's payload via the codec for its concrete kind:
// an event via event.MarshalEvent (fails closed on an Ephemeral event), a command
// via command.MarshalCommand, a fence via MarshalLeaseFence. The switch is over the
// sealed JournalRecord sum, so the default arm is unreachable for an in-package
// record; it fails closed with a typed RecordKindError rather than panicking if a
// foreign type ever satisfies the marker.
func marshalRecord(rec JournalRecord) ([]byte, error) {
	switch r := rec.(type) {
	case EventRecord:
		payload, err := event.MarshalEvent(r.Event())
		if err != nil {
			return nil, &MarshalRecordError{Subject: r.Subject(), Cause: err}
		}
		return payload, nil
	case CommandRecord:
		payload, err := command.MarshalCommand(r.Command())
		if err != nil {
			return nil, &MarshalRecordError{Subject: r.Subject(), Cause: err}
		}
		return payload, nil
	case FenceRecord:
		payload, err := MarshalLeaseFence(r.Fence())
		if err != nil {
			return nil, &MarshalRecordError{Subject: r.Subject(), Cause: err}
		}
		return payload, nil
	default:
		return nil, &RecordKindError{Subject: rec.Subject()}
	}
}
