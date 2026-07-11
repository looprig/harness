package loopruntime

import (
	"reflect"
	"time"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
)

// configFromBound resolves the immutable public contract into actor-private wiring.
func configFromBound(bound loop.BoundDefinition, modeName loop.ModeName) (runtimeConfig, error) {
	if bound == nil || (reflect.ValueOf(bound).Kind() == reflect.Ptr && reflect.ValueOf(bound).IsNil()) {
		return runtimeConfig{}, &loop.BindError{Kind: loop.BindInvalidDefinition, Index: -1}
	}
	if modeName == "" {
		modeName = bound.InitialMode()
	}
	mode, ok := bound.Mode(modeName)
	if !ok {
		return runtimeConfig{}, &loop.BindError{Kind: loop.BindInvalidDefinition, Name: string(modeName), Index: -1}
	}
	model := mode.Model
	limits := mode.ToolLimits
	return runtimeConfig{
		Client: bound.Client(), Model: model, System: bound.EffectiveSystem(), DrainTimeout: bound.DrainTimeout(),
		AgentName: bound.Name(), Engine: bound.Engine(), RuntimeContext: bound.RuntimeContext(),
		Tools: ToolSet{Permission: bound.Permission(), Registry: mode.Tools, Middlewares: bound.Middlewares(), MaxToolIterations: limits.Iterations, MaxToolCallsPerTurn: limits.Calls, MaxParallelToolCalls: limits.Parallel},
	}, nil
}

// runtimeConfig is frozen actor wiring resolved from one BoundDefinition.
type runtimeConfig struct {
	Client       inference.Client // required — caller constructs via auto.New at composition root
	Model        inference.Model  // secret-free model descriptor (name, endpoint, sampling) — stamped onto every Request; carries NO system prompt and NO secret
	System       string           // per-agent system prompt — sent on every Request AND hashed into the config fingerprint; the connection secret rides the Client, never here
	DrainTimeout time.Duration    // optional — bounds the hard-kill wait for a cancelled turn to drain; New defaults it to 5s

	// AgentName is the immutable attribution name the loop runs under (the agent/role
	// driving it, e.g. "operator"). It is stamped onto the loop's LoopStarted at creation
	// and never changes. Empty (the zero value) means an unnamed/plain loop. The session
	// reads it when publishing LoopStarted; restore validates the root loop's stamped name
	// against the configured primary's AgentName.
	AgentName identity.AgentName

	// Engine selects the loop backend. Zero = EngineNative (the historical path).
	// Foreign engines route construction through the injected foreign Builder at
	// the session composition root; loop.New itself only ever builds native.
	Engine loop.Engine

	// Tools is the runner's view of the tool subsystem (the consumer surface in
	// deps.go). Its runaway-guard caps are defaulted by New when zero;
	// Permission/Registry/Middlewares are left as the composition root set them
	// (nil is valid).
	Tools ToolSet

	// RuntimeContext, when non-nil, yields the volatile per-turn context blocks
	// (date/cwd/git) the loop appends at the TAIL of each turn's request — AFTER the
	// committed messages, and as a transient addition that NEVER enters committed
	// history and NEVER touches the System prompt (the cached prefix). It is consulted once
	// per turn, so each turn rides a single FRESH block and stale blocks never
	// accumulate. nil (the zero value, the New default) means OFF: the request is
	// assembled exactly as it was before — no extra blocks. The interface keeps the
	// loop free of os/exec; the concrete provider is wired at the composition root.
	RuntimeContext RuntimeContextProvider

	// idGen mints the loop's correlation IDs: the per-turn TurnID, each StepID,
	// and each tool-call ToolExecutionID. It is unexported, so the composition root cannot
	// set it: New defaults it to uuid.New. It exists only as a test seam for
	// exercising the crypto/rand failure branches.
	idGen idGenerator

	// now is the clock the loop's event Factory mints CreatedAt from. It is
	// unexported, so the composition root cannot set it: New defaults it to
	// time.Now. It exists only as a test seam so a test can pin CreatedAt
	// deterministically (mirrors idGen).
	now event.Clock

	// eventFactory mints the EventID + CreatedAt stamped onto every ENDURING loop
	// event at the single publish chokepoint (Ephemeral events are never persisted
	// and so are never stamped). It is unexported, so the composition root cannot
	// set it: New defaults it to a Factory built from idGen + now. It exists as a
	// test seam so a test can inject a deterministic (or failing) factory.
	eventFactory *event.Factory

	// afterDrain is a test-only synchronization seam invoked by foldPending in the
	// turn goroutine AFTER cfg.drainPending has moved the inbox into the actor's
	// draining buffer but BEFORE the first TurnFoldedInto commit. It is unexported,
	// so the composition root cannot set it; New never assigns it, so it is nil in
	// production and foldPending's nil check skips it entirely. It exists only so a
	// test can cancel the loop deterministically in the post-drain/pre-commit window
	// to exercise the draining-buffer abnormal-return sweep.
	afterDrain func()
}
