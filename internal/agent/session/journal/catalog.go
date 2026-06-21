package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/nats-io/nats.go"
)

// catalogBucket is the JetStream KV bucket holding the derived session catalog: one
// SessionMeta per session, keyed by session id. It is a rebuildable cache, NOT a
// source of truth — the per-session stream is authoritative; this bucket is a
// replay-free index for the session picker.
const catalogBucket = "urvi_sessions"

// titleMaxLen bounds the derived Title: a short label cut from the first user
// message's text. A picker shows a one-line preview, so the title is the message's
// first line, truncated to this many runes.
const titleMaxLen = 80

// SessionStatus is the lifecycle phase the catalog records for a session. It is a
// closed typed enum (not a free-form string) so a picker can switch on it and a typo
// cannot silently mislabel a session.
type SessionStatus string

const (
	// StatusActive marks a session whose primary loop is running (the SessionStarted
	// default until a SessionStopped flips it).
	StatusActive SessionStatus = "active"
	// StatusStopped marks a session that emitted SessionStopped (a clean shutdown). It
	// survives on disk and is brought back by restore — Stopped is a phase, not a delete.
	StatusStopped SessionStatus = "stopped"
)

// SessionMeta is the derived per-session catalog entry: the small, replay-free record
// the session picker reads to list sessions without opening a single stream consumer.
// It is JSON (snake_case) stored one-per-session in the catalogBucket KV, keyed by
// SessionID. It is a cache rebuilt from the authoritative stream when missing or stale
// (RepairCatalog) — never the source of truth.
type SessionMeta struct {
	// SessionID is the session this entry describes (also the KV key).
	SessionID uuid.UUID `json:"session_id"`
	// Title is a short, human-readable label derived from the first turn's user
	// message (its first line, truncated). Empty until a first TurnStarted is seen.
	Title string `json:"title,omitempty"`
	// CreatedAt is when the session started (SessionStarted's CreatedAt).
	CreatedAt time.Time `json:"created_at,omitzero"`
	// LastActiveAt is the most recent activity instant (bumped by TurnStarted,
	// StepDone, RestoreDone), stamped from the catalog's injected clock at update time.
	LastActiveAt time.Time `json:"last_active_at,omitzero"`
	// Status is the session's lifecycle phase (active until SessionStopped → stopped).
	Status SessionStatus `json:"status,omitempty"`
	// AgentKind names the agent role (from SessionStarted's ConfigFingerprint). It is
	// passthrough: empty until the agent threads its kind through loop.Config.
	AgentKind string `json:"agent_kind,omitempty"`
	// LoopCount is the number of loops registered in the session: the primary plus one
	// per LoopStarted.
	LoopCount int `json:"loop_count,omitempty"`
	// ConfigFingerprint is the config identity the session started under, for the
	// picker to surface a config change on restore.
	ConfigFingerprint event.ConfigFingerprint `json:"config_fingerprint,omitzero"`
}

// CatalogSetupError wraps a failure to provision or bind the catalog KV bucket in
// NewCatalog. It carries the bucket name and unwraps to the underlying NATS error so a
// caller can errors.As both this and the wrapped cause. It mirrors LeaseSetupError.
type CatalogSetupError struct {
	Bucket string
	Cause  error
}

func (e *CatalogSetupError) Error() string {
	return "journal: catalog bucket setup for " + strconv.Quote(e.Bucket) + ": " + e.Cause.Error()
}
func (e *CatalogSetupError) Unwrap() error { return e.Cause }

// CatalogReadError wraps a failure to read or decode a catalog entry (a KV Get/Keys
// error that is not "not found", or a malformed stored SessionMeta). It carries the
// bucket and (when known) the session, and unwraps to the cause. ListSessions and
// RepairCatalog surface it; the best-effort UpdateOnEvent logs+swallows it (it must
// never fail the append).
type CatalogReadError struct {
	Bucket    string
	SessionID uuid.UUID
	Cause     error
}

func (e *CatalogReadError) Error() string {
	return "journal: read catalog entry for session " + e.SessionID.String() +
		" in " + strconv.Quote(e.Bucket) + ": " + e.Cause.Error()
}
func (e *CatalogReadError) Unwrap() error { return e.Cause }

// CatalogWriteError wraps a failure to write a catalog entry (a KV Put/encode error).
// It carries the bucket and session and unwraps to the cause. The best-effort
// UpdateOnEvent logs+swallows it; RepairCatalog surfaces it (a repair the caller asked
// for that could not persist is a real failure).
type CatalogWriteError struct {
	Bucket    string
	SessionID uuid.UUID
	Cause     error
}

func (e *CatalogWriteError) Error() string {
	return "journal: write catalog entry for session " + e.SessionID.String() +
		" in " + strconv.Quote(e.Bucket) + ": " + e.Cause.Error()
}
func (e *CatalogWriteError) Unwrap() error { return e.Cause }

// CatalogEncodeError wraps a failure to marshal a SessionMeta to JSON. A SessionMeta
// is value-typed, so this is effectively unreachable, but the codec returns a typed
// error rather than dropping the json.Marshal error to satisfy errors-are-typed.
type CatalogEncodeError struct{ Cause error }

func (e *CatalogEncodeError) Error() string {
	return "journal: encode session meta: " + e.Cause.Error()
}
func (e *CatalogEncodeError) Unwrap() error { return e.Cause }

// CatalogClock is the time seam for the catalog: it stamps LastActiveAt at update
// time. Injecting it makes activity-bump assertions deterministic in tests. It mirrors
// event.Clock / LeaseClock.
type CatalogClock func() time.Time

// applyEvent folds one catalog-relevant event into a SessionMeta and reports whether
// the event changed it (false => the event is not catalog-relevant, so no upsert is
// needed). It is the single source of truth for the event→field mapping, shared by the
// inline UpdateOnEvent (read-modify-write one KV entry) and RepairCatalog (fold the
// whole stream then write once). It is PURE except for the injected now clock, so the
// mapping is unit-testable without a KV.
//
//   - SessionStarted: stamps SessionID, CreatedAt (the event's CreatedAt),
//     ConfigFingerprint, AgentKind (from the fingerprint — passthrough), Status=active,
//     and counts the primary loop (LoopCount := 1 if not already higher).
//   - TurnStarted: sets Title from the user message if not already set (first turn
//     wins), and bumps LastActiveAt.
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
// non-empty line of its concatenated text, truncated to titleMaxLen runes. A nil
// message or one with no text yields "" (the picker shows a placeholder). It never
// returns multi-line text — a title is a one-line preview.
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
// JSON, an unknown field, or trailing bytes — an ambiguous entry is a corrupt cache
// entry, surfaced as an error so the caller can repair rather than silently mis-list.
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

// errTrailingCatalogData is the leaf cause when a stored catalog entry has bytes after
// its JSON object. It carries no context fields, so a sentinel is permitted.
var errTrailingCatalogData = errors.New("journal: trailing data after session meta")

// catalogKey is the KV key for a session's catalog entry: the session id. UUID text is
// a valid KV key (only [-/_=.a-zA-Z0-9]).
func catalogKey(sessionID uuid.UUID) string { return sessionID.String() }

// CatalogLogger is the narrow logging seam the best-effort catalog update writes to
// when a KV read/write fails: the catalog is derivable, so a failure is logged and
// swallowed, NEVER surfaced to the append path. It is a single-method interface
// (Interface Segregation); a nop default keeps existing wiring unchanged.
type CatalogLogger interface {
	// CatalogUpdateFailed is called with the typed error when a best-effort catalog
	// update could not read or write its KV entry. The implementation must not panic
	// and must not re-raise — it is the end of the error's life.
	CatalogUpdateFailed(err error)
}

// nopCatalogLogger is the default CatalogLogger: it drops the error. It is the safe
// default so a caller that does not inject a logger never panics on a nil logger.
type nopCatalogLogger struct{}

func (nopCatalogLogger) CatalogUpdateFailed(error) {}

// Catalog maintains the derived session catalog in the catalogBucket KV. It has one
// reason to change: how the catalog is indexed. UpdateOnEvent folds a single event into
// the keyed entry (best-effort, post-append); ListSessions reads the bucket only (no
// stream consumer); RepairCatalog rebuilds an entry from the authoritative stream.
type Catalog struct {
	kv       nats.KeyValue
	bucket   string
	now      CatalogClock
	log      CatalogLogger
	replayer EventReplayer // read-side, for RepairCatalog's stream scan (may be nil → repair disabled)
}

// CatalogOption configures a Catalog at construction. Applied in order over a defaults
// struct, so a later option overrides an earlier one.
type CatalogOption func(*catalogOptions)

type catalogOptions struct {
	bucket   string
	now      CatalogClock
	log      CatalogLogger
	replayer EventReplayer
}

// WithCatalogBucket overrides the KV bucket name. An empty name is ignored (the default
// is kept), so the Catalog owns its invariant.
func WithCatalogBucket(name string) CatalogOption {
	return func(o *catalogOptions) {
		if name != "" {
			o.bucket = name
		}
	}
}

// WithCatalogClock injects the clock LastActiveAt is stamped from. A nil clock is
// ignored (time.Now is kept).
func WithCatalogClock(now CatalogClock) CatalogOption {
	return func(o *catalogOptions) {
		if now != nil {
			o.now = now
		}
	}
}

// WithCatalogLogger injects the logger best-effort update failures are reported to. A
// nil logger is ignored (the nop default is kept).
func WithCatalogLogger(log CatalogLogger) CatalogOption {
	return func(o *catalogOptions) {
		if log != nil {
			o.log = log
		}
	}
}

// NewCatalog provisions (creating if absent) the session-catalog KV bucket and returns
// a Catalog over it. js must be a bound JetStream context (the embedded server is
// started at the composition root, not here). The bucket keeps no history and never
// expires (unlike the lease bucket, a catalog entry has no TTL — a session's index
// survives until the session is explicitly deleted, which is out of scope).
func NewCatalog(js nats.JetStreamContext, opts ...CatalogOption) (*Catalog, error) {
	o := catalogOptions{bucket: catalogBucket, now: time.Now, log: nopCatalogLogger{}}
	for _, opt := range opts {
		opt(&o)
	}
	if js == nil {
		return nil, &CatalogSetupError{Bucket: o.bucket, Cause: errNilJetStream}
	}
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:   o.bucket,
		Storage:  nats.FileStorage,
		History:  1,
		Replicas: 1,
	})
	if err != nil {
		return nil, &CatalogSetupError{Bucket: o.bucket, Cause: err}
	}
	return &Catalog{kv: kv, bucket: o.bucket, now: o.now, log: o.log, replayer: o.replayer}, nil
}

// UpdateOnEvent folds ev into the session's catalog entry: it reads the current entry
// (or an empty one), applies the event's field mapping (applyEvent), and writes it back
// — but ONLY for a catalog-relevant event (a no-op event short-circuits before any KV
// I/O). It is BEST-EFFORT: any KV read/write/decode error is reported to the injected
// logger and swallowed (returns nil). It MUST NEVER fail the underlying append — the
// catalog is derivable, so a lost update is repaired later, never propagated. The
// returned error is always nil; the signature keeps a nil-error contract for the
// appender seam.
func (c *Catalog) UpdateOnEvent(ctx context.Context, ev event.Event) error {
	// Decide relevance on a zero meta first so a no-op event never touches the KV.
	if _, changed := applyEvent(SessionMeta{}, ev, c.now); !changed {
		return nil
	}
	sid := ev.EventHeader().SessionID

	current, err := c.load(sid)
	if err != nil {
		c.log.CatalogUpdateFailed(err)
		return nil
	}
	updated, _ := applyEvent(current, ev, c.now)
	if err := c.store(updated); err != nil {
		c.log.CatalogUpdateFailed(err)
		return nil
	}
	return nil
}

// load reads the session's catalog entry, returning a zero SessionMeta (no error) when
// the key is absent — the upsert path treats absence as "create". A read/decode error
// other than not-found is returned as a typed *CatalogReadError.
func (c *Catalog) load(sid uuid.UUID) (SessionMeta, error) {
	entry, err := c.kv.Get(catalogKey(sid))
	if errors.Is(err, nats.ErrKeyNotFound) {
		return SessionMeta{}, nil
	}
	if err != nil {
		return SessionMeta{}, &CatalogReadError{Bucket: c.bucket, SessionID: sid, Cause: err}
	}
	meta, derr := decodeSessionMeta(entry.Value())
	if derr != nil {
		return SessionMeta{}, &CatalogReadError{Bucket: c.bucket, SessionID: sid, Cause: derr}
	}
	return meta, nil
}

// store encodes and writes meta to its keyed catalog entry, returning a typed
// *CatalogWriteError on an encode or KV Put failure.
func (c *Catalog) store(meta SessionMeta) error {
	val, err := encodeSessionMeta(meta)
	if err != nil {
		return &CatalogWriteError{Bucket: c.bucket, SessionID: meta.SessionID, Cause: err}
	}
	if _, err := c.kv.Put(catalogKey(meta.SessionID), val); err != nil {
		return &CatalogWriteError{Bucket: c.bucket, SessionID: meta.SessionID, Cause: err}
	}
	return nil
}
