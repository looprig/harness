package tui

import (
	"context"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
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
}

// OpenAgent constructs a fresh Agent. The composition root binds it to
// registry.Open(name); the TUI calls it on /clear to replace the current agent.
type OpenAgent func(context.Context) (Agent, error)
