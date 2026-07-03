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
// satisfy Event (isEvent + Class + Scope + EndsTurn + EventHeader). The intended
// "exactly one lifecycle mixin and exactly one scope mixin" is enforced from both
// sides: embedding two of a kind makes the promoted selector ambiguous (won't
// compile), and embedding zero leaves the method missing (won't satisfy Event) —
// so any count other than one fails these assertions.
var (
	// Session-scoped events.
	_ Event = SessionStarted{}
	_ Event = SessionActive{}
	_ Event = SessionIdle{}
	_ Event = SessionStopped{}
	_ Event = RestoreStarted{}
	_ Event = RestoreDone{}
	_ Event = RestoreErrored{}
	_ Event = WorkspaceCheckpointed{}

	// Loop-scoped events.
	_ Event = LoopIdle{}
	_ Event = LoopStarted{}

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
	_ Event = ToolCallStarted{}
	_ Event = ToolCallCompleted{}
)
