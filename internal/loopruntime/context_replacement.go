package loopruntime

import (
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/inference"
)

// StaleCompactionError reports the complete measurement identity that failed
// the actor's compare-and-swap. A stale proposal never mutates live state.
type StaleCompactionError struct {
	ExpectedBasis              event.ContextBasis
	ActualBasis                event.ContextBasis
	ExpectedModel              inference.ModelKey
	ActualModel                inference.ModelKey
	ExpectedRequestFingerprint [32]byte
	ActualRequestFingerprint   [32]byte
}

func (*StaleCompactionError) Error() string {
	return "loopruntime: compaction replacement measurement is stale"
}

type actorContextReplacement struct {
	tracker contextTracker
}

// prepareActorContextReplacement performs every fallible live-state check before
// the canonical terminal append. The returned plan applies without validation
// or I/O after CompactionCommitted is durable.
func prepareActorContextReplacement(
	state loopState,
	attempt compactionAttempt,
	success *compactionPreparedSuccess,
	settings contextAdmissionSettings,
) (actorContextReplacement, error) {
	actualBasis := state.contextTracker.currentBasis()
	actualModel := state.context.Model
	actualFingerprint := state.context.RequestFingerprint
	if success == nil || !state.hasContext || state.context.Basis != attempt.Basis || actualBasis != attempt.Basis ||
		actualModel != success.Model || actualFingerprint != success.RequestFingerprint {
		expectedModel := inference.ModelKey{}
		expectedFingerprint := [32]byte{}
		if success != nil {
			expectedModel = success.Model
			expectedFingerprint = success.RequestFingerprint
		}
		return actorContextReplacement{}, &StaleCompactionError{
			ExpectedBasis: attempt.Basis, ActualBasis: actualBasis,
			ExpectedModel: expectedModel, ActualModel: actualModel,
			ExpectedRequestFingerprint: expectedFingerprint, ActualRequestFingerprint: actualFingerprint,
		}
	}
	tracker := contextTracker{}
	postContext, err := success.PostCount.measurement(attempt.Basis)
	if err != nil {
		return actorContextReplacement{}, err
	}
	if err := tracker.restore(attempt.Basis, true, postContext, true, event.ContextBasis{}, false, settings); err != nil {
		return actorContextReplacement{}, err
	}
	return actorContextReplacement{tracker: tracker}, nil
}

// apply projects the already-durable canonical replacement into actor memory.
// It deliberately leaves inbox/draining and all turn identity untouched.
func (p actorContextReplacement) apply(state *loopState, committed event.CompactionCommitted) {
	tracker := p.tracker
	tracker.basis = committed.PostContext.Basis
	tracker.measurement = committed.PostContext
	tracker.hasMeasurement = true
	state.msgs = content.AgenticMessages{cloneUserMessage(committed.Summary)}
	state.context = committed.PostContext
	state.hasContext = true
	state.contextTracker = tracker
}

type turnContextReplacement struct {
	Summary *content.UserMessage
}

// applyTurnContextReplacement is the private turn-goroutine half of the actor
// handshake. Only request history changes; identity, usage, and tool counters do
// not.
func applyTurnContextReplacement(config *turnConfig, state *turnState, replacement turnContextReplacement) {
	config.base = content.AgenticMessages{}
	state.msgs = content.AgenticMessages{cloneUserMessage(replacement.Summary)}
}
