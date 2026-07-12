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
	SessionID   uuid.UUID
	Policy      checkpointPolicy
	Store       *workspacestore.Store
	Root        string
	Mode        WorkspacePlacementMode
	Coordinator tool.WorkspaceCoordinator
	Publisher   checkedCheckpointPublisher
	Factory     *event.Factory
	Idle        func() bool
	Fault       func(error)
	Faulted     func() error
	Recover     func()
	Admission   func(context.Context) (func(), error)
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
	activeCancel      context.CancelFunc
	activationPending bool
}

type checkpointRequest struct {
	ctx              context.Context
	trigger          event.Event
	alreadyPublished bool
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
	if c.cfg.Faulted != nil {
		if fault := c.cfg.Faulted(); fault != nil {
			_ = c.cfg.Publisher.PublishEventChecked(ctx, trigger)
			return &CheckpointError{Kind: CheckpointFaulted, Cause: fault}
		}
	}
	if !c.beginOperation() {
		return c.cfg.Publisher.PublishEventChecked(ctx, trigger)
	}
	defer c.wg.Done()
	published := false
	publish := func() error {
		err := c.cfg.Publisher.PublishEventChecked(ctx, trigger)
		if err == nil {
			published = true
		}
		return err
	}
	_, err := c.commit(ctx, trigger, checkpointTriggerKind(trigger), publish, nil)
	if err != nil && !published {
		// An admission/permit failure precedes the normal trigger position. Preserve
		// the already-completed execution edge even though no checkpoint can commit.
		_ = publish()
	}
	if err != nil && c.cfg.Fault != nil {
		c.cfg.Fault(err)
	}
	return err
}

func (c *checkpointController) sessionIdle(ctx context.Context, idle event.SessionIdle, publish func() error) error {
	if c == nil || !c.matches(idle) {
		return publish()
	}
	if c.cfg.Policy.Priority == checkpointBestEffort {
		return c.bestEffortBoundaryWithCommit(ctx, idle, publish)
	}
	if c.cfg.Faulted != nil {
		if fault := c.cfg.Faulted(); fault != nil {
			_ = publish()
			return &CheckpointError{Kind: CheckpointFaulted, Cause: fault}
		}
	}
	if !c.beginOperation() {
		return publish()
	}
	defer c.wg.Done()
	published := false
	trackedPublish := func() error {
		err := publish()
		if err == nil {
			published = true
		}
		return err
	}
	_, err := c.commit(ctx, idle, event.SnapshotTriggerIdle, trackedPublish, nil)
	if err != nil && !published {
		_ = trackedPublish()
	}
	if err != nil && c.cfg.Fault != nil {
		c.cfg.Fault(err)
	}
	return err
}

func (c *checkpointController) manual(ctx context.Context) (workspacestore.Ref, error) {
	if c == nil {
		return "", &CheckpointError{Kind: CheckpointUnavailable}
	}
	if !c.beginManualOperation() {
		return "", &CheckpointError{Kind: CheckpointCanceled, Cause: context.Canceled}
	}
	defer c.wg.Done()
	ref, err := c.commit(ctx, nil, event.SnapshotTriggerManual, nil, nil)
	if err != nil && c.cfg.Policy.Priority == checkpointRequired && c.cfg.Fault != nil {
		c.cfg.Fault(err)
	}
	if err == nil && c.cfg.Recover != nil {
		c.cfg.Recover()
	}
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
		workerCtx, cancel := context.WithCancel(req.ctx)
		defer cancel()
		if c.cfg.Mode != PlacementShared {
			c.mu.Lock()
			c.activeCancel = cancel
			cancelNow := c.activationPending
			c.activationPending = false
			c.mu.Unlock()
			if cancelNow {
				cancel()
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
		_, err := c.commit(workerCtx, req.trigger, checkpointTriggerKind(req.trigger), commit, onAccepted)
		if accepted != nil && !signaled {
			// Best-effort progress still requires the execution trigger to survive a
			// permit/snapshot setup failure. Publish it without a checkpoint and retry
			// on the next eligible boundary.
			if !req.alreadyPublished {
				if triggerCommit != nil {
					err = triggerCommit()
				} else {
					err = c.cfg.Publisher.PublishEventChecked(req.ctx, req.trigger)
				}
			}
			accepted <- err
		}
		c.mu.Lock()
		c.activeCancel = nil
		c.mu.Unlock()
		c.finishBestEffort()
	}()
}

func (c *checkpointController) beginOperation() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.wg.Add(1)
	return true
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
		cancel()
	}
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

func (c *checkpointController) commit(caller context.Context, trigger event.Event, kind event.SnapshotTriggerKind, triggerCommit func() error, accepted func()) (workspacestore.Ref, error) {
	if c.cfg.Store == nil || c.cfg.Coordinator == nil || c.cfg.Factory == nil {
		return "", &CheckpointError{Kind: CheckpointUnavailable}
	}
	ctx, cancel := c.operationContext(caller)
	defer cancel()
	if c.cfg.Admission != nil && (c.cfg.Policy.Priority == checkpointRequired || kind == event.SnapshotTriggerManual) {
		releaseAdmission, err := c.cfg.Admission(ctx)
		if err != nil {
			return "", c.classifyError(err)
		}
		defer releaseAdmission()
	}
	if kind == event.SnapshotTriggerManual && (c.cfg.Idle == nil || !c.cfg.Idle()) {
		return "", &CheckpointError{Kind: CheckpointNotIdle}
	}
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
	ref, err := c.cfg.Store.Snapshot(ctx, c.cfg.Root)
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
