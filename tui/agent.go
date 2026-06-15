package tui

import (
	"context"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// Agent is the narrow surface the TUI drives. *personalassistant.Assistant
// satisfies it structurally; the TUI never imports any agent package.
type Agent interface {
	StreamBlocks(ctx context.Context, blocks []content.Block) (*llm.StreamReader[event.Event], error)
	Interrupt(ctx context.Context) (bool, error)
	Close(ctx context.Context) error
	// AcceptsImages reports whether the model accepts image blocks, so buildBlocks
	// can reject image @path tokens at the boundary instead of failing mid-turn.
	AcceptsImages() bool

	// Approve resolves a pending tool-call permission gate, granting it at the
	// chosen persistence scope. callID identifies the gate (carried on the
	// PermissionRequested event). The agent wrapper delegates to its session.
	Approve(ctx context.Context, callID uuid.UUID, scope tool.ApprovalScope) error
	// Deny resolves a pending tool-call permission gate by failing it closed
	// (fail-secure); nothing is persisted. The wrapper delegates to its session.
	Deny(ctx context.Context, callID uuid.UUID) error
	// ProvideAnswer supplies the user's reply to a pending AskUser request
	// identified by callID. It is the TUI-facing name for the session's
	// ProvideUserInput; the wrapper delegates to it.
	ProvideAnswer(ctx context.Context, callID uuid.UUID, answer string) error
}

// OpenAgent constructs a fresh Agent. The composition root binds it to
// registry.Open(name); the TUI calls it on /clear to replace the current agent.
type OpenAgent func(context.Context) (Agent, error)
