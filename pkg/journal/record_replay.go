package journal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/uuid"
	"github.com/nats-io/nats.go"
)

// RecordReplayer is the journal's FULL read side: it opens an ordered cursor over a
// session's stream and surfaces EVERY record — events, commands, AND fences — in
// stream-sequence order. It is the data seam the transcript export consumes: the
// narrower EventReplayer subject-filters to enduring-event subjects only and therefore
// DROPS every CommandRecord (the user's gate decisions). Reading the whole stream in
// sequence instead yields events and commands interleaved in append/causal order — the
// merged stream the transcript builder needs. The concrete *recordStreamReplayer
// satisfies it; NewRecordReplayer is the composition-time constructor (same inputs as
// NewEventReplayer).
type RecordReplayer interface {
	// Open binds a consumer over the WHOLE session stream (all subjects) and returns a
	// cursor positioned at req.From. The embedded server is NOT started here (that is the
	// composition root's job); js must already be bound. Only the cold path
	// (Follow:false) is implemented; Follow:true returns a typed *FollowUnsupportedError,
	// matching EventReplayer.
	Open(ctx context.Context, req ReplayRequest) (RecordCursor, error)
}

// RecordCursor yields a session's journal records in stream-sequence order. Next returns
// the next decoded JournalRecord (an EventRecord, CommandRecord, or FenceRecord) with its
// stream sequence, io.EOF once the cold backlog is drained, or a typed error on a
// malformed/missing/corrupt record. Close releases the underlying consumer; it is
// idempotent and safe to call after an error. It is the all-subjects counterpart to
// EventCursor (which yields events only).
type RecordCursor interface {
	// Next returns the next record and its stream sequence, or io.EOF when the cold
	// backlog is exhausted. A decode/object error fails secure: the cursor surfaces the
	// typed error rather than skipping or zero-valuing the record.
	Next(ctx context.Context) (JournalRecord, uint64, error)
	// Close tears down the consumer. Idempotent: a second call is a no-op.
	Close() error
}

// recordStreamReplayer is the concrete RecordReplayer over one JetStream context and one
// session's object store. Like streamReplayer it holds no per-replay state: every Open
// builds an independent consumer + cursor, so concurrent replays do not interfere. It
// reuses the EventReplayer's typed errors (ReplaySetupError/ReplayReadError/
// ReplayDecodeError, the Object* errors, FollowUnsupportedError) — the read-side failure
// vocabulary is identical; only the subject set and the decode dispatch differ.
type recordStreamReplayer struct {
	js      nats.JetStreamContext
	objects objectFetcher
}

// NewRecordReplayer wires a full-stream replayer over a bound JetStream context and the
// session's object store, mirroring NewEventReplayer's composition inputs. It returns the
// narrow RecordReplayer interface so callers depend on Open alone. objects is the
// per-session bucket (SessionObjectBucket) the write side offloaded over-threshold records
// to (of ANY kind — a command can be offloaded just like an event); the replayer
// rehydrates from it.
func NewRecordReplayer(js nats.JetStreamContext, objects nats.ObjectStore) RecordReplayer {
	return &recordStreamReplayer{js: js, objects: objects}
}

// Open binds an ephemeral pull consumer over the session stream filtered to ALL of the
// session's subjects (events, commands, and fences) and returns a cursor positioned at
// req.From. The tip (LastSeq) is snapshotted here so the cold cursor knows when the
// backlog is drained. Like EventReplayer.Open it is cold-only: Follow:true fails closed
// with a *FollowUnsupportedError rather than silently behaving as a cold cursor.
//
// A single consumer with a multi-subject filter delivers messages in STREAM-SEQUENCE
// order regardless of how many subjects it filters — it walks the stream's monotonic
// sequence emitting matching records. Because the journal is a single fenced writer
// producing a gap-free monotonic sequence, walking this one consumer over every subject
// is exactly walking session order across events, commands, and fences.
func (r *recordStreamReplayer) Open(ctx context.Context, req ReplayRequest) (RecordCursor, error) {
	if req.SessionID.IsZero() {
		return nil, &ReplaySetupError{Stream: "", Reason: "zero session id"}
	}
	stream := StreamName(req.SessionID)
	if req.Follow {
		// Cold-only: fail closed rather than behave as a cold cursor (mirrors EventReplayer).
		return nil, &FollowUnsupportedError{Stream: stream}
	}

	// Snapshot the tip BEFORE subscribing so the cold cursor's caught-up boundary is the
	// durable length at Open time (later appends are out of scope for a cold replay).
	info, err := r.js.StreamInfo(stream, nats.Context(ctx))
	if err != nil {
		return nil, &ReplaySetupError{Stream: stream, Reason: "stream info", Cause: err}
	}
	lastSeq := info.State.LastSeq

	filters := recordSubjectFilters(req.SessionID, req.LoopID)

	// An ephemeral pull consumer (no durable name) with AckNone — this is a read-only
	// replay, acks would only add round-trips — and an inactivity threshold so an
	// abandoned cursor self-cleans. ConsumerFilterSubjects requires BindStream + an empty
	// subject (mirrors EventReplayer.Open and the GC scan).
	opts := []nats.SubOpt{
		nats.BindStream(stream),
		nats.ConsumerFilterSubjects(filters...),
		nats.AckNone(),
		nats.InactiveThreshold(replayInactiveThreshold),
	}
	if req.From.fromSeq > 0 {
		opts = append(opts, nats.StartSequence(req.From.fromSeq))
	} else {
		opts = append(opts, nats.DeliverAll())
	}

	sub, err := r.js.PullSubscribe("", "", opts...)
	if err != nil {
		return nil, &ReplaySetupError{Stream: stream, Reason: "pull subscribe", Cause: err}
	}

	// The consumer's NumPending is the filter-aware count of matching records pending
	// right now; capture it so an empty-for-this-filter backlog EOFs cleanly instead of
	// tripping the "tip-says-pending but fetch-came-back-empty" anomaly guard. (With the
	// all-subjects filter this matches the raw tip, but the same guard keeps the
	// loop-narrowed filter correct.)
	matchPending, err := consumerNumPending(sub)
	if err != nil {
		return nil, &ReplaySetupError{Stream: stream, Reason: "consumer info", Cause: err}
	}

	return &recordCursor{
		stream:       stream,
		objects:      r.objects,
		sub:          sub,
		lastSeq:      lastSeq,
		fromSeq:      req.From.fromSeq,
		delivered:    0,
		emptyBacklog: matchPending == 0,
	}, nil
}

// recordSubjectFilters builds the consumer's subject filter set for a record replay.
// Unlike eventSubjectFilters (events only), it includes commands and fences:
//   - LoopID zero: the single '>' wildcard over the whole session (allSessionSubjects),
//     matching session events, every loop's events, every loop's commands, and fences.
//   - LoopID non-zero: the session-event and fence subjects (session-global) plus that
//     one loop's event and command subjects — a loop-narrowed full record view.
//
// The subjects are built via the shared subject builders (never hand-rolled), so the
// filter cannot drift from what the writer emits.
func recordSubjectFilters(sessionID, loopID uuid.UUID) []string {
	if loopID.IsZero() {
		return []string{allSessionSubjects(sessionID)}
	}
	return []string{
		SessionEventSubject(sessionID),
		FenceSubject(sessionID),
		LoopEventSubject(sessionID, loopID),
		LoopCommandSubject(sessionID, loopID),
	}
}

// recordCursor is the concrete RecordCursor over one ephemeral pull consumer. Its
// bounded-fetch + drain bookkeeping intentionally duplicates streamCursor's (in replay.go)
// rather than sharing a core: Task 10 is a scoped, additive change and replay.go /
// EventReplayer are deliberately left untouched, so the cold-cursor machinery is copied
// here. The only behavioral delta is decode, which dispatches by the record's subject into
// the right JournalRecord variant instead of always decoding an event. It is not safe for
// concurrent Next calls — a cursor is a single-reader handle — but mu guards Close against
// a racing Next.
type recordCursor struct {
	stream  string
	objects objectFetcher

	mu       sync.Mutex
	sub      *nats.Subscription
	closed   bool
	caughtUp bool

	// lastSeq is the stream tip captured at Open; it bounds the cold replay to the
	// durable length at Open time. The primary drain-done signal is caughtUp (set when a
	// delivered message reports NumPending==0); this is the secondary guard.
	lastSeq uint64
	// fromSeq is the inclusive start sequence (0 for Beginning); used only to detect an
	// empty-backlog open (start past the tip) so Next EOFs immediately.
	fromSeq uint64
	// delivered is the highest stream sequence handed to the caller so far.
	delivered uint64
	// emptyBacklog is true when the consumer reported NumPending==0 at Open: nothing
	// matches this cursor's filter, so the first Next EOFs immediately without a fetch.
	emptyBacklog bool
}

// Next fetches and decodes the next backlog record. It returns io.EOF when the cold
// backlog is exhausted. The drain bookkeeping is identical to streamCursor.Next: the
// primary drain-done signal is the consumer's NumPending (zero ⇒ next Next EOFs without a
// fetch); the Open-time tip (lastSeq) is the secondary bound; an empty fetch while the tip
// says backlog remains fails closed as a *ReplayReadError rather than truncating.
// Decode/object failures fail secure as typed errors and do NOT advance the cursor.
func (c *recordCursor) Next(ctx context.Context) (JournalRecord, uint64, error) {
	c.mu.Lock()
	if c.closed || c.caughtUp {
		c.mu.Unlock()
		return nil, 0, io.EOF
	}
	sub := c.sub
	if c.lastSeq == 0 || c.emptyBacklog || c.delivered >= c.lastSeq || c.fromSeq > c.lastSeq {
		c.mu.Unlock()
		return nil, 0, io.EOF
	}
	c.mu.Unlock()

	msg, err := c.fetchOne(ctx, sub)
	if err != nil {
		return nil, 0, err
	}
	if msg == nil {
		// Empty fetch while the tip said backlog remains and no NumPending==0 has been
		// seen — provably NOT caught up. EOF here would silently truncate. Fail closed,
		// disambiguating the caller's own deadline from the internal backstop (mirrors
		// streamCursor.Next).
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, &ReplayReadError{Stream: c.stream, Cause: ctxErr}
		}
		return nil, 0, &ReplayReadError{Stream: c.stream, Cause: errEmptyFetchBacklogRemaining}
	}

	seq, pending, err := messageMeta(msg, c.stream)
	if err != nil {
		return nil, 0, err
	}

	rec, err := c.decode(ctx, msg, seq)
	if err != nil {
		return nil, 0, err
	}

	c.mu.Lock()
	c.delivered = seq
	if pending == 0 {
		c.caughtUp = true
	}
	c.mu.Unlock()
	return rec, seq, nil
}

// fetchOne pulls a single backlog message, bounding the wait by the internal
// replayFetchTimeout backstop derived from the caller's context. An empty fetch (timeout
// or zero-length batch) returns (nil, nil) — the "no message this round" signal Next
// disambiguates against the tip and the caller context; any OTHER fetch failure fails
// closed as a *ReplayReadError. It is a verbatim copy of streamCursor.fetchOne because
// this task is scoped additive and leaves replay.go / EventReplayer untouched (not because
// same-package sharing is impossible — both cursors live in package journal).
//
// TODO(follow-up): the fail-closed empty-fetch guard above plus the delivered/caughtUp
// drain bookkeeping in Next now exist as two hand-synced copies (here and streamCursor in
// replay.go). A future change that is allowed to touch replay.go should extract them into
// a single shared unexported cold-cursor core, which would also fix the copied
// consumer-leak-on-error path (a PullSubscribe whose subsequent consumerNumPending fails
// returns without Unsubscribe, leaking the ephemeral consumer until InactiveThreshold).
func (c *recordCursor) fetchOne(ctx context.Context, sub *nats.Subscription) (*nats.Msg, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, replayFetchTimeout)
	defer cancel()

	msgs, err := sub.Fetch(1, nats.Context(fetchCtx))
	if err != nil {
		if errors.Is(err, nats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
			return nil, nil
		}
		return nil, &ReplayReadError{Stream: c.stream, Cause: err}
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	return msgs[0], nil
}

// decode turns a delivered backlog message into the correct JournalRecord variant. It
// first resolves the record's authoritative bytes (inline body, or the rehydrated +
// integrity-verified object for an offloaded record), then dispatches on the message
// SUBJECT — the same routing the writer used to place the record: an event subject decodes
// an event into an EventRecord; the command subject decodes a command into a CommandRecord;
// the fence subject decodes a LeaseFence into a FenceRecord. A subject that does not parse
// fails closed as a *ReplayDecodeError rather than being silently classified — fail-secure
// at the untrusted-restore boundary.
func (c *recordCursor) decode(ctx context.Context, msg *nats.Msg, seq uint64) (JournalRecord, error) {
	kind, sid, lid, err := ParseSubject(msg.Subject)
	if err != nil {
		return nil, &ReplayDecodeError{Sequence: seq, Cause: err}
	}

	data, err := c.resolveBytes(ctx, msg, seq)
	if err != nil {
		return nil, err
	}

	switch kind {
	case SubjectSessionEvent, SubjectLoopEvent:
		ev, err := event.UnmarshalEvent(data)
		if err != nil {
			return nil, &ReplayDecodeError{Sequence: seq, Cause: err}
		}
		return NewEventRecord(ev), nil
	case SubjectLoopCommand:
		cmd, err := command.UnmarshalCommand(data)
		if err != nil {
			return nil, &ReplayDecodeError{Sequence: seq, Cause: err}
		}
		// The dispatch target (sid, lid) is recovered from the command's own subject —
		// the writer encoded it there since a command may not carry its own routing.
		return NewCommandRecord(sid, lid, cmd), nil
	case SubjectFence:
		fence, err := UnmarshalLeaseFence(data)
		if err != nil {
			return nil, &ReplayDecodeError{Sequence: seq, Cause: err}
		}
		return NewFenceRecord(sid, fence), nil
	default:
		// ParseSubject only returns the four kinds above; this is the unreachable
		// fail-secure arm (a future kind must add an explicit case rather than slipping
		// through as a zero-valued record).
		return nil, &ReplayDecodeError{Sequence: seq, Cause: &SubjectParseError{Subject: msg.Subject, Reason: "unhandled subject kind " + kind.String()}}
	}
}

// resolveBytes returns the authoritative payload bytes for a delivered message: an inline
// record (no objectIDHeader) yields its body verbatim; an offloaded record parses its
// pointer body, gates the CodecVersion, fetches the named object, and RE-VERIFIES the
// fetched bytes hash to the pointer's content-addressed id before returning them. Every
// failure is a typed fail-secure error carrying the record's stream sequence. This mirrors
// streamCursor.decode/rehydrate but stops at the verified bytes (the kind-specific decode
// is decode's job), so any record kind — event, command, or fence — rehydrates uniformly.
func (c *recordCursor) resolveBytes(ctx context.Context, msg *nats.Msg, seq uint64) ([]byte, error) {
	if msg.Header.Get(objectIDHeader) == "" {
		// Inline: the body is the marshaled record verbatim.
		return msg.Data, nil
	}

	ptr, err := unmarshalPointer(msg.Data)
	if err != nil {
		return nil, &ReplayDecodeError{Sequence: seq, Cause: err}
	}
	if ptr.CodecVersion != pointerCodecVersion {
		return nil, &ObjectCodecVersionError{
			Sequence: seq, ObjectID: ptr.ObjectID, Got: ptr.CodecVersion, Want: pointerCodecVersion,
		}
	}

	// The object fetch carries the caller's context (bounded I/O): nats.Context satisfies
	// GetObjectOpt, so the fetch is cancelled/deadlined with Next's ctx.
	bytes, err := c.objects.GetBytes(ptr.ObjectID, nats.Context(ctx))
	if err != nil {
		if errors.Is(err, nats.ErrObjectNotFound) {
			return nil, &ObjectMissingError{Sequence: seq, ObjectID: ptr.ObjectID, Cause: err}
		}
		return nil, &ReplayReadError{Stream: c.stream, Cause: err}
	}

	// Semantic integrity check: the fetched bytes must hash to the id the pointer named.
	sum := sha256.Sum256(bytes)
	gotHash := hex.EncodeToString(sum[:])
	if gotHash != ptr.ObjectID {
		return nil, &ObjectCorruptError{Sequence: seq, ObjectID: ptr.ObjectID, GotHash: gotHash}
	}
	return bytes, nil
}

// Close tears down the consumer. It is idempotent: a second call is a no-op. Unsubscribe
// deletes the ephemeral consumer eagerly (the InactiveThreshold is only the abandoned-
// cursor backstop). A nil subscription (defensive) closes cleanly.
func (c *recordCursor) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.sub == nil {
		return nil
	}
	if err := c.sub.Unsubscribe(); err != nil {
		return &ReplayReadError{Stream: c.stream, Cause: err}
	}
	return nil
}
