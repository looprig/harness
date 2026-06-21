package journal

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/nats-io/nats.go"
)

// catalogScanTimeout bounds a RepairCatalog stream scan independent of the caller's
// context: a repair walks one session's whole event backlog, so it carries its own
// deadline so a wedged read cannot hang the caller forever.
const catalogScanTimeout = 30 * time.Second

// errEmptyRepair is the leaf cause when RepairCatalog scans a session's stream and
// finds no SessionStarted — there is nothing to index (an empty or non-existent
// session). It carries no context fields, so a sentinel is permitted.
var errEmptyRepair = errors.New("journal: no SessionStarted found while repairing catalog")

// EmptySessionError reports that RepairCatalog could not rebuild a session's entry
// because its stream carries no SessionStarted (nothing to index). It carries the
// session and unwraps to errEmptyRepair.
type EmptySessionError struct{ SessionID uuid.UUID }

func (e *EmptySessionError) Error() string {
	return "journal: cannot repair catalog for session " + e.SessionID.String() +
		": " + errEmptyRepair.Error()
}
func (e *EmptySessionError) Unwrap() error { return errEmptyRepair }

// errNoReplayer is the leaf cause when RepairCatalog is called on a Catalog built
// without a replayer (WithCatalogReplayer never supplied). It carries no context
// fields, so a sentinel is permitted.
var errNoReplayer = errors.New("journal: catalog has no replayer; cannot repair from stream")

// WithCatalogReplayer injects the read-side replayer RepairCatalog scans the session's
// stream through. A nil replayer is ignored; if none is ever set, RepairCatalog fails
// with a typed error rather than silently doing nothing.
func WithCatalogReplayer(r EventReplayer) CatalogOption {
	return func(o *catalogOptions) {
		if r != nil {
			o.replayer = r
		}
	}
}

// ListSessions returns every catalog entry by reading the KV bucket ONLY — keys then
// values — with ZERO event replay and NO stream consumer. It is the session picker's
// data source: a replay-free index. An empty bucket returns an empty slice (not an
// error); a corrupt entry surfaces a typed *CatalogReadError so the caller can repair.
// ctx is accepted for signature uniformity; the legacy KV API is not context-cancelable
// (each call is bounded by the client's default request timeout), mirroring the lease
// manager.
func (c *Catalog) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	keys, err := c.kv.Keys()
	if errors.Is(err, nats.ErrNoKeysFound) {
		return []SessionMeta{}, nil
	}
	if err != nil {
		return nil, &CatalogReadError{Bucket: c.bucket, Cause: err}
	}
	metas := make([]SessionMeta, 0, len(keys))
	for _, key := range keys {
		entry, gerr := c.kv.Get(key)
		if errors.Is(gerr, nats.ErrKeyNotFound) {
			// Deleted between Keys and Get: skip it (a concurrent delete is not a corrupt
			// entry).
			continue
		}
		if gerr != nil {
			return nil, &CatalogReadError{Bucket: c.bucket, Cause: gerr}
		}
		meta, derr := decodeSessionMeta(entry.Value())
		if derr != nil {
			return nil, &CatalogReadError{Bucket: c.bucket, Cause: derr}
		}
		metas = append(metas, meta)
	}
	return metas, nil
}

// RepairCatalog rebuilds a session's catalog entry from the authoritative stream — the
// repair path for a missing or stale (or corrupt) entry. Since the catalog is derived,
// repair reconstructs it by folding the session's Enduring events (the same applyEvent
// mapping the inline update uses) over an ordered cold replay, then writing the result
// once. It scans events ONLY (the replayer never opens cmd/fence subjects). A session
// whose stream carries no SessionStarted yields a typed *EmptySessionError (nothing to
// index). Unlike UpdateOnEvent, repair is NOT best-effort: a read/write failure is
// surfaced (the caller explicitly asked to repair).
func (c *Catalog) RepairCatalog(ctx context.Context, sessionID uuid.UUID) (SessionMeta, error) {
	if c.replayer == nil {
		return SessionMeta{}, &CatalogReadError{Bucket: c.bucket, SessionID: sessionID, Cause: errNoReplayer}
	}
	scanCtx, cancel := context.WithTimeout(ctx, catalogScanTimeout)
	defer cancel()

	cursor, err := c.replayer.Open(scanCtx, ReplayRequest{SessionID: sessionID, From: Beginning()})
	if err != nil {
		return SessionMeta{}, &CatalogReadError{Bucket: c.bucket, SessionID: sessionID, Cause: err}
	}
	defer func() { _ = cursor.Close() }()

	var meta SessionMeta
	sawStart := false
	for {
		ev, _, nerr := cursor.Next(scanCtx)
		if errors.Is(nerr, io.EOF) {
			break
		}
		if nerr != nil {
			return SessionMeta{}, &CatalogReadError{Bucket: c.bucket, SessionID: sessionID, Cause: nerr}
		}
		if _, ok := ev.(event.SessionStarted); ok {
			sawStart = true
		}
		meta, _ = applyEvent(meta, ev, c.now)
	}
	if !sawStart {
		return SessionMeta{}, &EmptySessionError{SessionID: sessionID}
	}
	// Ensure the entry is keyed by the requested session even if no event carried it
	// (defensive; SessionStarted always sets it).
	meta.SessionID = sessionID
	if err := c.store(meta); err != nil {
		return SessionMeta{}, err
	}
	return meta, nil
}
