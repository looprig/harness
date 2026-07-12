package sessionruntime

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

// queued reports the current queue length under the coordinator mutex. It is a
// test-only inspection seam used to build deterministic barriers (await an acquirer
// to actually enqueue) without sleeps.
func (c *workspaceCoordinator) queued() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue)
}

// counts snapshots the coordinator's active-permit state under the mutex.
func (c *workspaceCoordinator) counts() (shared int, exclusive bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sharedCount, c.exclusive
}

// awaitQueued spins (yielding, never sleeping) until the queue reaches length n. The
// transition is guaranteed to happen (an Acquire always enqueues before blocking), so
// this is deterministic; the large bound only guards against a genuine hang.
func awaitQueued(t *testing.T, c *workspaceCoordinator, n int) {
	t.Helper()
	for i := 0; i < 5_000_000; i++ {
		if c.queued() == n {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("timed out awaiting queue length %d (have %d)", n, c.queued())
}

// staticHealth is a fixed LeaseHealth used in health tests.
type staticHealth struct{ err error }

func (h staticHealth) Healthy() error { return h.err }

func mustAcquire(t *testing.T, c *workspaceCoordinator, op tool.WorkspaceOperation, path string) tool.WorkspacePermit {
	t.Helper()
	p, err := c.Acquire(context.Background(), op, path)
	if err != nil {
		t.Fatalf("Acquire(%v, %q) error = %v", op, path, err)
	}
	return p
}

func TestWorkspaceCoordinatorAcquireValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		op        tool.WorkspaceOperation
		path      string
		wantErr   bool
		wantPath  bool // expect *WorkspacePathError
		wantOpErr bool // expect *InvalidWorkspaceOperationError
	}{
		{name: "path mutation valid", op: tool.WorkspaceOperationPathMutation, path: "/ws/a.txt"},
		{name: "whole mutation valid", op: tool.WorkspaceOperationWholeMutation, path: ""},
		{name: "checkpoint valid", op: tool.WorkspaceOperationCheckpoint, path: ""},
		{name: "path mutation empty path", op: tool.WorkspaceOperationPathMutation, path: "", wantErr: true, wantPath: true},
		{name: "whole mutation non-empty path", op: tool.WorkspaceOperationWholeMutation, path: "/ws/a.txt", wantErr: true, wantPath: true},
		{name: "checkpoint non-empty path", op: tool.WorkspaceOperationCheckpoint, path: "/ws/a.txt", wantErr: true, wantPath: true},
		{name: "unknown operation zero", op: tool.WorkspaceOperation(0), path: "", wantErr: true, wantOpErr: true},
		{name: "unknown operation high", op: tool.WorkspaceOperation(99), path: "", wantErr: true, wantOpErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newWorkspaceCoordinator(nil)
			p, err := c.Acquire(context.Background(), tt.op, tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Acquire() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if p != nil {
					t.Fatalf("Acquire() returned a permit alongside error")
				}
				var pathErr *WorkspacePathError
				var opErr *InvalidWorkspaceOperationError
				if tt.wantPath && !errors.As(err, &pathErr) {
					t.Fatalf("error = %T, want *WorkspacePathError", err)
				}
				if tt.wantOpErr && !errors.As(err, &opErr) {
					t.Fatalf("error = %T, want *InvalidWorkspaceOperationError", err)
				}
				return
			}
			if p == nil {
				t.Fatalf("Acquire() returned nil permit without error")
			}
			p.Release()
		})
	}
}

// Different canonical paths do NOT serialize: acquiring path B while path A is still
// held returns immediately, and both permits are active concurrently.
func TestWorkspaceCoordinatorDifferentPathsConcurrent(t *testing.T) {
	t.Parallel()
	c := newWorkspaceCoordinator(nil)
	pa := mustAcquire(t, c, tool.WorkspaceOperationPathMutation, "/ws/a.txt")
	pb := mustAcquire(t, c, tool.WorkspaceOperationPathMutation, "/ws/b.txt")
	if shared, _ := c.counts(); shared != 2 {
		t.Fatalf("sharedCount = %d, want 2 (different paths concurrent)", shared)
	}
	pa.Release()
	pb.Release()
	if shared, _ := c.counts(); shared != 0 {
		t.Fatalf("sharedCount = %d, want 0 after release", shared)
	}
}

// The SAME canonical path serializes: a second acquirer of path A blocks until the
// first releases.
func TestWorkspaceCoordinatorSamePathSerializes(t *testing.T) {
	t.Parallel()
	c := newWorkspaceCoordinator(nil)
	first := mustAcquire(t, c, tool.WorkspaceOperationPathMutation, "/ws/a.txt")

	granted := make(chan tool.WorkspacePermit, 1)
	go func() {
		p := mustAcquire(t, c, tool.WorkspaceOperationPathMutation, "/ws/a.txt")
		granted <- p
	}()
	awaitQueued(t, c, 1) // the second acquirer is blocked

	select {
	case <-granted:
		t.Fatal("second same-path acquirer was granted while the first was held")
	default:
	}

	first.Release()
	second := <-granted // now it proceeds
	second.Release()
}

// A WholeMutation permit is exclusive against a PathMutation, in both orders.
func TestWorkspaceCoordinatorWholeExcludesPath(t *testing.T) {
	t.Parallel()

	t.Run("whole blocks path", func(t *testing.T) {
		t.Parallel()
		c := newWorkspaceCoordinator(nil)
		whole := mustAcquire(t, c, tool.WorkspaceOperationWholeMutation, "")
		granted := make(chan tool.WorkspacePermit, 1)
		go func() { granted <- mustAcquire(t, c, tool.WorkspaceOperationPathMutation, "/ws/a.txt") }()
		awaitQueued(t, c, 1)
		select {
		case <-granted:
			t.Fatal("path mutation granted while whole-workspace permit held")
		default:
		}
		whole.Release()
		(<-granted).Release()
	})

	t.Run("path blocks whole", func(t *testing.T) {
		t.Parallel()
		c := newWorkspaceCoordinator(nil)
		path := mustAcquire(t, c, tool.WorkspaceOperationPathMutation, "/ws/a.txt")
		granted := make(chan tool.WorkspacePermit, 1)
		go func() { granted <- mustAcquire(t, c, tool.WorkspaceOperationWholeMutation, "") }()
		awaitQueued(t, c, 1)
		select {
		case <-granted:
			t.Fatal("whole-workspace permit granted while a path mutation held")
		default:
		}
		path.Release()
		(<-granted).Release()
	})
}

// A Checkpoint permit excludes EVERY mutation (path and whole), and every mutation
// excludes a checkpoint.
func TestWorkspaceCoordinatorCheckpointExcludesAll(t *testing.T) {
	t.Parallel()

	t.Run("checkpoint blocks path and whole", func(t *testing.T) {
		t.Parallel()
		c := newWorkspaceCoordinator(nil)
		ckpt := mustAcquire(t, c, tool.WorkspaceOperationCheckpoint, "")
		gotPath := make(chan tool.WorkspacePermit, 1)
		gotWhole := make(chan tool.WorkspacePermit, 1)
		go func() { gotPath <- mustAcquire(t, c, tool.WorkspaceOperationPathMutation, "/ws/a.txt") }()
		go func() { gotWhole <- mustAcquire(t, c, tool.WorkspaceOperationWholeMutation, "") }()
		awaitQueued(t, c, 2)
		select {
		case <-gotPath:
			t.Fatal("path mutation granted during checkpoint")
		case <-gotWhole:
			t.Fatal("whole mutation granted during checkpoint")
		default:
		}
		ckpt.Release()
		// The path and whole waiters mutually exclude, so exactly one is granted at a
		// time; drain both in whichever order they become ready (releasing each lets
		// the other proceed).
		for i := 0; i < 2; i++ {
			select {
			case p := <-gotPath:
				p.Release()
			case p := <-gotWhole:
				p.Release()
			}
		}
	})

	t.Run("path blocks checkpoint", func(t *testing.T) {
		t.Parallel()
		c := newWorkspaceCoordinator(nil)
		path := mustAcquire(t, c, tool.WorkspaceOperationPathMutation, "/ws/a.txt")
		granted := make(chan tool.WorkspacePermit, 1)
		go func() { granted <- mustAcquire(t, c, tool.WorkspaceOperationCheckpoint, "") }()
		awaitQueued(t, c, 1)
		select {
		case <-granted:
			t.Fatal("checkpoint granted while a path mutation held")
		default:
		}
		path.Release()
		(<-granted).Release()
	})
}

// A waiting exclusive acquirer prevents a NEW shared acquirer from being granted: the
// exclusive is served before the later shared (no reader-preference starvation).
func TestWorkspaceCoordinatorStarvationFreedom(t *testing.T) {
	t.Parallel()
	c := newWorkspaceCoordinator(nil)
	s1 := mustAcquire(t, c, tool.WorkspaceOperationPathMutation, "/ws/a.txt")

	exGrant := make(chan tool.WorkspacePermit, 1)
	go func() { exGrant <- mustAcquire(t, c, tool.WorkspaceOperationWholeMutation, "") }()
	awaitQueued(t, c, 1) // exclusive enqueued first

	s2Grant := make(chan tool.WorkspacePermit, 1)
	go func() { s2Grant <- mustAcquire(t, c, tool.WorkspaceOperationPathMutation, "/ws/b.txt") }()
	awaitQueued(t, c, 2) // new shared enqueued behind the exclusive

	// Releasing S1 must grant the EXCLUSIVE, not the newer shared S2.
	s1.Release()
	ex := <-exGrant
	if shared, exclusive := c.counts(); shared != 0 || !exclusive {
		t.Fatalf("after exclusive grant counts = (shared %d, exclusive %v), want (0, true)", shared, exclusive)
	}
	if c.queued() != 1 {
		t.Fatalf("queued = %d, want 1 (S2 still blocked behind exclusive)", c.queued())
	}
	select {
	case <-s2Grant:
		t.Fatal("new shared acquirer starved the waiting exclusive")
	default:
	}

	ex.Release()
	(<-s2Grant).Release()
}

// A ctx cancellation during the wait removes the waiter and returns a typed error;
// nothing leaks or deadlocks afterward.
func TestWorkspaceCoordinatorCancelRemovesWaiter(t *testing.T) {
	t.Parallel()
	c := newWorkspaceCoordinator(nil)
	held := mustAcquire(t, c, tool.WorkspaceOperationWholeMutation, "")

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := c.Acquire(ctx, tool.WorkspaceOperationPathMutation, "/ws/a.txt")
		errCh <- err
	}()
	awaitQueued(t, c, 1)

	cancel()
	err := <-errCh
	var canceled *AcquireCanceledError
	if !errors.As(err, &canceled) {
		t.Fatalf("Acquire() error = %T, want *AcquireCanceledError", err)
	}
	awaitQueued(t, c, 0) // waiter removed

	// No leak/deadlock: after releasing the holder a fresh acquire still works.
	held.Release()
	mustAcquire(t, c, tool.WorkspaceOperationPathMutation, "/ws/a.txt").Release()
}

// Canceling a queued waiter WAKES a successor made grantable by its removal: a
// canceled exclusive that was holding back a shared waiter on a free path releases it
// immediately, without waiting for an unrelated holder to release.
func TestWorkspaceCoordinatorCancelWakesSuccessor(t *testing.T) {
	t.Parallel()
	c := newWorkspaceCoordinator(nil)
	sa := mustAcquire(t, c, tool.WorkspaceOperationPathMutation, "/ws/a.txt")

	exCtx, cancelEx := context.WithCancel(context.Background())
	exErr := make(chan error, 1)
	go func() {
		_, err := c.Acquire(exCtx, tool.WorkspaceOperationWholeMutation, "")
		exErr <- err
	}()
	awaitQueued(t, c, 1) // the exclusive is queued first

	sbGrant := make(chan tool.WorkspacePermit, 1)
	go func() { sbGrant <- mustAcquire(t, c, tool.WorkspaceOperationPathMutation, "/ws/b.txt") }()
	awaitQueued(t, c, 2) // the shared waiter is queued behind the exclusive

	// Cancel the exclusive: removing it must WAKE the shared successor (path b is
	// free), which is granted while the unrelated holder sa is STILL held. Without the
	// wake-after-remove this read would block until sa releases.
	cancelEx()
	var canceled *AcquireCanceledError
	if err := <-exErr; !errors.As(err, &canceled) {
		t.Fatalf("exclusive Acquire error = %T, want *AcquireCanceledError", err)
	}
	sb := <-sbGrant
	if shared, exclusive := c.counts(); shared != 2 || exclusive {
		t.Fatalf("counts = (shared %d, exclusive %v), want (2, false) — sa + woken sb both held", shared, exclusive)
	}
	sa.Release()
	sb.Release()
}

// A ctx already done before Acquire returns immediately without enqueuing.
func TestWorkspaceCoordinatorCancelBeforeEnqueue(t *testing.T) {
	t.Parallel()
	c := newWorkspaceCoordinator(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, err := c.Acquire(ctx, tool.WorkspaceOperationPathMutation, "/ws/a.txt")
	if p != nil {
		t.Fatalf("Acquire() returned a permit for a canceled ctx")
	}
	var canceled *AcquireCanceledError
	if !errors.As(err, &canceled) {
		t.Fatalf("Acquire() error = %T, want *AcquireCanceledError", err)
	}
	if c.queued() != 0 {
		t.Fatalf("queued = %d, want 0 (no waiter enqueued)", c.queued())
	}
}

func TestWorkspaceCoordinatorHealthy(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("lease down")
	tests := []struct {
		name    string
		health  LeaseHealth
		wantErr error
	}{
		{name: "nil health is healthy", health: nil, wantErr: nil},
		{name: "healthy delegate", health: staticHealth{err: nil}, wantErr: nil},
		{name: "unhealthy delegate", health: staticHealth{err: sentinel}, wantErr: sentinel},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newWorkspaceCoordinator(tt.health)
			if err := c.Healthy(); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Healthy() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// Release is idempotent: a double release does not corrupt the active counts.
func TestWorkspaceCoordinatorReleaseIdempotent(t *testing.T) {
	t.Parallel()
	c := newWorkspaceCoordinator(nil)
	p := mustAcquire(t, c, tool.WorkspaceOperationPathMutation, "/ws/a.txt")
	p.Release()
	p.Release() // second release is a no-op
	if shared, exclusive := c.counts(); shared != 0 || exclusive {
		t.Fatalf("after double release counts = (shared %d, exclusive %v), want (0, false)", shared, exclusive)
	}
	// The path is free again and a fresh acquire yields exactly one shared slot.
	p2 := mustAcquire(t, c, tool.WorkspaceOperationPathMutation, "/ws/a.txt")
	if shared, _ := c.counts(); shared != 1 {
		t.Fatalf("sharedCount = %d, want 1", shared)
	}
	p2.Release()
}

// Concurrent mixed acquisitions and releases complete without deadlock or race (run
// under -race). Every goroutine acquires then releases, so progress is guaranteed.
func TestWorkspaceCoordinatorConcurrentStress(t *testing.T) {
	t.Parallel()
	c := newWorkspaceCoordinator(nil)
	const workers = 64
	paths := []string{"/ws/a", "/ws/b", "/ws/c"}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				mustAcquire(t, c, tool.WorkspaceOperationWholeMutation, "").Release()
			case 1:
				mustAcquire(t, c, tool.WorkspaceOperationCheckpoint, "").Release()
			default:
				mustAcquire(t, c, tool.WorkspaceOperationPathMutation, paths[i%len(paths)]).Release()
			}
		}(i)
	}
	wg.Wait()
	if shared, exclusive := c.counts(); shared != 0 || exclusive || c.queued() != 0 {
		t.Fatalf("after stress counts = (shared %d, exclusive %v, queued %d), want all zero/false", shared, exclusive, c.queued())
	}
}
