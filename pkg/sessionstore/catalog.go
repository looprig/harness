package sessionstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/journal"
	"github.com/ciram-co/looprig/pkg/uuid"
	"github.com/ciram-co/storekit"
)

// titleMaxLen bounds the derived Title: a short label cut from the first user message's
// text. A picker shows a one-line preview, so the title is the message's first line,
// truncated to this many runes.
const titleMaxLen = 80

// catalogMaxCASRetries bounds the read-modify-write retry loop UpdateOnEvent (and
// RepairCatalog's store) run when a concurrent writer wins the rev-CAS. storekit.KV has
// NO unconditional Put — every Put is a revision compare-and-swap — so emulating the
// NATS catalog's last-write-wins "the update lands" guarantee means re-reading the newer
// revision and retrying. The bound keeps a pathologically contended key from spinning
// forever; exhausting it surfaces a typed *CatalogConflictError.
const catalogMaxCASRetries = 8

// catalogScanTimeout bounds a RepairCatalog stream scan independent of the caller's
// context: a repair walks one session's whole event backlog, so it carries its own
// deadline so a wedged read cannot hang the caller forever.
const catalogScanTimeout = 30 * time.Second

// SessionStatus is the lifecycle phase the catalog records for a session. It is a closed
// typed enum (not a free-form string) so a picker can switch on it and a typo cannot
// silently mislabel a session.
type SessionStatus string

const (
	// StatusActive marks a session whose primary loop is running (the SessionStarted
	// default until a SessionStopped flips it).
	StatusActive SessionStatus = "active"
	// StatusStopped marks a session that emitted SessionStopped (a clean shutdown). It
	// survives on disk and is brought back by restore — Stopped is a phase, not a delete.
	StatusStopped SessionStatus = "stopped"
)

// SessionMeta is the derived per-session catalog entry: the small, replay-free record the
// session picker reads to list sessions without opening a single ledger cursor. It is
// JSON (snake_case) stored one-per-session in storekit.KV, keyed by the session's ledger
// name ("sessions/<uuid>"). It is a cache rebuilt from the authoritative ledger when
// missing or stale (RepairCatalog) — never the source of truth.
type SessionMeta struct {
	// SessionID is the session this entry describes.
	SessionID uuid.UUID `json:"session_id"`
	// Title is a short, human-readable label derived from the first turn's user message
	// (its first line, truncated). Empty until a first TurnStarted is seen.
	Title string `json:"title,omitempty"`
	// CreatedAt is when the session started (SessionStarted's CreatedAt).
	CreatedAt time.Time `json:"created_at,omitzero"`
	// LastActiveAt is the most recent activity instant (bumped by TurnStarted, StepDone,
	// RestoreDone), stamped from the catalog's injected clock at update time.
	LastActiveAt time.Time `json:"last_active_at,omitzero"`
	// Status is the session's lifecycle phase (active until SessionStopped -> stopped).
	Status SessionStatus `json:"status,omitempty"`
	// AgentKind names the agent role (from SessionStarted's ConfigFingerprint). It is
	// passthrough: empty until the agent threads its kind through loop.Config.
	AgentKind string `json:"agent_kind,omitempty"`
	// LoopCount is the number of loops registered in the session: the primary plus one
	// per LoopStarted.
	LoopCount int `json:"loop_count,omitempty"`
	// ConfigFingerprint is the config identity the session started under, for the picker
	// to surface a config change on restore.
	ConfigFingerprint event.ConfigFingerprint `json:"config_fingerprint,omitzero"`
}

// CatalogReadError wraps a failure to read or decode a catalog entry (a KV Get/Keys error
// that is not "not found", or a malformed stored SessionMeta). It carries (when known)
// the session and unwraps to the cause. ListSessions and RepairCatalog surface it; the
// best-effort UpdateOnEvent logs+swallows it (it must never fail the append).
type CatalogReadError struct {
	SessionID uuid.UUID
	Cause     error
}

func (e *CatalogReadError) Error() string {
	return "sessionstore: read catalog entry for session " + e.SessionID.String() + ": " + e.Cause.Error()
}
func (e *CatalogReadError) Unwrap() error { return e.Cause }

// CatalogWriteError wraps a failure to write a catalog entry (a KV Put/encode error). It
// carries the session and unwraps to the cause. The best-effort UpdateOnEvent logs+swallows
// it; RepairCatalog surfaces it (a repair the caller asked for that could not persist is a
// real failure).
type CatalogWriteError struct {
	SessionID uuid.UUID
	Cause     error
}

func (e *CatalogWriteError) Error() string {
	return "sessionstore: write catalog entry for session " + e.SessionID.String() + ": " + e.Cause.Error()
}
func (e *CatalogWriteError) Unwrap() error { return e.Cause }

// CatalogEncodeError wraps a failure to marshal a SessionMeta to JSON. A SessionMeta is
// value-typed, so this is effectively unreachable, but the codec returns a typed error
// rather than dropping the json.Marshal error to satisfy errors-are-typed.
type CatalogEncodeError struct{ Cause error }

func (e *CatalogEncodeError) Error() string {
	return "sessionstore: encode session meta: " + e.Cause.Error()
}
func (e *CatalogEncodeError) Unwrap() error { return e.Cause }

// CatalogConflictError reports that a catalog update could not win the KV revision-CAS
// within catalogMaxCASRetries attempts: a persistently contended key. It has no storekit
// analog in the NATS catalog (JetStream KV Put was unconditional last-write-wins); it
// exists because storekit.KV is CAS-only. UpdateOnEvent logs+swallows it (best-effort);
// RepairCatalog surfaces it.
type CatalogConflictError struct {
	SessionID uuid.UUID
	Attempts  int
}

func (e *CatalogConflictError) Error() string {
	return "sessionstore: catalog entry for session " + e.SessionID.String() +
		" lost the revision-CAS after " + strconv.Itoa(e.Attempts) + " attempts"
}

// errEmptyRepair is the leaf cause when RepairCatalog scans a session's ledger and finds
// no SessionStarted — there is nothing to index (an empty or non-existent session). It
// carries no context fields, so a sentinel is permitted.
var errEmptyRepair = errors.New("sessionstore: no SessionStarted found while repairing catalog")

// EmptySessionError reports that RepairCatalog could not rebuild a session's entry because
// its ledger carries no SessionStarted (nothing to index). It carries the session and
// unwraps to errEmptyRepair.
type EmptySessionError struct{ SessionID uuid.UUID }

func (e *EmptySessionError) Error() string {
	return "sessionstore: cannot repair catalog for session " + e.SessionID.String() + ": " + errEmptyRepair.Error()
}
func (e *EmptySessionError) Unwrap() error { return errEmptyRepair }

// errNoReplayer is the leaf cause when RepairCatalog is called on a Catalog built without
// an EventReplayerOpener. It carries no context fields, so a sentinel is permitted.
var errNoReplayer = errors.New("sessionstore: catalog has no replayer; cannot repair from ledger")

// errTrailingCatalogData is the leaf cause when a stored catalog entry has bytes after its
// JSON object. It carries no context fields, so a sentinel is permitted.
var errTrailingCatalogData = errors.New("sessionstore: trailing data after session meta")

// CatalogClock is the time seam for the catalog: it stamps LastActiveAt at update time.
// Injecting it makes activity-bump assertions deterministic in tests.
type CatalogClock func() time.Time

// CatalogLogger is the narrow logging seam the best-effort catalog update writes to when a
// KV read/write fails: the catalog is derivable, so a failure is logged and swallowed,
// NEVER surfaced to the append path. It is a single-method interface (Interface
// Segregation); a nop default keeps existing wiring unchanged.
type CatalogLogger interface {
	// CatalogUpdateFailed is called with the typed error when a best-effort catalog update
	// could not read or write its KV entry. The implementation must not panic and must not
	// re-raise — it is the end of the error's life.
	CatalogUpdateFailed(err error)
}

// nopCatalogLogger is the default CatalogLogger: it drops the error. It is the safe default
// so a caller that does not inject a logger never panics on a nil logger.
type nopCatalogLogger struct{}

func (nopCatalogLogger) CatalogUpdateFailed(error) {}

// EventReplayerOpener is the narrow seam RepairCatalog folds a session's ledger through: it
// opens a read-side event replayer for one session. *Store satisfies it via
// OpenEventReplayer (Dependency Inversion — the catalog depends on this method alone, not
// the whole Store). A nil opener disables repair (RepairCatalog fails with a typed error).
type EventReplayerOpener interface {
	OpenEventReplayer(id uuid.UUID, req ReplayRequest) (journal.EventReplayer, error)
}

// applyEvent folds one catalog-relevant event into a SessionMeta and reports whether the
// event changed it (false => the event is not catalog-relevant, so no upsert is needed). It
// is the single source of truth for the event->field mapping, shared by the inline
// UpdateOnEvent (read-modify-write one KV entry) and RepairCatalog (fold the whole ledger
// then write once). It is PURE except for the injected now clock, so the mapping is
// unit-testable without a KV.
//
//   - SessionStarted: stamps SessionID, CreatedAt, ConfigFingerprint, AgentKind (from the
//     fingerprint — passthrough), Status=active, and counts the primary loop.
//   - TurnStarted: sets Title from the user message if not already set (first turn wins),
//     and bumps LastActiveAt.
//   - StepDone / RestoreDone: bump LastActiveAt.
//   - LoopStarted: increment LoopCount.
//   - SessionStopped: flip Status to stopped.
//   - anything else: no-op (returns changed=false).
func applyEvent(meta SessionMeta, ev event.Event, now CatalogClock) (SessionMeta, bool) {
	switch e := ev.(type) {
	case event.SessionStarted:
		meta.SessionID = e.SessionID
		meta.CreatedAt = e.CreatedAt
		meta.ConfigFingerprint = e.Config
		meta.AgentKind = e.Config.AgentKind
		meta.Status = StatusActive
		if meta.LoopCount < 1 {
			meta.LoopCount = 1
		}
		return meta, true
	case event.TurnStarted:
		meta.SessionID = e.SessionID
		if meta.Title == "" {
			meta.Title = deriveTitle(e.Message)
		}
		meta.LastActiveAt = now()
		return meta, true
	case event.StepDone:
		meta.SessionID = e.SessionID
		meta.LastActiveAt = now()
		return meta, true
	case event.RestoreDone:
		meta.SessionID = e.SessionID
		meta.LastActiveAt = now()
		return meta, true
	case event.LoopStarted:
		meta.SessionID = e.SessionID
		meta.LoopCount++
		return meta, true
	case event.SessionStopped:
		meta.SessionID = e.SessionID
		meta.Status = StatusStopped
		return meta, true
	default:
		return meta, false
	}
}

// deriveTitle extracts a short label from the first turn's user message: the first
// non-empty line of its concatenated text, truncated to titleMaxLen runes. A nil message
// or one with no text yields "" (the picker shows a placeholder). It never returns
// multi-line text — a title is a one-line preview.
func deriveTitle(msg *content.UserMessage) string {
	if msg == nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range msg.Blocks {
		if tb, ok := blk.(*content.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	text := b.String()
	// First non-empty line only.
	line := ""
	for _, l := range strings.Split(text, "\n") {
		if s := strings.TrimSpace(l); s != "" {
			line = s
			break
		}
	}
	return truncateRunes(line, titleMaxLen)
}

// truncateRunes returns s cut to at most max runes (not bytes), so a multi-byte rune is
// never split. It returns s unchanged when it already fits.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// encodeSessionMeta marshals a SessionMeta to its JSON KV value.
func encodeSessionMeta(meta SessionMeta) ([]byte, error) {
	data, err := json.Marshal(meta)
	if err != nil {
		return nil, &CatalogEncodeError{Cause: err}
	}
	return data, nil
}

// decodeSessionMeta decodes a stored catalog entry value, failing closed on malformed
// JSON, an unknown field, or trailing bytes — an ambiguous entry is a corrupt cache entry,
// surfaced as an error so the caller can repair rather than silently mis-list.
func decodeSessionMeta(data []byte) (SessionMeta, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var meta SessionMeta
	if err := dec.Decode(&meta); err != nil {
		return SessionMeta{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return SessionMeta{}, errTrailingCatalogData
	}
	return meta, nil
}

// catalogOptions is the resolved knob set OpenCatalog applies its CatalogOptions over.
type catalogOptions struct {
	now    CatalogClock
	log    CatalogLogger
	opener EventReplayerOpener
}

// CatalogOption configures a Catalog at OpenCatalog time. Applied in order over a defaults
// struct, so a later option overrides an earlier one.
type CatalogOption func(*catalogOptions)

// WithCatalogClock injects the clock LastActiveAt is stamped from. A nil clock is ignored
// (time.Now is kept).
func WithCatalogClock(now CatalogClock) CatalogOption {
	return func(o *catalogOptions) {
		if now != nil {
			o.now = now
		}
	}
}

// WithCatalogLogger injects the logger best-effort update failures are reported to. A nil
// logger is ignored (the nop default is kept).
func WithCatalogLogger(log CatalogLogger) CatalogOption {
	return func(o *catalogOptions) {
		if log != nil {
			o.log = log
		}
	}
}

// WithCatalogReplayer overrides the EventReplayerOpener RepairCatalog folds a session's
// ledger through. A nil opener is ignored (OpenCatalog defaults it to the owning Store, so
// repair works out of the box). It exists so a test can inject a scripted opener.
func WithCatalogReplayer(opener EventReplayerOpener) CatalogOption {
	return func(o *catalogOptions) {
		if opener != nil {
			o.opener = opener
		}
	}
}

// Catalog maintains the derived session catalog in storekit.KV: one SessionMeta per
// session, keyed by the session's ledger name. It has one reason to change: how the catalog
// is indexed. UpdateOnEvent folds a single event into the keyed entry (best-effort,
// post-append); ListSessions reads the KV only (no ledger cursor); RepairCatalog rebuilds
// an entry from the authoritative ledger.
type Catalog struct {
	kv     storekit.KV
	now    CatalogClock
	log    CatalogLogger
	opener EventReplayerOpener // for RepairCatalog's ledger scan (nil => repair disabled)
}

// OpenCatalog returns a Catalog over the Store's KV. Repair is enabled by default (the
// opener defaults to the Store itself, which can open a per-session event replayer); a
// clock, logger, or a different opener may be injected. It does no I/O and cannot fail —
// the KV is already wired into the Composite Open validated.
func (s *Store) OpenCatalog(opts ...CatalogOption) *Catalog {
	o := catalogOptions{now: time.Now, log: nopCatalogLogger{}, opener: s}
	for _, opt := range opts {
		opt(&o)
	}
	return &Catalog{kv: s.backend.KV, now: o.now, log: o.log, opener: o.opener}
}

// UpdateOnEvent folds ev into the session's catalog entry via a bounded read-modify-write
// under KV revision-CAS — but ONLY for a catalog-relevant event (a no-op event
// short-circuits before any KV I/O). It is BEST-EFFORT: any KV read/write/decode error (or
// exhausted CAS retries) is reported to the injected logger and swallowed (returns nil). It
// MUST NEVER fail the underlying append — the catalog is derivable, so a lost update is
// repaired later, never propagated. The returned error is always nil; the signature keeps a
// nil-error contract for the appender seam.
func (c *Catalog) UpdateOnEvent(ctx context.Context, ev event.Event) error {
	// Decide relevance on a zero meta first so a no-op event never touches the KV.
	if _, changed := applyEvent(SessionMeta{}, ev, c.now); !changed {
		return nil
	}
	sid := ev.EventHeader().SessionID
	if err := c.upsert(ctx, sid, ev); err != nil {
		c.log.CatalogUpdateFailed(err)
	}
	return nil
}

// upsert performs the bounded read-modify-write: read the current entry (or an empty one),
// fold ev, and Put under revision-CAS; on a *storekit.ConflictError a concurrent writer
// advanced the revision, so re-read and retry. Exhausting the retries returns a typed
// *CatalogConflictError. A read/decode fault or a non-conflict write fault is terminal and
// returned as its typed error.
func (c *Catalog) upsert(ctx context.Context, sid uuid.UUID, ev event.Event) error {
	for attempt := 0; attempt < catalogMaxCASRetries; attempt++ {
		current, rev, err := c.load(ctx, sid)
		if err != nil {
			return err
		}
		updated, _ := applyEvent(current, ev, c.now)
		serr := c.store(ctx, sid, rev, updated)
		if serr == nil {
			return nil
		}
		var conflict *storekit.ConflictError
		if !errors.As(serr, &conflict) {
			return serr
		}
	}
	return &CatalogConflictError{SessionID: sid, Attempts: catalogMaxCASRetries}
}

// load reads the session's catalog entry, returning its current revision so the caller can
// CAS a follow-up write. An absent key yields a zero SessionMeta, revision 0, and NO error
// — the upsert path treats absence as "create" (a rev-0 Put is create-only). A read/decode
// error other than not-found is returned as a typed *CatalogReadError.
func (c *Catalog) load(ctx context.Context, sid uuid.UUID) (SessionMeta, uint64, error) {
	key, err := sessionName(sid)
	if err != nil {
		return SessionMeta{}, 0, &CatalogReadError{SessionID: sid, Cause: err}
	}
	val, rev, err := c.kv.Get(ctx, key)
	if err != nil {
		var notFound *storekit.KeyNotFoundError
		if errors.As(err, &notFound) {
			return SessionMeta{}, 0, nil
		}
		return SessionMeta{}, 0, &CatalogReadError{SessionID: sid, Cause: err}
	}
	meta, derr := decodeSessionMeta(val)
	if derr != nil {
		return SessionMeta{}, 0, &CatalogReadError{SessionID: sid, Cause: derr}
	}
	return meta, rev, nil
}

// store encodes and writes meta to the session's keyed entry under revision-CAS on rev
// (rev 0 requires the key absent). It returns a typed *CatalogWriteError on an encode or KV
// Put failure; a *storekit.ConflictError is wrapped but still recoverable via errors.As so
// the upsert/repair retry loop can detect the lost CAS.
func (c *Catalog) store(ctx context.Context, sid uuid.UUID, rev uint64, meta SessionMeta) error {
	key, err := sessionName(sid)
	if err != nil {
		return &CatalogWriteError{SessionID: sid, Cause: err}
	}
	val, err := encodeSessionMeta(meta)
	if err != nil {
		return &CatalogWriteError{SessionID: sid, Cause: err}
	}
	if _, err := c.kv.Put(ctx, key, rev, val); err != nil {
		return &CatalogWriteError{SessionID: sid, Cause: err}
	}
	return nil
}

// ListSessions returns every catalog entry by reading the KV ONLY — keys then values —
// with ZERO ledger replay and NO cursor. It is the session picker's data source: a
// replay-free index. Entries come back sorted ascending by session id (the storekit
// KV.Keys canonical order — a deterministic improvement over the NATS catalog's arbitrary
// order). An empty catalog returns an empty slice (not an error); a corrupt entry surfaces
// a typed *CatalogReadError so the caller can repair.
func (c *Catalog) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	keys, err := c.kv.Keys(ctx, sessionsPrefix)
	if err != nil {
		return nil, &CatalogReadError{Cause: err}
	}
	metas := make([]SessionMeta, 0, len(keys))
	for _, key := range keys {
		val, _, gerr := c.kv.Get(ctx, key)
		if gerr != nil {
			var notFound *storekit.KeyNotFoundError
			if errors.As(gerr, &notFound) {
				// Deleted between Keys and Get: skip it (a concurrent delete is not a corrupt
				// entry).
				continue
			}
			return nil, &CatalogReadError{Cause: gerr}
		}
		meta, derr := decodeSessionMeta(val)
		if derr != nil {
			return nil, &CatalogReadError{Cause: derr}
		}
		metas = append(metas, meta)
	}
	return metas, nil
}

// RepairCatalog rebuilds a session's catalog entry from the authoritative ledger — the
// repair path for a missing, stale, or corrupt entry. Since the catalog is derived, repair
// reconstructs it by folding the session's events (the same applyEvent mapping the inline
// update uses) over an ordered cold replay, then writing the result once under revision-CAS.
// It scans events ONLY (the event replayer never surfaces command/fence records). A session
// whose ledger carries no SessionStarted yields a typed *EmptySessionError (nothing to
// index). Unlike UpdateOnEvent, repair is NOT best-effort: a read/write failure is surfaced
// (the caller explicitly asked to repair). A Catalog with no opener fails with a typed
// *CatalogReadError unwrapping errNoReplayer.
func (c *Catalog) RepairCatalog(ctx context.Context, sessionID uuid.UUID) (SessionMeta, error) {
	if c.opener == nil {
		return SessionMeta{}, &CatalogReadError{SessionID: sessionID, Cause: errNoReplayer}
	}
	replayer, err := c.opener.OpenEventReplayer(sessionID, ReplayRequest{FromSeq: 0})
	if err != nil {
		return SessionMeta{}, &CatalogReadError{SessionID: sessionID, Cause: err}
	}
	scanCtx, cancel := context.WithTimeout(ctx, catalogScanTimeout)
	defer cancel()

	meta, err := c.foldSession(scanCtx, sessionID, replayer)
	if err != nil {
		return SessionMeta{}, err
	}
	// Ensure the entry is keyed by the requested session even if no event carried it
	// (defensive; SessionStarted always sets it).
	meta.SessionID = sessionID
	if err := c.storeRetry(ctx, sessionID, meta); err != nil {
		return SessionMeta{}, err
	}
	return meta, nil
}

// foldSession replays session sessionID's events through replayer and folds them into a
// SessionMeta, requiring at least one SessionStarted (else *EmptySessionError). A cursor
// read failure is surfaced as a typed *CatalogReadError.
func (c *Catalog) foldSession(ctx context.Context, sessionID uuid.UUID, replayer journal.EventReplayer) (SessionMeta, error) {
	cursor, err := replayer.Open(ctx, journal.ReplayRequest{SessionID: sessionID, From: journal.Beginning()})
	if err != nil {
		return SessionMeta{}, &CatalogReadError{SessionID: sessionID, Cause: err}
	}
	defer func() { _ = cursor.Close() }()

	var meta SessionMeta
	sawStart := false
	for {
		ev, _, nerr := cursor.Next(ctx)
		if errors.Is(nerr, io.EOF) {
			break
		}
		if nerr != nil {
			return SessionMeta{}, &CatalogReadError{SessionID: sessionID, Cause: nerr}
		}
		if _, ok := ev.(event.SessionStarted); ok {
			sawStart = true
		}
		meta, _ = applyEvent(meta, ev, c.now)
	}
	if !sawStart {
		return SessionMeta{}, &EmptySessionError{SessionID: sessionID}
	}
	return meta, nil
}

// storeRetry writes an already-folded meta under the bounded revision-CAS retry loop: it
// re-reads the current revision each attempt (the rebuilt meta is authoritative, so the
// prior value is discarded) and Puts, retrying on a lost CAS. Exhausting the retries
// returns a typed *CatalogConflictError. It is repair's non-best-effort counterpart to
// upsert (which folds an event per attempt).
func (c *Catalog) storeRetry(ctx context.Context, sid uuid.UUID, meta SessionMeta) error {
	for attempt := 0; attempt < catalogMaxCASRetries; attempt++ {
		_, rev, err := c.load(ctx, sid)
		if err != nil {
			return err
		}
		serr := c.store(ctx, sid, rev, meta)
		if serr == nil {
			return nil
		}
		var conflict *storekit.ConflictError
		if !errors.As(serr, &conflict) {
			return serr
		}
	}
	return &CatalogConflictError{SessionID: sid, Attempts: catalogMaxCASRetries}
}
