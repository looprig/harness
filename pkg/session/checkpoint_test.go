package session

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// checkpointOrderProbe shares one monotonic sequence between the Blobs Put wrapper
// (countingBlobs) and the event appender (checkpointAppender), so a test can PROVE the
// snapshot blob became durable (Put returned) BEFORE the WorkspaceCheckpointed event was
// appended. putSeq is stamped when a Put returns; appendSeq when the checkpoint event is
// appended; putCount tallies real (non-deduped) uploads.
type checkpointOrderProbe struct {
	seq       atomic.Int64
	putSeq    atomic.Int64 // sequence stamp of the last successful Blobs.Put (0 = never)
	appendSeq atomic.Int64 // sequence stamp of the WorkspaceCheckpointed append (0 = never)
	putCount  atomic.Int64 // number of successful Blobs.Put calls (dedup no-ops don't count)
}

// countingBlobs wraps a storage.Blobs, stamping each successful Put with the shared
// monotonic sequence AFTER the underlying Put returns — i.e. at the instant the blob is
// durable — so the order test compares that instant against the event append.
type countingBlobs struct {
	inner storage.Blobs
	probe *checkpointOrderProbe
}

func (b *countingBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	if err := b.inner.Put(ctx, key, r); err != nil {
		return err
	}
	b.probe.putCount.Add(1)
	b.probe.putSeq.Store(b.probe.seq.Add(1))
	return nil
}

func (b *countingBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return b.inner.Get(ctx, key)
}
func (b *countingBlobs) Delete(ctx context.Context, key string) error {
	return b.inner.Delete(ctx, key)
}
func (b *countingBlobs) List(ctx context.Context, prefix string) ([]string, error) {
	return b.inner.List(ctx, prefix)
}

// checkpointAppender is an eventAppender double that records every appended event AND
// stamps the shared sequence at the moment it appends a WorkspaceCheckpointed, so the
// order test can assert putSeq < appendSeq (snapshot-before-append).
type checkpointAppender struct {
	mu     sync.Mutex
	events []event.Event
	probe  *checkpointOrderProbe
}

func (a *checkpointAppender) AppendEvent(_ context.Context, ev event.Event) (uint64, error) {
	a.mu.Lock()
	a.events = append(a.events, ev)
	n := len(a.events)
	a.mu.Unlock()
	if _, ok := ev.(event.WorkspaceCheckpointed); ok {
		a.probe.appendSeq.Store(a.probe.seq.Add(1))
	}
	return uint64(n), nil
}

// checkpointFixture builds a workspacestore.Store over an in-memory Blobs (wrapped in
// probe when non-nil) plus a temp workspace root seeded with one file, so a
// CheckpointWorkspace has a real, non-empty tree to snapshot.
func checkpointFixture(t *testing.T, probe *checkpointOrderProbe) (*workspacestore.Store, string) {
	t.Helper()
	var blobs storage.Blobs = memstore.New().Blobs
	if probe != nil {
		blobs = &countingBlobs{inner: blobs, probe: probe}
	}
	ws, err := workspacestore.Open(blobs)
	if err != nil {
		t.Fatalf("workspacestore.Open: %v", err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("workspace contents"), 0o600); err != nil {
		t.Fatalf("seed workspace file: %v", err)
	}
	return ws, root
}

// countCheckpointed counts WorkspaceCheckpointed events in an appended slice.
func countCheckpointed(events []event.Event) int {
	n := 0
	for _, ev := range events {
		if _, ok := ev.(event.WorkspaceCheckpointed); ok {
			n++
		}
	}
	return n
}

// lastCheckpointed returns the LAST WorkspaceCheckpointed in an appended slice (the
// record tail the resume path reads back), if any.
func lastCheckpointed(events []event.Event) (event.WorkspaceCheckpointed, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if wc, ok := events[i].(event.WorkspaceCheckpointed); ok {
			return wc, true
		}
	}
	return event.WorkspaceCheckpointed{}, false
}

// TestCheckpointWorkspace proves the end-to-end capability: a session wired
// WithWorkspaceStore snapshots its configured root and records a WorkspaceCheckpointed
// (Ref == the returned ref) through the durable tap; an unconfigured session fails closed
// with *WorkspaceNotConfiguredError and records nothing.
func TestCheckpointWorkspace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		configured bool
	}{
		{name: "configured session snapshots and records WorkspaceCheckpointed", configured: true},
		{name: "unconfigured session fails closed", configured: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			rec := &recordingEventAppender{}
			opts := []Option{WithEventAppender(rec)}
			if tt.configured {
				ws, root := checkpointFixture(t, nil)
				opts = append(opts, WithWorkspaceStore(ws, root))
			}
			s, err := New(ctx, cfg(&stubLLM{}), opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			ref, err := s.CheckpointWorkspace(ctx)

			if !tt.configured {
				if ref != "" {
					t.Errorf("CheckpointWorkspace ref = %q, want empty on unconfigured session", ref)
				}
				var notConf *WorkspaceNotConfiguredError
				if !errors.As(err, &notConf) {
					t.Fatalf("err = %v, want *WorkspaceNotConfiguredError", err)
				}
				if got := countCheckpointed(rec.snapshot()); got != 0 {
					t.Errorf("appended %d WorkspaceCheckpointed on unconfigured session, want 0", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("CheckpointWorkspace: %v", err)
			}
			if ref == "" {
				t.Fatal("CheckpointWorkspace returned an empty Ref on the happy path")
			}
			if _, perr := workspacestore.ParseRef(string(ref)); perr != nil {
				t.Errorf("returned Ref %q is not grammar-valid: %v", ref, perr)
			}
			last, ok := lastCheckpointed(rec.snapshot())
			if !ok {
				t.Fatal("no WorkspaceCheckpointed appended through the durable tap")
			}
			if last.Ref != string(ref) {
				t.Errorf("appended WorkspaceCheckpointed.Ref = %q, want %q", last.Ref, ref)
			}
		})
	}
}

// TestCheckpointWorkspaceSnapshotBeforeAppend is the load-bearing ordering proof: the
// snapshot blob must be durably Put BEFORE the WorkspaceCheckpointed event is appended
// (so a crash between the two leaks an unreferenced blob, never a dangling ref). A shared
// monotonic sequence stamps the Put (when it returns) and the append; putSeq < appendSeq
// proves the order and FAILS if the two lines were reordered.
func TestCheckpointWorkspaceSnapshotBeforeAppend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	probe := &checkpointOrderProbe{}
	ws, root := checkpointFixture(t, probe)
	app := &checkpointAppender{probe: probe}

	s, err := New(ctx, cfg(&stubLLM{}), WithEventAppender(app), WithWorkspaceStore(ws, root))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	ref, err := s.CheckpointWorkspace(ctx)
	if err != nil {
		t.Fatalf("CheckpointWorkspace: %v", err)
	}
	if ref == "" {
		t.Fatal("CheckpointWorkspace returned an empty Ref")
	}

	putSeq := probe.putSeq.Load()
	appendSeq := probe.appendSeq.Load()
	if putSeq == 0 {
		t.Fatal("snapshot blob was never Put — cannot prove snapshot-before-append")
	}
	if appendSeq == 0 {
		t.Fatal("WorkspaceCheckpointed was never appended — cannot prove snapshot-before-append")
	}
	if putSeq >= appendSeq {
		t.Fatalf("snapshot Put (seq %d) did not precede WorkspaceCheckpointed append (seq %d): snapshot-before-append violated",
			putSeq, appendSeq)
	}
}

// TestCheckpointWorkspaceReCheckpointNoOpUpload proves the content-addressed dedup
// survives the session boundary: checkpointing the same unchanged tree twice returns the
// same Ref and the second Snapshot performs NO upload (the blob already exists).
func TestCheckpointWorkspaceReCheckpointNoOpUpload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	probe := &checkpointOrderProbe{}
	ws, root := checkpointFixture(t, probe)
	rec := &recordingEventAppender{}

	s, err := New(ctx, cfg(&stubLLM{}), WithEventAppender(rec), WithWorkspaceStore(ws, root))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	ref1, err := s.CheckpointWorkspace(ctx)
	if err != nil {
		t.Fatalf("first CheckpointWorkspace: %v", err)
	}
	afterFirst := probe.putCount.Load()

	ref2, err := s.CheckpointWorkspace(ctx)
	if err != nil {
		t.Fatalf("second CheckpointWorkspace: %v", err)
	}
	afterSecond := probe.putCount.Load()

	if ref1 != ref2 {
		t.Fatalf("re-checkpoint of unchanged tree returned different refs: %q then %q", ref1, ref2)
	}
	if afterFirst != 1 {
		t.Fatalf("first checkpoint Put count = %d, want exactly 1", afterFirst)
	}
	if afterSecond != afterFirst {
		t.Fatalf("second checkpoint re-uploaded (Put count %d -> %d), want a dedup no-op", afterFirst, afterSecond)
	}
}
