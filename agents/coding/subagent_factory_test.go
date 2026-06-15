package coding

import (
	"context"
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/registry"
)

// newTestFactory builds a codingFactory over a fake client for unit tests. It
// uses its own cancelable root so the test can tear down any child the factory
// spawns; t.Cleanup cancels it as a backstop against a leaked child actor.
func newTestFactory(t *testing.T, client *fakeLLM) *codingFactory {
	t.Helper()
	rootCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f, err := newCodingFactory("/tmp/workspace-root", client, newHTTPClient(), rootCtx, testSpec())
	if err != nil {
		t.Fatalf("newCodingFactory: %v", err)
	}
	return f
}

// TestFactoryNewCodingRoundTrips proves the happy path of the recursion-safe
// factory: factory.New(ctx, "coding") resolves the persona, lazily builds a child
// session, and the returned Subsession.Invoke drives that child to a TurnDone and
// returns the concatenated text of the child's final message. The child is driven
// by a fake client scripted to stream "child-reply", proving the adapter projects
// TurnDone.Message text correctly.
func TestFactoryNewCodingRoundTrips(t *testing.T) {
	t.Parallel()

	f := newTestFactory(t, &fakeLLM{chunks: []content.Chunk{textChunk("child-"), textChunk("reply")}})

	sub, err := f.New(context.Background(), codingSkill)
	if err != nil {
		t.Fatalf("factory.New(coding) error = %v", err)
	}
	got, err := sub.Invoke(context.Background(), "do the thing")
	if err != nil {
		t.Fatalf("Subsession.Invoke error = %v", err)
	}
	if got != "child-reply" {
		t.Errorf("Invoke text = %q, want %q", got, "child-reply")
	}
}

// TestFactoryNewUnknownSkill proves an unknown skill resolves to a typed
// *registry.UnknownNameError and builds NO child (the registry lookup fails before
// any session is constructed). The Subagent tool surfaces this as a tool-result
// error string.
func TestFactoryNewUnknownSkill(t *testing.T) {
	t.Parallel()

	f := newTestFactory(t, &fakeLLM{})

	tests := []struct {
		name  string
		skill string
	}{
		{name: "unknown name", skill: "nonexistent-skill"},
		{name: "empty name", skill: ""},
		{name: "whitespace name", skill: "   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sub, err := f.New(context.Background(), tt.skill)
			if sub != nil {
				t.Errorf("Subsession = %v, want nil on unknown skill", sub)
			}
			var une *registry.UnknownNameError
			if !errors.As(err, &une) {
				t.Fatalf("err = %v, want *registry.UnknownNameError", err)
			}
		})
	}
}

// TestFactoryChildTurnFailureSurfacesError proves a child whose turn fails (the
// fake's provider errors) makes Subsession.Invoke return a typed
// *SubagentTurnError carrying the cause — the Subagent tool turns this into a
// tool-result error string rather than crashing.
func TestFactoryChildTurnFailureSurfacesError(t *testing.T) {
	t.Parallel()

	f := newTestFactory(t, &fakeLLM{streamErr: errFakeProvider})

	sub, err := f.New(context.Background(), codingSkill)
	if err != nil {
		t.Fatalf("factory.New(coding) error = %v", err)
	}
	got, err := sub.Invoke(context.Background(), "do the thing")
	if got != "" {
		t.Errorf("Invoke text = %q, want empty on failure", got)
	}
	var ste *SubagentTurnError
	if !errors.As(err, &ste) {
		t.Fatalf("err = %v, want *SubagentTurnError", err)
	}
	if !errors.Is(ste.Cause, errFakeProvider) {
		t.Errorf("SubagentTurnError.Cause = %v, want errors.Is errFakeProvider", ste.Cause)
	}
}

// TestAIMessageText proves the projection helper concatenates only text blocks,
// ignores non-text blocks, and tolerates a nil message.
func TestAIMessageText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  *content.AIMessage
		want string
	}{
		{name: "nil message", msg: nil, want: ""},
		{
			name: "single text block",
			msg:  &content.AIMessage{Message: content.Message{Blocks: []content.Block{&content.TextBlock{Text: "hi"}}}},
			want: "hi",
		},
		{
			name: "multiple text blocks concatenate",
			msg:  &content.AIMessage{Message: content.Message{Blocks: []content.Block{&content.TextBlock{Text: "he"}, &content.TextBlock{Text: "llo"}}}},
			want: "hello",
		},
		{
			name: "non-text blocks ignored",
			msg:  &content.AIMessage{Message: content.Message{Blocks: []content.Block{&content.ThinkingBlock{Thinking: "secret"}, &content.TextBlock{Text: "ok"}}}},
			want: "ok",
		},
		{
			name: "no blocks",
			msg:  &content.AIMessage{Message: content.Message{Blocks: nil}},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := aiMessageText(tt.msg); got != tt.want {
				t.Errorf("aiMessageText() = %q, want %q", got, tt.want)
			}
		})
	}
}
