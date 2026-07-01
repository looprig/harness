package api

import (
	"sync"

	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// Gate kinds. A gate is either a permission approval (PermissionRequested) or a
// free-form user-input prompt (UserInputRequested). Named constants keep the
// registry off bare magic strings.
const (
	kindPermission = "permission"
	kindUserInput  = "user-input"
)

// pendingGate is one open, unresolved gate in the registry, keyed elsewhere by
// its ToolExecutionID. It records the producing loop (so a resolve can route
// back to the right loop) and the human-facing prompt for a reconnect snapshot.
type pendingGate struct {
	LoopID uuid.UUID
	Kind   string // kindPermission | kindUserInput
	Prompt string
}

// openGate is a flattened registry row for the reconnect endpoint (Task 13's GET
// /gates): pendingGate plus the ToolExecutionID key it was stored under.
type openGate struct {
	ToolExecutionID uuid.UUID
	LoopID          uuid.UUID
	Kind            string
	Prompt          string
}

// supervisor owns the agent's whole-session event subscription and maintains the
// pending-gate registry from it. It is deliberately INDEPENDENT of any client SSE
// stream: it opens its own subscription and records/drops gates whether or not a
// client is attached, so a reconnecting client can recover the open-gate set. Its
// single responsibility is the registry lifecycle; gate ROUTING (Approve/Deny/
// ProvideAnswer) belongs to the HTTP surface, which reads the registry via lookup.
type supervisor struct {
	sub  event.Subscription
	done chan struct{} // closed when the run goroutine has drained and exited

	stopOnce sync.Once
	closeErr error // sub.Close() result, cached so repeat stop() calls return it

	mu      sync.Mutex
	gates   map[uuid.UUID]pendingGate // keyed by ToolExecutionID
	exitErr error                     // run's exit cause (see exitError)
}

// newSupervisor opens the agent's whole-session subscription (every loop, both
// classes) and starts the single run goroutine that maintains the registry. On a
// Subscribe failure it returns the error with no goroutine started (nothing to
// leak). The caller owns teardown via stop.
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
		sub:   sub,
		done:  make(chan struct{}),
		gates: make(map[uuid.UUID]pendingGate),
	}
	go s.run()
	return s, nil
}

// run is the single consumer goroutine: it ranges the subscription channel and
// folds each event into the registry until the channel closes (on stop or a
// hub-forced loss), then records why it exited and signals done. All registry
// mutation happens on this one goroutine plus the mutex-guarded reader methods.
func (s *supervisor) run() {
	for ev := range s.sub.Events() {
		s.apply(ev)
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

// apply folds one event into the registry.
//
// There is NO "PermissionResolved" event — nothing on the stream announces a gate
// was answered. So the drop is inferred from downstream facts:
//
//   - ToolCallStarted / ToolCallCompleted whose ToolExecutionID is registered: an
//     approved permission's call has begun/finished, and an AskUser answer likewise
//     completes its tool — either way the gate is resolved, so drop that one entry.
//   - TurnDone / TurnFailed / TurnInterrupted: the turn ended, so ANY still-open
//     gate for that loop is moot. This is the backstop that also covers a Deny
//     (which produces no matching tool-completed event) and any gate abandoned by
//     an abnormal turn end. Drop every entry whose LoopID matches the terminal's.
//
// Events are matched by value: the loop package emits every event as a value
// (e.g. emit(event.PermissionRequested{...})) and the hub forwards it unchanged.
func (s *supervisor) apply(ev event.Event) {
	switch e := ev.(type) {
	case event.PermissionRequested:
		prompt := ""
		if e.Request != nil { // Request is json:"-" and nil on a replayed/restored event
			prompt = e.Request.Description()
		}
		s.record(e.ToolExecutionID, pendingGate{LoopID: e.EventHeader().LoopID, Kind: kindPermission, Prompt: prompt})
	case event.UserInputRequested:
		s.record(e.ToolExecutionID, pendingGate{LoopID: e.EventHeader().LoopID, Kind: kindUserInput, Prompt: e.Question})
	case event.ToolCallStarted:
		s.drop(e.ToolExecutionID)
	case event.ToolCallCompleted:
		s.drop(e.ToolExecutionID)
	case event.TurnDone:
		s.dropLoop(e.EventHeader().LoopID)
	case event.TurnFailed:
		s.dropLoop(e.EventHeader().LoopID)
	case event.TurnInterrupted:
		s.dropLoop(e.EventHeader().LoopID)
	}
}

// record inserts (or overwrites) the gate for toolExecutionID.
func (s *supervisor) record(toolExecutionID uuid.UUID, gate pendingGate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gates[toolExecutionID] = gate
}

// drop removes the single gate keyed by toolExecutionID (no-op if absent).
func (s *supervisor) drop(toolExecutionID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.gates, toolExecutionID)
}

// dropLoop removes every gate produced by loopID — the turn-terminal backstop.
func (s *supervisor) dropLoop(loopID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for teid, gate := range s.gates {
		if gate.LoopID == loopID {
			delete(s.gates, teid)
		}
	}
}

// lookup returns the pending gate for toolExecutionID, if any. It is the mutex-
// guarded read the HTTP gate-routing uses to resolve the producing LoopID before
// dispatching Approve/Deny/ProvideAnswer.
func (s *supervisor) lookup(toolExecutionID uuid.UUID) (pendingGate, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gate, ok := s.gates[toolExecutionID]
	return gate, ok
}

// list returns a snapshot of every open gate for the reconnect endpoint. The
// snapshot is a fresh slice so callers can never observe or mutate live registry
// state; order is unspecified (map iteration).
func (s *supervisor) list() []openGate {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]openGate, 0, len(s.gates))
	for teid, gate := range s.gates {
		out = append(out, openGate{ToolExecutionID: teid, LoopID: gate.LoopID, Kind: gate.Kind, Prompt: gate.Prompt})
	}
	return out
}

// exitError reports why the run goroutine exited, for a caller that must decide
// whether the registry is still trustworthy. While the supervisor is alive it is
// nil; after an INTENTIONAL stop() it stays nil (a hub Close makes sub.Err()
// return nil); after a hub-forced subscription LOSS it is the non-nil loss cause.
// The reconnect endpoint (GET /gates) reads it to avoid serving a frozen, stale
// registry as if it were the live open-gate set.
func (s *supervisor) exitError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitErr
}

// stop tears down the subscription and JOINS the run goroutine: it closes the
// subscription (unblocking the range in run) and waits for run to drain and
// signal done, so teardown is ordered — the registry is quiescent once stop
// returns. It is safe to call more than once (guarded by stopOnce); a second call
// is a no-op that returns the cached Close error so a caller can log it.
func (s *supervisor) stop() error {
	s.stopOnce.Do(func() {
		s.closeErr = s.sub.Close() // unblocks the range in run()
		<-s.done                   // run() closes done after recording exitErr
	})
	return s.closeErr
}
