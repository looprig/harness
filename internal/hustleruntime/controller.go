package hustleruntime

import (
	"context"
	"math"
	"sync"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/hustle"
)

// Controller owns the two independent participation lanes. Task 19 layers the
// inference and audit state machine over the ownership seam in this file.
type Controller struct {
	sessionCtx context.Context
	blocking   *lane
	background *lane
	newRunID   RunIDFactory

	admissionMu sync.Mutex
	closeOnce   sync.Once
	closeErr    error
}

// New validates and constructs the two bounded lanes without starting workers.
func New(sessionCtx context.Context, config Config) (*Controller, error) {
	if sessionCtx == nil {
		return nil, &ConfigError{Reason: ConfigInvalidContext, Field: "context"}
	}
	if err := validateLaneLimits("blocking", config.Blocking); err != nil {
		return nil, err
	}
	if err := validateLaneLimits("background", config.Background); err != nil {
		return nil, err
	}
	newRunID := config.NewRunID
	if newRunID == nil {
		newRunID = uuid.New
	}
	return &Controller{
		sessionCtx: sessionCtx,
		blocking:   newLane(hustle.ParticipationBlocking, config.Blocking),
		background: newLane(hustle.ParticipationBackground, config.Background),
		newRunID:   newRunID,
	}, nil
}

func validateLaneLimits(name string, limits LaneLimits) error {
	if limits.Concurrent <= 0 {
		return &ConfigError{Reason: ConfigInvalidConcurrent, Field: name + ".concurrent"}
	}
	if limits.Queued < 0 || limits.Queued > maxLaneQueued {
		return &ConfigError{Reason: ConfigInvalidQueued, Field: name + ".queued"}
	}
	if limits.Concurrent > math.MaxInt-limits.Queued {
		return &ConfigError{Reason: ConfigCapacityOverflow, Field: name}
	}
	return nil
}

// ownedRun is the Task 18 extension seam: Task 19 owns one, waits for its FIFO
// execution grant, performs audit/execution, then finalizes it exactly once.
type ownedRun struct {
	controller    *Controller
	lane          *lane
	id            hustle.RunID
	participation hustle.Participation
	callerCtx     context.Context
	finalizer     Finalizer
	sequence      uint64
	state         runState // guarded by lane.mu
	granted       chan struct{}
	done          chan struct{}

	finalizeOnce sync.Once
	finalizeErr  error
}

func (c *Controller) own(ctx context.Context, participation hustle.Participation, finalizer Finalizer) (*ownedRun, error) {
	if ctx == nil {
		return nil, &AdmissionError{Reason: AdmissionInvalidContext, Participation: participation}
	}
	lane := c.laneFor(participation)
	if lane == nil {
		return nil, &AdmissionError{Reason: AdmissionInvalidParticipation, Participation: participation}
	}
	if finalizer == nil {
		return nil, &AdmissionError{Reason: AdmissionNilFinalizer, Participation: participation}
	}
	if err := ctx.Err(); err != nil {
		return nil, &AdmissionError{Reason: AdmissionInvalidContext, Participation: participation, Cause: err}
	}
	id, err := c.newRunID()
	if err != nil || id.IsZero() {
		return nil, &AdmissionError{Reason: AdmissionRunID, Participation: participation, Cause: err}
	}
	run := &ownedRun{
		controller: c, lane: lane, id: hustle.RunID(id), participation: participation,
		callerCtx: ctx, finalizer: finalizer, granted: make(chan struct{}), done: make(chan struct{}),
	}
	c.admissionMu.Lock()
	defer c.admissionMu.Unlock()
	if err := lane.enqueue(run); err != nil {
		return nil, err
	}
	return run, nil
}

func (c *Controller) laneFor(participation hustle.Participation) *lane {
	switch participation {
	case hustle.ParticipationBlocking:
		return c.blocking
	case hustle.ParticipationBackground:
		return c.background
	default:
		return nil
	}
}

func (r *ownedRun) awaitExecution() error {
	for {
		switch r.lane.stateOf(r) {
		case runExecuting:
			return nil
		case runFinalizing, runDone:
			<-r.done
			return r.finalizeErr
		}
		select {
		case <-r.granted:
			return nil
		case <-r.callerCtx.Done():
			if r.lane.cancelQueued(r) {
				return r.finishQueued(context.WithoutCancel(r.controller.sessionCtx), QueueFailureCanceled, r.callerCtx.Err())
			}
		case <-r.controller.sessionCtx.Done():
			if r.lane.cancelQueued(r) {
				return r.finishQueued(context.WithoutCancel(r.controller.sessionCtx), QueueFailureCanceled, r.controller.sessionCtx.Err())
			}
		case <-r.done:
			return r.finalizeErr
		}
	}
}

func (r *ownedRun) finalize(ctx context.Context, outcome hustle.Outcome) error {
	return r.finish(ctx, outcome, nil)
}

func (r *ownedRun) finishQueued(ctx context.Context, reason QueueFailureReason, cause error) error {
	failure := &QueueFailureError{
		RunID: r.id, Participation: r.participation, Stage: hustle.StageQueue, Reason: reason, Cause: cause,
	}
	return r.finish(ctx, hustle.Outcome{Err: failure}, failure)
}

func (r *ownedRun) finish(ctx context.Context, outcome hustle.Outcome, queueFailure *QueueFailureError) error {
	r.finalizeOnce.Do(func() {
		defer close(r.done)
		if queueFailure == nil {
			if err := r.lane.beginFinalizing(r); err != nil {
				r.finalizeErr = err
				return
			}
		}
		if ctx == nil {
			ctx = context.Background()
		}
		if err := r.finalizer(ctx, outcome); err != nil {
			finalizerErr := &FinalizerError{RunID: r.id, Cause: err}
			if queueFailure != nil {
				queueFailure.FinalizerErr = finalizerErr
			} else {
				r.finalizeErr = finalizerErr
			}
		}
		if queueFailure != nil {
			r.finalizeErr = queueFailure
		}
		r.lane.complete(r)
	})
	<-r.done
	return r.finalizeErr
}

// Close atomically closes both admissions before resolving every queued-owned
// node. Already executing nodes retain ownership and no queued node is granted.
func (c *Controller) Close(ctx context.Context) error {
	c.closeOnce.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		c.admissionMu.Lock()
		blocking := c.blocking.closeQueued()
		background := c.background.closeQueued()
		c.admissionMu.Unlock()
		failures := make([]*FinalizerError, 0)
		for _, run := range append(blocking, background...) {
			err := run.finishQueued(ctx, QueueFailureClosed, nil)
			var queueFailure *QueueFailureError
			if errorsAsQueueFailure(err, &queueFailure) && queueFailure.FinalizerErr != nil {
				failures = append(failures, queueFailure.FinalizerErr)
			}
		}
		if len(failures) > 0 {
			c.closeErr = &CloseError{Failures: failures}
		}
	})
	return c.closeErr
}

func errorsAsQueueFailure(err error, target **QueueFailureError) bool {
	queueFailure, ok := err.(*QueueFailureError)
	if ok {
		*target = queueFailure
	}
	return ok
}
