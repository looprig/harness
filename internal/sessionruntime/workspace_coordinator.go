package sessionruntime

import (
	"context"
	"strconv"
	"sync"

	"github.com/looprig/harness/pkg/tool"
)

// workspace_coordinator.go implements the session-scoped workspace mutation
// coordinator (design §"Native checkpoint boundary and workspace gate" and
// §"File-tool optimistic concurrency and binding"). ONE workspaceCoordinator is
// shared by every primer and delegate loop in a session; it hands out mutation
// permits that realize the exclusion model:
//
//   - A PathMutation permit is SHARED across DIFFERENT canonical paths (many run
//     concurrently) but EXCLUSIVE on the SAME canonical path (cross-loop per-path
//     serialization — a loop's private observation map only serializes WITHIN a
//     loop, so the coordinator's path permit is what serializes the same real file
//     across loops).
//   - A WholeMutation permit (Bash and other unknown-path mutators) is EXCLUSIVE
//     against every PathMutation and every other exclusive permit.
//   - A Checkpoint permit is the exclusive snapshot/restore gate — semantically
//     identical exclusion to WholeMutation, distinguished only so a checkpoint actor
//     names its intent (the boundary Task 15 consumes).
//
// FAIRNESS / STARVATION-FREEDOM: once an exclusive acquirer is waiting, NEW shared
// acquirers queue behind it (writer preference) — the wake scan stops at the first
// exclusive waiter, so no unbounded stream of path mutations can starve a pending
// checkpoint or Bash.
//
// CANCELLATION: Acquire is ctx-aware. A ctx that is done before or during the wait
// returns a typed *AcquireCanceledError and removes the waiter from the queue (or, if
// the grant raced in, releases it) so nothing leaks and no permit is stranded.

// LeaseHealth reports whether the workspace lease that underpins harness-managed
// mutations is currently healthy. A structured mutator MUST NOT commit when Healthy
// returns an error (fail-secure). A nil LeaseHealth means "no lease to verify" and is
// treated as always healthy (the bare/no-lease deployment).
type LeaseHealth interface {
	Healthy() error
}

// permitClass is the coordinator's internal exclusion class. PathMutation maps to
// classShared; WholeMutation and Checkpoint both map to classExclusive.
type permitClass uint8

const (
	classShared    permitClass = iota // a path mutation: shared session mode, per-path exclusive
	classExclusive                    // whole-workspace / checkpoint: exclusive against all
)

// waiter is one queued Acquire. ready is closed exactly once, under the coordinator
// mutex, when the waiter is granted; granted records that grant under the same mutex
// so a racing ctx cancellation can distinguish "granted" from "still queued" without
// a data race.
type waiter struct {
	class   permitClass
	path    string
	ready   chan struct{}
	granted bool
}

// workspaceCoordinator is the session-scoped mutation coordinator. All state is
// guarded by mu; the wake scan is the single place that transitions a queued waiter
// to granted, so the exclusion invariants live in exactly one function.
type workspaceCoordinator struct {
	health LeaseHealth

	mu          sync.Mutex
	sharedCount int                 // active PathMutation permits (all paths)
	heldPaths   map[string]struct{} // canonical paths currently held by an active shared permit
	exclusive   bool                // an exclusive permit is currently held
	queue       []*waiter           // FIFO of ungranted acquirers
}

// newWorkspaceCoordinator returns a session-scoped coordinator whose Healthy
// delegates to health (nil ⇒ always healthy).
func newWorkspaceCoordinator(health LeaseHealth) *workspaceCoordinator {
	return &workspaceCoordinator{health: health, heldPaths: make(map[string]struct{})}
}

// Acquire blocks until the requested permit is granted or ctx is done. A done ctx
// (before enqueue, or during the wait) returns a typed *AcquireCanceledError and
// leaves no waiter queued and no permit stranded.
func (c *workspaceCoordinator) Acquire(ctx context.Context, operation tool.WorkspaceOperation, canonicalPath string) (tool.WorkspacePermit, error) {
	class, err := classifyOperation(operation, canonicalPath)
	if err != nil {
		return nil, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, &AcquireCanceledError{Cause: ctxErr}
	}

	w := &waiter{class: class, path: canonicalPath, ready: make(chan struct{})}
	c.mu.Lock()
	c.queue = append(c.queue, w)
	c.wakeLocked()
	c.mu.Unlock()

	select {
	case <-w.ready:
		return &grantedPermit{coord: c, w: w}, nil
	case <-ctx.Done():
		c.mu.Lock()
		defer c.mu.Unlock()
		if w.granted {
			// The grant raced in as ctx fired: we own a live permit nobody else can
			// see, so release it here (releaseLocked re-runs the wake scan) and report
			// the cancellation.
			c.releaseLocked(w)
			return nil, &AcquireCanceledError{Cause: ctx.Err()}
		}
		c.removeFromQueueLocked(w)
		// Removing an ungranted waiter can unblock a waiter behind it (e.g. a canceled
		// exclusive that was holding back a shared waiter on a free path), so re-run the
		// wake scan — keeping "wake after any queue mutation" a uniform, local discipline.
		c.wakeLocked()
		return nil, &AcquireCanceledError{Cause: ctx.Err()}
	}
}

// Healthy reports lease health, delegating to the injected LeaseHealth (nil ⇒ nil).
func (c *workspaceCoordinator) Healthy() error {
	if c.health == nil {
		return nil
	}
	return c.health.Healthy()
}

// wakeLocked grants every currently-grantable waiter from the front of the queue.
// It is the SOLE grant point, called under mu after every enqueue and every release.
//
// Writer preference (starvation-freedom): the scan stops at the first exclusive
// waiter — either granting it (when it is at head-of-line and no permit is active) or
// returning — so nothing behind an exclusive waiter is granted, and no new shared
// acquirer (appended at the back) can jump ahead of a pending exclusive.
//
// Different-path concurrency: a shared waiter whose path is currently held is SKIPPED
// (i++), letting a later different-path shared waiter proceed; the skipped waiter is
// granted by a later wake when its path frees (it remains the earliest waiter for
// that path, so it is never starved).
func (c *workspaceCoordinator) wakeLocked() {
	i := 0
	for i < len(c.queue) {
		w := c.queue[i]
		if w.class == classExclusive {
			if i == 0 && c.sharedCount == 0 && !c.exclusive {
				c.exclusive = true
				c.grantLocked(0, w)
				// exclusive is now held, so nothing else can be granted this pass.
				return
			}
			return
		}
		if c.exclusive {
			return
		}
		if _, busy := c.heldPaths[w.path]; busy {
			i++
			continue
		}
		c.heldPaths[w.path] = struct{}{}
		c.sharedCount++
		c.grantLocked(i, w)
	}
}

// grantLocked removes queue[index] and marks/opens the waiter as granted. The caller
// holds mu.
func (c *workspaceCoordinator) grantLocked(index int, w *waiter) {
	c.queue = append(c.queue[:index], c.queue[index+1:]...)
	w.granted = true
	close(w.ready)
}

// releaseLocked returns a granted permit's resources and re-runs the wake scan. The
// caller holds mu. It is idempotent at the permit boundary via grantedPermit's Once.
func (c *workspaceCoordinator) releaseLocked(w *waiter) {
	switch w.class {
	case classExclusive:
		c.exclusive = false
	case classShared:
		if c.sharedCount > 0 {
			c.sharedCount--
		}
		delete(c.heldPaths, w.path)
	}
	c.wakeLocked()
}

// removeFromQueueLocked removes an ungranted waiter from the queue (a no-op if it is
// no longer present). The caller holds mu.
func (c *workspaceCoordinator) removeFromQueueLocked(w *waiter) {
	for i, q := range c.queue {
		if q == w {
			c.queue = append(c.queue[:i], c.queue[i+1:]...)
			return
		}
	}
}

// grantedPermit is the tool.WorkspacePermit returned by a successful Acquire. Release
// is idempotent (Once) so callers may safely defer it immediately after acquisition.
type grantedPermit struct {
	coord *workspaceCoordinator
	w     *waiter
	once  sync.Once
}

// Release returns the permit's resources exactly once.
func (p *grantedPermit) Release() {
	p.once.Do(func() {
		p.coord.mu.Lock()
		defer p.coord.mu.Unlock()
		p.coord.releaseLocked(p.w)
	})
}

// classifyOperation validates the operation/path pairing and maps it to an internal
// exclusion class. PathMutation requires a non-empty path; whole/checkpoint require an
// empty path.
func classifyOperation(operation tool.WorkspaceOperation, path string) (permitClass, error) {
	switch operation {
	case tool.WorkspaceOperationPathMutation:
		if path == "" {
			return 0, &WorkspacePathError{Operation: operation, Reason: "a path mutation requires a non-empty canonical path"}
		}
		return classShared, nil
	case tool.WorkspaceOperationWholeMutation, tool.WorkspaceOperationCheckpoint:
		if path != "" {
			return 0, &WorkspacePathError{Operation: operation, Reason: "a whole-workspace operation requires an empty canonical path"}
		}
		return classExclusive, nil
	default:
		return 0, &InvalidWorkspaceOperationError{Operation: operation}
	}
}

// AcquireCanceledError reports that an Acquire returned because its ctx was done
// before or during the wait. Cause is the ctx error (context.Canceled or
// context.DeadlineExceeded). No permit was granted; no waiter remains queued.
type AcquireCanceledError struct{ Cause error }

func (e *AcquireCanceledError) Error() string {
	return "sessionruntime: workspace permit acquisition canceled: " + e.Cause.Error()
}

func (e *AcquireCanceledError) Unwrap() error { return e.Cause }

// InvalidWorkspaceOperationError reports an Acquire with an operation value the
// coordinator does not recognize (fail-secure: no permit is granted).
type InvalidWorkspaceOperationError struct{ Operation tool.WorkspaceOperation }

func (e *InvalidWorkspaceOperationError) Error() string {
	return "sessionruntime: invalid workspace operation: " + strconv.Itoa(int(e.Operation))
}

// WorkspacePathError reports an Acquire whose canonicalPath does not match its
// operation (a path mutation with an empty path, or a whole-workspace operation with
// a non-empty path).
type WorkspacePathError struct {
	Operation tool.WorkspaceOperation
	Reason    string
}

func (e *WorkspacePathError) Error() string {
	return "sessionruntime: invalid workspace permit path: " + e.Reason
}

var _ tool.WorkspaceCoordinator = (*workspaceCoordinator)(nil)
