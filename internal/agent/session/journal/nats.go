package journal

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/inventivepotter/urvi/internal/uuid"
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
// sessionID and returns a single-writer SessionJournal over it. The stream is the
// keep-everything log for one session: Limits retention, no age/byte discard, one
// replica. Construction is idempotent — a second process binding the same session's
// existing stream succeeds and initializes its expected-sequence fence from the
// stream's current tip (LastSeq), so it appends after the last durable record
// rather than re-fencing from zero.
//
// js is a bound JetStream context; the journal never starts a server (that is the
// composition root's job, Phase 10). All management I/O carries a deadline. It
// returns the narrow SessionJournal interface so callers depend on Append alone,
// not the concrete writer.
func NewSessionJournal(js nats.JetStreamContext, sessionID uuid.UUID, opts ...Option) (SessionJournal, error) {
	if js == nil {
		return nil, &StreamSetupError{Stream: StreamName(sessionID), Phase: PhaseAdd, Cause: errNilJetStream}
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

	// Initialize the fence from the stream's current tip. A fresh stream reports
	// LastSeq 0 (so the first publish fences on 0 and lands at seq 1); an existing
	// stream reports its last durable sequence, so a rebind appends after it.
	info, err := js.StreamInfo(name, nats.Context(ctx))
	if err != nil {
		return nil, &StreamSetupError{Stream: name, Phase: PhaseInfo, Cause: err}
	}

	return &streamJournal{
		js:            js,
		stream:        name,
		appendTimeout: o.appendTimeout,
		expectedSeq:   info.State.LastSeq,
	}, nil
}

// errNilJetStream is the leaf cause when NewSessionJournal is handed a nil
// JetStream context. It carries no context fields, so a sentinel is permitted.
var errNilJetStream = errors.New("journal: nil JetStream context")

// ensureStream creates the per-session stream, tolerating an already-existing one.
// AddStream on a fresh name creates it; on an existing name with an identical config
// JetStream returns success, and on an existing name with a DIFFERENT config it
// returns ErrStreamNameAlreadyInUse — which we treat as "already provisioned, bind
// it" rather than an error, because a rebind must not clobber a live stream's config
// (config evolution is a deliberate later concern, not a constructor side effect).
func ensureStream(ctx context.Context, js nats.JetStreamContext, sessionID uuid.UUID) error {
	name := StreamName(sessionID)
	_, err := js.AddStream(streamConfig(sessionID), nats.Context(ctx))
	if err == nil {
		return nil
	}
	if errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
		// Already provisioned by us or another binder; bind to it as-is. The
		// follow-on StreamInfo read in NewSessionJournal confirms it is reachable.
		return nil
	}
	return &StreamSetupError{Stream: name, Phase: PhaseAdd, Cause: err}
}

// streamConfig is the per-session stream's configuration: the keep-everything log.
// Limits retention with unlimited messages/bytes and no max-age means nothing is
// ever discarded; one replica suits the embedded single-node server. The subject
// filter urvi.session.<sid>.> captures every session/loop/command/fence subject for
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
		// TODO(Phase 5): size policy — MaxMsgSize / server max_payload / object-store
		// offload for oversized records is deferred; defaults stand for now.
		// TODO(Phase 10): set the embedded server's SyncInterval (power-loss durability
		// knob, design round 5) at composition root — it is a server/FileStore option set
		// when the embedded server is created in cmd/cli, not a StreamConfig field.
	}
}

// streamSubjectFilter returns the wildcard subject the session stream binds:
// "urvi.session.<sid>.>" — every subject under this session's root. The trailing
// '>' captures the session, loop-event, command, and fence leaves uniformly.
func streamSubjectFilter(sessionID uuid.UUID) string {
	return subjectRoot + "." + sessionID.String() + ".>"
}
