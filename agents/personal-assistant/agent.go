// Package personalassistant is a conversational personal-assistant agent built
// on the session engine. It wraps a session.AgentSession with a fixed persona
// and a named model, exposing a small text-in / event-out surface.
package personalassistant

import (
	"context"
	"os"
	"strings"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/session"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/auto"
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
	rootCtx, cancel := context.WithCancel(context.Background())
	sess, err := session.NewAgent(rootCtx, loop.Config{Client: client, Model: spec})
	if err != nil {
		cancel()
		return nil, err
	}
	return &Assistant{session: sess, cancel: cancel, acceptsImages: spec.AcceptsImages}, nil
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
// briefly observe *command.TurnBusyError until the cancelled turn unwinds.
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

// Interrupt cancels the running turn. Returns true if a turn was cancelled.
func (a *Assistant) Interrupt(ctx context.Context) (bool, error) {
	return a.session.Interrupt(ctx)
}

// AcceptsImages reports whether the underlying model accepts image blocks.
func (a *Assistant) AcceptsImages() bool { return a.acceptsImages }

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
