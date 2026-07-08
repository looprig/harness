package api

import (
	"sync"

	"github.com/looprig/harness/pkg/event"
)

// supervisor owns a whole-session event subscription for session lifecycle
// management. Gate state is authoritative in the agent/session; the API does not
// maintain an open-gate registry.
type supervisor struct {
	sub  event.Subscription
	done chan struct{} // closed when the run goroutine has drained and exited

	stopOnce sync.Once
	closeErr error // sub.Close() result, cached so repeat stop() calls return it

	mu      sync.Mutex
	exitErr error // run's exit cause (see exitError)
}

// newSupervisor opens the agent's whole-session subscription (every loop, both
// classes) and starts the drain goroutine. On a Subscribe failure it returns the
// error with no goroutine started (nothing to leak). The caller owns teardown via
// stop.
func newSupervisor(agent Agent) (*supervisor, error) {
	filter := event.EventFilter{
		Ephemeral: event.LoopScope{All: true},
		Enduring:  event.LoopScope{All: true},
	}
	sub, err := agent.Subscribe(filter)
	if err != nil {
		return nil, err
	}
	s := &supervisor{
		sub:  sub,
		done: make(chan struct{}),
	}
	go s.run()
	return s, nil
}

// run drains the subscription until the channel closes (on stop or a hub-forced
// loss), then records why it exited and signals done.
func (s *supervisor) run() {
	for range s.sub.Events() {
	}
	// The channel closed. sub.Err() distinguishes the two exit paths: nil for an
	// intentional stop() (hub Close), the non-nil loss cause for a hub-forced drop.
	// Record it BEFORE closing done so a stop() joiner and exitError() reader both
	// observe the settled cause.
	err := s.sub.Err()
	s.mu.Lock()
	s.exitErr = err
	s.mu.Unlock()
	close(s.done)
}

// exitError reports why the run goroutine exited. While the supervisor is alive
// it is nil; after an INTENTIONAL stop() it stays nil (a hub Close makes
// sub.Err() return nil); after a hub-forced subscription LOSS it is the non-nil
// loss cause.
func (s *supervisor) exitError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitErr
}

// stop tears down the subscription and JOINS the run goroutine: it closes the
// subscription (unblocking the range in run) and waits for run to drain and
// signal done, so teardown is ordered. It is safe to call more than once (guarded
// by stopOnce); a second call is a no-op that returns the cached Close error so a
// caller can log it.
func (s *supervisor) stop() error {
	s.stopOnce.Do(func() {
		s.closeErr = s.sub.Close() // unblocks the range in run()
		<-s.done                   // run() closes done after recording exitErr
	})
	return s.closeErr
}
