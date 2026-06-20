package tui

import (
	"context"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// EventStream is the narrow consumer-facing handle the TUI reads whole-session
// events from. It is event.Subscription — the read+teardown contract the session
// hub's *EventSubscription satisfies structurally — so the TUI depends on the
// interface, not the concrete hub type. Events yields the filtered fan-in stream;
// it closes on Close or on a hub-forced loss, after which Err reports the typed
// cause (nil for an intentional Close).
type EventStream = event.Subscription

// Agent is the narrow surface the TUI drives. *personalassistant.Assistant
// satisfies it structurally; the TUI never imports any agent package.
type Agent interface {
	StreamBlocks(ctx context.Context, blocks []content.Block) (*llm.StreamReader[event.Event], error)
	// Submit sends input fire-and-forget as a queueable UserInput; the returned
	// InputID correlates the Reply events (Cause.CommandID) that report the outcome.
	Submit(ctx context.Context, blocks []content.Block) (uuid.UUID, error)
	// PrimaryLoopID is the loop whose live Ephemeral stream the TUI watches; used to
	// build the DefaultEventFilter for the session subscription.
	PrimaryLoopID() uuid.UUID
	Interrupt(ctx context.Context) (bool, error)
	Close(ctx context.Context) error
	// AcceptsImages reports whether the model accepts image blocks, so buildBlocks
	// can reject image @path tokens at the boundary instead of failing mid-turn.
	AcceptsImages() bool

	// Subscribe attaches a whole-session event consumer to the agent's session
	// fan-in with the given filter and returns its EventStream. It is the seam the
	// TUI uses to observe events across the entire session (every loop), distinct
	// from the per-turn StreamBlocks reader: the per-turn reader closes when one
	// turn ends, whereas a session subscription spans turns and loops. The caller
	// must Close the returned stream when done. Use DefaultEventFilter for the
	// single-loop TUI default. The full transport switch from the per-turn reader
	// to this subscription is owned by the separate TUI-adoption spec.
	Subscribe(filter event.EventFilter) (EventStream, error)

	// Approve resolves a pending tool-call permission gate, granting it at the
	// chosen persistence scope. loopID is the loop that opened the gate (the
	// PermissionRequested event's Header.LoopID) so the reply is dispatched to the
	// right loop in a multi-loop session; callID identifies the gate. The agent
	// wrapper delegates to its session.
	Approve(ctx context.Context, loopID, callID uuid.UUID, scope tool.ApprovalScope) error
	// Deny resolves a pending tool-call permission gate by failing it closed
	// (fail-secure); nothing is persisted. loopID names the gate-opening loop so the
	// reply reaches the right loop. The wrapper delegates to its session.
	Deny(ctx context.Context, loopID, callID uuid.UUID) error
	// ProvideAnswer supplies the user's reply to a pending AskUser request
	// identified by callID. loopID names the gate-opening loop so the answer reaches
	// the right loop. It is the TUI-facing name for the session's ProvideUserInput;
	// the wrapper delegates to it.
	ProvideAnswer(ctx context.Context, loopID, callID uuid.UUID, answer string) error
}

// DefaultEventFilter is the single-loop TUI's declared interest for a session
// subscription: live Ephemeral events (TokenDelta + tool lifecycle, i.e.
// ToolCallStarted/Completed) from the PRIMARY loop only, and Enduring events
// (StepDone, gates, terminals) from EVERY loop. This is the spec's example filter —
// a TUI streams the primary loop's live progress (tokens AND tool spinners) while
// still seeing the finalized output of any subagent loop at StepDone granularity
// (those appear collapsed-but-present, attributed by Header.LoopID). Session-scoped
// events (SessionStarted/Active/Idle/Stopped) bypass the loop filter and always
// deliver.
//
// primaryLoopID names the loop whose live firehose the TUI wants; a subagent's
// tokens AND its tool-lifecycle chatter, excluded by the Ephemeral scope, never even
// enter the subscriber's egress buffer — by design amendment 1 the subagent's tools
// surface only via its Enduring StepDone, not a live per-call view.
func DefaultEventFilter(primaryLoopID uuid.UUID) event.EventFilter {
	return event.EventFilter{
		Ephemeral: event.LoopScope{Loops: map[uuid.UUID]struct{}{primaryLoopID: {}}},
		Enduring:  event.LoopScope{All: true},
	}
}

// OpenAgent constructs a fresh Agent. The composition root binds it to
// registry.Open(name); the TUI calls it on /clear to replace the current agent.
type OpenAgent func(context.Context) (Agent, error)
