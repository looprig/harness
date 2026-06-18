// Package event defines the sealed union of loop-machine events.
//
// Every concrete event embeds a Header (producer identity), exactly one lifecycle
// mixin (ephemeral, enduring, or terminal — supplying Class()/EndsTurn()), and
// exactly one scope mixin (sessionScoped or loopScoped — supplying Scope()). The
// compile-time assertions below pin the sealed union: every concrete event type
// must satisfy Event, and the list is the authoritative enumeration of the union.
// Adding a new event without adding it here is harmless, but removing a type or
// breaking its interface satisfaction fails the build here first.
package event

// Sealed-union interface-satisfaction assertions. Each concrete event must
// satisfy Event (isEvent + Class + Scope + EndsTurn + EventHeader). Embedding two
// lifecycle mixins or two scope mixins makes the selectors ambiguous and the type
// stops satisfying Event, so these assertions also enforce "exactly one of each".
var (
	// Session-scoped events.
	_ Event = SessionStarted{}
	_ Event = SessionActive{}
	_ Event = SessionIdle{}
	_ Event = SessionStopped{}

	// Loop-scoped event.
	_ Event = LoopIdle{}

	// Turn/step-scoped events.
	_ Event = TokenDelta{}
	_ Event = TurnStarted{}
	_ Event = StepDone{}
	_ Event = TurnFoldedInto{}
	_ Event = InputCancelled{}
	_ Event = TurnDone{}
	_ Event = TurnFailed{}
	_ Event = TurnInterrupted{}

	// Gate/tool lifecycle events.
	_ Event = PermissionRequested{}
	_ Event = UserInputRequested{}
	_ Event = UserInputRequestedSink{}
	_ Event = ToolCallStarted{}
	_ Event = ToolCallCompleted{}
)

// Redactable-satisfaction assertions for the events that carry sensitive payload.
// These pin which events provide a SinkProjection; the runtime test
// TestRedactableImplementations enforces the complementary "must NOT redact" set.
var (
	_ Redactable = PermissionRequested{}
	_ Redactable = UserInputRequested{}
	_ Redactable = ToolCallCompleted{}
	_ Redactable = TokenDelta{}
	_ Redactable = TurnDone{}
)
