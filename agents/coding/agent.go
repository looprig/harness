// Package coding is the full-tool coding agent built on the session engine. It
// wraps a session.Session with the Togo coding persona and a named model, wiring
// ALL eleven tools (read/search, write/edit/exec, web, ask/todo, and a
// recursion-safe Subagent) each with only the dependencies it needs. It exposes
// the tui.Agent surface plus the Approve/Deny/ProvideAnswer gate trio.
package coding

import (
	"context"
	"crypto/tls"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/inventivepotter/urvi/agents/coding/prompts"
	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/session"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/auto"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/inventivepotter/urvi/tools"
)

// model is the named model this coding agent runs on. v1 reuses Kimi K2 (a strong
// agentic-coding model already in the catalog, text-only); swapping models is a
// one-line change here. Read-only after init: do not reassign or mutate it — the
// parallel fake-client tests read it concurrently.
var model = llm.ChutesKimiK2()

// envAPIKey is the only value read from the environment. The value is the NAME
// of an env var, not a secret; the #nosec annotation documents that gosec's
// G101 "hardcoded credentials" heuristic (which matches on the identifier name)
// is a false positive here.
const envAPIKey = "LLM_API_KEY" // #nosec G101 -- env var name, not a credential

// Coding is a persona-bearing wrapper over a session.Session. The caller
// owns it and must call Close to release the underlying actor goroutine.
type Coding struct {
	session       *session.Session
	cancel        context.CancelFunc // cancels the session's root context; called by Close
	acceptsImages bool               // captured from spec at construction; reported by AcceptsImages
}

// New constructs a Coding agent. The session runs under an agent-owned root
// context, so its lifetime is controlled by Close, not by ctx: ctx only bounds
// construction (New fails fast if it is already cancelled) and does not stop the
// session once New has returned — a request-scoped or timeout ctx is therefore
// safe to pass. New reads LLM_API_KEY (the only env-sourced value), refuses an
// unclassified provider (fail secure), fails loud if the provider requires a key
// and none is set, then builds the provider client via auto.New and starts the
// session actor. The caller owns the Coding agent and must call Close to release it.
func New(ctx context.Context) (*Coding, error) {
	needsKey, err := model.Provider.RequiresKey()
	if err != nil {
		return nil, err // unclassified provider — fail secure
	}
	apiKey := os.Getenv(envAPIKey)
	if needsKey && strings.TrimSpace(apiKey) == "" {
		// env is a boundary: treat whitespace-only as missing so the failure is
		// loud at startup, not deferred to provider call time.
		return nil, &MissingEnvError{Var: envAPIKey}
	}
	// Pass the key verbatim — never normalize credential material. The TrimSpace
	// above is a presence check, not a sanitizer.
	spec := model.Spec(apiKey, prompts.SystemPrompt)
	client, err := auto.New(spec) // validates spec + dispatches on provider
	if err != nil {
		return nil, err
	}
	return newWithClient(ctx, client, spec)
}

// newWithClient is the construction seam shared by New and tests; tests inject a
// fake llm.LLM here, avoiding real environment reads and network calls. It gives
// the session a root context derived from context.Background() — independent of
// the caller's ctx — so a request-scoped or timeout ctx passed to New cannot
// later tear the session down. ctx bounds only this construction call.
func newWithClient(ctx context.Context, client llm.LLM, spec llm.ModelSpec) (*Coding, error) {
	if err := ctx.Err(); err != nil {
		return nil, &session.SessionError{Kind: session.SessionContextDone, Cause: err}
	}
	// The workspace root is the process working directory: file tools are confined
	// to it and the PermissionChecker uses it for containment + path relativisation.
	root, err := os.Getwd()
	if err != nil {
		return nil, &WorkspaceRootError{Cause: err}
	}

	// The session's root context — independent of the caller's ctx — owns the
	// actor's lifetime AND is the base lifetime handed to the Subagent factory for
	// the children it spawns. Built before the tool set because the Subagent tool
	// needs it (design §4a).
	rootCtx, cancel := context.WithCancel(context.Background())

	httpCl := newHTTPClient()
	// The factory holds the deps to build child sessions lazily (per spawn); it
	// must be constructed before buildToolSet because the Subagent tool references
	// it. Children reuse the same factory, so the depth cap (carried in ctx by the
	// Subagent tool) bounds the recursion.
	factory, err := newCodingFactory(root, client, httpCl, rootCtx, spec)
	if err != nil {
		cancel()
		return nil, err
	}

	toolSet := buildToolSet(root, httpCl, rootCtx, factory)
	sess, err := session.New(rootCtx, loop.Config{Client: client, Model: spec, Tools: toolSet})
	if err != nil {
		cancel()
		return nil, err
	}
	return &Coding{session: sess, cancel: cancel, acceptsImages: spec.AcceptsImages}, nil
}

// autoApprovedTools is the coding agent's hard-approve set (design §4c): the six
// intrinsically safe, side-effect-free tools that run without prompting.
// WriteFile, EditFile, Bash, Fetch, and WebSearch are deliberately ABSENT — they
// mutate the filesystem, run a shell, or reach the network, so they stay Ask (the
// user approves each call). The names match each tool's Info().Name exactly; the
// PermissionChecker matches on them.
var autoApprovedTools = []string{"ReadFile", "Glob", "Grep", "Todo", "AskUser", "Subagent"}

// buildToolSet assembles ALL eleven tools and the fail-secure PermissionChecker
// that gates them (design §4d). It is shared by the parent session and every
// child the Subagent factory spawns, so a child gets the same full tool set —
// including a Subagent tool wired with the SAME factory, which lets a child spawn
// a depth-capped grandchild (the depth lives in ctx and is enforced by the
// Subagent tool, not here).
//
// Least privilege: each tool gets only the deps it needs — the read/search tools
// get the workspace root plus the checker as their ReadGuard; the write/exec
// tools get only the root; the web tools get an HTTP client with explicit
// timeouts and a TLS 1.2 floor and never touch the filesystem; AskUser/Todo are
// self-contained; Subagent gets the narrow factory plus the session root.
//
// A FRESH PermissionChecker is built per call so the parent and each child have
// independent session-scope approval state (a child's grant never leaks into the
// parent's policy, and vice versa).
func buildToolSet(root string, httpCl *http.Client, rootCtx context.Context, factory tools.SubagentFactory) loop.ToolSet {
	policy := tools.PermissionPolicy{
		WorkspaceRoot: root,
		HardDeny:      tools.DefaultHardDeny(),
		HardApprove:   tools.HardApproveRules{Tools: autoApprovedTools},
	}
	pc := tools.NewPermissionChecker(policy)

	registry := []tool.InvokableTool{
		tools.NewReadFile(root, pc),
		tools.NewGlob(root, pc),
		tools.NewGrep(root, pc),
		tools.NewWriteFile(root),
		tools.NewEditFile(root),
		tools.NewBash(root),
		tools.NewFetch(httpCl),
		tools.NewWebSearch(tools.NewDuckDuckGoProvider(httpCl)),
		tools.NewAskUser(),
		tools.NewTodo(),
		tools.NewSubagent(factory, rootCtx),
	}
	// Middlewares nil and the runaway-guard caps zero on purpose: loop.New applies
	// its safe defaults for the caps; the manifest declares no middleware in v1.
	return loop.ToolSet{Permission: pc, Registry: registry}
}

// httpClientTimeout bounds every web request the Fetch/WebSearch tools make, so
// a hung endpoint can never block a tool call indefinitely (CLAUDE.md: no
// unbounded blocking).
const httpClientTimeout = 30 * time.Second

// newHTTPClient builds the single *http.Client shared by Fetch and the
// DuckDuckGo provider. It sets an explicit overall timeout and pins the TLS floor
// to 1.2 (never InsecureSkipVerify), per CLAUDE.md's TLS rules.
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: httpClientTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
}

// Submit delivers a multimodal user message FIRE-AND-FORGET as a queueable
// (AllowFold) UserInput and returns the InputID — the Cause.CommandID the resulting
// Reply events (InputQueued / TurnStarted / TurnFoldedInto / TurnRejected /
// InputCancelled) carry on the session fan-in. The Go error is non-nil only when
// the command could not be handed to the loop (loop gone, or ctx done); the turn
// outcome is observed on the Subscribe stream, never returned. It delegates to the
// session; the wrapper holds no submit state of its own.
func (c *Coding) Submit(ctx context.Context, blocks []content.Block) (uuid.UUID, error) {
	return c.session.Submit(ctx, blocks)
}

// Subscribe attaches a whole-session event consumer to the session fan-in with
// filter and returns its event.Subscription (the session hub's *EventSubscription,
// which satisfies the interface). It is the seam a TUI/CLI uses to observe events
// across the whole session — every loop (including subagent loops), spanning turns.
// The caller Closes the returned subscription when done.
func (c *Coding) Subscribe(filter event.EventFilter) (event.Subscription, error) {
	return c.session.SubscribeEvents(filter)
}

// PrimaryLoopID returns the session's primary loop id, so a subscriber can build
// its EventFilter (primary-only Ephemeral + all-loop Enduring).
func (c *Coding) PrimaryLoopID() uuid.UUID { return c.session.PrimaryLoopID() }

// Interrupt cancels the running turn. Returns true if a turn was cancelled.
func (c *Coding) Interrupt(ctx context.Context) (bool, error) {
	return c.session.Interrupt(ctx)
}

// AcceptsImages reports whether the underlying model accepts image blocks. For
// v1's Kimi K2 (text-only) this is false.
func (c *Coding) AcceptsImages() bool { return c.acceptsImages }

// Approve resolves a pending tool-call permission gate, granting it at scope. It
// delegates verbatim to the session; the wrapper holds no gate state of its own.
// loopID is the loop that opened the gate, so the reply reaches the right loop in a
// multi-loop session (the session falls back to its primary loop for a zero id).
func (c *Coding) Approve(ctx context.Context, loopID, callID uuid.UUID, scope tool.ApprovalScope) error {
	return c.session.Approve(ctx, loopID, callID, scope)
}

// Deny resolves a pending tool-call permission gate by failing it closed
// (fail-secure); nothing is persisted. It delegates to the session. loopID names
// the gate-opening loop so the reply is dispatched there, not unconditionally to
// the primary loop.
func (c *Coding) Deny(ctx context.Context, loopID, callID uuid.UUID) error {
	return c.session.Deny(ctx, loopID, callID)
}

// ProvideAnswer supplies the user's reply to a pending AskUser request. It is the
// TUI-facing name for the session's ProvideUserInput, to which it delegates.
// loopID names the gate-opening loop so the answer reaches the right loop.
func (c *Coding) ProvideAnswer(ctx context.Context, loopID, callID uuid.UUID, answer string) error {
	return c.session.ProvideUserInput(ctx, loopID, callID, answer)
}

// Close gracefully shuts the session down and releases the session's root
// context. It blocks until the actor exits (or ctx is done), then cancels the
// root as a backstop so the actor goroutine cannot leak even if Shutdown timed
// out on ctx. Cancelling the root also tears down any in-flight child Subagent
// session (children derive their root from it). Safe to call more than once.
func (c *Coding) Close(ctx context.Context) error {
	err := c.session.Shutdown(ctx)
	c.cancel()
	return err
}
