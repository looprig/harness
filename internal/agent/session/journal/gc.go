package journal

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/nats-io/nats.go"
)

// gcGraceWindow is the minimum age an UNREFERENCED object must reach before GC may
// reap it. It exists to protect an object whose offload upload has landed but whose
// pointer append is still in flight (upload-before-append: the object is durable
// BEFORE any pointer references it, so for a window there is a live object with no
// pointer yet). It MUST exceed the longest possible upload->pointer-append gap.
//
// That gap is bounded by the write path's three serial deadlines on a single Append:
//   - defaultAppendTimeout (5s): the per-append publish round-trip deadline.
//   - the ambiguous-ack resolve retry: a second publish, again bounded by
//     defaultAppendTimeout (5s) — so up to ~10s of publishing for one record.
//   - dedupWindow (2m): a lost-ack republish under the same Nats-Msg-Id dedups
//     against the already-committed record only within this window; an append whose
//     ack was lost may be re-driven (and thus reference its uploaded object) as late
//     as the dedup window's end.
//
// The dedup window (2m) dominates. The grace window is set to a generous multiple of
// the dominant term plus the publish budget so a just-uploaded object whose pointer
// is still being committed is NEVER reaped: an object younger than this is left alone
// even if no pointer references it yet. Content addressing makes GC idempotent, so
// erring long here only delays reclaiming a true orphan — it never risks deleting a
// live one.
const gcGraceWindow = dedupWindow + 2*defaultAppendTimeout + 3*time.Minute // 2m + 10s + 3m = 5m10s

// GCClock is the time seam for orphan-GC: it supplies "now" against which an object's
// server-set ObjectInfo.ModTime is compared for the grace check. Injecting it makes
// the grace boundary deterministic in tests (advance now far past a real ModTime
// rather than sleeping). It mirrors LeaseClock and event.Clock.
type GCClock func() time.Time

// gcLister is the narrow object-store surface orphan-GC depends on (Interface
// Segregation, mirroring the write-side objectPutter and read-side objectFetcher):
// enumerate objects, read one object's info, and delete one object by name. The
// vendored nats.ObjectStore satisfies it; GC never depends on the full store.
type gcLister interface {
	List(opts ...nats.ListObjectsOpt) ([]*nats.ObjectInfo, error)
	Delete(name string) error
	GetInfo(name string, opts ...nats.GetObjectInfoOpt) (*nats.ObjectInfo, error)
}

// GCLeaseNotHeldError reports that GC was refused because the session's single-writer
// lease is not held (released, or overtaken by a higher epoch). GC deletes objects, so
// it must run only as the single writer; running unguarded could reap an object a live
// owner still references. It fails closed with this typed error and deletes nothing. It
// carries the session and the (stale) epoch the refused lease held.
type GCLeaseNotHeldError struct {
	SessionID uuid.UUID
	Epoch     uint64
}

func (e *GCLeaseNotHeldError) Error() string {
	return "journal: orphan-GC refused for session " + e.SessionID.String() +
		": lease at epoch " + strconv.FormatUint(e.Epoch, 10) + " not held"
}

func (e *GCLeaseNotHeldError) Unwrap() error {
	return &LeaseLostError{SessionID: e.SessionID, Epoch: e.Epoch}
}

// GCScanError reports a failure to scan the session stream for the set of referenced
// object ids (binding the consumer, reading consumer info, or a non-benign fetch
// failure). GC fails closed: without a COMPLETE referenced set it cannot safely decide
// which objects are orphans, so it deletes nothing rather than risk reaping a still-
// referenced object. It carries the stream and unwraps to the underlying NATS cause.
type GCScanError struct {
	Stream string
	Cause  error
}

func (e *GCScanError) Error() string {
	return "journal: orphan-GC scan of " + strconv.Quote(e.Stream) + ": " + e.Cause.Error()
}
func (e *GCScanError) Unwrap() error { return e.Cause }

// GCListError reports a failure to list the per-session object bucket. GC fails closed:
// without the object inventory it cannot decide what to reap, so it deletes nothing. It
// carries the bucket name and unwraps to the underlying NATS cause. ErrNoObjectsFound
// (an empty bucket) is NOT an error and never produces this.
type GCListError struct {
	Bucket string
	Cause  error
}

func (e *GCListError) Error() string {
	return "journal: orphan-GC list of " + strconv.Quote(e.Bucket) + ": " + e.Cause.Error()
}
func (e *GCListError) Unwrap() error { return e.Cause }

// GCDeleteError reports a failure to delete one orphaned object. GC surfaces it rather
// than silently swallowing the failure, so a caller learns the bucket could not be
// fully reclaimed. It carries the bucket and object id and unwraps to the NATS cause.
type GCDeleteError struct {
	Bucket   string
	ObjectID string
	Cause    error
}

func (e *GCDeleteError) Error() string {
	return "journal: orphan-GC delete of object " + strconv.Quote(e.ObjectID) +
		" in " + strconv.Quote(e.Bucket) + ": " + e.Cause.Error()
}
func (e *GCDeleteError) Unwrap() error { return e.Cause }

// GCResult summarizes one GC pass: how many objects the bucket held, how many were
// referenced by a live pointer, how many were skipped as too young (within the grace
// window), and how many orphans were reaped. The counts let a caller log/observe a
// pass without re-deriving them.
type GCResult struct {
	// Scanned is the number of objects the bucket held at the start of the pass.
	Scanned int
	// Referenced is the number of objects still referenced by an in-stream pointer.
	Referenced int
	// WithinGrace is the number of unreferenced objects skipped because they are
	// younger than the grace window (protecting in-flight uploads).
	WithinGrace int
	// Deleted is the number of orphaned objects reaped this pass.
	Deleted int
}

// GCOption configures an ObjectGC at construction. Applied in order over a defaults
// struct, so a later option overrides an earlier one.
type GCOption func(*gcOptions)

type gcOptions struct {
	now GCClock
}

// WithGCClock injects the clock the grace check compares object ModTimes against. A nil
// clock is ignored (time.Now is kept).
func WithGCClock(now GCClock) GCOption {
	return func(o *gcOptions) {
		if now != nil {
			o.now = now
		}
	}
}

// ObjectGC reaps orphaned offload objects from one session's object bucket: objects no
// in-stream pointer references AND that are older than the grace window. It is
// lease-guarded — it runs only while holding a valid single-writer lease, so it is the
// single deleter and cannot race the offload writer. It is the GC analogue of
// SessionJournal (write) and EventReplayer (read), wired at the composition root.
type ObjectGC struct {
	js        nats.JetStreamContext
	objects   gcLister
	lease     ownershipToken
	sessionID uuid.UUID
	now       GCClock
}

// NewObjectGC wires an orphan-GC over a bound JetStream context, the session's object
// bucket, and the session's single-writer lease (DIP: the composition root acquires the
// lease and passes it in; GC never acquires one). It depends only on the narrow
// ownershipToken view of the lease — never Release — so it cannot tamper with the lease
// lifecycle. js must already be bound; the embedded server is started elsewhere.
func NewObjectGC(js nats.JetStreamContext, objects nats.ObjectStore, lease Lease, sessionID uuid.UUID, opts ...GCOption) (*ObjectGC, error) {
	if js == nil {
		return nil, &GCScanError{Stream: StreamName(sessionID), Cause: errNilJetStream}
	}
	if objects == nil {
		return nil, &GCListError{Bucket: SessionObjectBucket(sessionID), Cause: errNilObjectStore}
	}
	if lease == nil {
		return nil, &GCScanError{Stream: StreamName(sessionID), Cause: errNilLease}
	}
	o := gcOptions{now: time.Now}
	for _, opt := range opts {
		opt(&o)
	}
	return &ObjectGC{
		js:        js,
		objects:   objects,
		lease:     lease,
		sessionID: sessionID,
		now:       o.now,
	}, nil
}

// errNilObjectStore is the leaf cause when NewObjectGC is handed a nil object store. A
// sentinel is permitted (no context fields).
var errNilObjectStore = errors.New("journal: nil object store")

// gcScanTimeout bounds the whole stream-scan round-trip in collectReferenced when the
// caller's context carries no deadline of its own, so a wedged consumer cannot hang GC
// forever. Mirrors the journal's other management deadlines.
const gcScanTimeout = 30 * time.Second

// gcFetchTimeout bounds a single backlog fetch during the referenced-id scan. A drained
// consumer reports NumPending==0 and the scan ends without any fetch reaching this; if a
// fetch DOES time out while records remain, the scan fails closed (it must not reap with
// an incomplete referenced set). Mirrors replayFetchTimeout.
const gcFetchTimeout = 2 * time.Second

// GC runs one orphan-GC pass under the held lease. It (1) refuses unless the lease is
// valid — GC deletes, so it must be the single writer; (2) scans the session stream's
// EVENT subjects for pointer records and builds the set of referenced object ids; (3)
// lists the per-session object bucket and deletes each object that is BOTH unreferenced
// AND older than the grace window (now - ModTime > gcGraceWindow). It returns a summary
// of the pass. Every failure is a typed fail-closed error; on a scan/list failure it
// deletes nothing.
func (g *ObjectGC) GC(ctx context.Context) (GCResult, error) {
	// Lease guard: GC is the single deleter. Refuse if the lease is not held.
	if !g.leaseHeld() {
		return GCResult{}, &GCLeaseNotHeldError{SessionID: g.sessionID, Epoch: g.lease.Epoch()}
	}

	referenced, err := g.collectReferenced(ctx)
	if err != nil {
		return GCResult{}, err
	}

	objs, err := g.listObjects(ctx)
	if err != nil {
		return GCResult{}, err
	}

	return g.reap(referenced, objs)
}

// leaseHeld reports whether the ownership lease is still held: both the validity flag
// and the loss channel must say so. Mirrors the journal's write-side guard. The grace
// window plus the single-deleter guarantee are GC's safety; this is the fast-path gate.
func (g *ObjectGC) leaseHeld() bool {
	if !g.lease.Valid() {
		return false
	}
	select {
	case <-g.lease.Lost():
		return false
	default:
		return true
	}
}

// reap deletes each object that is BOTH unreferenced AND older than the grace window,
// re-checking the lease before each delete so a lease lost mid-pass stops further
// deletes immediately (fail secure). It returns the pass summary.
func (g *ObjectGC) reap(referenced map[string]struct{}, objs []*nats.ObjectInfo) (GCResult, error) {
	res := GCResult{Scanned: len(objs)}
	now := g.now()
	for _, info := range objs {
		if info == nil {
			continue
		}
		if _, ok := referenced[info.Name]; ok {
			res.Referenced++
			continue
		}
		// Unreferenced. Reap only if older than the grace window — a younger object may
		// be a just-uploaded one whose pointer append is still in flight.
		if now.Sub(info.ModTime) <= gcGraceWindow {
			res.WithinGrace++
			continue
		}
		// Re-guard the lease before each delete: a loss mid-pass must stop deleting at
		// once rather than finish reaping as a no-longer-single writer.
		if !g.leaseHeld() {
			return res, &GCLeaseNotHeldError{SessionID: g.sessionID, Epoch: g.lease.Epoch()}
		}
		if err := g.objects.Delete(info.Name); err != nil {
			return res, &GCDeleteError{Bucket: SessionObjectBucket(g.sessionID), ObjectID: info.Name, Cause: err}
		}
		res.Deleted++
	}
	return res, nil
}

// listObjects enumerates the per-session bucket, treating an empty bucket
// (ErrNoObjectsFound) as zero objects rather than an error. Any other failure fails
// closed as a *GCListError.
func (g *ObjectGC) listObjects(ctx context.Context) ([]*nats.ObjectInfo, error) {
	objs, err := g.objects.List(nats.Context(ctx))
	if err != nil {
		if errors.Is(err, nats.ErrNoObjectsFound) {
			return nil, nil
		}
		return nil, &GCListError{Bucket: SessionObjectBucket(g.sessionID), Cause: err}
	}
	return objs, nil
}

// collectReferenced scans the session stream's EVENT subjects (pointers only ever
// appear on event subjects) and builds the set of object ids referenced by an in-stream
// pointer record — read from each message's Urvi-Object-Id header (no body decode, no
// object fetch: GC must NOT rehydrate, or a deliberately-orphaned object would make the
// scan fail). It binds its own ephemeral AckNone pull consumer over the same subject
// filter the replayer uses, walks the backlog to the Open-time tip, and returns the id
// set. It fails closed with a *GCScanError on any non-benign failure: an incomplete
// referenced set could orphan a live object.
func (g *ObjectGC) collectReferenced(ctx context.Context) (map[string]struct{}, error) {
	stream := StreamName(g.sessionID)

	scanCtx, cancel := context.WithTimeout(ctx, gcScanTimeout)
	defer cancel()

	filters := eventSubjectFilters(g.sessionID, uuid.UUID{}) // all loops' events + session events.
	sub, err := g.js.PullSubscribe("", "",
		nats.BindStream(stream),
		nats.ConsumerFilterSubjects(filters...),
		nats.AckNone(),
		nats.DeliverAll(),
		nats.InactiveThreshold(replayInactiveThreshold),
	)
	if err != nil {
		return nil, &GCScanError{Stream: stream, Cause: err}
	}
	defer func() { _ = sub.Unsubscribe() }()

	// Filter-aware backlog size: zero matching event records means no pointers exist, so
	// the referenced set is empty and we skip the fetch loop entirely.
	pending, err := consumerNumPending(sub)
	if err != nil {
		return nil, &GCScanError{Stream: stream, Cause: err}
	}

	referenced := make(map[string]struct{})
	if pending == 0 {
		return referenced, nil
	}

	if err := g.drainReferenced(scanCtx, sub, stream, referenced); err != nil {
		return nil, err
	}
	return referenced, nil
}

// drainReferenced pulls the matching backlog one message at a time, recording each
// message's Urvi-Object-Id header (when present) into referenced, until the consumer
// reports NumPending==0 (drained). A benign fetch timeout while records remain — or any
// other fetch failure — fails closed as a *GCScanError: the scan must be COMPLETE
// before any delete, so a partial scan never proceeds to reaping.
func (g *ObjectGC) drainReferenced(ctx context.Context, sub *nats.Subscription, stream string, referenced map[string]struct{}) error {
	for {
		fetchCtx, cancel := context.WithTimeout(ctx, gcFetchTimeout)
		msgs, err := sub.Fetch(1, nats.Context(fetchCtx))
		cancel()
		if err != nil {
			// Any fetch failure (timeout included) while we have not seen NumPending==0 is
			// fail-closed: an incomplete referenced set must never drive a delete.
			return &GCScanError{Stream: stream, Cause: err}
		}
		if len(msgs) == 0 {
			return &GCScanError{Stream: stream, Cause: errGCEmptyFetchBacklogRemaining}
		}
		msg := msgs[0]

		// A pointer record is MARKED by the presence of the Urvi-Object-Id header; an
		// inline event has no such header and references no object.
		if id := msg.Header.Get(objectIDHeader); id != "" {
			referenced[id] = struct{}{}
		}

		meta, err := msg.Metadata()
		if err != nil {
			return &GCScanError{Stream: stream, Cause: err}
		}
		if meta.NumPending == 0 {
			return nil
		}
	}
}

// errGCEmptyFetchBacklogRemaining is the leaf cause when a backlog fetch came back empty
// during the referenced-id scan even though the consumer never reported NumPending==0 —
// a read anomaly on a healthy local store. Failing closed here is mandatory: proceeding
// with a partial referenced set could orphan a still-referenced object.
var errGCEmptyFetchBacklogRemaining = errors.New("journal: orphan-GC scan fetch returned no message while records remain pending")
