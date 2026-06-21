package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/nats-io/nats.go"
)

// Lease is the single-writer ownership token for one session's durable stream. A
// SessionJournal depends on it (DIP): the composition root acquires a Lease via a
// LeaseManager and passes it in; the journal stamps the lease's Epoch into its first
// LeaseFence and refuses to append once the lease is lost. The holder (the
// composition root), not the journal, calls Release — the journal only reads Epoch
// and the validity/loss signals (see ownershipToken, the narrower view it depends on).
type Lease interface {
	ownershipToken
	// SessionID is the session this lease grants single-writer ownership of.
	SessionID() uuid.UUID
	// Release relinquishes the lease: stops the heartbeat, marks it no longer held
	// (firing Lost), and best-effort clears the entry so a successor can re-acquire
	// without waiting out the TTL. Idempotent.
	Release(ctx context.Context) error
}

// ownershipToken is the narrow view the journal depends on (interface segregation):
// the fencing epoch plus the validity/loss signals it needs to gate appends. It does
// NOT expose Release — the journal never controls the lease lifecycle.
type ownershipToken interface {
	// Epoch is the monotonically increasing fencing epoch this lease holds. A higher
	// epoch always out-ranks a lower one; the journal stamps it into its LeaseFence.
	Epoch() uint64
	// Valid reports whether the lease is still held (not released, not lost to a
	// higher-epoch takeover). A journal must refuse to append once this is false.
	Valid() bool
	// Lost returns a channel closed when the lease is lost — released by the holder,
	// or overtaken by a higher epoch detected on a heartbeat renewal. It never carries
	// a value; select on it to react to loss.
	Lost() <-chan struct{}
}

// defaultLeaseBucket is the JetStream KV bucket holding one entry per session,
// keyed by session id. Single-node embedded server: one replica.
const defaultLeaseBucket = "urvi_session_leases"

// defaultLeaseTTL is the lease validity window: an entry whose ExpiresAt is older
// than now (per the injected clock) is treated as expired and eligible for CAS
// takeover. The holder renews ExpiresAt on a heartbeat at a fraction of this.
const defaultLeaseTTL = 30 * time.Second

// leaseRecord is the JSON value stored in the KV entry for a session. Epoch is the
// monotonic fencing epoch; Holder is the unique acquirer id (so a holder can tell its
// own entry from a successor's); ExpiresAt is the wall-clock instant after which the
// lease is considered expired and may be taken over by CAS. Application-level expiry
// (this field) is the authoritative, clock-injectable check; the bucket TTL is only a
// coarse backstop for a truly dead holder.
type leaseRecord struct {
	Epoch     uint64    `json:"epoch"`
	Holder    string    `json:"holder"`
	ExpiresAt time.Time `json:"expires_at"`
}

// LeaseSetupError wraps a failure to provision or bind the lease KV bucket in
// NewLeaseManager. It carries the bucket name and unwraps to the underlying NATS
// error so a caller can errors.As both this and the wrapped cause.
type LeaseSetupError struct {
	Bucket string
	Cause  error
}

func (e *LeaseSetupError) Error() string {
	return "journal: lease bucket setup for " + strconv.Quote(e.Bucket) + ": " + e.Cause.Error()
}
func (e *LeaseSetupError) Unwrap() error { return e.Cause }

// LeaseHeldError reports that Acquire lost the single-holder race: the session's
// lease is currently held by a live (unexpired) holder, or a concurrent acquirer won
// the CAS. It carries the session and the epoch currently fenced into the bucket so a
// caller can log who holds it. It is the expected, non-fatal "someone else owns this
// session" outcome — the loser must not write to the stream.
type LeaseHeldError struct {
	SessionID uuid.UUID
	Epoch     uint64
}

func (e *LeaseHeldError) Error() string {
	return "journal: session " + e.SessionID.String() +
		" lease held at epoch " + strconv.FormatUint(e.Epoch, 10)
}

// LeaseLostError reports an operation attempted on a lease that is no longer held:
// it was released, or a higher-epoch holder took over (detected on a heartbeat
// renewal). It carries the session and the lease's epoch. The journal returns it
// (wrapped) when an Append is attempted after the lease is lost.
type LeaseLostError struct {
	SessionID uuid.UUID
	Epoch     uint64
}

func (e *LeaseLostError) Error() string {
	return "journal: session " + e.SessionID.String() +
		" lease at epoch " + strconv.FormatUint(e.Epoch, 10) + " lost"
}

// LeaseReadError wraps a failure to read or decode the lease entry during Acquire or
// a heartbeat renewal (a KV Get/Update error that is neither not-found nor a CAS
// conflict, or a malformed stored record). It fails closed: an ambiguous read never
// silently grants ownership.
type LeaseReadError struct {
	SessionID uuid.UUID
	Cause     error
}

func (e *LeaseReadError) Error() string {
	return "journal: read lease for session " + e.SessionID.String() + ": " + e.Cause.Error()
}
func (e *LeaseReadError) Unwrap() error { return e.Cause }

// LeaseClock is the time seam for the lease manager: it mints ExpiresAt and decides
// whether a stored entry is expired. Injecting it makes TTL-expiry deterministic in
// tests (advance the clock past ExpiresAt). It mirrors event.Clock.
type LeaseClock func() time.Time

// LeaseOption configures a LeaseManager at construction. Applied in order over a
// defaults struct, so a later option overrides an earlier one.
type LeaseOption func(*leaseOptions)

type leaseOptions struct {
	bucket string
	ttl    time.Duration
	now    LeaseClock
}

// WithLeaseBucket overrides the KV bucket name. An empty name is ignored (the
// default is kept), so the manager owns its invariant.
func WithLeaseBucket(name string) LeaseOption {
	return func(o *leaseOptions) {
		if name != "" {
			o.bucket = name
		}
	}
}

// WithLeaseTTL sets the lease validity window. A non-positive value is ignored and
// the default is kept.
func WithLeaseTTL(d time.Duration) LeaseOption {
	return func(o *leaseOptions) {
		if d > 0 {
			o.ttl = d
		}
	}
}

// WithLeaseClock injects the clock the manager mints ExpiresAt from and checks
// expiry against. A nil clock is ignored (time.Now is kept).
func WithLeaseClock(now LeaseClock) LeaseOption {
	return func(o *leaseOptions) {
		if now != nil {
			o.now = now
		}
	}
}

// LeaseManager hands out single-writer Leases over a JetStream KV bucket, one entry
// per session keyed by session id. Acquire fences a monotonically increasing epoch
// via CAS so only one holder wins and every new owner out-ranks every prior one. It
// is the composition-time factory; it never decides who appends — that is the
// journal, gated on the Lease it returns.
type LeaseManager struct {
	kv     nats.KeyValue
	bucket string
	ttl    time.Duration
	now    LeaseClock
}

// NewLeaseManager provisions (creating if absent) the session-lease KV bucket and
// returns a manager over it. The bucket's own TTL is set to a generous multiple of
// the lease TTL as a coarse backstop: application-level ExpiresAt (clock-injectable)
// is the authoritative expiry check, but the bucket TTL eventually reaps an entry
// whose holder died without releasing. js must be a bound JetStream context.
func NewLeaseManager(js nats.JetStreamContext, opts ...LeaseOption) (*LeaseManager, error) {
	o := leaseOptions{bucket: defaultLeaseBucket, ttl: defaultLeaseTTL, now: time.Now}
	for _, opt := range opts {
		opt(&o)
	}
	if js == nil {
		return nil, &LeaseSetupError{Bucket: o.bucket, Cause: errNilJetStream}
	}

	// Backstop bucket TTL: long enough that application-level expiry (ExpiresAt, which
	// the injected clock controls in tests) always fires first, so the bucket TTL never
	// races a deterministic expiry test, yet bounded so a dead holder's entry is reaped.
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:   o.bucket,
		TTL:      backstopBucketTTL(o.ttl),
		Storage:  nats.FileStorage,
		History:  1,
		Replicas: 1,
	})
	if err != nil {
		return nil, &LeaseSetupError{Bucket: o.bucket, Cause: err}
	}
	return &LeaseManager{kv: kv, bucket: o.bucket, ttl: o.ttl, now: o.now}, nil
}

// backstopBucketTTL returns the bucket-level (server wall-clock) TTL: a generous
// multiple of the lease TTL, floored so a very short test TTL still yields a bucket
// TTL safely longer than a test run. The bucket TTL must never expire an entry before
// the application-level ExpiresAt check would (that check is the deterministic one);
// it only reaps entries from holders that died without releasing.
func backstopBucketTTL(ttl time.Duration) time.Duration {
	const floor = time.Hour
	if mult := ttl * 100; mult > floor {
		return mult
	}
	return floor
}

// leaseKey is the KV key for a session's lease entry: the session id. UUID text is a
// valid KV key (only [-/_=.a-zA-Z0-9]).
func leaseKey(sessionID uuid.UUID) string { return sessionID.String() }

// Acquire grants single-writer ownership of sessionID by fencing a monotonically
// increasing epoch into the lease bucket via CAS:
//
//   - entry absent: Create {Epoch:1, ...}. A losing CAS (someone created concurrently)
//     → *LeaseHeldError.
//   - entry present but expired (ExpiresAt < now): Update(rev) to {Epoch:prev+1, ...}.
//     A losing CAS (a concurrent acquirer or a renew bumped the revision) →
//     *LeaseHeldError.
//   - entry present and live: *LeaseHeldError (the holder still owns it).
//
// On success it starts a heartbeat goroutine renewing ExpiresAt and returns a live
// Lease. The epoch is monotonic across acquisitions because each takeover writes
// prev+1; only one holder wins a race because CAS (Create / Update(rev)) is atomic.
func (m *LeaseManager) Acquire(ctx context.Context, sessionID uuid.UUID) (Lease, error) {
	key := leaseKey(sessionID)
	holder, err := uuid.New()
	if err != nil {
		return nil, &LeaseReadError{SessionID: sessionID, Cause: err}
	}

	entry, err := m.kv.Get(key)
	switch {
	case errors.Is(err, nats.ErrKeyNotFound):
		return m.createLease(ctx, sessionID, holder.String(), 1)
	case err != nil:
		return nil, &LeaseReadError{SessionID: sessionID, Cause: err}
	}

	rec, derr := decodeLeaseRecord(entry.Value())
	if derr != nil {
		return nil, &LeaseReadError{SessionID: sessionID, Cause: derr}
	}
	// Live (unexpired) holder still owns the session: refuse.
	if !m.expired(rec) {
		return nil, &LeaseHeldError{SessionID: sessionID, Epoch: rec.Epoch}
	}
	// Expired: CAS-replace the stale entry, fencing the next epoch.
	return m.updateLease(ctx, sessionID, holder.String(), rec.Epoch+1, entry.Revision())
}

// createLease CAS-creates a fresh entry (no prior entry) at the given epoch and, on
// success, returns a started lease. A losing Create (ErrKeyExists) means a concurrent
// acquirer won → *LeaseHeldError.
func (m *LeaseManager) createLease(ctx context.Context, sessionID uuid.UUID, holder string, epoch uint64) (Lease, error) {
	val, err := m.encode(epoch, holder)
	if err != nil {
		return nil, err
	}
	rev, err := m.kv.Create(leaseKey(sessionID), val)
	if err != nil {
		if errors.Is(err, nats.ErrKeyExists) {
			return nil, &LeaseHeldError{SessionID: sessionID, Epoch: epoch}
		}
		return nil, &LeaseReadError{SessionID: sessionID, Cause: err}
	}
	return m.start(sessionID, holder, epoch, rev), nil
}

// updateLease CAS-updates an expired entry at lastRev to the next epoch and, on
// success, returns a started lease. A losing Update (revision moved on) means a
// concurrent acquirer or a stale holder's renew bumped it → *LeaseHeldError.
func (m *LeaseManager) updateLease(ctx context.Context, sessionID uuid.UUID, holder string, epoch, lastRev uint64) (Lease, error) {
	val, err := m.encode(epoch, holder)
	if err != nil {
		return nil, err
	}
	rev, err := m.kv.Update(leaseKey(sessionID), val, lastRev)
	if err != nil {
		if isKVCASConflict(err) {
			return nil, &LeaseHeldError{SessionID: sessionID, Epoch: epoch - 1}
		}
		return nil, &LeaseReadError{SessionID: sessionID, Cause: err}
	}
	return m.start(sessionID, holder, epoch, rev), nil
}

// encode mints a leaseRecord at epoch/holder with ExpiresAt = now + ttl and marshals
// it. The journal's lease-record JSON is encoded here, never by a caller.
func (m *LeaseManager) encode(epoch uint64, holder string) ([]byte, error) {
	rec := leaseRecord{Epoch: epoch, Holder: holder, ExpiresAt: m.now().Add(m.ttl)}
	return encodeLeaseRecord(rec)
}

// expired reports whether rec's ExpiresAt is at or before the manager's current
// clock — the authoritative, clock-injectable expiry check.
func (m *LeaseManager) expired(rec leaseRecord) bool {
	return !rec.ExpiresAt.After(m.now())
}

// start constructs a live *kvLease at the given epoch/revision and launches its
// heartbeat goroutine. The lease owns its own renewal and loss signaling from here.
func (m *LeaseManager) start(sessionID uuid.UUID, holder string, epoch, rev uint64) *kvLease {
	l := &kvLease{
		kv:        m.kv,
		now:       m.now,
		ttl:       m.ttl,
		sessionID: sessionID,
		holder:    holder,
		epoch:     epoch,
		rev:       rev,
		lost:      make(chan struct{}),
		stop:      make(chan struct{}),
	}
	l.startHeartbeat()
	return l
}

// kvLease is the concrete Lease: a single holder of a session's KV lease entry. It
// heartbeats its ExpiresAt forward on a background goroutine via CAS on its own
// revision; a failed renew (a higher epoch / different holder took the entry) marks
// the lease lost. mu guards the mutable epoch/rev/validity; the heartbeat and the
// holder's Valid/Release calls are concurrent.
type kvLease struct {
	kv  nats.KeyValue
	now LeaseClock
	ttl time.Duration

	sessionID uuid.UUID
	holder    string

	mu    sync.Mutex
	epoch uint64
	rev   uint64
	done  bool // lost or released: the lease no longer owns the session

	lost     chan struct{} // closed once on loss (also on release)
	stop     chan struct{} // closed by Release to stop the heartbeat
	stopOnce sync.Once
	lostOnce sync.Once
	wg       sync.WaitGroup
}

// Epoch returns the fencing epoch this lease holds.
func (l *kvLease) Epoch() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.epoch
}

// SessionID returns the session this lease owns.
func (l *kvLease) SessionID() uuid.UUID { return l.sessionID }

// Valid reports whether the lease is still held (not lost, not released).
func (l *kvLease) Valid() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.done
}

// Lost returns the channel closed when the lease is lost or released.
func (l *kvLease) Lost() <-chan struct{} { return l.lost }

// Release relinquishes the lease: it stops the heartbeat, marks the lease no longer
// held (closing Lost), and best-effort CAS-updates the entry to an immediately-expired
// state that PRESERVES the epoch, so a successor can re-acquire at once (the entry is
// expired) AND fences a strictly higher epoch (the prior epoch survives in the entry
// for prev+1). Deleting the entry instead would reset the next acquirer to epoch 1 and
// break monotonicity. The update fails silently if a higher epoch already replaced us
// — that successor already owns the session.
func (l *kvLease) Release(ctx context.Context) error {
	l.stopOnce.Do(func() { close(l.stop) })
	l.wg.Wait()

	l.markLost()

	l.mu.Lock()
	epoch, holder, rev := l.epoch, l.holder, l.rev
	l.mu.Unlock()

	// Write the entry expired-now (preserving epoch) under CAS on our own revision.
	val, err := encodeLeaseRecord(leaseRecord{Epoch: epoch, Holder: holder, ExpiresAt: l.now()})
	if err != nil {
		return err
	}
	if _, err := l.kv.Update(leaseKey(l.sessionID), val, rev); err != nil {
		if isKVCASConflict(err) || errors.Is(err, nats.ErrKeyNotFound) {
			return nil
		}
		return &LeaseReadError{SessionID: l.sessionID, Cause: err}
	}
	return nil
}

// markLost transitions the lease to not-held and closes Lost exactly once. Safe to
// call from both Release and the heartbeat.
func (l *kvLease) markLost() {
	l.mu.Lock()
	l.done = true
	l.mu.Unlock()
	l.lostOnce.Do(func() { close(l.lost) })
}

// startHeartbeat launches the renewal goroutine. It renews at ttl/3 so two renews
// fit inside one TTL window (tolerating one missed beat). On a renewal CAS conflict —
// a higher epoch / different holder took the entry — it marks the lease lost and
// exits; the holder observes loss via Valid()/Lost() and stops writing.
func (l *kvLease) startHeartbeat() {
	interval := l.ttl / 3
	if interval <= 0 {
		interval = l.ttl
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-t.C:
				if !l.renew() {
					l.markLost()
					return
				}
			}
		}
	}()
}

// renew CAS-updates the lease entry to push ExpiresAt forward, keyed on the lease's
// own revision. It returns true if the lease is still ours (renewed), false if it was
// lost (a higher epoch / different holder holds the entry, or it vanished). A
// transient read error is treated as "not lost yet" — the next beat retries — so a
// blip does not surrender a live lease; only a definitive CAS conflict, a foreign
// holder, or a vanished entry loses it.
func (l *kvLease) renew() bool {
	l.mu.Lock()
	epoch, holder, rev := l.epoch, l.holder, l.rev
	l.mu.Unlock()

	val, err := encodeLeaseRecord(leaseRecord{Epoch: epoch, Holder: holder, ExpiresAt: l.now().Add(l.ttl)})
	if err != nil {
		// Encoding a uint64+string+time can't realistically fail; treat as transient.
		return true
	}
	newRev, err := l.kv.Update(leaseKey(l.sessionID), val, rev)
	if err == nil {
		l.mu.Lock()
		l.rev = newRev
		l.mu.Unlock()
		return true
	}
	if isKVCASConflict(err) || errors.Is(err, nats.ErrKeyNotFound) {
		// Definitive loss: someone CAS-replaced our revision (higher epoch) or the
		// entry is gone. Surrender the lease.
		return false
	}
	// Transient (transport/timeout): keep the lease, retry next beat.
	return true
}

// isKVCASConflict reports whether err is the KV compare-and-swap rejection: a
// Create/Update whose expected-last-subject-sequence did not match (the key exists or
// its revision moved). The vendored legacy KV surfaces this as ErrKeyExists or a
// *nats.APIError carrying the wrong-last-sequence code; match on both.
func isKVCASConflict(err error) bool {
	if errors.Is(err, nats.ErrKeyExists) {
		return true
	}
	var apiErr *nats.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == nats.JSErrCodeStreamWrongLastSequence
}

// LeaseEncodeError wraps a failure to marshal a leaseRecord to JSON. A leaseRecord is
// a uint64 + string + time, so this is effectively unreachable, but the codec returns
// a typed error rather than dropping the json.Marshal error to satisfy the
// errors-are-typed contract.
type LeaseEncodeError struct{ Cause error }

func (e *LeaseEncodeError) Error() string { return "journal: encode lease record: " + e.Cause.Error() }
func (e *LeaseEncodeError) Unwrap() error { return e.Cause }

// encodeLeaseRecord marshals a leaseRecord to its JSON value.
func encodeLeaseRecord(rec leaseRecord) ([]byte, error) {
	data, err := json.Marshal(rec)
	if err != nil {
		return nil, &LeaseEncodeError{Cause: err}
	}
	return data, nil
}

// decodeLeaseRecord decodes a stored lease entry value, failing closed on malformed
// JSON, an unknown field, or trailing bytes — an ambiguous entry never silently
// grants ownership.
func decodeLeaseRecord(data []byte) (leaseRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var rec leaseRecord
	if err := dec.Decode(&rec); err != nil {
		return leaseRecord{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return leaseRecord{}, errTrailingLeaseData
	}
	return rec, nil
}

// errTrailingLeaseData is the leaf cause when a stored lease entry has bytes after
// its JSON object. It carries no context fields, so a sentinel is permitted.
var errTrailingLeaseData = errors.New("journal: trailing data after lease record")
