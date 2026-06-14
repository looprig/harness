# Personal Assistant Agent Design

**Date:** 2026-06-13

---

## Goal

Build the first concrete agent on top of the existing execution engine:
`agents/personal-assistant/agent.go`. It is a thin, persona-bearing wrapper over
`internal/session.AgentSession` — the engine's first real consumer and the
template every later agent follows.

Builds on the established, already-implemented foundation:

- `internal/content` — content vocabulary (`Block`, `Chunk`, `AgenticMessages`)
- `internal/llm` — provider-neutral inference (`LLM`, `ModelSpec`, `Request`, `StreamReader`)
- `internal/llm/auto` — provider factory `New(ModelSpec) (LLM, error)`
- `internal/agent/loop` — the actor execution engine (`Event` types, `Config`)
- `internal/session` — `AgentSession` with `Invoke`, `Stream`, `Interrupt`, `Shutdown`

Because tools and persistent memory are explicitly deferred platform-wide, a v1
personal assistant is a single-stream conversational agent: a **persona** (system
prompt) over a **named model** with the **secret** supplied at runtime.

---

## Scope

**In scope**

- `internal/llm` — additive: a named `Model` definition type, a `Spec`
  materializer, a `Provider.RequiresKey` helper, and a small named catalog.
- `agents/` — new application-layer root directory (composition root).
- `agents/personal-assistant/agent.go` — `package personalassistant`: the
  `Assistant` wrapper with `New`, `Send`, `Stream`, `Close`, typed errors, and a
  test seam.

**Out of scope (deferred)**

- CLI / REPL wiring (`cmd/urvi/main.go` stays `func main() {}`).
- Tools, multi-step tool loop, persistent memory, journal/WAL, checkpoint/resume.
- Configurable persona (v1 ships one hardcoded prompt).
- Event sinks wired into the assistant (logging/tracing) — the engine supports
  them; nothing is attached here yet.
- Multiple agents / a registry.

---

## Configuration model — three concerns, three owners

The driving decision: provider and model are **named, typed definitions in
code**, not environment strings. Only the secret comes from the environment.

| Concern | Owner | Source |
|---|---|---|
| Which model + how to reach it + sampling | `internal/llm` | named `Model` definitions (code) |
| The secret (API key) | `agents/personal-assistant` | `LLM_API_KEY` env var |
| The persona (system prompt) | `agents/personal-assistant` | `personaPrompt` constant |

This keeps secrets out of code (CLAUDE.md), keeps the model catalog reusable and
agent-agnostic, and confines the only environment read to a single secret.

---

## `internal/llm` — additive changes

No new imports, no new external dependencies. `internal/llm` already imports only
`internal/content`. The catalog is pure data plus two small helpers.

### `Model` — a named, secret-free model definition

```go
// Model is a named, secret-free model definition: which model, how to reach it,
// and default sampling. It deliberately omits APIKey (a secret) and System (a
// per-agent concern). Materialize a full ModelSpec with Spec.
type Model struct {
    Provider    Provider
    BaseURL     string   // the provider's primary base URL (see endpoint note below)
    Name        string   // the provider's model identifier
    Temperature *float64 // optional sampling default
    MaxTokens   *int     // optional sampling default
}

// Spec materializes a ModelSpec from this definition, injecting the secret API
// key and the caller's system prompt. Pointer-valued sampling fields are deep
// copied so a returned spec never aliases the definition's state: a caller that
// mutates *spec.Temperature cannot reach back into a shared Model.
func (m Model) Spec(apiKey, system string) ModelSpec {
    return ModelSpec{
        Provider:    m.Provider,
        BaseURL:     m.BaseURL,
        APIKey:      apiKey,
        Model:       m.Name,
        System:      system,
        Temperature: cloneFloat64Ptr(m.Temperature),
        MaxTokens:   cloneIntPtr(m.MaxTokens),
    }
}

// cloneFloat64Ptr returns a fresh pointer to a copy of *p, or nil when p is nil.
// Concrete (not generic) to honor the repo rule against `any` outside
// serialization/plugin boundaries.
func cloneFloat64Ptr(p *float64) *float64 {
    if p == nil {
        return nil
    }
    v := *p
    return &v
}

// cloneIntPtr returns a fresh pointer to a copy of *p, or nil when p is nil.
func cloneIntPtr(p *int) *int {
    if p == nil {
        return nil
    }
    v := *p
    return &v
}
```

### `Provider.RequiresKey` — fail-secure gate for missing secrets

Every known provider is classified explicitly, and an unknown provider returns a
typed `*ValidationError` rather than defaulting to "no key required". This fails
secure: a hosted provider added to `auto.New` but not classified here makes `New`
refuse to start instead of silently skipping startup secret validation. (A bare
`default: return false` would fail open — the bug this method exists to prevent.)

```go
// RequiresKey reports whether the provider needs an API key, and errors on an
// unknown provider so a newly added one must be classified here before it can be
// used. Hosted, attested providers (phala, chutes) require a key; a local LM
// Studio endpoint does not.
func (p Provider) RequiresKey() (bool, error) {
    switch p {
    case ProviderLMStudio:
        return false, nil
    case ProviderPhala, ProviderChutes:
        return true, nil
    default:
        return false, &ValidationError{Field: "Provider", Reason: "unknown provider; API-key policy undefined"}
    }
}
```

### Named catalog

Secret-free, reusable across agents. The assistant selects one entry by name.
Entries are **functions, not exported vars**, so each call returns a fresh value
and no caller can mutate shared catalog state (an exported `var Model` would be
globally reassignable and its pointer fields globally mutable).

```go
// ChutesKimiK2 returns the Moonshot Kimi K2 model definition served through
// Chutes' TEE-attested endpoint. Chutes resolves the model name to a chute UUID
// via /v1/models at request time, so Name is the value sent on every request.
// BaseURL is the e2e/evidence apiBase, which chutes.New does NOT default — it
// must be explicit.
func ChutesKimiK2() Model {
    return Model{
        Provider: ProviderChutes,
        BaseURL:  "https://api.chutes.ai",
        Name:     "moonshotai/Kimi-K2.6-TEE",
    }
}
```

Additional entries (LM Studio local, other chutes/phala models) are added here as
needed, each as its own function.

**Provider endpoint note.** `auto.New` passes `ModelSpec.BaseURL` as each
provider's first constructor argument. `lmstudio.New` and `phala.New` default an
empty value to their production endpoint, so a `Model` for those providers may
leave `BaseURL` empty. `chutes.New` is the exception: it defaults its `llmBase`
(`https://llm.chutes.ai`, used only for model→chute resolution) but **not** its
`apiBase` (`https://api.chutes.ai`, the e2e/invoke + evidence host that
`auto.New` feeds from `BaseURL`). A chutes `Model` must therefore set `BaseURL`
to `https://api.chutes.ai` explicitly; leaving it empty yields broken request
URLs at call time. (A future cleanup could make `chutes.New` default an empty
`apiBase` like the other providers; until then the catalog carries it.)

---

## `agents/personal-assistant/agent.go`

`package personalassistant`. A thin pass-through wrapper: it adds the persona and
the `string → []*content.Block` convenience and delegates to the session,
returning the engine's own `loop.Event` / `StreamReader` unchanged. No remapping,
no information loss.

### Package-level definitions

```go
// model is the named model this assistant runs on. Swapping models is a one-line
// change here.
var model = llm.ChutesKimiK2()

// personaPrompt is the assistant's entire identity in v1.
const personaPrompt = `You are a helpful, concise personal assistant. Answer ` +
    `directly and accurately. When a request is ambiguous, ask one focused ` +
    `clarifying question before proceeding. Prefer plain language over jargon, ` +
    `keep responses as short as the task allows, and say so plainly when you ` +
    `do not know something rather than guessing.`

// envAPIKey is the only value read from the environment.
const envAPIKey = "LLM_API_KEY"
```

### The `Assistant` type and surface

```go
type Assistant struct {
    session *session.AgentSession
    cancel  context.CancelFunc // cancels the session's root context; called by Close
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
    spec := model.Spec(apiKey, personaPrompt) // pass the original key through, untrimmed
    client, err := auto.New(spec)             // validates spec + dispatches on provider
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
    return &Assistant{session: sess, cancel: cancel}, nil
}

// Send delivers one user message and blocks until the turn reaches a terminal
// event, returning it unchanged as one of the value types loop.TurnDone,
// loop.TurnFailed, or loop.TurnInterrupted. The Go error return is nil for all
// three terminal outcomes: a provider failure surfaces as a loop.TurnFailed
// whose Err field carries the original cause from the provider or engine, not as
// a Go error. That cause may be a typed engine error (e.g. *loop.EmptyResponseError
// or *loop.TurnPanicError) or a provider error passed through as-is; some current
// providers still return fmt.Errorf values, so callers must not assume Err is
// errors.As-reachable to a specific concrete type. The Go error is non-nil only
// when no turn completed — transport failures such as the loop having exited or
// ctx being done — and the event is then nil. Cancel ctx to interrupt the
// in-flight turn; Send then returns loop.TurnInterrupted with a nil error.
func (a *Assistant) Send(ctx context.Context, text string) (loop.Event, error) {
    blocks, err := userBlocks(text)
    if err != nil {
        return nil, err
    }
    return a.session.Invoke(ctx, blocks)
}

// Stream delivers one user message and returns the session's event stream:
// TurnStarted, TokenDelta×N, then one terminal event, then EOF. Callers must
// read until EOF or call sr.Close(). Calling sr.Close() abandons the stream and
// interrupts the turn asynchronously: it cancels and returns immediately without
// waiting for the actor to reach idle, so an immediately following Send may
// briefly observe *loop.TurnBusyError until the cancelled turn unwinds. The
// session is therefore eventually reusable, not reusable on the next line.
func (a *Assistant) Stream(ctx context.Context, text string) (*llm.StreamReader[loop.Event], error) {
    blocks, err := userBlocks(text)
    if err != nil {
        return nil, err
    }
    return a.session.Stream(ctx, blocks)
}

// Close gracefully shuts the session down and releases the session's root
// context. It blocks until the actor exits (or ctx is done), then cancels the
// root as a backstop so the actor goroutine cannot leak even if the graceful
// Shutdown timed out on ctx. Calling Close after the session has exited is a
// no-op for Shutdown; the cancel is idempotent.
func (a *Assistant) Close(ctx context.Context) error {
    err := a.session.Shutdown(ctx)
    a.cancel()
    return err
}
```

### Input helper

```go
// userBlocks wraps user text into a single text content block. It rejects blank
// input before the session is touched.
func userBlocks(text string) ([]*content.Block, error) {
    if strings.TrimSpace(text) == "" {
        return nil, &EmptyInputError{}
    }
    return []*content.Block{{
        Type: content.TypeText,
        Text: &content.TextBlock{Text: text},
    }}, nil
}
```

### Interruption

No dedicated `Interrupt` method: `loop.Interrupt` is already a command type that
`session.Interrupt` wraps, and the chosen API exposes both stop paths the engine
provides — cancel the `ctx` passed to `Send`/`Stream`, or call `sr.Close()` on a
stream. A standalone wrapper method would be redundant surface.

### Typed errors

```go
// MissingEnvError is returned by New when a required environment variable is
// unset. In v1 the only required variable is LLM_API_KEY, and only when the
// selected model's provider requires a key.
type MissingEnvError struct{ Var string }

func (e *MissingEnvError) Error() string {
    return "personalassistant: required environment variable " + e.Var + " is not set"
}

// EmptyInputError is returned by Send and Stream when the user text is empty or
// whitespace only.
type EmptyInputError struct{}

func (e *EmptyInputError) Error() string {
    return "personalassistant: input text is empty"
}
```

Construction failures from dependencies pass through unwrapped because they are
already concrete typed errors. `New` may therefore return, besides
`*MissingEnvError`:

- `*llm.ValidationError` — from `Provider.RequiresKey` (unclassified provider) or
  from `auto.New` (invalid spec / unknown provider).
- `*session.SessionError` — from `session.NewAgent` (e.g. context done, id
  generation failed).
- `*loop.ConfigError` — `session.NewAgent` passes `loop.New`'s error through
  directly (missing client, invalid model), so this concrete type surfaces too.

`errors.As` reaches each at the call site without an extra wrapper.

---

## Tests

`agents/personal-assistant/agent_test.go`, table-driven, run with `-race`. Two
groups by parallelism:

- **Fake-client tests** drive `newWithClient` with a fake `llm.LLM` and can use
  `t.Parallel()`. They never read or mutate the package-level `model`.
- **Env tests** drive `New` and use `t.Setenv` to set/clear `LLM_API_KEY`;
  `t.Setenv` forbids `t.Parallel()`, so these run serially. This is a deliberate
  exception to the parallel-table convention, noted in a comment in the test.
  They read the package-level `model` but do not mutate it.
- **Global `model` is not overridden in tests.** Reassigning the package-level
  `var model` is a shared-state mutation that `go test -race` would flag against
  parallel fake-client tests. The provider-key branches are covered without it:
  `Provider.RequiresKey` is unit-tested directly on `Provider` values, and the
  `New` gate is exercised through the real chutes `model` (key set → success;
  key unset → `*MissingEnvError`). If a future test genuinely must swap `model`,
  it runs serially (no `t.Parallel()`) and restores the original via `t.Cleanup`.

| Case | Assert |
|---|---|
| `New` happy (key set) | with `LLM_API_KEY` set, returns non-nil `*Assistant`, no error; test closes it to avoid leaking the actor goroutine |
| `New` missing key, provider requires | model provider requires a key, `LLM_API_KEY` unset → `*MissingEnvError{Var: "LLM_API_KEY"}` |
| `New` whitespace-only key | provider requires a key, `LLM_API_KEY` set to `"   "` → `*MissingEnvError{Var: "LLM_API_KEY"}` (env test) |
| `newWithClient` happy | fake client → non-nil `*Assistant`, no error |
| `newWithClient` pre-cancelled ctx | ctx already cancelled → `*session.SessionError{Kind: SessionContextDone}`; no session started |
| ctx independence from session | construct via `newWithClient` with a cancellable ctx, cancel that ctx, then `Send` still completes a turn — proving the session root is not the caller ctx; then `Close` |
| `Send` happy | fake streams text → returns `loop.TurnDone` value, nil Go error |
| `Send` provider failure | fake returns a sentinel provider error → returns `loop.TurnFailed` value, nil Go error, and `errors.Is(ev.(loop.TurnFailed).Err, fakeErr)` (the fake's own error, which the test controls — not a claim about real providers) |
| `Send` blank input | `""` / whitespace → `*EmptyInputError`, session untouched |
| `Stream` ordered events | reader yields `TurnStarted` → `TokenDelta`×N → `TurnDone` → EOF |
| `Stream` blank input | `""` / whitespace → `*EmptyInputError` |
| `Stream` Close interrupts, eventually reusable | `sr.Close()` cancels the turn; a subsequent `Send` is retried (may briefly return `*loop.TurnBusyError`) and eventually succeeds — never asserts reuse on the immediate next call |
| `Close` then `Send` | after `Close`, `Send` returns `*session.SessionError{Kind: SessionLoopExited}`; `Close` is also safe to call twice |
| `Model.Spec` materializes | `Spec(key, system)` carries Provider/BaseURL/Name→Model/sampling and injects APIKey + System |
| `Model.Spec` clones pointers | a `Model` with non-nil `Temperature` **and** `MaxTokens`; mutating `*spec.Temperature` and `*spec.MaxTokens` on the returned spec changes neither a second spec derived from the same `Model` nor the `Model` itself (guards both `cloneFloat64Ptr` and `cloneIntPtr`) |
| `Provider.RequiresKey` known | `lmstudio` → `(false, nil)`; `phala`/`chutes` → `(true, nil)` |
| `Provider.RequiresKey` unknown | an unrecognized provider → `(false, *llm.ValidationError)` (fail secure) |

The fake `llm.LLM` is a table-driven double implementing `Invoke` and `Stream`
with scripted chunks and a context-aware `Stream` so interruption tests unblock
on cancellation.

---

## Import layering

```
internal/uuid                  (no internal imports)
internal/content               (no internal imports)
internal/llm                →  internal/content        (+ Model, Spec, RequiresKey, catalog)
internal/llm/auto           →  internal/llm, internal/llm/openaiapi/*
internal/agent/loop         →  internal/uuid, internal/content, internal/llm
internal/session            →  internal/uuid, internal/agent/loop, internal/llm, internal/content
agents/personal-assistant   →  internal/content, internal/llm, internal/llm/auto,
                               internal/agent/loop, internal/session
cmd/urvi                       (unchanged; stays empty until a CLI is wired)
```

The agent depends only on interfaces and the composition factory (`auto.New`),
never on a concrete provider — dependency inversion holds.

---

## Explicitly deferred

- **CLI / REPL** — a runnable assistant wired through `cmd/urvi` (read stdin,
  stream tokens to the terminal, Ctrl-C → ctx cancel, EOF → `Close`).
- **Configurable persona** — env- or file-driven system prompt; multiple
  personas.
- **Tools and memory** — deferred platform-wide; a v1 turn is one
  `client.Stream` call.
- **Observability sinks** — attach a logging/tracing/journal `EventSink` to the
  assistant's `loop.Config.Sinks`.
- **Wider model catalog** — LM Studio local and additional hosted entries.
