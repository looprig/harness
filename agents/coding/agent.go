// Package coding is the full-tool coding agent built on the session engine. It
// wraps a session.Session with the Togo coding persona and a named model, wiring
// ALL eleven tools (read/search, write/edit/exec, web, ask/todo, and a
// recursion-safe Subagent) each with only the dependencies it needs. It exposes
// the tui.Agent surface plus the Approve/Deny/ProvideAnswer gate trio.
package coding

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/inventivepotter/urvi/agents/coding/prompts"
	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/session"
	"github.com/inventivepotter/urvi/internal/agent/session/journal"
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

	// teardown is the composition-root persistence teardown the persisted constructors
	// install: it releases the single-writer lease (so a successor can re-acquire without
	// waiting out the TTL) and stops the GC ticker. It is nil for a non-persisted (headless
	// / fake-only) agent, so Close is unchanged in that mode. Run AFTER session.Shutdown so
	// the journal has finished its last append before the lease is relinquished. Idempotent
	// (guarded by teardownOnce) so Close can safely be called more than once.
	teardown     func(context.Context) error
	teardownOnce sync.Once

	// replayer is the journal-backed read side a RESTORED session's ReplayBacklog drains
	// for the TUI's cold-restore repaint. It is nil for a NEW session (new sessions have no
	// backlog to repaint → ReplayBacklog returns nil). restoredSessionID/restoredPrimaryLoopID
	// scope the cold replay to the primary loop's session view.
	replayer              journal.EventReplayer
	restoredSessionID     uuid.UUID
	restoredPrimaryLoopID uuid.UUID
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
	client, spec, err := buildClient()
	if err != nil {
		return nil, err
	}
	return newWithClient(ctx, client, spec)
}

// buildClient is the credential/provider boundary shared by New and NewPersistent: it
// reads LLM_API_KEY (the only env-sourced value), refuses an unclassified provider (fail
// secure), fails loud (typed *MissingEnvError) if the provider requires a key and none is
// set, then builds + validates the provider client via auto.New. It is the single place
// the API key is read and passed verbatim (never normalized — the TrimSpace is a presence
// check, not a sanitizer), so the #nosec rationale on envAPIKey lives in one spot.
func buildClient() (llm.LLM, llm.ModelSpec, error) {
	needsKey, err := model.Provider.RequiresKey()
	if err != nil {
		return nil, llm.ModelSpec{}, err // unclassified provider — fail secure
	}
	apiKey := os.Getenv(envAPIKey)
	if needsKey && strings.TrimSpace(apiKey) == "" {
		// env is a boundary: treat whitespace-only as missing so the failure is
		// loud at startup, not deferred to provider call time.
		return nil, llm.ModelSpec{}, &MissingEnvError{Var: envAPIKey}
	}
	// Pass the key verbatim — never normalize credential material.
	spec := model.Spec(apiKey, prompts.SystemPrompt)
	client, err := auto.New(spec) // validates spec + dispatches on provider
	if err != nil {
		return nil, llm.ModelSpec{}, err
	}
	return client, spec, nil
}

// newWithClient is the construction seam shared by New and tests; tests inject a
// fake llm.LLM here, avoiding real environment reads and network calls. It gives
// the session a root context derived from context.Background() — independent of
// the caller's ctx — so a request-scoped or timeout ctx passed to New cannot
// later tear the session down. ctx bounds only this construction call.
func newWithClient(ctx context.Context, client llm.LLM, spec llm.ModelSpec) (*Coding, error) {
	build, err := newLoopBuild(ctx, client, spec)
	if err != nil {
		return nil, err
	}
	// Headless / no-persistence: a bare session.New (nop appenders). The spawner is
	// late-bound after the session exists (see codingSpawner doc).
	sess, err := session.New(build.rootCtx, build.cfg)
	if err != nil {
		build.cancel()
		return nil, err
	}
	build.spawner.session = sess
	return &Coding{session: sess, cancel: build.cancel, acceptsImages: spec.AcceptsImages}, nil
}

// loopBuild is the shared construction artifact both the headless (newWithClient) and
// persisted (newPersistentWithClient) constructors produce before calling session.New /
// session.Restore: the agent-owned root context (+ its cancel), the late-bindable
// spawner, and the loop.Config. Extracting it keeps the workspace-root + http-client +
// tool-set + spawner-cycle wiring in ONE place so the two construction paths cannot drift.
type loopBuild struct {
	rootCtx context.Context
	cancel  context.CancelFunc
	spawner *codingSpawner
	cfg     loop.Config
}

// newLoopBuild assembles the shared loopBuild: it validates ctx (fail-fast if already
// cancelled), resolves the workspace root, builds the agent-owned root context, the HTTP
// client, the spawner (empty session, late-bound by the caller after the session exists),
// and the tool set, then returns the loop.Config. On any failure it returns a typed error
// and leaks nothing (the root context is only created after the fail-fast checks pass, and
// the caller owns cancelling it once it has a Coding).
func newLoopBuild(ctx context.Context, client llm.LLM, spec llm.ModelSpec) (loopBuild, error) {
	if err := ctx.Err(); err != nil {
		return loopBuild{}, &session.SessionError{Kind: session.SessionContextDone, Cause: err}
	}
	// The workspace root is the process working directory: file tools are confined
	// to it and the PermissionChecker uses it for containment + path relativisation.
	root, err := os.Getwd()
	if err != nil {
		return loopBuild{}, &WorkspaceRootError{Cause: err}
	}

	// The session's root context — independent of the caller's ctx — owns the
	// actor's lifetime (and, transitively, every sub-loop the session spawns).
	rootCtx, cancel := context.WithCancel(context.Background())

	httpCl := newHTTPClient()

	// Resolve the spawner↔session cycle by late-binding: the Subagent tool needs the
	// spawner, and the spawner needs the live session, but the tools must be built BEFORE
	// session.New (it takes the ToolSet). So build the spawner empty here; the caller wires
	// spawner.session once — synchronously, after session construction, before any turn.
	spawner := &codingSpawner{root: root, httpCl: httpCl, client: client, spec: spec}
	toolSet := buildToolSet(root, httpCl, spawner)
	return loopBuild{
		rootCtx: rootCtx,
		cancel:  cancel,
		spawner: spawner,
		cfg:     loop.Config{Client: client, Model: spec, Tools: toolSet},
	}, nil
}

// autoApprovedTools is the coding agent's hard-approve set (design §4c): the six
// intrinsically safe, side-effect-free tools that run without prompting.
// WriteFile, EditFile, Bash, Fetch, and WebSearch are deliberately ABSENT — they
// mutate the filesystem, run a shell, or reach the network, so they stay Ask (the
// user approves each call). The names match each tool's Info().Name exactly; the
// PermissionChecker matches on them.
var autoApprovedTools = []string{"ReadFile", "Glob", "Grep", "Todo", "AskUser", "Subagent"}

// buildToolSet assembles ALL eleven tools and the fail-secure PermissionChecker
// that gates them (design §4d). It is called for the parent session and AGAIN, by
// codingSpawner.Spawn, for every in-session sub-loop, so a sub-loop gets the same
// full tool set — including a Subagent tool wired with the SAME spawner, which
// lets a sub-loop spawn a grandchild (recursion; unbounded — the depth cap was
// dropped, design §8).
//
// Least privilege: each tool gets only the deps it needs — the read/search tools
// get the workspace root plus the checker as their ReadGuard; the write/exec
// tools get only the root; the web tools get an HTTP client with explicit
// timeouts and a TLS 1.2 floor and never touch the filesystem; AskUser/Todo are
// self-contained; Subagent gets only the narrow spawner.
//
// A FRESH PermissionChecker is built per call so the parent and each sub-loop have
// independent session-scope approval state (a sub-loop's grant never leaks into
// the parent's policy, and vice versa) — this is the per-loop approval isolation
// guarantee, so it must stay per-call.
func buildToolSet(root string, httpCl *http.Client, spawner tools.Spawner) loop.ToolSet {
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
		tools.NewSubagent(spawner),
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
// UserInput and returns the InputID — the Cause.CommandID the resulting
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

// SessionID returns the underlying session's id — the composition root reads it to print
// the session being resumed and to key the catalog/lease. It is read-only identity.
func (c *Coding) SessionID() uuid.UUID { return c.session.SessionID }

// ReplayBacklog returns the RESTORED session's historical Enduring events for the
// TUI's cold-restore repaint, in session order. A freshly-constructed session (New) is
// NOT a restore: it has no replayer wired (c.replayer is nil), so this returns nil and
// the TUI skips the repaint — the new-session behavior is unchanged. A RESTORED session
// opens the primary loop's Enduring view (session subject + that loop's event subject),
// drains the EventCursor to io.EOF into a materialized slice, and surfaces the journal's
// typed fail-secure errors (a missing/corrupt offload object) unchanged — the TUI shows a
// non-fatal restore-error notice; the live stream is unaffected. ctx bounds the read.
func (c *Coding) ReplayBacklog(ctx context.Context) ([]event.Event, error) {
	if c.replayer == nil {
		return nil, nil // not a restore (new session) — nothing to repaint
	}
	cursor, err := c.replayer.Open(ctx, journal.ReplayRequest{
		SessionID: c.restoredSessionID,
		LoopID:    c.restoredPrimaryLoopID,
		From:      journal.Beginning(),
		Follow:    false, // cold restore: io.EOF at the backlog end
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close() }()

	var out []event.Event
	for {
		ev, _, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err // typed fail-secure error (object missing/corrupt) — surfaced unchanged
		}
		out = append(out, ev)
	}
}

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
// out on ctx. Cancelling the root also tears down every in-session subagent loop
// (they run under the same session root). Safe to call more than once.
//
// For a PERSISTED agent it then runs the composition-root teardown ONCE (releasing the
// single-writer lease so a successor can re-acquire without waiting out the TTL, and
// stopping the GC ticker) — AFTER session.Shutdown so the journal has finished its last
// append before ownership is relinquished. The teardown runs even when Shutdown returns
// an error (a faulted session still must release its lease). A teardown error is joined
// onto the Shutdown error so neither is lost.
func (c *Coding) Close(ctx context.Context) error {
	err := c.session.Shutdown(ctx)
	c.cancel()
	if c.teardown != nil {
		c.teardownOnce.Do(func() {
			if terr := c.teardown(ctx); terr != nil {
				err = errors.Join(err, terr)
			}
		})
	}
	return err
}
