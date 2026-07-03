package journal

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/looprig/harness/pkg/uuid"
	"github.com/nats-io/nats.go"
)

// streamSetupTimeout bounds the stream create/bind round-trips in NewSessionJournal
// (AddStream + StreamInfo). Construction must not block forever on an unreachable
// or wedged JetStream server; the constructor fails closed with a typed error past
// this deadline.
const streamSetupTimeout = 10 * time.Second

// dedupWindow is the JetStream message-deduplication window: the span over which the
// server remembers a Nats-Msg-Id and silently drops a republish of the same id. The
// journal sets it explicitly (rather than inheriting the server default) because the
// keep-everything contract depends on it: it backs Task 4.5's ambiguous-ack retry,
// where an Append whose ack was lost is re-published under the same IdempotencyID and
// must dedup against the record the server already committed. A window too short would
// turn a benign retry into a duplicate durable record.
const dedupWindow = 2 * time.Minute

// MarshalRecordError wraps a failure to encode a record's payload before publish.
// It names the destination subject so a caller can correlate the failure to the
// record without re-inspecting the payload, and unwraps to the underlying codec
// error (an *event.EphemeralNotPersistableError, *command.UnknownCommandTypeError,
// a *FenceEncodeError, etc.) for errors.As inspection.
type MarshalRecordError struct {
	Subject string
	Cause   error
}

func (e *MarshalRecordError) Error() string {
	return "journal: marshal record for " + strconv.Quote(e.Subject) + ": " + e.Cause.Error()
}
func (e *MarshalRecordError) Unwrap() error { return e.Cause }

// RecordKindError reports a JournalRecord whose concrete type is outside the sealed
// sum the serializer encodes. It is unreachable for an in-package record (the sum is
// sealed by the unexported marker); it exists so marshalRecord's default arm fails
// closed with a typed error rather than panicking.
type RecordKindError struct {
	Subject string
}

func (e *RecordKindError) Error() string {
	return "journal: unknown record kind for subject " + strconv.Quote(e.Subject)
}

// AppendError wraps a failure to publish a record to the session stream. It carries
// the destination subject, the record's Nats-Msg-Id, and the expected-last-sequence
// fence the publish was attempted under, and unwraps to the underlying NATS error
// (a context deadline, a Nats-Expected-Last-Sequence rejection, a transport error).
// The fence stays unadvanced when this is returned, so the next Append re-fences on
// the same tip — the seam Task 4.5 builds ambiguous-ack resolution onto.
type AppendError struct {
	Subject  string
	MsgID    string
	Expected uint64
	Cause    error
}

func (e *AppendError) Error() string {
	return "journal: append to " + strconv.Quote(e.Subject) +
		" (msg-id " + strconv.Quote(e.MsgID) +
		", expected-seq " + strconv.FormatUint(e.Expected, 10) + "): " + e.Cause.Error()
}
func (e *AppendError) Unwrap() error { return e.Cause }

// FenceViolationError reports that the single-writer invariant is broken: after an
// ambiguous append (a lost ack) the serializer resolved the tip at sequence N+1 and
// found a record that is NOT the one it was publishing — a foreign Nats-Msg-Id at the
// sequence we were fencing on. This is unrecoverable at the journal layer: another
// writer advanced the stream, so the durable log can no longer be extended safely from
// here. It carries the contested sequence, the id we expected (ours), and the id we
// found (the intruder's) so a caller can audit exactly which record collided.
//
// The hub maps this to a SessionPersistenceFault (reject new commands / NewLoop, wake
// WaitIdle waiters). The journal layer must not reference hub types; it surfaces this
// typed error and the hub/composition root translates it.
type FenceViolationError struct {
	Subject   string
	Sequence  uint64 // the contested stream sequence (the expected N+1)
	WantMsgID string // our record's Nats-Msg-Id (the one we were publishing)
	GotMsgID  string // the Nats-Msg-Id actually stored at Sequence (the intruder's)
}

func (e *FenceViolationError) Error() string {
	return "journal: fence violation at seq " + strconv.FormatUint(e.Sequence, 10) +
		" on " + strconv.Quote(e.Subject) +
		": stored msg-id " + strconv.Quote(e.GotMsgID) +
		" is not ours " + strconv.Quote(e.WantMsgID) + " (single-writer invariant broken)"
}

// AmbiguousAckError reports an append whose ack was lost and could NOT be resolved
// within the bounded retry: the publish timed out, the retry to resolve it also timed
// out (or otherwise stayed ambiguous), so the serializer cannot tell whether the
// record landed. The fence stays unadvanced, so the next Append re-fences on the same
// tip; the caller decides whether to fail the session or retry later. It carries the
// subject, the record's Nats-Msg-Id, the expected sequence the publish fenced on, and
// the underlying ambiguous cause (a deadline / timeout / no-response).
type AmbiguousAckError struct {
	Subject  string
	MsgID    string
	Expected uint64
	Cause    error
}

func (e *AmbiguousAckError) Error() string {
	return "journal: ambiguous ack (unresolved) for " + strconv.Quote(e.Subject) +
		" (msg-id " + strconv.Quote(e.MsgID) +
		", expected-seq " + strconv.FormatUint(e.Expected, 10) + "): " + e.Cause.Error()
}
func (e *AmbiguousAckError) Unwrap() error { return e.Cause }

// ResolveReadError reports that the ambiguous-ack resolver could not read the tip
// record (GetMsg at the contested sequence failed for a reason other than
// not-found): without that read it cannot decide commit-vs-conflict, so it fails
// closed rather than guessing. It carries the sequence it tried to read and the
// underlying read error.
type ResolveReadError struct {
	Subject  string
	Sequence uint64
	Cause    error
}

func (e *ResolveReadError) Error() string {
	return "journal: resolve read of seq " + strconv.FormatUint(e.Sequence, 10) +
		" on " + strconv.Quote(e.Subject) + ": " + e.Cause.Error()
}
func (e *ResolveReadError) Unwrap() error { return e.Cause }

// setupPhase names the stream-management call in NewSessionJournal that failed. It
// is a closed typed enum (not a free-form string) so callers can switch on the phase
// and a typo cannot silently mislabel a failure.
type setupPhase string

const (
	// PhaseAdd is the AddStream create/already-in-use call in ensureStream.
	PhaseAdd setupPhase = "add"
	// PhaseInfo is the StreamInfo tip read that initializes the fence.
	PhaseInfo setupPhase = "info"
	// PhaseVerify is the existing-stream config verification in ensureStream: an
	// already-provisioned stream whose config diverges from the durability contract
	// fails here rather than being bound as-is.
	PhaseVerify setupPhase = "verify"
)

// StreamSetupError wraps a failure to create, bind, verify, or read the per-session
// stream in NewSessionJournal. It carries the stream name and a typed phase tag (the
// management call that failed) and unwraps to the underlying NATS error so a caller
// can errors.As both this and the wrapped cause.
type StreamSetupError struct {
	Stream string
	Phase  setupPhase
	Cause  error
}

func (e *StreamSetupError) Error() string {
	return "journal: stream setup (" + string(e.Phase) + ") for " + strconv.Quote(e.Stream) + ": " + e.Cause.Error()
}
func (e *StreamSetupError) Unwrap() error { return e.Cause }

// JournalNotReadyError reports an Append attempted before the journal's opening
// LeaseFence was acknowledged. NewSessionJournal writes the LeaseFence as the
// journal's first append and only marks the journal ready once it lands; an Append
// before that (which can only happen if a caller drove the concrete writer directly)
// fails closed with this typed error rather than racing the fence. It carries the
// session so a caller can correlate the failure.
type JournalNotReadyError struct {
	SessionID uuid.UUID
}

func (e *JournalNotReadyError) Error() string {
	return "journal: session " + e.SessionID.String() + " not ready (opening LeaseFence not yet acknowledged)"
}

// JournalLeaseLostError reports an Append refused because the journal's ownership
// lease was lost — released by the holder or overtaken by a higher epoch. Once the
// lease is gone the journal fails every append fast and never re-fetches or advances
// its expected sequence: a new owner (higher epoch) has written, or will write, its
// own LeaseFence to advance the stream, so this stale journal's expected-sequence
// fence would reject the append at the stream anyway. Failing here is the fast-path
// guard; the stream fence is the hard backstop. It carries the session and the lost
// lease's epoch and unwraps to a *LeaseLostError for errors.As.
type JournalLeaseLostError struct {
	SessionID uuid.UUID
	Epoch     uint64
}

func (e *JournalLeaseLostError) Error() string {
	return "journal: session " + e.SessionID.String() +
		" append refused: lease at epoch " + strconv.FormatUint(e.Epoch, 10) + " lost"
}

func (e *JournalLeaseLostError) Unwrap() error {
	return &LeaseLostError{SessionID: e.SessionID, Epoch: e.Epoch}
}

// errNilLease is the leaf cause when NewSessionJournal is handed a nil Lease. The
// lease is a required dependency (DIP): the composition root acquires it and passes
// it in. A sentinel is permitted (no context fields).
var errNilLease = errors.New("journal: nil lease")

// Option configures a SessionJournal at construction. Options are applied in order
// over a defaults struct, so a later option overrides an earlier one.
type Option func(*journalOptions)

// journalOptions holds the constructor-tunable knobs. It is unexported; callers
// set it through Option functions so the journal owns its invariants (e.g. a
// non-positive append timeout falls back to the default).
type journalOptions struct {
	appendTimeout time.Duration
}

// WithAppendTimeout sets the per-append publish deadline (the bound applied inside
// Append, independent of the caller's context). A non-positive value is ignored and
// the default (defaultAppendTimeout) is kept.
func WithAppendTimeout(d time.Duration) Option {
	return func(o *journalOptions) {
		if d > 0 {
			o.appendTimeout = d
		}
	}
}

// NewSessionJournal binds (creating if absent) the per-session JetStream stream for
// sessionID and returns a single-writer SessionJournal over it, taking ownership of
// the stream by writing the lease's epoch as the journal's FIRST append — a
// LeaseFence on the session's fence subject, fenced like any other append. The stream
// is the keep-everything log for one session: Limits retention, no age/byte discard,
// one replica. Construction is idempotent at the stream level — a second process
// binding the same session's existing stream succeeds and initializes its
// expected-sequence fence from the stream's current tip (LastSeq) — but ownership is
// NOT: the opening LeaseFence advances the stream, so a stale prior owner's next
// append fails the expected-sequence fence. The journal refuses every Append until
// that LeaseFence is acknowledged.
//
// lease is a required dependency (DIP): the composition root (or a test) acquires it
// from a LeaseManager and passes it in; the journal never acquires a lease itself. It
// stamps lease.Epoch() into the LeaseFence and refuses to append once the lease is
// lost (a higher-epoch owner has taken over).
//
// js is a bound JetStream context; the journal never starts a server (that is the
// composition root's job, Phase 10). All management I/O carries a deadline. It
// returns the narrow SessionJournal interface so callers depend on Append alone,
// not the concrete writer.
func NewSessionJournal(js nats.JetStreamContext, sessionID uuid.UUID, lease Lease, opts ...Option) (SessionJournal, error) {
	if js == nil {
		return nil, &StreamSetupError{Stream: StreamName(sessionID), Phase: PhaseAdd, Cause: errNilJetStream}
	}
	if lease == nil {
		return nil, &StreamSetupError{Stream: StreamName(sessionID), Phase: PhaseAdd, Cause: errNilLease}
	}
	o := journalOptions{appendTimeout: defaultAppendTimeout}
	for _, opt := range opts {
		opt(&o)
	}

	name := StreamName(sessionID)
	ctx, cancel := context.WithTimeout(context.Background(), streamSetupTimeout)
	defer cancel()

	if err := ensureStream(ctx, js, sessionID); err != nil {
		return nil, err
	}

	// Bind (creating if absent) the per-session object store the offload path uploads
	// over-threshold records to. Idempotent, mirroring ensureStream: a rebind binds the
	// existing bucket. Done alongside the stream so the journal is ready to offload from
	// the first Append.
	objects, err := ensureObjectStore(js, sessionID)
	if err != nil {
		return nil, err
	}

	// Initialize the fence from the stream's current tip. A fresh stream reports
	// LastSeq 0 (so the first publish fences on 0 and lands at seq 1); an existing
	// stream reports its last durable sequence, so a rebind appends after it.
	info, err := js.StreamInfo(name, nats.Context(ctx))
	if err != nil {
		return nil, &StreamSetupError{Stream: name, Phase: PhaseInfo, Cause: err}
	}

	j := &streamJournal{
		js:            js,
		stream:        name,
		sessionID:     sessionID,
		lease:         lease,
		objects:       objects,
		appendTimeout: o.appendTimeout,
		expectedSeq:   info.State.LastSeq,
	}
	// Default publish seam: the real fenced PublishMsg. Tests swap it via SwapPublish.
	j.publish = j.defaultPublish

	// Take ownership: the FIRST append is the LeaseFence, fenced on the current tip. If
	// a stale prior owner (or a higher-epoch successor) advanced the stream, this fails
	// the expected-sequence fence and construction fails closed — a stale owner cannot
	// even open a journal. Only once this lands is the journal ready to accept Appends.
	if err := j.writeOpeningFence(ctx); err != nil {
		return nil, err
	}
	return j, nil
}

// errNilJetStream is the leaf cause when NewSessionJournal is handed a nil
// JetStream context. It carries no context fields, so a sentinel is permitted.
var errNilJetStream = errors.New("journal: nil JetStream context")

// ensureStream creates the per-session stream, tolerating an already-existing one
// only when its config still honors the durability contract. AddStream on a fresh
// name creates it; on an existing name with an IDENTICAL config JetStream returns
// success; on an existing name with a DIFFERENT config it returns
// ErrStreamNameAlreadyInUse. That already-in-use signal is NOT taken as "bind it as
// is": a divergent stream (e.g. WorkQueue retention, narrower subjects) would silently
// break the keep-everything guarantee. Instead we fetch the live config and verify it
// — binding only a contract-compatible stream and failing closed with a typed verify
// error otherwise. (A merely additive/forward config evolution is still deferred; we
// verify the load-bearing invariants, not byte-equality.)
func ensureStream(ctx context.Context, js nats.JetStreamContext, sessionID uuid.UUID) error {
	name := StreamName(sessionID)
	_, err := js.AddStream(streamConfig(sessionID), nats.Context(ctx))
	if err == nil {
		return nil
	}
	if errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
		// Already provisioned (by us or another binder) with a different config; do
		// not fail open — verify it still satisfies the durability contract.
		return verifyExistingStream(ctx, js, name, sessionID)
	}
	return &StreamSetupError{Stream: name, Phase: PhaseAdd, Cause: err}
}

// verifyExistingStream reads the live config of an already-provisioned stream and
// confirms it still honors the load-bearing durability invariants the journal depends
// on: Limits retention (never WorkQueue/Interest, which discard consumed records) and
// the exact session-rooted subject filter (a narrower filter would silently drop
// subjects of this session). On any divergence it fails closed with a typed verify
// StreamSetupError rather than binding the divergent stream. On match it returns nil
// and the caller binds as-is — config evolution beyond these invariants is deferred.
func verifyExistingStream(ctx context.Context, js nats.JetStreamContext, name string, sessionID uuid.UUID) error {
	info, err := js.StreamInfo(name, nats.Context(ctx))
	if err != nil {
		return &StreamSetupError{Stream: name, Phase: PhaseVerify, Cause: err}
	}
	if info.Config.Retention != nats.LimitsPolicy {
		return &StreamSetupError{Stream: name, Phase: PhaseVerify, Cause: &streamConfigMismatchError{
			Field: "retention", Want: nats.LimitsPolicy.String(), Got: info.Config.Retention.String(),
		}}
	}
	wantSubjects := []string{streamSubjectFilter(sessionID)}
	if !slices.Equal(info.Config.Subjects, wantSubjects) {
		return &StreamSetupError{Stream: name, Phase: PhaseVerify, Cause: &streamConfigMismatchError{
			Field: "subjects",
			Want:  strings.Join(wantSubjects, ","),
			Got:   strings.Join(info.Config.Subjects, ","),
		}}
	}
	return nil
}

// streamConfigMismatchError is the leaf cause when an already-provisioned stream's
// live config diverges from the durability contract on a verified field. It names the
// field and the want/got values so a caller can errors.As it off the verify
// StreamSetupError and report exactly which invariant was violated.
type streamConfigMismatchError struct {
	Field string
	Want  string
	Got   string
}

func (e *streamConfigMismatchError) Error() string {
	return "existing stream " + strconv.Quote(e.Field) + " = " + strconv.Quote(e.Got) +
		", want " + strconv.Quote(e.Want)
}

// streamConfig is the per-session stream's configuration: the keep-everything log.
// Limits retention with unlimited messages/bytes and no max-age means nothing is
// ever discarded; one replica suits the embedded single-node server. The subject
// filter looprig.session.<sid>.> captures every session/loop/command/fence subject for
// this session (and only this session — the sid token isolates streams).
func streamConfig(sessionID uuid.UUID) *nats.StreamConfig {
	return &nats.StreamConfig{
		Name:      StreamName(sessionID),
		Subjects:  []string{streamSubjectFilter(sessionID)},
		Retention: nats.LimitsPolicy,
		// Keep everything: unlimited count/bytes, no age expiry, no per-subject cap.
		MaxMsgs:           -1,
		MaxBytes:          -1,
		MaxAge:            0,
		MaxMsgsPerSubject: -1,
		Replicas:          1,
		// Explicit dedup window (do not inherit the server default): backs 4.5's
		// ambiguous-ack retry, where a lost-ack republish under the same Nats-Msg-Id
		// must dedup against the already-committed record.
		Duplicates: dedupWindow,
		// Inline ceiling (Task 5.1): the hard per-message size cap enforced by the
		// stream as defense-in-depth. The journal already offloads any record over
		// inlineThreshold (512 KiB) to the object store, so a correctly-built inline
		// message or pointer always fits well under this; a stray oversized inline
		// publish is rejected by the server rather than silently truncated/accepted.
		MaxMsgSize: streamInlineCeiling,
		// The SyncInterval power-loss durability knob (design round 5) is a server/FileStore
		// option, not a StreamConfig field — it is set on the embedded server at the
		// composition root (internal/persistence/embedded.go).
	}
}

// streamSubjectFilter returns the wildcard subject the session stream binds:
// "looprig.session.<sid>.>" — every subject under this session's root. The trailing
// '>' captures the session, loop-event, command, and fence leaves uniformly.
func streamSubjectFilter(sessionID uuid.UUID) string {
	return subjectRoot + "." + sessionID.String() + ".>"
}
