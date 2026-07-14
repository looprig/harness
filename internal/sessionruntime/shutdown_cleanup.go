package sessionruntime

import (
	"context"
	"time"

	"github.com/looprig/harness/pkg/command"
)

// ShutdownCleanupPhase identifies the session-owned teardown phase that exceeded
// its private cleanup deadline.
type ShutdownCleanupPhase string

const (
	ShutdownCleanupLoopSend        ShutdownCleanupPhase = "loop_send"
	ShutdownCleanupLoopDrain       ShutdownCleanupPhase = "loop_drain"
	ShutdownCleanupCheckpointDrain ShutdownCleanupPhase = "checkpoint_drain"
	ShutdownCleanupHubStop         ShutdownCleanupPhase = "hub_stop"
)

// ShutdownCleanupTimeoutError reports a finite session-owned teardown deadline.
// Cause remains inspectable with errors.Is(err, context.DeadlineExceeded).
type ShutdownCleanupTimeoutError struct {
	Phase   ShutdownCleanupPhase
	Timeout time.Duration
	Cause   error
}

func (e *ShutdownCleanupTimeoutError) Error() string {
	return "session: shutdown cleanup timed out (" + string(e.Phase) + ")"
}

func (e *ShutdownCleanupTimeoutError) Unwrap() error { return e.Cause }

type shutdownCleanupTimeouts struct {
	hustle     time.Duration
	loopSend   time.Duration
	loopDrain  time.Duration
	checkpoint time.Duration
	hub        time.Duration
}

func (s *Session) resolveShutdownTimeouts(snapshot []loopSnapshot) shutdownCleanupTimeouts {
	base := s.constructionAbortTimeout
	if base <= 0 {
		base = defaultConstructionAbortTimeout
	}
	loopTimeout := durationAdd(base, maxLoopDrainTimeout(snapshot))
	hustleTimeout := durationAdd(base, s.hustleCleanupTimeout())
	checkpointTimeout := base
	if s.snapshotPolicy != nil && s.snapshotPolicy.Timeout > 0 {
		checkpointTimeout = durationAdd(base, s.snapshotPolicy.Timeout)
	}
	derived := shutdownCleanupTimeouts{
		hustle: hustleTimeout, loopSend: loopTimeout, loopDrain: loopTimeout,
		checkpoint: checkpointTimeout, hub: base,
	}
	return derived.withOverrides(s.shutdownTimeouts)
}

func (t shutdownCleanupTimeouts) withOverrides(overrides shutdownCleanupTimeouts) shutdownCleanupTimeouts {
	return shutdownCleanupTimeouts{
		hustle:     timeoutOverride(t.hustle, overrides.hustle),
		loopSend:   timeoutOverride(t.loopSend, overrides.loopSend),
		loopDrain:  timeoutOverride(t.loopDrain, overrides.loopDrain),
		checkpoint: timeoutOverride(t.checkpoint, overrides.checkpoint),
		hub:        timeoutOverride(t.hub, overrides.hub),
	}
}

func timeoutOverride(derived, override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	return derived
}

func maxLoopDrainTimeout(snapshot []loopSnapshot) time.Duration {
	var maximum time.Duration
	for _, item := range snapshot {
		if item.handle != nil && item.handle.bound != nil && item.handle.bound.DrainTimeout() > maximum {
			maximum = item.handle.bound.DrainTimeout()
		}
	}
	return maximum
}

func (s *Session) hustleCleanupTimeout() time.Duration {
	perRun := durationAdd(s.hustleLimits.WorkerDrainTimeout, s.hustleLimits.FinalizationTimeout)
	perRun = durationAdd(perRun, durationMultiply(s.hustleLimits.AuditTimeout, 4))
	var timeout time.Duration
	for _, capacity := range []int{
		s.hustleLimits.BlockingConcurrent, s.hustleLimits.BlockingQueued,
		s.hustleLimits.BackgroundConcurrent, s.hustleLimits.BackgroundQueued,
	} {
		timeout = durationAdd(timeout, durationMultiply(perRun, capacity))
	}
	return timeout
}

func durationAdd(left, right time.Duration) time.Duration {
	const maximum = time.Duration(1<<63 - 1)
	if left <= 0 {
		return right
	}
	if right <= 0 {
		return left
	}
	if left > maximum-right {
		return maximum
	}
	return left + right
}

func durationMultiply(value time.Duration, count int) time.Duration {
	const maximum = time.Duration(1<<63 - 1)
	if value <= 0 || count <= 0 {
		return 0
	}
	if value > maximum/time.Duration(count) {
		return maximum
	}
	return value * time.Duration(count)
}

func cleanupTimeoutError(phase ShutdownCleanupPhase, timeout time.Duration, cause error) error {
	return &ShutdownCleanupTimeoutError{Phase: phase, Timeout: timeout, Cause: cause}
}

func cleanupContext(root context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if root == nil {
		root = context.Background()
	}
	return context.WithTimeout(root, timeout)
}

func (s *Session) closeHustles(root context.Context, timeout time.Duration) error {
	if s.hustleController == nil {
		return nil
	}
	ctx, cancel := cleanupContext(root, timeout)
	defer cancel()
	// Close may synchronously finalize queued ownership. Its trusted callbacks
	// carry their own audit/finalization deadlines; this outer context is input,
	// never authorization to detach the session from owned cleanup.
	return s.hustleController.Close(ctx)
}

func (s *Session) sendLoopShutdowns(root context.Context, snapshot []loopSnapshot, timeout time.Duration) ([]shutdownTarget, error) {
	ctx, cancel := cleanupContext(root, timeout)
	defer cancel()
	targets := make([]shutdownTarget, 0, len(snapshot))
	for _, item := range snapshot {
		target, sent, err := s.sendLoopShutdown(ctx, item)
		if err != nil {
			s.cancelLoopSnapshot(snapshot)
			return targets, cleanupTimeoutError(ShutdownCleanupLoopSend, timeout, err)
		}
		if sent {
			targets = append(targets, target)
		}
	}
	return targets, nil
}

func (s *Session) sendLoopShutdown(ctx context.Context, item loopSnapshot) (shutdownTarget, bool, error) {
	id, err := s.newCommandID()
	if err != nil || item.handle == nil || item.handle.backend == nil {
		return shutdownTarget{}, false, nil
	}
	ack := make(chan error, 1)
	cmd := command.Shutdown{Header: command.Header{CommandID: id, CreatedAt: s.stampNow()}, Ack: ack}
	s.appendShutdownCommand(ctx, item.loopID, cmd)
	select {
	case item.handle.backend.CommandSink() <- cmd:
		return shutdownTarget{loop: item.handle.backend, ack: ack}, true, nil
	case <-item.handle.backend.DoneChan():
		return shutdownTarget{}, false, nil
	case <-ctx.Done():
		return shutdownTarget{}, false, ctx.Err()
	}
}

func (s *Session) waitLoopShutdowns(root context.Context, snapshot []loopSnapshot, targets []shutdownTarget, timeout time.Duration) error {
	ctx, cancel := cleanupContext(root, timeout)
	defer cancel()
	var firstErr error
	for _, target := range targets {
		select {
		case err := <-target.ack:
			if err != nil && firstErr == nil {
				firstErr = err
			}
		case <-target.loop.DoneChan():
		case <-ctx.Done():
			s.cancelLoopSnapshot(snapshot)
			return combineShutdownErrors(firstErr, cleanupTimeoutError(ShutdownCleanupLoopDrain, timeout, ctx.Err()))
		}
	}
	return firstErr
}

func (s *Session) cancelLoopSnapshot(snapshot []loopSnapshot) {
	for _, item := range snapshot {
		if item.handle != nil && item.handle.cancel != nil {
			item.handle.cancel()
		}
	}
}

func (s *Session) waitHustlesDrained() {
	if s.hustleController == nil {
		return
	}
	<-s.hustleController.Drained()
}

func (s *Session) stopCheckpoints(root context.Context, timeout time.Duration) error {
	if s.checkpoints == nil {
		return nil
	}
	ctx, cancel := cleanupContext(root, timeout)
	defer cancel()
	s.checkpoints.shutdownUntil(ctx)
	if err := ctx.Err(); err != nil {
		return cleanupTimeoutError(ShutdownCleanupCheckpointDrain, timeout, err)
	}
	return nil
}

func (s *Session) stopHub(root context.Context, timeout time.Duration) error {
	if s.hub == nil {
		return nil
	}
	ctx, cancel := cleanupContext(root, timeout)
	defer cancel()
	s.hub.StopSession(ctx)
	if err := ctx.Err(); err != nil {
		return cleanupTimeoutError(ShutdownCleanupHubStop, timeout, err)
	}
	return nil
}
