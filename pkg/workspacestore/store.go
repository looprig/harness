package workspacestore

import (
	"github.com/ciram-co/storekit"
)

// Store captures and restores a session's workspace tree as immutable,
// content-addressed snapshots over a storekit.Blobs backend. It holds only that
// backend and its resolved Options; every operation carries its own state, so a
// Store is as safe for concurrent use as the backend it wraps.
type Store struct {
	blobs storekit.Blobs
	opts  Options
}

// Options carries the resolved knobs that tune a Store. It is deliberately
// minimal: SpoolDir chooses where Snapshot spools its archive temp file. The
// extraction bounds that guard Materialize against hostile archives (max entries,
// max bytes) land with that method and are not present here yet.
type Options struct {
	// SpoolDir is the directory in which Snapshot creates its spool temp file. The
	// zero value (empty string) selects the operating system's default temp
	// directory. A large working set is spooled here in full, so point it at a
	// volume with room for one archive.
	SpoolDir string
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
	return &Store{blobs: b, opts: o}, nil
}

// NilBlobsError reports that Open was called with a nil Blobs backend. It carries
// no fields: the failure mode is fully described by its type, which callers match
// with errors.As.
type NilBlobsError struct{}

func (e *NilBlobsError) Error() string {
	return "workspacestore: nil Blobs backend"
}
