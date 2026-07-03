package workspacestore

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/looprig/storekit"
	"github.com/looprig/storekit/memstore"
)

// deleteCountingBlobs wraps a storekit.Blobs and counts Delete invocations so a
// test can prove GC issues exactly one Delete per swept snapshot — and none on a
// re-sweep of an already-clean set or on an already-cancelled run. Put/Get/List
// are promoted from the embedded interface unchanged.
type deleteCountingBlobs struct {
	storekit.Blobs
	deletes atomic.Int64
}

func (d *deleteCountingBlobs) Delete(ctx context.Context, key string) error {
	d.deletes.Add(1)
	return d.Blobs.Delete(ctx, key)
}

// listFailBlobs fails every List with a fixed error so a test can prove a Blobs
// enumeration failure surfaces as an unwrap-able *GCError with Op "list".
// Put/Get/Delete are promoted from the embedded backend.
type listFailBlobs struct {
	storekit.Blobs
	err error
}

func (l *listFailBlobs) List(ctx context.Context, prefix string) ([]string, error) {
	return nil, l.err
}

// deleteFailBlobs fails every Delete with a fixed error so a test can prove a
// Blobs delete failure surfaces as an unwrap-able *GCError carrying Op "delete"
// and the offending Ref. List/Put/Get are promoted from the embedded backend, so
// the store can still be seeded and enumerated.
type deleteFailBlobs struct {
	storekit.Blobs
	err error
}

func (d *deleteFailBlobs) Delete(ctx context.Context, key string) error {
	return d.err
}

// seedSnapshots snapshots n distinct one-file trees through s and returns their
// Refs. Distinct file content yields distinct archives, hence distinct sha256
// Refs and distinct blob keys — so the caller can freely partition them into
// live and non-live sets.
func seedSnapshots(t *testing.T, s *Store, n int) []Ref {
	t.Helper()
	refs := make([]Ref, n)
	for i := 0; i < n; i++ {
		root := t.TempDir()
		buildTree(t, root, []node{
			{path: "f.txt", kind: kindFile, content: "tree-" + strconv.Itoa(i) + "\n", mode: 0o644},
		})
		ref, err := s.Snapshot(context.Background(), root)
		if err != nil {
			t.Fatalf("seed Snapshot #%d: %v", i, err)
		}
		refs[i] = ref
	}
	return refs
}

// listBlobKeys returns the set of blob keys currently under blobKeyPrefix.
func listBlobKeys(t *testing.T, b storekit.Blobs) map[string]struct{} {
	t.Helper()
	keys, err := b.List(context.Background(), blobKeyPrefix)
	if err != nil {
		t.Fatalf("List(%q): %v", blobKeyPrefix, err)
	}
	set := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		set[k] = struct{}{}
	}
	return set
}

// refSetEqual reports whether the deleted slice is exactly want as a set: same
// membership, and no duplicates (GC must report each swept Ref once).
func refSetEqual(got []Ref, want map[Ref]struct{}) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[Ref]struct{}, len(got))
	for _, r := range got {
		if _, dup := seen[r]; dup {
			return false
		}
		if _, ok := want[r]; !ok {
			return false
		}
		seen[r] = struct{}{}
	}
	return true
}

// strSetEqual reports whether two string sets have identical membership.
func strSetEqual(got, want map[string]struct{}) bool {
	if len(got) != len(want) {
		return false
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			return false
		}
	}
	return true
}

// TestGCLiveSetSweep is the core mark-and-sweep contract: GC deletes exactly the
// snapshot blobs whose Ref is not in the caller's live set, leaves the live ones,
// and returns precisely the Refs it removed (order-independent).
func TestGCLiveSetSweep(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		total   int
		liveIdx []int
	}{
		{name: "partial live keeps the live, sweeps the rest", total: 4, liveIdx: []int{0, 2}},
		{name: "all live deletes nothing", total: 3, liveIdx: []int{0, 1, 2}},
		{name: "none live sweeps everything", total: 3, liveIdx: nil},
		{name: "empty store is a no-op", total: 0, liveIdx: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			blobs := memstore.New().Blobs
			s, err := Open(blobs)
			if err != nil {
				t.Fatalf("Open(): %v", err)
			}
			refs := seedSnapshots(t, s, tt.total)

			live := make(map[Ref]struct{})
			liveKeys := make(map[string]struct{})
			for _, idx := range tt.liveIdx {
				live[refs[idx]] = struct{}{}
				liveKeys[refs[idx].blobKey()] = struct{}{}
			}
			wantDeleted := make(map[Ref]struct{})
			for _, r := range refs {
				if _, ok := live[r]; !ok {
					wantDeleted[r] = struct{}{}
				}
			}

			deleted, err := s.GC(context.Background(), live)
			if err != nil {
				t.Fatalf("GC(): %v", err)
			}
			if !refSetEqual(deleted, wantDeleted) {
				t.Errorf("GC() deleted = %v, want set %v", deleted, wantDeleted)
			}

			// Exactly the live blobs remain; every non-live blob is gone.
			remaining := listBlobKeys(t, blobs)
			if !strSetEqual(remaining, liveKeys) {
				t.Errorf("remaining keys = %v, want %v", remaining, liveKeys)
			}
		})
	}
}

// TestGCFailSecureForeignKey is the load-bearing safety test: a key under the
// workspaces/ prefix that does not parse as a v1 Ref must never be deleted, even
// under an empty live set — GC only ever sweeps blobs it recognizes as its own
// snapshots. A genuine snapshot alongside it is still swept, proving GC is active.
func TestGCFailSecureForeignKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		foreignKey string
	}{
		{name: "non-hex suffix", foreignKey: blobKeyPrefix + "not-a-valid-ref"},
		{name: "hex one short of 64", foreignKey: blobKeyPrefix + strings.Repeat("a", refHexLen-1)},
		{name: "hex one over 64", foreignKey: blobKeyPrefix + strings.Repeat("a", refHexLen+1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Sanity: the foreign key must be a storable name but not a valid Ref,
			// otherwise the test would not exercise the fail-secure branch.
			if _, err := ParseRef(refPrefix + strings.TrimPrefix(tt.foreignKey, blobKeyPrefix)); err == nil {
				t.Fatalf("test bug: %q parses as a valid Ref", tt.foreignKey)
			}

			blobs := memstore.New().Blobs
			s, err := Open(blobs)
			if err != nil {
				t.Fatalf("Open(): %v", err)
			}

			victim := seedSnapshots(t, s, 1)[0]

			if err := blobs.Put(context.Background(), tt.foreignKey, strings.NewReader("foreign")); err != nil {
				t.Fatalf("seed foreign key %q: %v", tt.foreignKey, err)
			}

			deleted, err := s.GC(context.Background(), map[Ref]struct{}{})
			if err != nil {
				t.Fatalf("GC(): %v", err)
			}

			// The genuine snapshot is the only thing GC reports deleting.
			if len(deleted) != 1 || deleted[0] != victim {
				t.Fatalf("GC() deleted = %v, want exactly [%q]", deleted, victim)
			}
			for _, r := range deleted {
				if r.blobKey() == tt.foreignKey {
					t.Errorf("GC reported the foreign key %q as deleted", tt.foreignKey)
				}
			}

			// The foreign key survives; the non-live snapshot is gone.
			remaining := listBlobKeys(t, blobs)
			if _, ok := remaining[tt.foreignKey]; !ok {
				t.Errorf("GC deleted foreign key %q — a key it does not recognize must be left untouched", tt.foreignKey)
			}
			if _, ok := remaining[victim.blobKey()]; ok {
				t.Errorf("GC left non-live snapshot %q in place", victim)
			}
		})
	}
}

// TestGCIdempotent proves a second GC with the same (empty) live set is a no-op:
// nothing left to delete, and — via the Delete counter — no Delete calls at all
// on the second pass.
func TestGCIdempotent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		total int
	}{
		{name: "several snapshots swept then re-swept", total: 3},
		{name: "single snapshot", total: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dc := &deleteCountingBlobs{Blobs: memstore.New().Blobs}
			s, err := Open(dc)
			if err != nil {
				t.Fatalf("Open(): %v", err)
			}
			seedSnapshots(t, s, tt.total)

			first, err := s.GC(context.Background(), map[Ref]struct{}{})
			if err != nil {
				t.Fatalf("first GC(): %v", err)
			}
			if len(first) != tt.total {
				t.Fatalf("first GC deleted %d refs, want %d", len(first), tt.total)
			}
			afterFirst := dc.deletes.Load()
			if afterFirst != int64(tt.total) {
				t.Fatalf("first GC issued %d Delete calls, want %d", afterFirst, tt.total)
			}

			second, err := s.GC(context.Background(), map[Ref]struct{}{})
			if err != nil {
				t.Fatalf("second GC(): %v", err)
			}
			if len(second) != 0 {
				t.Errorf("second GC deleted %v, want none", second)
			}
			if extra := dc.deletes.Load() - afterFirst; extra != 0 {
				t.Errorf("second GC issued %d extra Delete calls, want 0", extra)
			}
		})
	}
}

// TestGCContextCancelled documents the cancellation contract: an already-cancelled
// ctx makes GC return promptly with a context error and delete nothing — GC checks
// the ctx before listing and before every delete, so no blob is removed under a
// dead context, whether or not the store holds snapshots.
func TestGCContextCancelled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		total int
	}{
		{name: "cancelled with snapshots present", total: 3},
		{name: "cancelled on an empty store", total: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dc := &deleteCountingBlobs{Blobs: memstore.New().Blobs}
			s, err := Open(dc)
			if err != nil {
				t.Fatalf("Open(): %v", err)
			}
			seedSnapshots(t, s, tt.total)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			deleted, err := s.GC(ctx, map[Ref]struct{}{})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("GC() error = %v, want context.Canceled", err)
			}
			if len(deleted) != 0 {
				t.Errorf("cancelled GC deleted %v, want none", deleted)
			}
			if got := dc.deletes.Load(); got != 0 {
				t.Errorf("cancelled GC issued %d Delete calls, want 0", got)
			}
		})
	}
}

// TestGCListError proves a Blobs enumeration failure surfaces as an unwrap-able
// *GCError tagged Op "list" with no Ref, and that no Refs are reported deleted.
func TestGCListError(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("blobs list failed")

	tests := []struct {
		name  string
		cause error
	}{
		{name: "list failure surfaces as GCError", cause: errBoom},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lb := &listFailBlobs{Blobs: memstore.New().Blobs, err: tt.cause}
			s, err := Open(lb)
			if err != nil {
				t.Fatalf("Open(): %v", err)
			}

			deleted, err := s.GC(context.Background(), map[Ref]struct{}{})
			if deleted != nil {
				t.Errorf("GC() deleted = %v, want nil on a list failure", deleted)
			}
			var ge *GCError
			if !errors.As(err, &ge) {
				t.Fatalf("GC() error = %v (%T), want *GCError", err, err)
			}
			if ge.Op != "list" {
				t.Errorf("GCError.Op = %q, want %q", ge.Op, "list")
			}
			if ge.Ref != "" {
				t.Errorf("GCError.Ref = %q, want empty on a list failure", ge.Ref)
			}
			if !errors.Is(err, tt.cause) {
				t.Errorf("GC() error does not unwrap to the injected cause %v", tt.cause)
			}
		})
	}
}

// TestGCDeleteError proves a Blobs delete failure surfaces as an unwrap-able
// *GCError tagged Op "delete" and carrying the offending (valid) Ref, and that GC
// fails closed on the first failing Delete without reporting a phantom deletion.
func TestGCDeleteError(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("blobs delete failed")

	tests := []struct {
		name  string
		cause error
	}{
		{name: "delete failure surfaces as GCError", cause: errBoom},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Seed through the raw backend, then wrap it so every Delete fails.
			base := memstore.New().Blobs
			seedStore, err := Open(base)
			if err != nil {
				t.Fatalf("Open(seed): %v", err)
			}
			seedSnapshots(t, seedStore, 2)

			db := &deleteFailBlobs{Blobs: base, err: tt.cause}
			s, err := Open(db)
			if err != nil {
				t.Fatalf("Open(): %v", err)
			}

			deleted, err := s.GC(context.Background(), map[Ref]struct{}{})
			var ge *GCError
			if !errors.As(err, &ge) {
				t.Fatalf("GC() error = %v (%T), want *GCError", err, err)
			}
			if ge.Op != "delete" {
				t.Errorf("GCError.Op = %q, want %q", ge.Op, "delete")
			}
			if _, perr := ParseRef(string(ge.Ref)); perr != nil {
				t.Errorf("GCError.Ref = %q is not a valid Ref: %v", ge.Ref, perr)
			}
			if !errors.Is(err, tt.cause) {
				t.Errorf("GC() error does not unwrap to the injected cause %v", tt.cause)
			}
			if len(deleted) != 0 {
				t.Errorf("GC() deleted = %v, want none when the first Delete fails", deleted)
			}
		})
	}
}
