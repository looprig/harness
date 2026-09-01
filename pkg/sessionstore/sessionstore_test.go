package sessionstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

type pathReporter struct {
	paths []string
}

func (r pathReporter) StoragePaths() []string { return r.paths }

type reportingLedger struct {
	storage.Ledger
	pathReporter
}

type reportingLeaser struct {
	storage.Leaser
	pathReporter
}

type reportingKV struct {
	storage.KV
	pathReporter
}

type reportingOrderedIndex struct {
	storage.OrderedIndex
	pathReporter
}

type reportingBlobs struct {
	storage.Blobs
	pathReporter
}

var errOpenKVDeadlineRequired = errors.New("sessionstore test: Open KV call requires a deadline")

type openDeadlineKV struct {
	storage.KV
	getSawDeadline bool
	putSawDeadline bool
}

func (k *openDeadlineKV) Get(ctx context.Context, key string) ([]byte, uint64, error) {
	deadline, ok := ctx.Deadline()
	remaining := time.Until(deadline)
	if !ok || ctx.Done() == nil || remaining <= 0 || remaining > openTimeout {
		return nil, 0, errOpenKVDeadlineRequired
	}
	k.getSawDeadline = true
	return k.KV.Get(ctx, key)
}

func (k *openDeadlineKV) Put(ctx context.Context, key string, expectedRevision uint64, value []byte) (uint64, error) {
	deadline, ok := ctx.Deadline()
	remaining := time.Until(deadline)
	if !ok || ctx.Done() == nil || remaining <= 0 || remaining > openTimeout {
		return 0, errOpenKVDeadlineRequired
	}
	k.putSawDeadline = true
	return k.KV.Put(ctx, key, expectedRevision, value)
}

func (b reportingBlobs) BlobReaderCloseBound() time.Duration {
	return b.Blobs.(storage.BlobReaderLifecycle).BlobReaderCloseBound()
}

// mustUUID parses the canonical 8-4-4-4-12 form or fails the test. It gives the
// name-derivation tests a fixed, readable id instead of a random one.
func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := id.UnmarshalText([]byte(s)); err != nil {
		t.Fatalf("UnmarshalText(%q) = %v", s, err)
	}
	return id
}

// TestOpen covers the backend-validation boundary: a fully-wired composite opens,
// and a nil composite or any nil primitive field is rejected with a typed
// *InvalidBackendError naming the missing piece (fail closed, no panic).
func TestOpen(t *testing.T) {
	t.Parallel()
	full := memstore.New()
	tests := []struct {
		name     string
		backend  *storage.Composite
		wantErr  bool
		wantMiss string
	}{
		{name: "full composite opens", backend: full, wantErr: false},
		{name: "nil composite rejected", backend: nil, wantErr: true, wantMiss: "composite"},
		{
			name:     "nil ledger rejected",
			backend:  &storage.Composite{Ledger: nil, Leaser: full.Leaser, KV: full.KV, OrderedIndex: full.OrderedIndex, Blobs: full.Blobs},
			wantErr:  true,
			wantMiss: "Ledger",
		},
		{
			name:     "nil leaser rejected",
			backend:  &storage.Composite{Ledger: full.Ledger, Leaser: nil, KV: full.KV, OrderedIndex: full.OrderedIndex, Blobs: full.Blobs},
			wantErr:  true,
			wantMiss: "Leaser",
		},
		{
			name:     "nil kv rejected",
			backend:  &storage.Composite{Ledger: full.Ledger, Leaser: full.Leaser, KV: nil, OrderedIndex: full.OrderedIndex, Blobs: full.Blobs},
			wantErr:  true,
			wantMiss: "KV",
		},
		{
			name:     "nil blobs rejected",
			backend:  &storage.Composite{Ledger: full.Ledger, Leaser: full.Leaser, KV: full.KV, OrderedIndex: full.OrderedIndex, Blobs: nil},
			wantErr:  true,
			wantMiss: "Blobs",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, err := Open(tt.backend)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Open() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var ibe *InvalidBackendError
				if !errors.As(err, &ibe) {
					t.Fatalf("Open() err = %v, want *InvalidBackendError", err)
				}
				if ibe.Missing != tt.wantMiss {
					t.Errorf("Missing = %q, want %q", ibe.Missing, tt.wantMiss)
				}
				if st != nil {
					t.Errorf("Open() store = %v, want nil on error", st)
				}
				return
			}
			if st == nil {
				t.Fatal("Open() store = nil, want non-nil on success")
			}
		})
	}
}

func TestOpenBoundsReleasedLayoutMarkerIO(t *testing.T) {
	t.Parallel()
	base := memstore.New()
	kv := &openDeadlineKV{KV: base.KV}
	backend, err := storage.NewCompositeWithOrderedIndex(
		base.Ledger,
		base.Leaser,
		kv,
		base.Blobs,
		base.OrderedIndex,
	)
	if err != nil {
		t.Fatalf("NewCompositeWithOrderedIndex() error = %v", err)
	}
	store, err := Open(backend)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if store == nil {
		t.Fatal("Open() store = nil")
	}
	if !kv.getSawDeadline || !kv.putSawDeadline {
		t.Fatalf("layout marker deadlines = (Get %v, Put %v), want both true", kv.getSawDeadline, kv.putSawDeadline)
	}
}

func TestOpenOwnsOneBackendSnapshotAcrossFacadeAndDurableOperations(t *testing.T) {
	caller := memstore.New()
	original := *caller
	store, err := Open(caller, WithOffloadThreshold(1))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	replacement := memstore.New()
	caller.Ledger = replacement.Ledger
	caller.Leaser = replacement.Leaser
	caller.KV = replacement.KV
	caller.OrderedIndex = replacement.OrderedIndex
	caller.Blobs = replacement.Blobs

	if store.backend.Ledger != original.Ledger || store.backend.Leaser != original.Leaser ||
		store.backend.OrderedIndex != original.OrderedIndex || store.backend.Blobs != original.Blobs {
		t.Fatal("Store backend interfaces changed when the caller mutated its Composite after Open")
	}
	bounded, ok := store.backend.KV.(boundedKV)
	if !ok || bounded.KV != original.KV {
		t.Fatalf("Store KV snapshot = %T, want bounded wrapper over original KV", store.backend.KV)
	}

	id := newTestUUID(t)
	lease, err := store.AcquireLease(context.Background(), id)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	writer, err := store.OpenJournal(context.Background(), id, lease)
	if err != nil {
		t.Fatalf("OpenJournal() error = %v", err)
	}
	record := commandRecordWithEncodedSize(t, id, 1024)
	if _, err := writer.Append(context.Background(), record); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	replayer, err := store.OpenInternalRecordReplayer(id, ReplayRequest{FromSeq: 2})
	if err != nil {
		t.Fatalf("OpenInternalRecordReplayer() error = %v", err)
	}
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{})
	if err != nil {
		t.Fatalf("RecordReplayer.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = cursor.Close() })
	if _, seq, err := cursor.Next(context.Background()); err != nil || seq != 2 {
		t.Fatalf("snapshot replay = (seq %d, err %v), want seq 2 success", seq, err)
	}
	if tip, err := replacement.Ledger.Tip(context.Background(), ledgerName(id)); err != nil || tip != 0 {
		t.Fatalf("replacement ledger tip = (tip %d, err %v), want untouched tip 0", tip, err)
	}
}

// TestOpenOptions covers option resolution: Open applies the 512 KiB default and a
// positive WithOffloadThreshold overrides it, while a non-positive value is ignored
// (the option owns its invariant, mirroring the journal lease options).
func TestOpenOptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts []Option
		want int
	}{
		{name: "default threshold", opts: nil, want: defaultOffloadThreshold},
		{name: "positive override applied", opts: []Option{WithOffloadThreshold(4096)}, want: 4096},
		{name: "zero override ignored", opts: []Option{WithOffloadThreshold(0)}, want: defaultOffloadThreshold},
		{name: "negative override ignored", opts: []Option{WithOffloadThreshold(-1)}, want: defaultOffloadThreshold},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, err := Open(memstore.New(), tt.opts...)
			if err != nil {
				t.Fatalf("Open() err = %v", err)
			}
			if st.opts.OffloadThreshold != tt.want {
				t.Errorf("OffloadThreshold = %d, want %d", st.opts.OffloadThreshold, tt.want)
			}
		})
	}
}

func TestPersistencePaths(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	first := filepath.Join(base, "a")
	second := filepath.Join(base, "b")
	for _, path := range []string{first, second} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("Mkdir(%q): %v", path, err)
		}
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(first, alias); err != nil {
		t.Fatalf("Symlink(%q, %q): %v", first, alias, err)
	}
	wantFirst, err := filepath.EvalSymlinks(first)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", first, err)
	}
	wantSecond, err := filepath.EvalSymlinks(second)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", second, err)
	}
	missingTail := filepath.Join(alias, "missing", "tail")
	wantMissingTail := filepath.Join(wantFirst, "missing", "tail")
	broken := filepath.Join(base, "broken")
	if err := os.Symlink(filepath.Join(base, "absent"), broken); err != nil {
		t.Fatalf("Symlink(broken): %v", err)
	}

	local := memstore.New()
	tests := []struct {
		name    string
		backend *storage.Composite
		want    []string
		wantErr bool
	}{
		{
			name:    "remote providers report none",
			backend: memstore.New(),
			want:    nil,
		},
		{
			name: "empty reporter paths report none",
			backend: &storage.Composite{
				Ledger: reportingLedger{
					Ledger:       local.Ledger,
					pathReporter: pathReporter{paths: []string{""}},
				},
				Leaser:       local.Leaser,
				KV:           local.KV,
				OrderedIndex: local.OrderedIndex,
				Blobs:        local.Blobs,
			},
			want: nil,
		},
		{
			name: "reporter paths are canonical sorted and deduplicated",
			backend: &storage.Composite{
				Ledger: reportingLedger{
					Ledger:       local.Ledger,
					pathReporter: pathReporter{paths: []string{second, alias}},
				},
				Leaser: reportingLeaser{
					Leaser:       local.Leaser,
					pathReporter: pathReporter{paths: []string{filepath.Join(first, "."), second}},
				},
				KV: reportingKV{
					KV:           local.KV,
					pathReporter: pathReporter{paths: []string{filepath.Join(base, "a", "..", "b")}},
				},
				OrderedIndex: local.OrderedIndex,
				Blobs: reportingBlobs{
					Blobs:        local.Blobs,
					pathReporter: pathReporter{paths: []string{""}},
				},
			},
			want: []string{wantFirst, wantSecond},
		},
		{
			name: "missing tail below symlink ancestor is canonicalized",
			backend: &storage.Composite{
				Ledger: reportingLedger{
					Ledger:       local.Ledger,
					pathReporter: pathReporter{paths: []string{missingTail}},
				},
				Leaser:       local.Leaser,
				KV:           local.KV,
				OrderedIndex: local.OrderedIndex,
				Blobs:        local.Blobs,
			},
			want: []string{wantMissingTail},
		},
		{
			name: "broken symlink fails closed",
			backend: &storage.Composite{
				Ledger: reportingLedger{
					Ledger:       local.Ledger,
					pathReporter: pathReporter{paths: []string{filepath.Join(broken, "tail")}},
				},
				Leaser:       local.Leaser,
				KV:           local.KV,
				OrderedIndex: local.OrderedIndex,
				Blobs:        local.Blobs,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			st, err := Open(tt.backend)
			if err != nil {
				t.Fatalf("Open() err = %v", err)
			}
			got, err := st.PersistencePaths()
			if (err != nil) != tt.wantErr {
				t.Fatalf("PersistencePaths() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var pathErr *PersistencePathError
				if !errors.As(err, &pathErr) {
					t.Fatalf("PersistencePaths() err = %T %v, want *PersistencePathError", err, err)
				}
				if got != nil {
					t.Errorf("PersistencePaths() paths = %v on error, want nil", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("PersistencePaths() = %v, want %v", got, tt.want)
			}
			if len(got) == 0 {
				return
			}
			got[0] = filepath.Join(base, "mutated")
			next, err := st.PersistencePaths()
			if err != nil {
				t.Fatalf("PersistencePaths() after caller mutation err = %v", err)
			}
			if !reflect.DeepEqual(next, tt.want) {
				t.Errorf("PersistencePaths() after caller mutation = %v, want %v", next, tt.want)
			}
		})
	}
}

func TestPersistencePathsUsesOwnedOrderedIndexReporter(t *testing.T) {
	t.Parallel()

	indexPath := t.TempDir()
	original := memstore.New()
	backend := &storage.Composite{
		Ledger: original.Ledger,
		Leaser: original.Leaser,
		KV:     original.KV,
		Blobs:  original.Blobs,
		OrderedIndex: reportingOrderedIndex{
			OrderedIndex: original.OrderedIndex,
			pathReporter: pathReporter{paths: []string{indexPath}},
		},
	}
	store, err := Open(backend)
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}

	// The facade owns the shallow snapshot made by Open, so replacing every
	// interface in the caller's Composite cannot hide an index-only local path.
	replacement := memstore.New()
	backend.Ledger = replacement.Ledger
	backend.Leaser = replacement.Leaser
	backend.KV = replacement.KV
	backend.OrderedIndex = replacement.OrderedIndex
	backend.Blobs = replacement.Blobs

	want, err := filepath.EvalSymlinks(indexPath)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", indexPath, err)
	}
	got, err := store.PersistencePaths()
	if err != nil {
		t.Fatalf("PersistencePaths() err = %v", err)
	}
	if !reflect.DeepEqual(got, []string{want}) {
		t.Fatalf("PersistencePaths() = %v, want owned OrderedIndex path %q", got, want)
	}
}

// TestSessionName covers the ledger-name derivation: "sessions/<uuid>" for any id
// (including the zero uuid), and that every derived name passes storage.ValidateName
// so a session can never address an invalid backend location.
func TestSessionName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		id   uuid.UUID
		want string
	}{
		{
			name: "zero uuid",
			id:   uuid.UUID{},
			want: "sessions/00000000-0000-0000-0000-000000000000",
		},
		{
			name: "fixed uuid",
			id:   mustUUID(t, "0123abcd-4567-4890-8abc-def012345678"),
			want: "sessions/0123abcd-4567-4890-8abc-def012345678",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ledgerName(tt.id); got != tt.want {
				t.Errorf("ledgerName() = %q, want %q", got, tt.want)
			}
			name, err := sessionName(tt.id)
			if err != nil {
				t.Fatalf("sessionName() err = %v, want nil", err)
			}
			if name != tt.want {
				t.Errorf("sessionName() = %q, want %q", name, tt.want)
			}
			if err := storage.ValidateName(name); err != nil {
				t.Errorf("ValidateName(%q) = %v, want nil", name, err)
			}
		})
	}
}

// TestPersistencePathsUsesWrappedKVReporter observes the KV's local root
// UNIQUELY: no other primitive reports a path, so the reported set is the KV's
// contribution alone.
//
// KV is the one primitive Open wraps (boundedKV, to bound provider I/O), and
// wrapping embeds the storage.KV INTERFACE, which does not promote the concrete
// provider's optional StoragePaths. Without boundedKV's explicit forwarder,
// PersistencePaths' storage.PathReporter assertion silently stops seeing the
// KV — a refactor disabling a source-derived guard without touching it. The
// shared TestPersistencePaths cannot catch that: there the KV reports a path the
// Ledger and Leaser already report, so its contribution is never observed on its
// own.
func TestPersistencePathsUsesWrappedKVReporter(t *testing.T) {
	t.Parallel()

	kvPath := t.TempDir()
	local := memstore.New()
	backend := &storage.Composite{
		Ledger: local.Ledger,
		Leaser: local.Leaser,
		KV: reportingKV{
			KV:           local.KV,
			pathReporter: pathReporter{paths: []string{kvPath}},
		},
		OrderedIndex: local.OrderedIndex,
		Blobs:        local.Blobs,
	}
	store, err := Open(backend)
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}
	want, err := filepath.EvalSymlinks(kvPath)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", kvPath, err)
	}
	got, err := store.PersistencePaths()
	if err != nil {
		t.Fatalf("PersistencePaths() err = %v", err)
	}
	if !reflect.DeepEqual(got, []string{want}) {
		t.Fatalf("PersistencePaths() = %v, want the wrapped KV's own path %q", got, want)
	}
}
