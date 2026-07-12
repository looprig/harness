package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
)

type checkpointTrigger uint8

const (
	checkpointManual checkpointTrigger = iota + 1
	checkpointOnIdle
	checkpointOnTurnDone
	checkpointOnStepDone
)

type checkpointPriority uint8

const (
	checkpointBestEffort checkpointPriority = iota
	checkpointRequired
)

var errCheckpointActivated = errors.New("sessionruntime: checkpoint canceled by session activation")

type checkpointPolicy struct {
	Trigger  checkpointTrigger
	Priority checkpointPriority
	Timeout  time.Duration
}

// SnapshotPolicy is the internal composition form of rig.SnapshotPolicy. pkg/rig owns
// the public API and converts its validated values into this dependency-only shape.
type SnapshotPolicy struct {
	Trigger  checkpointTrigger
	Priority checkpointPriority
	Timeout  time.Duration
}

const (
	SnapshotManual     = checkpointManual
	SnapshotOnIdle     = checkpointOnIdle
	SnapshotOnTurnDone = checkpointOnTurnDone
	SnapshotOnStepDone = checkpointOnStepDone
	SnapshotBestEffort = checkpointBestEffort
	SnapshotRequired   = checkpointRequired
)

func (p SnapshotPolicy) internal() checkpointPolicy {
	return checkpointPolicy{Trigger: p.Trigger, Priority: p.Priority, Timeout: p.Timeout}
}

type checkedCheckpointPublisher interface {
	PublishEventChecked(context.Context, event.Event) error
}

type checkpointControllerConfig struct {
	SessionID      uuid.UUID
	Policy         checkpointPolicy
	Store          *workspacestore.Store
	Root           string
	Mode           WorkspacePlacementMode
	Coordinator    tool.WorkspaceCoordinator
	Publisher      checkedCheckpointPublisher
	Factory        *event.Factory
	Idle           func() bool
	Fault          func(error)
	Faulted        func() error
	Recover        func()
	Admission      func(context.Context) (func(), error)
	RequiredQueued func(event.Event)
	ManualQueued   func()
	ObservePending func()
	ObserveError   func(error)
}

type checkpointController struct {
	cfg checkpointControllerConfig

	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once

	mu                sync.Mutex
	active            bool
	pending           *checkpointRequest
	manualWaiting     int
	closed            bool
	wg                sync.WaitGroup
	activeCancel      context.CancelCauseFunc
	activationPending bool
	requiredQueue     []*requiredRequest
	requiredRunner    bool
}

type checkpointRequest struct {
	ctx              context.Context
	trigger          event.Event
	alreadyPublished bool
}

type checkpointResult struct {
	ref workspacestore.Ref
	err error
}

type requiredRequest struct {
	ctx     context.Context
	trigger event.Event
	run     func(context.Context) (workspacestore.Ref, error)
	result  chan checkpointResult
	started bool
}

func newCheckpointController(cfg checkpointControllerConfig) *checkpointController {
	ctx, cancel := context.WithCancel(context.Background())
	return &checkpointController{cfg: cfg, ctx: ctx, cancel: cancel}
}

func (c *checkpointController) shutdown() {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.pending = nil
		c.mu.Unlock()
		c.cancel()
		c.wg.Wait()
	})
}

func (c *checkpointController) boundary(ctx context.Context, trigger event.Event) error {
	if c == nil || c.cfg.Publisher == nil {
		return &CheckpointError{Kind: CheckpointUnavailable}
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		// Shutdown never manufactures a snapshot trigger. A real terminal already
		// produced by a draining turn still remains durable/live.
		return c.cfg.Publisher.PublishEventChecked(ctx, trigger)
	}
	if !c.matches(trigger) {
		return c.cfg.Publisher.PublishEventChecked(ctx, trigger)
	}
	if c.cfg.Policy.Priority == checkpointBestEffort {
		return c.bestEffortBoundary(ctx, trigger)
	}
	_, err := c.runRequired(ctx, trigger, func(runCtx context.Context) (workspacestore.Ref, error) {
		published := false
		publish := func() error {
			err := c.cfg.Publisher.PublishEventChecked(runCtx, trigger)
			if err == nil {
				published = true
			}
			return err
		}
		ref, commitErr := c.commit(runCtx, trigger, checkpointTriggerKind(trigger), publish, nil)
		if commitErr != nil && !published {
			_ = publish()
		}
		return ref, commitErr
	})
	return err
}

func (c *checkpointController) sessionIdle(ctx context.Context, idle event.SessionIdle, publish func() error) error {
	if c == nil || !c.matches(idle) {
		return publish()
	}
	if c.cfg.Policy.Priority == checkpointBestEffort {
		return c.bestEffortBoundaryWithCommit(ctx, idle, publish)
	}
	_, err := c.runRequired(ctx, idle, func(runCtx context.Context) (workspacestore.Ref, error) {
		published := false
		trackedPublish := func() error {
			err := publish()
			if err == nil {
				published = true
			}
			return err
		}
		ref, commitErr := c.commit(runCtx, idle, event.SnapshotTriggerIdle, trackedPublish, nil)
		if commitErr != nil && !published {
			_ = trackedPublish()
		}
		return ref, commitErr
	})
	return err
}

func (c *checkpointController) manual(ctx context.Context) (workspacestore.Ref, error) {
	if c == nil {
		return "", &CheckpointError{Kind: CheckpointUnavailable}
	}
	if c.cfg.Policy.Priority == checkpointRequired {
		if c.cfg.ManualQueued != nil {
			c.cfg.ManualQueued()
		}
		return c.runRequired(ctx, nil, func(runCtx context.Context) (workspacestore.Ref, error) {
			return c.commit(runCtx, nil, event.SnapshotTriggerManual, nil, nil)
		})
	}
	if !c.beginManualOperation() {
		return "", &CheckpointError{Kind: CheckpointCanceled, Cause: context.Canceled}
	}
	if c.cfg.ManualQueued != nil {
		c.cfg.ManualQueued()
	}
	defer c.wg.Done()
	ref, err := c.commit(ctx, nil, event.SnapshotTriggerManual, nil, nil)
	c.mu.Lock()
	c.manualWaiting--
	start := c.takePendingLocked()
	if start != nil {
		c.wg.Add(1)
	}
	c.mu.Unlock()
	if start != nil {
		c.runBestEffort(*start, nil, nil)
	}
	return ref, err
}

func (c *checkpointController) bestEffortBoundary(ctx context.Context, trigger event.Event) error {
	return c.bestEffortBoundaryWithCommit(ctx, trigger, nil)
}

func (c *checkpointController) bestEffortBoundaryWithCommit(ctx context.Context, trigger event.Event, triggerCommit func() error) error {
	req := checkpointRequest{ctx: ctx, trigger: trigger}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		if triggerCommit != nil {
			return triggerCommit()
		}
		return c.cfg.Publisher.PublishEventChecked(ctx, trigger)
	}
	if !c.active {
		c.active = true
		c.wg.Add(1)
		accepted := make(chan error, 1)
		c.mu.Unlock()
		c.runBestEffort(req, triggerCommit, accepted)
		return <-accepted
	}
	c.mu.Unlock()

	// An active walk means this edge is coalescible. Persist/emit the execution edge
	// now, then retain only the latest already-published trigger for the next walk.
	var err error
	if triggerCommit != nil {
		err = triggerCommit()
	} else {
		err = c.cfg.Publisher.PublishEventChecked(ctx, trigger)
	}
	if err != nil {
		return err
	}
	req.alreadyPublished = true
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	if c.active || c.manualWaiting > 0 {
		copyReq := req
		c.pending = &copyReq
		c.mu.Unlock()
		return nil
	}
	c.active = true
	c.pending = nil
	c.wg.Add(1)
	c.mu.Unlock()
	c.runBestEffort(req, nil, nil)
	return nil
}

// runBestEffort starts a request whose wait-group slot was reserved while holding c.mu.
// Reserving before unlock prevents Shutdown's closed→Wait transition from missing it.
func (c *checkpointController) runBestEffort(req checkpointRequest, triggerCommit func() error, accepted chan error) {
	go func() {
		defer c.wg.Done()
		workerCtx, cancel := context.WithCancelCause(req.ctx)
		defer cancel(context.Canceled)
		if c.cfg.Mode != PlacementShared {
			c.mu.Lock()
			c.activeCancel = cancel
			cancelNow := c.activationPending
			c.activationPending = false
			c.mu.Unlock()
			if cancelNow {
				cancel(errCheckpointActivated)
			}
		}
		signaled := false
		onAccepted := func() {
			if accepted != nil && !signaled {
				accepted <- nil
				signaled = true
			}
		}
		commit := triggerCommit
		if req.alreadyPublished {
			commit = func() error { return nil }
		}
		_, commitErr := c.commit(workerCtx, req.trigger, checkpointTriggerKind(req.trigger), commit, onAccepted)
		observe := accepted == nil || signaled
		if accepted != nil && !signaled {
			// Best-effort progress still requires the execution trigger to survive a
			// permit/snapshot setup failure. Publish it without a checkpoint and retry
			// on the next eligible boundary.
			acceptErr := commitErr
			if !req.alreadyPublished {
				if triggerCommit != nil {
					acceptErr = triggerCommit()
				} else {
					acceptErr = c.cfg.Publisher.PublishEventChecked(req.ctx, req.trigger)
				}
			}
			accepted <- acceptErr
			// Once the trigger survives, the checkpoint failure is asynchronous from
			// the caller's perspective and must be observed internally.
			observe = acceptErr == nil
		}
		if observe {
			c.observeBestEffortError(workerCtx, commitErr)
		}
		c.mu.Lock()
		c.activeCancel = nil
		c.mu.Unlock()
		c.finishBestEffort()
	}()
}

func (c *checkpointController) runRequired(
	ctx context.Context,
	trigger event.Event,
	run func(context.Context) (workspacestore.Ref, error),
) (workspacestore.Ref, error) {
	req := &requiredRequest{ctx: ctx, trigger: trigger, run: run, result: make(chan checkpointResult, 1)}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", &CheckpointError{Kind: CheckpointCanceled, Cause: context.Canceled}
	}
	c.wg.Add(1)
	c.requiredQueue = append(c.requiredQueue, req)
	startRunner := !c.requiredRunner
	if startRunner {
		c.requiredRunner = true
	}
	c.mu.Unlock()
	if c.cfg.RequiredQueued != nil {
		c.cfg.RequiredQueued(trigger)
	}
	if startRunner {
		go c.runRequiredQueue()
	}

	select {
	case result := <-req.result:
		return result.ref, result.err
	case <-ctx.Done():
		c.mu.Lock()
		if !req.started {
			removed := false
			for i, queued := range c.requiredQueue {
				if queued == req {
					c.requiredQueue = append(c.requiredQueue[:i], c.requiredQueue[i+1:]...)
					removed = true
					break
				}
			}
			c.mu.Unlock()
			if removed {
				c.wg.Done()
				return "", &CheckpointError{Kind: CheckpointCanceled, Cause: ctx.Err()}
			}
		} else {
			c.mu.Unlock()
		}
		// Once selected by the FIFO runner, a request owns completion; caller
		// cancellation cannot let a successor overtake its durable transaction.
		result := <-req.result
		return result.ref, result.err
	}
}

func (c *checkpointController) runRequiredQueue() {
	for {
		c.mu.Lock()
		if len(c.requiredQueue) == 0 {
			c.requiredRunner = false
			c.mu.Unlock()
			return
		}
		req := c.requiredQueue[0]
		c.requiredQueue = c.requiredQueue[1:]
		req.started = true
		c.mu.Unlock()

		ref, err := req.run(c.ctx)
		req.result <- checkpointResult{ref: ref, err: err}
		c.wg.Done()
	}
}

func (c *checkpointController) beginManualOperation() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.wg.Add(1)
	c.manualWaiting++
	return true
}

// activated cancels an active best-effort quiescent walk when session progress resumes.
// Shared fuzzy walks deliberately continue because they make no quiescence claim.
func (c *checkpointController) activated() {
	if c == nil || c.cfg.Policy.Priority != checkpointBestEffort || c.cfg.Mode == PlacementShared {
		return
	}
	c.mu.Lock()
	cancel := c.activeCancel
	c.pending = nil
	if cancel == nil && c.active {
		c.activationPending = true
	}
	c.mu.Unlock()
	if cancel != nil {
		cancel(errCheckpointActivated)
	}
}

func (c *checkpointController) observeBestEffortError(workerCtx context.Context, err error) {
	if err == nil || c.cfg.ObserveError == nil {
		return
	}
	if c.cfg.ObservePending != nil {
		c.cfg.ObservePending()
	}
	// Activation and controller shutdown deliberately abandon a best-effort walk;
	// they are expected control flow, not operational checkpoint failures.
	if errors.Is(err, context.Canceled) &&
		(errors.Is(context.Cause(workerCtx), errCheckpointActivated) || c.ctx.Err() != nil) {
		return
	}
	c.cfg.ObserveError(err)
}

func (c *checkpointController) finishBestEffort() {
	c.mu.Lock()
	c.active = false
	c.activationPending = false
	start := c.takePendingLocked()
	if start != nil {
		c.wg.Add(1)
	}
	c.mu.Unlock()
	if start != nil {
		c.runBestEffort(*start, nil, nil)
	}
}

func (c *checkpointController) takePendingLocked() *checkpointRequest {
	if c.closed || c.active || c.manualWaiting > 0 || c.pending == nil {
		return nil
	}
	req := c.pending
	c.pending = nil
	c.active = true
	return req
}

func (c *checkpointController) matches(trigger event.Event) bool {
	switch c.cfg.Policy.Trigger {
	case checkpointOnIdle:
		_, ok := trigger.(event.SessionIdle)
		return ok
	case checkpointOnTurnDone:
		switch trigger.(type) {
		case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
			return true
		}
	case checkpointOnStepDone:
		_, ok := trigger.(event.StepDone)
		return ok
	}
	return false
}

func (c *checkpointController) commit(caller context.Context, trigger event.Event, kind event.SnapshotTriggerKind, triggerCommit func() error, accepted func()) (ref workspacestore.Ref, err error) {
	if c.cfg.Store == nil || c.cfg.Coordinator == nil || c.cfg.Factory == nil {
		err = &CheckpointError{Kind: CheckpointUnavailable}
		if trigger != nil {
			c.latchRequired(err)
		}
		return "", err
	}
	ctx, cancel := c.operationContext(caller)
	defer cancel()
	admissionHeld := false
	durabilityAttempted := false
	defer func() {
		if admissionHeld {
			return
		}
		if err != nil && (trigger != nil || durabilityAttempted) {
			c.latchRequired(err)
		} else if err == nil && kind == event.SnapshotTriggerManual && c.cfg.Recover != nil {
			c.cfg.Recover()
		}
	}()
	if c.cfg.Admission != nil && (c.cfg.Policy.Priority == checkpointRequired || kind == event.SnapshotTriggerManual) {
		releaseAdmission, admissionErr := c.cfg.Admission(ctx)
		if admissionErr != nil {
			err = c.classifyError(admissionErr)
			return "", err
		}
		admissionHeld = true
		defer func() {
			if err != nil && (trigger != nil || durabilityAttempted) {
				c.latchRequired(err)
			} else if err == nil && kind == event.SnapshotTriggerManual && c.cfg.Recover != nil {
				// Recovery clears the workspace fault and admission latch while the
				// writer is still held; readers wake only after releaseAdmission.
				c.cfg.Recover()
			}
			releaseAdmission()
		}()
	}
	if trigger != nil && c.cfg.Policy.Priority == checkpointRequired && c.cfg.Faulted != nil {
		if fault := c.cfg.Faulted(); fault != nil {
			publish := triggerCommit
			if publish == nil {
				publish = func() error { return c.cfg.Publisher.PublishEventChecked(ctx, trigger) }
			}
			if publishErr := publish(); publishErr != nil {
				return "", &CheckpointError{Kind: CheckpointTriggerAppendFailed, Cause: publishErr}
			}
			return "", &CheckpointError{Kind: CheckpointFaulted, Cause: fault}
		}
	}
	if kind == event.SnapshotTriggerManual && (c.cfg.Idle == nil || !c.cfg.Idle()) {
		return "", &CheckpointError{Kind: CheckpointNotIdle}
	}
	durabilityAttempted = true
	permit, err := c.cfg.Coordinator.Acquire(ctx, tool.WorkspaceOperationCheckpoint, "")
	if err != nil {
		return "", c.classifyError(err)
	}
	defer permit.Release()
	if err := c.cfg.Coordinator.Healthy(); err != nil {
		return "", c.classifyError(err)
	}
	if trigger != nil {
		publish := triggerCommit
		if publish == nil {
			publish = func() error { return c.cfg.Publisher.PublishEventChecked(ctx, trigger) }
		}
		if err := publish(); err != nil {
			return "", &CheckpointError{Kind: CheckpointTriggerAppendFailed, Cause: err}
		}
	}
	if accepted != nil {
		accepted()
	}
	ref, err = c.cfg.Store.Snapshot(ctx, c.cfg.Root)
	if err != nil {
		return "", c.classifyError(err)
	}
	var cause identity.Cause
	if trigger != nil {
		cause = checkpointCause(trigger.EventHeader())
	}
	h, err := c.cfg.Factory.Stamp(event.Header{Coordinates: identity.Coordinates{SessionID: c.cfg.SessionID}, Cause: cause})
	if err != nil {
		return "", &CheckpointError{Kind: CheckpointIDGenerationFailed, Cause: err}
	}
	cp := event.WorkspaceCheckpointed{Header: h, Ref: string(ref), Consistency: c.consistency(), Trigger: kind}
	if err := c.cfg.Publisher.PublishEventChecked(ctx, cp); err != nil {
		return "", &CheckpointError{Kind: CheckpointAppendFailed, Cause: err}
	}
	return ref, nil
}

func (c *checkpointController) latchRequired(err error) {
	if err != nil && c.cfg.Policy.Priority == checkpointRequired && c.cfg.Fault != nil {
		c.cfg.Fault(err)
	}
}

func (c *checkpointController) operationContext(caller context.Context) (context.Context, context.CancelFunc) {
	ctx, cancelCause := context.WithCancelCause(caller)
	stop := context.AfterFunc(c.ctx, func() { cancelCause(context.Canceled) })
	timeout := c.cfg.Policy.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	timed, cancelTimeout := context.WithTimeout(ctx, timeout)
	return timed, func() {
		stop()
		cancelTimeout()
		cancelCause(context.Canceled)
	}
}

func (c *checkpointController) classifyError(err error) error {
	kind := CheckpointFailed
	if errors.Is(err, context.DeadlineExceeded) {
		kind = CheckpointTimeout
	} else if errors.Is(err, context.Canceled) {
		kind = CheckpointCanceled
	}
	return &CheckpointError{Kind: kind, Cause: err}
}

func (c *checkpointController) consistency() event.SnapshotConsistency {
	if c.cfg.Mode == PlacementShared {
		return event.SnapshotFuzzy
	}
	return event.SnapshotQuiescent
}

func checkpointTriggerKind(trigger event.Event) event.SnapshotTriggerKind {
	switch trigger.(type) {
	case event.SessionIdle:
		return event.SnapshotTriggerIdle
	case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
		return event.SnapshotTriggerTurnDone
	case event.StepDone:
		return event.SnapshotTriggerStepDone
	default:
		return event.SnapshotTriggerKindUnknown
	}
}

func checkpointCause(h event.Header) identity.Cause {
	return identity.Cause{Coordinates: h.Coordinates, EventID: h.EventID, Agency: identity.AgencyMachine}
}

type CheckpointErrorKind string

const (
	CheckpointUnavailable         CheckpointErrorKind = "unavailable"
	CheckpointNotIdle             CheckpointErrorKind = "not_idle"
	CheckpointFaulted             CheckpointErrorKind = "faulted"
	CheckpointFailed              CheckpointErrorKind = "failed"
	CheckpointTimeout             CheckpointErrorKind = "timeout"
	CheckpointCanceled            CheckpointErrorKind = "canceled"
	CheckpointTriggerAppendFailed CheckpointErrorKind = "trigger_append_failed"
	CheckpointAppendFailed        CheckpointErrorKind = "append_failed"
	CheckpointIDGenerationFailed  CheckpointErrorKind = "id_generation_failed"
)

type CheckpointError struct {
	Kind  CheckpointErrorKind
	Cause error
}

func (e *CheckpointError) Error() string {
	msg := "sessionruntime: workspace checkpoint failed (" + string(e.Kind) + ")"
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *CheckpointError) Unwrap() error { return e.Cause }
