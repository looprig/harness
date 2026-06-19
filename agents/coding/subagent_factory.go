package coding

import (
	"context"
	"net/http"
	"strings"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/session"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/registry"
	"github.com/inventivepotter/urvi/tools"
)

// subagent_factory.go wires the concrete tools.SubagentFactory the coding
// manifest needs (design §4b, §4d). It is the composition-root adapter that lets
// the Subagent TOOL — which depends only on the narrow tools.SubagentFactory and
// tools.Subsession interfaces — spawn real child sessions without the tools/
// package ever importing session (keeping tools → session a one-way dependency
// that lives only here).
//
// RECURSION SAFETY. The factory holds the child-construction deps (root, spec,
// HTTP client, permission checker, skill→persona registry) and builds a child
// session ONLY when New is actually called for a spawn — never at parent
// construction, which would recurse forever. A child's tool set is built by the
// SAME buildToolSet helper the parent uses, and its Subagent tool is wired with
// the SAME factory, so a child can itself spawn a grandchild. The recursion is
// bounded by the depth cap the Subagent TOOL carries in ctx (tools/subagent.go,
// T14e): the factory does NOT manage depth — it only constructs.
//
// CHILD LIFETIME. Each spawn gets its own root context derived from the factory's
// rootCtx; the Subsession adapter Closes that child session (and cancels its root)
// after a single Invoke completes, so a spawn never leaks the child actor goroutine.

// codingSkill is the one persona/skill the v1 factory knows. The full skill
// catalog is out of scope (design "Out of scope"); an unknown skill resolves to a
// *registry.UnknownNameError, which the Subagent tool surfaces as a tool-result
// error string.
const codingSkill = "coding"

// childModelSpec is the narrow value a persona constructor yields: enough to build
// a child session.NewAgent. It carries the materialized spec (model + system
// prompt) and nothing else — the remaining deps (client, root, pc) belong to the
// factory, not the persona.
type childModelSpec struct {
	spec llm.ModelSpec
}

// codingFactory adapts the real session engine to tools.SubagentFactory. It owns
// only the construction deps; it builds no session until New is called.
type codingFactory struct {
	root     string                             // workspace root the child's file tools are confined to
	client   llm.LLM                            // provider client shared with the parent (no per-child client)
	httpCl   *http.Client                       // web client for the child's Fetch/WebSearch tools
	rootCtx  context.Context                    // base lifetime for child sessions (the manifest's session root)
	personas *registry.Registry[childModelSpec] // skill → child model spec
}

// newCodingFactory builds the factory from the deps already in hand at the
// composition root. It registers the v1 "coding" persona (the only skill); an
// unknown skill at spawn time yields a *registry.UnknownNameError. personas is
// constructed here (not injected) because the persona set is the manifest's own
// fixed policy in v1.
func newCodingFactory(root string, client llm.LLM, httpCl *http.Client, rootCtx context.Context, spec llm.ModelSpec) (*codingFactory, error) {
	personas := registry.New[childModelSpec]()
	cs := childModelSpec{spec: spec}
	if err := personas.Register(codingSkill, func(context.Context) (childModelSpec, error) {
		return cs, nil
	}); err != nil {
		return nil, err
	}
	return &codingFactory{
		root:     root,
		client:   client,
		httpCl:   httpCl,
		rootCtx:  rootCtx,
		personas: personas,
	}, nil
}

// New resolves skill → persona via the registry, then LAZILY builds a child
// session for the spawn. The child gets its own tool set (built by the shared
// buildToolSet, whose Subagent tool references THIS factory) so it can itself
// spawn a depth-capped grandchild. The child runs under its own cancelable root
// derived from the factory's rootCtx; the returned Subsession owns that root and
// releases it (and the child actor) when its single Invoke completes. An unknown
// skill or any construction failure is returned as an error, which the Subagent
// tool turns into a tool-result error string.
func (f *codingFactory) New(ctx context.Context, skill string) (tools.Subsession, error) {
	persona, err := f.personas.Open(ctx, strings.TrimSpace(skill))
	if err != nil {
		return nil, err // *registry.UnknownNameError on an unknown skill
	}

	// The child's lifetime is its own cancelable context derived from the
	// session root, NOT from ctx: ctx carries the spawn depth (which we must
	// honor for the cap) but it is the per-call turn context, so the child must
	// not outlive a single Invoke regardless. The adapter cancels childRoot in
	// Close, guaranteeing the actor goroutine cannot leak.
	childRoot, cancel := context.WithCancel(f.rootCtx)

	toolSet := buildToolSet(f.root, f.httpCl, childRoot, f)
	child, err := session.NewAgent(childRoot, loop.Config{Client: f.client, Model: persona.spec, Tools: toolSet})
	if err != nil {
		cancel()
		return nil, err
	}
	return &childSubsession{session: child, cancel: cancel}, nil
}

// childSubsession adapts a child session.AgentSession to the tools.Subsession
// contract: run the child to completion on one message and return its final
// assistant text. It owns the child's lifetime and tears it down after Invoke so
// a spawn never leaks the child actor goroutine.
type childSubsession struct {
	session *session.Sesssion
	cancel  context.CancelFunc // cancels the child root; called after Invoke
}

// Invoke drives the child to a terminal event on message and projects the result
// into a string. It Closes the child afterward (graceful Shutdown + root cancel
// as a backstop) so the actor goroutine is always released. The projection:
// TurnDone → concatenate the text blocks of Message; TurnFailed/TurnInterrupted →
// a typed error the Subagent tool surfaces as a tool-result string.
func (c *childSubsession) Invoke(ctx context.Context, message string) (string, error) {
	defer c.close(ctx)

	ev, err := c.session.Invoke(ctx, []content.Block{&content.TextBlock{Text: message}})
	if err != nil {
		return "", err
	}
	switch e := ev.(type) {
	case event.TurnDone:
		return aiMessageText(e.Message), nil
	case event.TurnFailed:
		return "", &SubagentTurnError{Cause: e.Err}
	case event.TurnInterrupted:
		return "", &SubagentInterruptedError{}
	default:
		return "", &SubagentTurnError{Cause: nil}
	}
}

// close gracefully shuts the child session down and cancels its root as a
// backstop, so the actor goroutine cannot leak even if Shutdown timed out.
func (c *childSubsession) close(ctx context.Context) {
	_ = c.session.Shutdown(ctx)
	c.cancel()
}

// aiMessageText concatenates the text of every *content.TextBlock in m, ignoring
// non-text blocks. A nil message yields the empty string.
func aiMessageText(m *content.AIMessage) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range m.Blocks {
		if tb, ok := blk.(*content.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

// compile-time assertion: codingFactory satisfies the tool's narrow factory
// interface, and childSubsession its narrow session interface.
var (
	_ tools.SubagentFactory = (*codingFactory)(nil)
	_ tools.Subsession      = (*childSubsession)(nil)
)
