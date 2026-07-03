package workspacestore

import (
	"github.com/looprig/storekit"
)

// Store captures and restores a session's workspace tree as immutable,
// content-addressed snapshots over a storekit.Blobs backend. It holds only that
// backend and its resolved Options; every operation carries its own state, so a
// Store is as safe for concurrent use as the backend it wraps.
type Store struct {
	blobs storekit.Blobs
	opts  Options
}

// defaultMaxEntries is the entry-count ceiling Open applies when the caller
// leaves MaxEntries unset — 2^20 entries, enough for any realistic working tree
// while still bounding a decompression bomb that inflates to countless tiny
// entries.
const defaultMaxEntries int64 = 1 << 20

// defaultMaxBytes is the cumulative extracted-byte ceiling Open applies when the
// caller leaves MaxBytes unset — 8 GiB, a generous room for a working set while
// still capping a bomb that inflates to an unbounded byte count.
const defaultMaxBytes int64 = 8 << 30

// Options carries the resolved knobs that tune a Store: where Snapshot spools its
// archive temp file, and the two bounds that guard Materialize against a hostile
// archive (a decompression bomb inflating to too many entries or too many bytes).
type Options struct {
	// SpoolDir is the directory in which Snapshot creates its spool temp file. The
	// zero value (empty string) selects the operating system's default temp
	// directory. A large working set is spooled here in full, so point it at a
	// volume with room for one archive.
	SpoolDir string

	// MaxEntries caps how many entries Materialize will read from a snapshot
	// archive before failing closed with *ArchiveLimitError — the guard against a
	// bomb that inflates to an unbounded number of tiny entries. A zero or negative
	// value is resolved to defaultMaxEntries by Open.
	MaxEntries int64

	// MaxBytes caps the cumulative number of bytes Materialize will write while
	// extracting a snapshot archive before failing closed with *ArchiveLimitError.
	// It is enforced against bytes actually written, never a header's declared
	// size, so a lying size field cannot breach it. A zero or negative value is
	// resolved to defaultMaxBytes by Open.
	MaxBytes int64
}

// Option incrementally configures Options at Open time. Options are applied in
// argument order, so a later Option overrides an earlier one that sets the same
// field.
type Option func(*Options)

// WithSpoolDir directs Snapshot to spool its archive temp file under dir instead
// of the operating system's default temp directory.
func WithSpoolDir(dir string) Option {
	return func(o *Options) { o.SpoolDir = dir }
}

// WithMaxEntries sets the maximum number of entries Materialize will read from a
// snapshot archive before rejecting it as a decompression bomb. A zero or
// negative n restores the default (defaultMaxEntries).
func WithMaxEntries(n int64) Option {
	return func(o *Options) { o.MaxEntries = n }
}

// WithMaxBytes sets the maximum cumulative number of bytes Materialize will write
// while extracting a snapshot archive before rejecting it as a decompression
// bomb. A zero or negative n restores the default (defaultMaxBytes).
func WithMaxBytes(n int64) Option {
	return func(o *Options) { o.MaxBytes = n }
}

// Open returns a Store over the given Blobs backend with opts applied. A nil
// backend is rejected up front with *NilBlobsError — a Store has nowhere to put
// snapshot bytes without one, so Open fails closed rather than hand back a Store
// that panics on first Snapshot.
func Open(b storekit.Blobs, opts ...Option) (*Store, error) {
	if b == nil {
		return nil, &NilBlobsError{}
	}
	var o Options
	for _, opt := range opts {
		opt(&o)
	}
	if o.MaxEntries <= 0 {
		o.MaxEntries = defaultMaxEntries
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = defaultMaxBytes
	}
	return &Store{blobs: b, opts: o}, nil
}

// NilBlobsError reports that Open was called with a nil Blobs backend. It carries
// no fields: the failure mode is fully described by its type, which callers match
// with errors.As.
type NilBlobsError struct{}

func (e *NilBlobsError) Error() string {
	return "workspacestore: nil Blobs backend"
}
