// Package personalassistant is a conversational personal-assistant agent built
// on the session engine. It wraps a session.AgentSession with a fixed persona
// and a named model, exposing a small text-in / event-out surface.
package personalassistant

import (
	"context"
	"crypto/tls"
	"net/http"
	"os"
	"strings"
	"time"

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

// model is the named model this assistant runs on. Swapping models is a one-line
// change here. Read-only after init: do not reassign or mutate it — the parallel
// fake-client tests read it concurrently, and Spec relies on it being stable.
var model = llm.ChutesKimiK2()

// personaPrompt is the assistant's entire identity in v1.
const personaPrompt = `You are a helpful, concise personal assistant. Answer ` +
	`directly and accurately. When a request is ambiguous, ask one focused ` +
	`clarifying question before proceeding. Prefer plain language over jargon, ` +
	`keep responses as short as the task allows, and say so plainly when you ` +
	`do not know something rather than guessing.`

// envAPIKey is the only value read from the environment. The value is the NAME
// of an env var, not a secret; the #nosec annotation documents that gosec's
// G101 "hardcoded credentials" heuristic (which matches on the identifier name)
// is a false positive here.
const envAPIKey = "LLM_API_KEY" // #nosec G101 -- env var name, not a credential

// Assistant is a persona-bearing wrapper over a session.AgentSession. The
// caller owns it and must call Close to release the underlying actor goroutine.
type Assistant struct {
	session       *session.AgentSession
	cancel        context.CancelFunc // cancels the session's root context; called by Close
	acceptsImages bool               // captured from spec at construction; reported by AcceptsImages
}

// New constructs an Assistant. The session runs under an assistant-owned root
// context, so its lifetime is controlled by Close, not by ctx: ctx only bounds
// construction (New fails fast if it is already cancelled) and does not stop the
// session once New has returned — a request-scoped or timeout ctx is therefore
// safe to pass. New reads LLM_API_KEY (the only env-sourced value), refuses an
// unclassified provider (fail secure), fails loud if the provider requires a key
// and none is set, then builds the provider client via auto.New and starts the
// session actor. The caller owns the Assistant and must call Close to release it.
func New(ctx context.Context) (*Assistant, error) {
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
	// above is a presence check, not a sanitizer: trimming a secret could corrupt
	// a key with semantically significant surrounding bytes and produce a baffling
	// 401. What the operator set is exactly what we send.
	spec := model.Spec(apiKey, personaPrompt)
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
func newWithClient(ctx context.Context, client llm.LLM, spec llm.ModelSpec) (*Assistant, error) {
	if err := ctx.Err(); err != nil {
		return nil, &session.SessionError{Kind: session.SessionContextDone, Cause: err}
	}
	// The workspace root is the process working directory: file tools are confined
	// to it and the PermissionChecker uses it for containment + path relativisation.
	root, err := os.Getwd()
	if err != nil {
		return nil, &WorkspaceRootError{Cause: err}
	}
	toolSet, err := buildToolSet(root)
	if err != nil {
		return nil, err
	}
	rootCtx, cancel := context.WithCancel(context.Background())
	sess, err := session.NewAgent(rootCtx, loop.Config{Client: client, Model: spec, Tools: toolSet})
	if err != nil {
		cancel()
		return nil, err
	}
	return &Assistant{session: sess, cancel: cancel, acceptsImages: spec.AcceptsImages}, nil
}

// autoApprovedTools is the personal assistant's hard-approve set: intrinsically
// safe, side-effect-free tools that run within the workspace without prompting.
// Fetch and WebSearch are deliberately ABSENT — they reach the network, so they
// stay Ask (the user approves each call). The names match each tool's
// Info().Name exactly; the PermissionChecker matches on them.
var autoApprovedTools = []string{"ReadFile", "Glob", "Grep", "Todo", "AskUser"}

// buildToolSet assembles the personal assistant's safe seven-tool subset and the
// fail-secure PermissionChecker that gates them (design §4d). Each tool is wired
// with only the dependencies it needs (least privilege): the read tools get the
// workspace root plus the checker as their ReadGuard; the web tools get an HTTP
// client with explicit timeouts and a TLS 1.2 floor; AskUser/Todo are self-
// contained. The subset is read/search/fetch/ask/todo only — no write, exec, or
// subagent tool is registered, so the assistant can never mutate the filesystem
// or run a shell.
func buildToolSet(root string) (loop.ToolSet, error) {
	policy := tools.PermissionPolicy{
		WorkspaceRoot: root,
		HardDeny:      tools.DefaultHardDeny(),
		HardApprove:   tools.HardApproveRules{Tools: autoApprovedTools},
	}
	pc := tools.NewPermissionChecker(policy)
	client := newHTTPClient()

	registry := []tool.InvokableTool{
		tools.NewReadFile(root, pc),
		tools.NewGlob(root, pc),
		tools.NewGrep(root, pc),
		tools.NewFetch(client),
		tools.NewWebSearch(tools.NewDuckDuckGoProvider(client)),
		tools.NewAskUser(),
		tools.NewTodo(),
	}
	// Middlewares nil and the runaway-guard caps zero on purpose: loop.New applies
	// its safe defaults for the caps; the manifest declares no middleware in v1.
	return loop.ToolSet{Permission: pc, Registry: registry}, nil
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

// Send delivers one user message and blocks until the turn reaches a terminal
// event, returning it unchanged as one of the value types event.TurnDone,
// event.TurnFailed, or event.TurnInterrupted. The Go error return is nil for all
// three terminal outcomes: a provider failure surfaces as a event.TurnFailed
// whose Err carries the original provider/engine cause, not as a Go error. The
// Go error is non-nil only when no turn completed (transport failures: the loop
// exited, or ctx done), and the event is then nil. Cancel ctx to interrupt the
// in-flight turn; Send then returns event.TurnInterrupted with a nil error.
func (a *Assistant) Send(ctx context.Context, text string) (event.Event, error) {
	blocks, err := userBlocks(text)
	if err != nil {
		return nil, err
	}
	return a.session.Invoke(ctx, blocks)
}

// Stream delivers one user message and returns the session's event stream:
// TurnStarted, TokenDelta×N, then one terminal event, then EOF. Callers must
// read until EOF or call sr.Close(). sr.Close() abandons the stream and
// interrupts the turn asynchronously, so an immediately following Send may
// briefly observe *session.TurnRejectedError until the cancelled turn unwinds.
func (a *Assistant) Stream(ctx context.Context, text string) (*llm.StreamReader[event.Event], error) {
	blocks, err := userBlocks(text)
	if err != nil {
		return nil, err
	}
	return a.session.Stream(ctx, blocks)
}

// StreamBlocks delivers a multimodal user message and returns the session's
// event stream: TurnStarted, TokenDelta×N, one terminal event, then EOF.
// Callers must read to EOF or call sr.Close().
func (a *Assistant) StreamBlocks(ctx context.Context, blocks []content.Block) (*llm.StreamReader[event.Event], error) {
	return a.session.Stream(ctx, blocks)
}

// Subscribe attaches a whole-session event consumer to the session fan-in with
// filter and returns its event.Subscription (the session hub's *EventSubscription,
// which satisfies the interface). It is the seam a TUI/CLI uses to observe events
// across the whole session — every loop, spanning turns — distinct from the
// per-turn StreamBlocks reader that closes when one turn ends. The caller Closes
// the returned subscription when done. PrimaryLoopID is exposed so the caller can
// build the single-loop default filter (primary-only live tokens, all-loop
// finalized output).
func (a *Assistant) Subscribe(filter event.EventFilter) (event.Subscription, error) {
	return a.session.SubscribeEvents(filter)
}

// PrimaryLoopID returns the session's primary loop id, so a subscriber can build
// its EventFilter (primary-only Ephemeral + all-loop Enduring).
func (a *Assistant) PrimaryLoopID() uuid.UUID { return a.session.PrimaryLoopID() }

// Interrupt cancels the running turn. Returns true if a turn was cancelled.
func (a *Assistant) Interrupt(ctx context.Context) (bool, error) {
	return a.session.Interrupt(ctx)
}

// AcceptsImages reports whether the underlying model accepts image blocks.
func (a *Assistant) AcceptsImages() bool { return a.acceptsImages }

// Approve resolves a pending tool-call permission gate, granting it at scope. It
// delegates verbatim to the session; the wrapper holds no gate state of its own.
func (a *Assistant) Approve(ctx context.Context, callID uuid.UUID, scope tool.ApprovalScope) error {
	return a.session.Approve(ctx, callID, scope)
}

// Deny resolves a pending tool-call permission gate by failing it closed
// (fail-secure); nothing is persisted. It delegates to the session.
func (a *Assistant) Deny(ctx context.Context, callID uuid.UUID) error {
	return a.session.Deny(ctx, callID)
}

// ProvideAnswer supplies the user's reply to a pending AskUser request. It is the
// TUI-facing name for the session's ProvideUserInput, to which it delegates.
func (a *Assistant) ProvideAnswer(ctx context.Context, callID uuid.UUID, answer string) error {
	return a.session.ProvideUserInput(ctx, callID, answer)
}

// Close gracefully shuts the session down and releases the session's root
// context. It blocks until the actor exits (or ctx is done), then cancels the
// root as a backstop so the actor goroutine cannot leak even if Shutdown timed
// out on ctx. Safe to call more than once.
func (a *Assistant) Close(ctx context.Context) error {
	err := a.session.Shutdown(ctx)
	a.cancel()
	return err
}

// userBlocks wraps user text into a single text content block. It rejects blank
// input before the session is touched.
func userBlocks(text string) ([]content.Block, error) {
	if strings.TrimSpace(text) == "" {
		return nil, &EmptyInputError{}
	}
	return []content.Block{&content.TextBlock{Text: text}}, nil
}
