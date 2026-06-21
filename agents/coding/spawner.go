package coding

import (
	"context"
	"net/http"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/session"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/tools"
)

// spawner.go wires the concrete tools.Spawner the coding manifest needs (design
// §2/§3). It is the composition-root adapter that lets the Subagent TOOL — which
// depends only on the narrow tools.Spawner interface — run an IN-SESSION subagent
// loop via session.Session.RunSubagent, without the tools/ package ever importing
// session (keeping tools → session a one-way dependency that lives only here).
//
// RECURSION. Each Spawn builds a FRESH ToolSet whose Subagent tool is wired with
// THIS same spawner, so a sub-loop can itself spawn a grandchild. The recursion is
// unbounded — the depth cap was intentionally dropped (design §8): subagent loops
// are siblings under the session, persist idle, and route follow-ups back, so
// there is no per-call child session to bound.

// codingSpawner adapts the real session engine to tools.Spawner. It owns the
// construction deps for the per-spawn ToolSet plus a late-bound reference to the
// live session that runs the sub-loop.
//
// LATE-BIND. The session field is set ONCE, synchronously, by newWithClient right
// after session.New returns and BEFORE any turn runs — the tools are built before
// the session exists (the Subagent tool needs this spawner, the spawner needs the
// session), so the cycle is resolved by a single post-construction assignment. No
// goroutine reads session until a turn invokes the Subagent tool, which cannot
// happen until after Submit, which cannot happen until after New has returned; so
// the unsynchronized write/read pair never races.
type codingSpawner struct {
	session *session.Session // late-bound after session.New (see newWithClient)
	root    string           // workspace root the sub-loop's file tools are confined to
	httpCl  *http.Client     // web client for the sub-loop's Fetch/WebSearch tools
	client  llm.LLM          // provider client shared with the parent (no per-loop client)
	spec    llm.ModelSpec    // model + system prompt the sub-loop runs on
}

// Spawn runs a subagent as an in-session loop spawned under parent, on message,
// and returns its final assistant text. It builds a FRESH ToolSet per call so the
// sub-loop gets its own session-scope PermissionChecker — a sub-loop's approval
// grants never leak into the parent's policy, and vice versa (per-loop approval
// isolation). The fresh tool set's Subagent tool is wired with THIS spawner, so a
// sub-loop can recurse into a grandchild.
func (sp *codingSpawner) Spawn(ctx context.Context, parent loop.Provenance, message string) (string, error) {
	toolSet := buildToolSet(sp.root, sp.httpCl, sp)
	cfg := loop.Config{Client: sp.client, Model: sp.spec, Tools: toolSet}
	blocks := []content.Block{&content.TextBlock{Text: message}}
	return sp.session.RunSubagent(ctx, parent, cfg, blocks)
}

// compile-time assertion: codingSpawner satisfies the tool's narrow Spawner
// interface.
var _ tools.Spawner = (*codingSpawner)(nil)
