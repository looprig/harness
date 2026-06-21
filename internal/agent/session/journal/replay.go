package journal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/nats-io/nats.go"
)

// replayInactiveThreshold is the ephemeral replay consumer's self-cleanup window: if the
// cursor is abandoned without Close (a panicked caller), the server tears the consumer
// down after this idle span so a leaked consumer cannot accumulate on the stream. Close
// deletes it eagerly; this is the backstop.
const replayInactiveThreshold = 5 * time.Minute

// replayFetchTimeout bounds EVERY single backlog Fetch round-trip: each fetch derives a
// child context from the caller's with this timeout, so no individual pull blocks longer
// than this even when the caller passed a long (or no) deadline. A cold backlog read of a
// local embedded stream returns promptly, and the consumer's NumPending==0 signal normally
// ends the drain WITHOUT any fetch reaching this timeout. If a fetch DOES time out while
// the Open-time tip still reports records pending, that is NOT treated as caught-up — it
// is a read anomaly that fails closed as a *ReplayReadError (see Next's empty-fetch
// branch), because EOFing there would silently truncate a restore. Kept short so the
// fail-closed path is reached promptly rather than after a long stall.
const replayFetchTimeout = 2 * time.Second

// errEmptyFetchBacklogRemaining is the leaf cause wrapped in a *ReplayReadError when a
// bounded backlog fetch came back empty even though the Open-time tip says records remain
// (delivered < lastSeq) and no server-authoritative NumPending==0 has been seen — and the
// CALLER context is still live (so this is the internal replayFetchTimeout backstop, not
// the caller's own deadline). It is a read anomaly on a healthy local store: failing
// closed here is mandatory, because returning io.EOF would silently truncate a restore.
var errEmptyFetchBacklogRemaining = errors.New("journal: bounded backlog fetch returned no message while the Open-time tip reports records still pending (no NumPending==0 observed)")

// StartPos is the closed value type naming where a replay begins: the stream beginning
// (every record) or a specific stream sequence (the dormant-snapshot hook). It is a
// value, not an interface, so a caller cannot smuggle a third start mode past the
// switch; the two constructors Beginning and FromSeq are the only ways to build one.
type StartPos struct {
	// fromSeq is 0 for Beginning and the (1-based) inclusive start sequence for FromSeq.
	// JetStream stream sequences are 1-based, so 0 unambiguously means "from the start".
	fromSeq uint64
}

// Beginning starts a replay at the stream's first record (DeliverAll).
func Beginning() StartPos { return StartPos{fromSeq: 0} }

// FromSeq starts a replay at stream sequence seq, inclusive — the dormant-snapshot hook
// (Phase 8 resumes after a snapshot's last applied sequence). A seq of 0 is equivalent
// to Beginning (there is no sequence 0 in a JetStream stream).
func FromSeq(seq uint64) StartPos { return StartPos{fromSeq: seq} }

// ReplayRequest selects which of a session's Enduring events to replay and how. The
// subject filter is derived from SessionID + LoopID: always the session-event subject,
// plus the loop-event subject for LoopID (a single loop) or all loops' event subjects
// (LoopID zero). Command and fence subjects are NEVER included — the replayer decodes
// only events.
type ReplayRequest struct {
	// SessionID is the session whose stream is replayed (required; a zero id yields a
	// setup error rather than a wildcard over every session).
	SessionID uuid.UUID
	// LoopID, when non-zero, narrows the loop-event filter to that single loop; zero
	// replays the session event subject plus every loop's event subject.
	LoopID uuid.UUID
	// From is where the backlog read begins: Beginning or FromSeq(n).
	From StartPos
	// Follow keeps the cursor live after the backlog drains (tailing new appends). Only
	// the cold path (Follow:false) is implemented in Task 5.3; Follow:true returns a
	// typed *FollowUnsupportedError from Open. See the package note on Open.
	Follow bool
}

// EventReplayer is the journal's read side: it opens an ordered cursor over a session's
// Enduring events. It is the narrow counterpart to SessionJournal (the write side) — a
// caller that only reads history depends on Open alone, not on stream-management or the
// object store. The concrete *streamReplayer satisfies it; NewEventReplayer is the
// composition-time constructor.
type EventReplayer interface {
	// Open binds a consumer over the session stream filtered to the request's event
	// subjects and returns a cursor positioned at req.From. The embedded server is NOT
	// started here (that is the composition root's job); js must already be bound.
	Open(ctx context.Context, req ReplayRequest) (EventCursor, error)
}

// EventCursor yields a session's Enduring events in stream-sequence order. Next returns
// the next decoded event with its stream sequence, io.EOF once the backlog is drained
// (cold mode), or a typed error on a malformed/missing/corrupt record. Close releases
// the underlying consumer; it is idempotent and safe to call after an error.
type EventCursor interface {
	// Next returns the next event and its stream sequence, or io.EOF when the cold
	// backlog is exhausted. A decode/object error fails secure: the cursor surfaces the
	// typed error rather than skipping or zero-valuing the record.
	Next(ctx context.Context) (event.Event, uint64, error)
	// Close tears down the consumer. Idempotent: a second call is a no-op.
	Close() error
}

// objectFetcher is the narrow object-store surface the replayer's rehydration path
// depends on (Interface Segregation, mirroring the write-side objectPutter): fetch bytes
// by content-addressed name. The vendored nats.ObjectStore satisfies it; the replayer
// never depends on the full store interface.
type objectFetcher interface {
	GetBytes(name string, opts ...nats.GetObjectOpt) ([]byte, error)
}

// ReplaySetupError reports a failure to bind the replay consumer in Open (a missing
// SessionID, a stream-info read failure, or a PullSubscribe failure). It carries the
// stream name and unwraps to the underlying NATS error so a caller can errors.As both
// this and the cause. It is the read-side analogue of StreamSetupError.
type ReplaySetupError struct {
	Stream string
	Reason string
	Cause  error
}

func (e *ReplaySetupError) Error() string {
	if e.Cause == nil {
		return "journal: replay setup for " + strconv.Quote(e.Stream) + ": " + e.Reason
	}
	return "journal: replay setup for " + strconv.Quote(e.Stream) + ": " + e.Reason + ": " + e.Cause.Error()
}
func (e *ReplaySetupError) Unwrap() error { return e.Cause }

// ReplayReadError reports a failure to read the next backlog message from the consumer
// (a Fetch failure that is not a benign timeout, or a message whose JetStream metadata
// cannot be read). It fails closed: the cursor surfaces it rather than guessing the
// stream position. It carries the stream name and unwraps to the underlying cause.
type ReplayReadError struct {
	Stream string
	Cause  error
}

func (e *ReplayReadError) Error() string {
	return "journal: replay read on " + strconv.Quote(e.Stream) + ": " + e.Cause.Error()
}
func (e *ReplayReadError) Unwrap() error { return e.Cause }

// ReplayDecodeError wraps a failure to decode a replayed record's body into an event:
// an undecodable offload pointer, or an UnmarshalEvent failure on the (inline or
// rehydrated) bytes. It carries the offending record's stream sequence and unwraps to
// the underlying codec error (a *PointerDecodeError, an *event.EventDecodeError, etc.).
type ReplayDecodeError struct {
	Sequence uint64
	Cause    error
}

func (e *ReplayDecodeError) Error() string {
	return "journal: replay decode at seq " + strconv.FormatUint(e.Sequence, 10) + ": " + e.Cause.Error()
}
func (e *ReplayDecodeError) Unwrap() error { return e.Cause }

// ObjectCodecVersionError reports an offload pointer whose CodecVersion is not the one
// this build understands (pointerCodecVersion). It fails closed at the untrusted restore
// boundary rather than misdecoding an object written by a future/unknown codec. Phase-8
// restore maps it (like the missing/corrupt errors) onto a RestoreErrored. It carries
// the record's stream sequence, the pointer's object id, and the version mismatch.
type ObjectCodecVersionError struct {
	Sequence uint64
	ObjectID string
	Got      uint32
	Want     uint32
}

func (e *ObjectCodecVersionError) Error() string {
	return "journal: offload pointer at seq " + strconv.FormatUint(e.Sequence, 10) +
		" (object " + strconv.Quote(e.ObjectID) + ") codec version " + strconv.FormatUint(uint64(e.Got), 10) +
		", want " + strconv.FormatUint(uint64(e.Want), 10)
}

// ObjectMissingError reports an offload pointer whose backing object is absent from the
// per-session bucket: a dangling pointer (the object was deleted or never landed). It
// fails secure — the cursor surfaces it rather than yielding a zero-valued event — so
// Phase-8 restore maps it onto a RestoreErrored rather than silently losing history. It
// carries the record's stream sequence and the missing object id.
type ObjectMissingError struct {
	Sequence uint64
	ObjectID string
	Cause    error
}

func (e *ObjectMissingError) Error() string {
	return "journal: offloaded object " + strconv.Quote(e.ObjectID) +
		" referenced at seq " + strconv.FormatUint(e.Sequence, 10) + " is missing: " + e.Cause.Error()
}
func (e *ObjectMissingError) Unwrap() error { return e.Cause }

// ObjectCorruptError reports an offloaded object whose fetched bytes do not hash to the
// content-addressed id the pointer named: sha256(bytes) != pointer.ObjectID, so the
// object has been corrupted or substituted. It fails secure — the cursor surfaces it
// rather than decoding tampered bytes — so Phase-8 restore maps it onto a RestoreErrored.
// It carries the record's stream sequence, the expected object id (the pointer's), and
// the actual hash of the fetched bytes.
type ObjectCorruptError struct {
	Sequence uint64
	ObjectID string // the id the pointer named (the expected sha256)
	GotHash  string // sha256 of the bytes actually fetched
}

func (e *ObjectCorruptError) Error() string {
	return "journal: offloaded object referenced at seq " + strconv.FormatUint(e.Sequence, 10) +
		" is corrupt: fetched bytes hash " + strconv.Quote(e.GotHash) +
		", pointer names " + strconv.Quote(e.ObjectID)
}

// FollowUnsupportedError is returned by Open when ReplayRequest.Follow is true. The live
// tailing path is deferred (Task 5.3 implements only the cold Follow:false backlog read);
// failing closed with a typed error is preferable to silently behaving as a cold cursor
// that EOFs at the current tip. The composition root (Phase 10 TUI) will revisit this.
type FollowUnsupportedError struct {
	Stream string
}

func (e *FollowUnsupportedError) Error() string {
	return "journal: live follow (Follow:true) is not yet implemented for " + strconv.Quote(e.Stream)
}

// streamReplayer is the concrete EventReplayer over one JetStream context and one
// session's object store. It holds no per-replay state: every Open builds an independent
// consumer + cursor, so concurrent replays of the same (or different) sessions do not
// interfere. js is the bound context (never started here); objects is the per-session
// bucket offloaded records rehydrate from.
type streamReplayer struct {
	js      nats.JetStreamContext
	objects objectFetcher
}

// NewEventReplayer wires a read-side replayer over a bound JetStream context and the
// session's object store, mirroring how NewSessionJournal is wired at the composition
// root (the embedded server is started elsewhere). It returns the narrow EventReplayer
// interface so callers depend on Open alone, not the concrete reader. objects is the
// per-session bucket (SessionObjectBucket) the write side offloaded over-threshold
// records to; the replayer rehydrates from it.
func NewEventReplayer(js nats.JetStreamContext, objects nats.ObjectStore) EventReplayer {
	return &streamReplayer{js: js, objects: objects}
}

// Open binds an ephemeral pull consumer over the session stream filtered to the request's
// event subjects (session-event plus the selected loop-event subject(s) — never cmd/
// fence) and returns a cursor positioned at req.From. The snapshot of the stream tip
// (LastSeq) is captured here so the cold cursor knows when the backlog is drained: Next
// returns io.EOF once it has delivered the record at that sequence (or immediately for an
// empty/no-matching stream).
//
// A single consumer with a multi-subject FilterSubjects delivers messages in STREAM-
// SEQUENCE order regardless of how many subjects it filters — it walks the stream's
// monotonic sequence emitting only matching records. Because the journal is a single
// fenced writer producing a gap-free monotonic sequence, walking this one consumer is
// exactly walking session order across the session-event and loop-event subjects.
func (r *streamReplayer) Open(ctx context.Context, req ReplayRequest) (EventCursor, error) {
	if req.SessionID.IsZero() {
		return nil, &ReplaySetupError{Stream: "", Reason: "zero session id"}
	}
	stream := StreamName(req.SessionID)
	if req.Follow {
		// Cold-only in Task 5.3: fail closed rather than behave as a cold cursor.
		return nil, &FollowUnsupportedError{Stream: stream}
	}

	// Snapshot the tip BEFORE subscribing so the cold cursor's caught-up boundary is the
	// durable length at Open time (later appends are out of scope for a cold replay).
	info, err := r.js.StreamInfo(stream, nats.Context(ctx))
	if err != nil {
		return nil, &ReplaySetupError{Stream: stream, Reason: "stream info", Cause: err}
	}
	lastSeq := info.State.LastSeq

	filters := eventSubjectFilters(req.SessionID, req.LoopID)

	// Build the start-policy + multi-subject-filter sub options. An ephemeral pull
	// consumer (no durable name) with AckNone — this is a read-only replay, acks would
	// only add round-trips — and an inactivity threshold so an abandoned cursor self-
	// cleans. ConsumerFilterSubjects requires BindStream + an empty subject.
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

	// The raw stream tip (lastSeq) counts EVERY record — including the command and
	// LeaseFence records on non-event subjects this consumer filters out. So lastSeq
	// alone cannot tell "backlog drained" from "the tip is a non-event record we will
	// never deliver": a session whose only record is the opening LeaseFence has
	// lastSeq>=1 yet ZERO matching events. The consumer's NumPending is the
	// filter-aware count of matching records pending right now; capture it so an
	// empty-for-this-filter backlog EOFs cleanly instead of tripping the
	// "tip-says-pending but fetch-came-back-empty" anomaly guard.
	matchPending, err := consumerNumPending(sub)
	if err != nil {
		return nil, &ReplaySetupError{Stream: stream, Reason: "consumer info", Cause: err}
	}

	return &streamCursor{
		stream:       stream,
		objects:      r.objects,
		sub:          sub,
		lastSeq:      lastSeq,
		fromSeq:      req.From.fromSeq,
		delivered:    0,
		emptyBacklog: matchPending == 0,
	}, nil
}

// consumerNumPending reads the just-created consumer's count of matching records still
// pending delivery — the filter-aware backlog size (events only; cmd/fence subjects are
// excluded by the consumer's filter). It fails closed with the underlying error so Open
// can surface a setup failure rather than guessing the backlog is empty.
func consumerNumPending(sub *nats.Subscription) (uint64, error) {
	ci, err := sub.ConsumerInfo()
	if err != nil {
		return 0, err
	}
	return ci.NumPending, nil
}

// eventSubjectFilters builds the consumer's subject filter set: always the session-event
// subject, plus the loop-event subject for a specific loopID, or the all-loops wildcard
// (...loop.*.event) when loopID is zero. Command and fence subjects are deliberately
// excluded — the replayer decodes events only. The subjects are built via the subject
// builders (never hand-rolled), so the filter cannot drift from what the writer emits.
func eventSubjectFilters(sessionID, loopID uuid.UUID) []string {
	session := SessionEventSubject(sessionID)
	if loopID.IsZero() {
		// All loops' event subjects: ...loop.*.event. Built from the loop-event subject of
		// a sentinel loop id then wildcarding the loop token — but to avoid hand-rolling,
		// derive the all-loops form from the constant leaves directly.
		return []string{session, allLoopsEventSubject(sessionID)}
	}
	return []string{session, LoopEventSubject(sessionID, loopID)}
}

// streamCursor is the concrete EventCursor over one ephemeral pull consumer. It fetches
// one backlog message per Next, decodes inline-or-offloaded bodies, and reports io.EOF
// once it has delivered the record at the tip snapshot (lastSeq). It is not safe for
// concurrent Next calls — a cursor is a single-reader handle — but mu guards Close
// against a racing Next so teardown is safe.
type streamCursor struct {
	stream  string
	objects objectFetcher

	mu       sync.Mutex
	sub      *nats.Subscription
	closed   bool
	caughtUp bool

	// lastSeq is the stream tip captured at Open. It bounds the cold replay to the
	// durable length at Open time: a record appended AFTER Open (beyond lastSeq) is not
	// delivered, so a slow drain cannot tail an ever-growing stream. The primary drain-
	// done signal is caughtUp (set when a delivered message reports NumPending==0); this
	// is the secondary guard.
	lastSeq uint64
	// fromSeq is the inclusive start sequence (0 for Beginning); used only to detect an
	// empty-backlog open (start past the tip) so Next EOFs immediately.
	fromSeq uint64
	// delivered is the highest stream sequence handed to the caller so far. Once it
	// reaches lastSeq the cold backlog is fully drained.
	delivered uint64
	// emptyBacklog is true when the consumer reported NumPending==0 at Open: no record
	// matches this cursor's event filter, so the first Next EOFs immediately without a
	// fetch. This is the filter-aware emptiness signal that the raw stream tip (lastSeq)
	// cannot give — the tip counts cmd/fence records the consumer never delivers.
	emptyBacklog bool
}

// Next fetches and decodes the next backlog event. It returns io.EOF when the cold
// backlog is exhausted. The primary drain-done signal is the consumer's NumPending: once
// a delivered message reports zero remaining matching records, the next Next returns EOF
// without a fetch. The Open-time tip (lastSeq) is the secondary bound — a record appended
// after Open (beyond lastSeq) is never delivered. Decode/object failures fail secure as
// typed errors; they do NOT advance the cursor past the offending record.
func (c *streamCursor) Next(ctx context.Context) (event.Event, uint64, error) {
	c.mu.Lock()
	if c.closed || c.caughtUp {
		c.mu.Unlock()
		return nil, 0, io.EOF
	}
	sub := c.sub
	// Empty stream, no record matching this filter (consumer NumPending==0 at Open —
	// e.g. the only record is the opening LeaseFence on a non-event subject), started
	// past the tip, or already delivered the tip record: nothing left to deliver in cold
	// mode.
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
		// Empty fetch while the tip said backlog remains (delivered < lastSeq) and no
		// NumPending==0 has ever been seen — so we are provably NOT caught up. EOF here
		// would silently truncate a restore. Fail closed instead.
		//
		// The empty fetch has two indistinguishable-at-the-Fetch-layer causes; we
		// disambiguate by inspecting the CALLER context first:
		//   1. the caller's own ctx deadline/cancellation expired mid-drain — surface it
		//      so the caller learns its budget ran out (the backlog is NOT drained);
		//   2. only the internal replayFetchTimeout backstop fired while the caller ctx is
		//      still live — a read anomaly on a healthy local store (the tip says there is
		//      more, yet a bounded fetch came back empty). Either way: a *ReplayReadError,
		//      never io.EOF.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, &ReplayReadError{Stream: c.stream, Cause: ctxErr}
		}
		return nil, 0, &ReplayReadError{Stream: c.stream, Cause: errEmptyFetchBacklogRemaining}
	}

	seq, pending, err := messageMeta(msg, c.stream)
	if err != nil {
		return nil, 0, err
	}

	ev, err := c.decode(ctx, msg, seq)
	if err != nil {
		return nil, 0, err
	}

	c.mu.Lock()
	c.delivered = seq
	// NumPending counts the matching records still queued AFTER this one; zero means this
	// was the last matching record, so the next Next can EOF without a (slow) empty fetch.
	if pending == 0 {
		c.caughtUp = true
	}
	c.mu.Unlock()
	return ev, seq, nil
}

// fetchOne pulls a single backlog message, ALWAYS bounding the wait by the internal
// replayFetchTimeout backstop derived from the caller's context (so no single fetch
// blocks unboundedly even when the caller passed a long or no deadline). An empty fetch
// (a timeout, or a zero-length batch) returns (nil, nil) — the "no message this round"
// signal that Next disambiguates against the tip and the caller context; any OTHER fetch
// failure fails closed as a *ReplayReadError.
//
// Returning (nil, nil) is deliberately NOT a "caught up" verdict: the timeout has two
// indistinguishable causes here (the caller's own deadline expiring, or the internal
// backstop firing on a slow store), and only Next — which knows delivered vs. lastSeq and
// can read the caller ctx's Err() — is positioned to decide whether an empty fetch means
// drained (EOF) or a read anomaly (error). fetchOne stays a dumb single-pull primitive.
func (c *streamCursor) fetchOne(ctx context.Context, sub *nats.Subscription) (*nats.Msg, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, replayFetchTimeout)
	defer cancel()

	msgs, err := sub.Fetch(1, nats.Context(fetchCtx))
	if err != nil {
		// A timeout (either the backstop or the caller deadline propagated through fetchCtx)
		// means no message arrived this round. Report it as an empty fetch (nil, nil) and let
		// Next decide; any non-timeout failure fails closed.
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

// decode turns a delivered backlog message into an event. An inline record (no
// objectIDHeader) decodes its body directly via UnmarshalEvent. An offloaded record
// parses its pointer body, gates the codec version, fetches the object, RE-VERIFIES the
// fetched bytes hash to the pointer's id, then decodes the rehydrated bytes. Every
// failure is a typed fail-secure error carrying the record's stream sequence.
func (c *streamCursor) decode(ctx context.Context, msg *nats.Msg, seq uint64) (event.Event, error) {
	if msg.Header.Get(objectIDHeader) == "" {
		// Inline: the body is the marshaled event verbatim.
		ev, err := event.UnmarshalEvent(msg.Data)
		if err != nil {
			return nil, &ReplayDecodeError{Sequence: seq, Cause: err}
		}
		return ev, nil
	}
	return c.rehydrate(ctx, msg, seq)
}

// rehydrate resolves an offloaded record: parse the pointer body, gate its CodecVersion
// against pointerCodecVersion (fail closed on an unknown/future version), fetch the named
// object, re-hash the fetched bytes against the pointer's content-addressed id (fail
// secure on a mismatch), then decode the verified bytes. A missing object →
// *ObjectMissingError; a hash mismatch → *ObjectCorruptError; a version mismatch →
// *ObjectCodecVersionError; an undecodable pointer or a decode failure on the verified
// bytes → *ReplayDecodeError. The header object id is advisory; the BODY pointer is
// authoritative (it is what reconcileTip compares on the write side).
func (c *streamCursor) rehydrate(ctx context.Context, msg *nats.Msg, seq uint64) (event.Event, error) {
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

	ev, err := event.UnmarshalEvent(bytes)
	if err != nil {
		return nil, &ReplayDecodeError{Sequence: seq, Cause: err}
	}
	return ev, nil
}

// Close tears down the consumer. It is idempotent: a second call is a no-op. Unsubscribe
// deletes the ephemeral consumer eagerly (the InactiveThreshold is only the abandoned-
// cursor backstop). A nil subscription (defensive) closes cleanly.
func (c *streamCursor) Close() error {
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

// messageMeta reads a delivered message's stream sequence and the consumer's NumPending
// (matching records still queued after this one) from its JetStream metadata. A message
// without parseable metadata fails closed as a *ReplayReadError rather than being yielded
// with a guessed sequence.
func messageMeta(msg *nats.Msg, stream string) (seq, pending uint64, err error) {
	meta, err := msg.Metadata()
	if err != nil {
		return 0, 0, &ReplayReadError{Stream: stream, Cause: err}
	}
	return meta.Sequence.Stream, meta.NumPending, nil
}
