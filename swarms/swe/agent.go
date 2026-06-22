package swe

import (
	"context"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/session"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// sessionAgent is a thin wrapper over a session.Session that exposes the tui.Agent
// surface (the streaming/lifecycle methods plus the Approve/Deny/ProvideAnswer gate
// trio). It is salvaged from agents/coding's Coding wrapper, generalized over an
// arbitrary primary loop.Config so the SAME wrapper drives the orchestrator (the
// swarm's primary) now and the operator-for-eval primary in a later phase. The
// caller owns it and must call Close to release the underlying actor goroutine.
//
// It holds no submit/gate/subscribe state of its own — every method delegates to
// the session — so the wrapper's sole responsibility is lifetime ownership (the
// agent-owned root cancel) and reporting the static AcceptsImages modality.
type sessionAgent struct {
	session       *session.Session
	cancel        context.CancelFunc // cancels the session's root context; called by Close
	acceptsImages bool               // captured from the primary spec at construction; reported by AcceptsImages
}

// newSessionAgent constructs a sessionAgent from a finished primary loop.Config and
// optional session options (e.g. session.WithLimits). It gives the session a root
// context derived from context.Background() — INDEPENDENT of the caller's ctx — so a
// request-scoped or timeout ctx passed in cannot later tear the session down; ctx
// bounds only this construction call. Because the session root is background-derived,
// session.New cannot observe a cancelled caller ctx, so newSessionAgent checks the
// caller ctx itself and fails fast with a typed *session.SessionError on a cancelled
// ctx (fail secure). On any session.New failure it cancels the root so nothing leaks.
func newSessionAgent(ctx context.Context, primary loop.Config, opts ...session.Option) (*sessionAgent, error) {
	if err := ctx.Err(); err != nil {
		return nil, &session.SessionError{Kind: session.SessionContextDone, Cause: err}
	}

	// The session's root context — independent of the caller's ctx — owns the actor's
	// lifetime (and, transitively, every sub-loop the session spawns).
	rootCtx, cancel := context.WithCancel(context.Background())

	sess, err := session.New(rootCtx, primary, opts...)
	if err != nil {
		cancel()
		return nil, err
	}
	return &sessionAgent{session: sess, cancel: cancel, acceptsImages: primary.Model.AcceptsImages}, nil
}

// Submit delivers a multimodal user message FIRE-AND-FORGET as a queueable
// UserInput and returns the InputID — the Cause.CommandID the resulting Reply
// events carry on the session fan-in. The Go error is non-nil only when the command
// could not be handed to the loop (loop gone, or ctx done); the turn outcome is
// observed on the Subscribe stream, never returned. Delegates to the session.
func (a *sessionAgent) Submit(ctx context.Context, blocks []content.Block) (uuid.UUID, error) {
	return a.session.Submit(ctx, blocks)
}

// Subscribe attaches a whole-session event consumer to the session fan-in with
// filter and returns its event.Subscription. It is the seam a TUI/CLI uses to
// observe events across the whole session (every loop, spanning turns). The caller
// Closes the returned subscription when done.
func (a *sessionAgent) Subscribe(filter event.EventFilter) (event.Subscription, error) {
	return a.session.SubscribeEvents(filter)
}

// PrimaryLoopID returns the session's primary loop id, so a subscriber can build its
// EventFilter (primary-only Ephemeral + all-loop Enduring).
func (a *sessionAgent) PrimaryLoopID() uuid.UUID { return a.session.PrimaryLoopID() }

// ReplayBacklog returns nil/nil: a headless (non-persisted) session has no journal
// to replay, so there is no cold-restore backlog to repaint — the TUI then behaves
// exactly as a fresh session. A persisted variant (a later phase) would open the
// journal replayer here, mirroring agents/coding's restored path.
func (a *sessionAgent) ReplayBacklog(ctx context.Context) ([]event.Event, error) {
	return nil, nil
}

// Interrupt cancels the running turn. Returns true if a turn was cancelled.
func (a *sessionAgent) Interrupt(ctx context.Context) (bool, error) {
	return a.session.Interrupt(ctx)
}

// AcceptsImages reports whether the underlying model accepts image blocks, captured
// from the primary spec at construction.
func (a *sessionAgent) AcceptsImages() bool { return a.acceptsImages }

// Approve resolves a pending tool-call permission gate, granting it at scope. It
// delegates to the session. loopID is the loop that opened the gate, so the reply
// reaches the right loop in a multi-loop session.
func (a *sessionAgent) Approve(ctx context.Context, loopID, callID uuid.UUID, scope tool.ApprovalScope) error {
	return a.session.Approve(ctx, loopID, callID, scope)
}

// Deny resolves a pending tool-call permission gate by failing it closed
// (fail-secure); nothing is persisted. It delegates to the session. loopID names the
// gate-opening loop so the reply is dispatched there.
func (a *sessionAgent) Deny(ctx context.Context, loopID, callID uuid.UUID) error {
	return a.session.Deny(ctx, loopID, callID)
}

// ProvideAnswer supplies the user's reply to a pending AskUser request. It is the
// TUI-facing name for the session's ProvideUserInput, to which it delegates. loopID
// names the gate-opening loop so the answer reaches the right loop.
func (a *sessionAgent) ProvideAnswer(ctx context.Context, loopID, callID uuid.UUID, answer string) error {
	return a.session.ProvideUserInput(ctx, loopID, callID, answer)
}

// Close gracefully shuts the session down and releases the session's root context.
// It blocks until the actor exits (or ctx is done), then cancels the root as a
// backstop so the actor goroutine cannot leak even if Shutdown timed out on ctx.
// Cancelling the root also tears down every in-session sub-loop (they run under the
// same session root). Safe to call more than once.
func (a *sessionAgent) Close(ctx context.Context) error {
	err := a.session.Shutdown(ctx)
	a.cancel()
	return err
}
