# Personal Assistant Agent Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build the personal-assistant agent — a thin persona wrapper over the session engine — plus the named-model catalog it selects from in `internal/llm`.

**Architecture:** Additive changes to `internal/llm` (a secret-free `Model` definition, a `Spec` materializer, a fail-secure `Provider.RequiresKey`, and a named catalog) plus a new `agents/personal-assistant` package whose `Assistant` wraps `session.AgentSession`. `New` reads only `LLM_API_KEY`; the session runs under an assistant-owned root context (so its lifetime is `Close`-controlled, not tied to the caller's ctx); `Send`/`Stream` pass the engine's `loop.Event` / `*llm.StreamReader[loop.Event]` through unchanged.

**Tech Stack:** Go (stdlib only — no new external deps), table-driven tests with `-race`, module `github.com/inventivepotter/urvi`.

**Design reference:** @docs/plans/2026-06-13-personal-assistant-design.md — read this first for full rationale.

---

## Conventions (apply to every task)

- **TDD loop per task:** write the failing test → run it, see it fail/not-compile → write the minimal implementation → run it, see it pass → commit.
- **Tests are table-driven** where there is more than one case (CLAUDE.md mandate). Each subtest calls `t.Parallel()` **except** tests that call `t.Setenv` (Go forbids `t.Parallel` there).
- **Run with race detector:** a test that passes without `-race` but not with it is not passing.
- **Per-test run:** `go test -race ./<pkg>/ -run TestName -v`.
- **Commit format** (repo style), and every commit message ends with the trailer:
  ```
  Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
  ```
- **Do NOT wire `cmd/urvi`** — the CLI is explicitly out of scope (deferred). `cmd/urvi/main.go` stays `func main() {}`.
- **No new external dependencies.** If you think you need one, stop and ask.
- **Package name nuance:** the directory is `agents/personal-assistant` (hyphen) but the Go package clause is `package personalassistant` (a valid identifier). This compiles; a future importer aliases it. The new agent test files are white-box (`package personalassistant`) because they exercise unexported `newWithClient`, `userBlocks`, and `model`.
- **New `internal/llm` test files are black-box (`package llm_test`)** importing `internal/llm`, to avoid colliding with existing `package llm` test helpers.

---

## Task 1: `internal/llm` — `Model` definition, `Spec` materializer, clone helpers

**Files:**
- Create: `internal/llm/model.go`
- Create: `internal/llm/model_test.go`

**Step 1: Write the failing test**

`internal/llm/model_test.go`:
```go
package llm_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/llm"
)

func mkF64(v float64) *float64 { return &v }
func mkInt(v int) *int         { return &v }

func eqF64Ptr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
func eqIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func TestModelSpec(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		model  llm.Model
		apiKey string
		system string
		want   llm.ModelSpec
	}{
		{
			name:   "full fields",
			model:  llm.Model{Provider: llm.ProviderChutes, BaseURL: "https://api.chutes.ai", Name: "m", Temperature: mkF64(0.5), MaxTokens: mkInt(128)},
			apiKey: "secret",
			system: "sys",
			want:   llm.ModelSpec{Provider: llm.ProviderChutes, BaseURL: "https://api.chutes.ai", APIKey: "secret", Model: "m", System: "sys", Temperature: mkF64(0.5), MaxTokens: mkInt(128)},
		},
		{
			name:   "nil sampling pointers stay nil",
			model:  llm.Model{Provider: llm.ProviderLMStudio, Name: "local"},
			apiKey: "",
			system: "s",
			want:   llm.ModelSpec{Provider: llm.ProviderLMStudio, Model: "local", System: "s"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.model.Spec(tt.apiKey, tt.system)
			if got.Provider != tt.want.Provider || got.BaseURL != tt.want.BaseURL ||
				got.APIKey != tt.want.APIKey || got.Model != tt.want.Model || got.System != tt.want.System {
				t.Errorf("Spec() scalar mismatch:\n got  %+v\n want %+v", got, tt.want)
			}
			if !eqF64Ptr(got.Temperature, tt.want.Temperature) {
				t.Errorf("Temperature pointer value mismatch")
			}
			if !eqIntPtr(got.MaxTokens, tt.want.MaxTokens) {
				t.Errorf("MaxTokens pointer value mismatch")
			}
		})
	}
}

// TestModelSpecClonesPointers guards both cloneFloat64Ptr and cloneIntPtr:
// mutating the returned spec's pointees must not reach a second spec or the Model.
func TestModelSpecClonesPointers(t *testing.T) {
	t.Parallel()
	m := llm.Model{Provider: llm.ProviderChutes, Name: "m", Temperature: mkF64(0.5), MaxTokens: mkInt(128)}
	s1 := m.Spec("k", "sys")
	s2 := m.Spec("k", "sys")

	*s1.Temperature = 0.99
	*s1.MaxTokens = 999

	if *s2.Temperature != 0.5 {
		t.Errorf("s2.Temperature mutated via s1: got %v want 0.5", *s2.Temperature)
	}
	if *s2.MaxTokens != 128 {
		t.Errorf("s2.MaxTokens mutated via s1: got %v want 128", *s2.MaxTokens)
	}
	if *m.Temperature != 0.5 {
		t.Errorf("model.Temperature mutated via s1: got %v want 0.5", *m.Temperature)
	}
	if *m.MaxTokens != 128 {
		t.Errorf("model.MaxTokens mutated via s1: got %v want 128", *m.MaxTokens)
	}
}
```

**Step 2: Run the test, verify it fails to compile**

Run: `go test -race ./internal/llm/ -run TestModelSpec -v`
Expected: build failure — `undefined: llm.Model` (and `Spec`).

**Step 3: Write the minimal implementation**

`internal/llm/model.go`:
```go
package llm

// Model is a named, secret-free model definition: which model, how to reach it,
// and default sampling. It deliberately omits APIKey (a secret) and System (a
// per-agent concern). Materialize a full ModelSpec with Spec.
type Model struct {
	Provider    Provider
	BaseURL     string
	Name        string
	Temperature *float64
	MaxTokens   *int
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

**Step 4: Run the test, verify it passes**

Run: `go test -race ./internal/llm/ -run TestModelSpec -v`
Expected: `PASS` (both `TestModelSpec` and `TestModelSpecClonesPointers`).

**Step 5: Commit**

```bash
git add internal/llm/model.go internal/llm/model_test.go
git commit -m "$(cat <<'EOF'
feat(llm): Model definition with secret-free Spec materializer

Model bundles provider/baseURL/name/sampling; Spec injects the API key and
system prompt and deep-copies pointer sampling fields so a returned ModelSpec
never aliases the definition.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: `internal/llm` — `Provider.RequiresKey` (fail-secure)

**Files:**
- Create: `internal/llm/provider.go`
- Create: `internal/llm/provider_test.go`

**Step 1: Write the failing test**

`internal/llm/provider_test.go`:
```go
package llm_test

import (
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/llm"
)

func TestProviderRequiresKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		provider llm.Provider
		want     bool
		wantErr  bool
	}{
		{name: "lmstudio no key", provider: llm.ProviderLMStudio, want: false, wantErr: false},
		{name: "phala requires key", provider: llm.ProviderPhala, want: true, wantErr: false},
		{name: "chutes requires key", provider: llm.ProviderChutes, want: true, wantErr: false},
		{name: "unknown errors", provider: llm.Provider("bogus"), want: false, wantErr: true},
		{name: "empty errors", provider: llm.Provider(""), want: false, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.provider.RequiresKey()
			if (err != nil) != tt.wantErr {
				t.Fatalf("RequiresKey() err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("RequiresKey() = %v, want %v", got, tt.want)
			}
			if tt.wantErr {
				var ve *llm.ValidationError
				if !errors.As(err, &ve) {
					t.Errorf("error is %T, want *llm.ValidationError", err)
				}
			}
		})
	}
}
```

**Step 2: Run the test, verify it fails**

Run: `go test -race ./internal/llm/ -run TestProviderRequiresKey -v`
Expected: build failure — `provider.RequiresKey undefined`.

**Step 3: Write the minimal implementation**

`internal/llm/provider.go`:
```go
package llm

// RequiresKey reports whether the provider needs an API key, and errors on an
// unknown provider so a newly added one must be classified here before it can be
// used. Hosted, attested providers (phala, chutes) require a key; a local LM
// Studio endpoint does not. A bare default-false would fail open — the bug this
// method exists to prevent.
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

**Step 4: Run the test, verify it passes**

Run: `go test -race ./internal/llm/ -run TestProviderRequiresKey -v`
Expected: `PASS`.

**Step 5: Commit**

```bash
git add internal/llm/provider.go internal/llm/provider_test.go
git commit -m "$(cat <<'EOF'
feat(llm): fail-secure Provider.RequiresKey

Enumerates every known provider; an unknown one returns *ValidationError so a
newly added provider must be classified before it can skip secret validation.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: `internal/llm` — named catalog (`ChutesKimiK2`)

**Files:**
- Create: `internal/llm/catalog.go`
- Create: `internal/llm/catalog_test.go`

**Step 1: Write the failing test**

`internal/llm/catalog_test.go`:
```go
package llm_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/llm"
)

func TestChutesKimiK2(t *testing.T) {
	t.Parallel()
	m := llm.ChutesKimiK2()

	if m.Provider != llm.ProviderChutes {
		t.Errorf("Provider = %q, want %q", m.Provider, llm.ProviderChutes)
	}
	if m.BaseURL != "https://api.chutes.ai" {
		t.Errorf("BaseURL = %q, want https://api.chutes.ai (chutes apiBase is not defaulted)", m.BaseURL)
	}
	if m.Name != "moonshotai/Kimi-K2.6-TEE" {
		t.Errorf("Name = %q, want moonshotai/Kimi-K2.6-TEE", m.Name)
	}

	needsKey, err := m.Provider.RequiresKey()
	if err != nil || !needsKey {
		t.Errorf("RequiresKey() = (%v, %v), want (true, nil)", needsKey, err)
	}
}
```

**Step 2: Run the test, verify it fails**

Run: `go test -race ./internal/llm/ -run TestChutesKimiK2 -v`
Expected: build failure — `undefined: llm.ChutesKimiK2`.

**Step 3: Write the minimal implementation**

`internal/llm/catalog.go`:
```go
package llm

// ChutesKimiK2 returns the Moonshot Kimi K2 model definition served through
// Chutes' TEE-attested endpoint. Chutes resolves the model name to a chute UUID
// via /v1/models at request time, so Name is the value sent on every request.
// BaseURL is the e2e/evidence apiBase, which chutes.New does NOT default — it
// must be explicit. Returned by value (not an exported var) so callers cannot
// mutate shared catalog state.
func ChutesKimiK2() Model {
	return Model{
		Provider: ProviderChutes,
		BaseURL:  "https://api.chutes.ai",
		Name:     "moonshotai/Kimi-K2.6-TEE",
	}
}
```

**Step 4: Run the test, verify it passes**

Run: `go test -race ./internal/llm/ -run TestChutesKimiK2 -v`
Expected: `PASS`.

**Step 5: Run the whole `internal/llm` package to confirm no regressions**

Run: `go test -race ./internal/llm/`
Expected: `ok  github.com/inventivepotter/urvi/internal/llm`.

**Step 6: Commit**

```bash
git add internal/llm/catalog.go internal/llm/catalog_test.go
git commit -m "$(cat <<'EOF'
feat(llm): named model catalog with ChutesKimiK2

Secret-free model definition for moonshotai/Kimi-K2.6-TEE via Chutes; explicit
apiBase because chutes.New does not default it.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: `agents/personal-assistant` — typed errors

**Files:**
- Create: `agents/personal-assistant/errors.go`
- Create: `agents/personal-assistant/errors_test.go`

**Step 1: Write the failing test**

`agents/personal-assistant/errors_test.go`:
```go
package personalassistant

import (
	"errors"
	"strings"
	"testing"
)

func TestMissingEnvError(t *testing.T) {
	t.Parallel()
	err := error(&MissingEnvError{Var: "LLM_API_KEY"})
	if !strings.Contains(err.Error(), "LLM_API_KEY") {
		t.Errorf("Error() = %q, want it to contain LLM_API_KEY", err.Error())
	}
	var me *MissingEnvError
	if !errors.As(err, &me) {
		t.Fatalf("errors.As(*MissingEnvError) failed")
	}
	if me.Var != "LLM_API_KEY" {
		t.Errorf("Var = %q, want LLM_API_KEY", me.Var)
	}
}

func TestEmptyInputError(t *testing.T) {
	t.Parallel()
	err := error(&EmptyInputError{})
	if err.Error() == "" {
		t.Errorf("Error() is empty")
	}
	var ee *EmptyInputError
	if !errors.As(err, &ee) {
		t.Fatalf("errors.As(*EmptyInputError) failed")
	}
}
```

**Step 2: Run the test, verify it fails**

Run: `go test -race ./agents/personal-assistant/ -run 'TestMissingEnvError|TestEmptyInputError' -v`
Expected: build failure — `undefined: MissingEnvError` / `EmptyInputError`.

**Step 3: Write the minimal implementation**

`agents/personal-assistant/errors.go`:
```go
package personalassistant

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

**Step 4: Run the test, verify it passes**

Run: `go test -race ./agents/personal-assistant/ -run 'TestMissingEnvError|TestEmptyInputError' -v`
Expected: `PASS`.

**Step 5: Commit**

```bash
git add agents/personal-assistant/errors.go agents/personal-assistant/errors_test.go
git commit -m "$(cat <<'EOF'
feat(agents): personal-assistant typed errors

MissingEnvError and EmptyInputError for the assistant boundary.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: `agents/personal-assistant` — `Assistant` core (wrapper + delegation), via fake client

This task builds everything except `New` (env gate), which lands in Task 6 so it
can stay offline-testable. The `Assistant`, `newWithClient`, `Send`, `Stream`,
`Close`, and `userBlocks` are exercised entirely through `newWithClient` + a fake
`llm.LLM` — no env, no network, no global `model`.

**Files:**
- Create: `agents/personal-assistant/agent.go`
- Create: `agents/personal-assistant/fake_test.go`
- Create: `agents/personal-assistant/agent_test.go`

**Step 1: Write the fake `llm.LLM` (test infrastructure)**

`agents/personal-assistant/fake_test.go`:
```go
package personalassistant

import (
	"context"
	"errors"
	"io"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
)

// errFakeProvider is a sentinel the provider-failure test asserts on via errors.Is.
var errFakeProvider = errors.New("fake provider failure")

// fakeLLM is a controllable llm.LLM for tests. The loop only ever calls Stream,
// so Invoke is a stub.
type fakeLLM struct {
	chunks    []content.Chunk
	streamErr error         // returned from Stream() before any chunk
	hold      chan struct{} // if non-nil, Next blocks on hold or ctx after chunks
}

func textChunk(s string) content.Chunk {
	return content.Chunk{Type: content.ChunkTypeText, Text: &content.TextChunk{Text: s}}
}

func (f *fakeLLM) Invoke(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return nil, errors.New("fakeLLM.Invoke not used")
}

func (f *fakeLLM) Stream(ctx context.Context, req llm.Request) (*llm.StreamReader[content.Chunk], error) {
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	i := 0
	next := func() (content.Chunk, error) {
		if i < len(f.chunks) {
			c := f.chunks[i]
			i++
			return c, nil
		}
		if f.hold != nil {
			select {
			case <-f.hold:
				return content.Chunk{}, io.EOF
			case <-ctx.Done():
				return content.Chunk{}, ctx.Err()
			}
		}
		return content.Chunk{}, io.EOF
	}
	return llm.NewStreamReader(next, nil), nil
}

// testSpec is a minimal valid ModelSpec for fake-client tests. The fake ignores
// it; loop.New only requires it to pass ModelSpec.Validate (zero ThinkingBudget).
func testSpec() llm.ModelSpec {
	return llm.ModelSpec{Provider: llm.ProviderLMStudio, Model: "fake-model"}
}
```

**Step 2: Write the failing tests**

`agents/personal-assistant/agent_test.go`:
```go
package personalassistant

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/session"
)

func textOf(m *content.AIMessage) string {
	var b strings.Builder
	for _, blk := range m.Blocks {
		if blk.Type == content.TypeText && blk.Text != nil {
			b.WriteString(blk.Text.Text)
		}
	}
	return b.String()
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestNewWithClientHappy(t *testing.T) {
	t.Parallel()
	a, err := newWithClient(context.Background(), &fakeLLM{}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient() error = %v", err)
	}
	if a == nil {
		t.Fatal("newWithClient() returned nil assistant")
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
}

func TestNewWithClientPreCancelledCtx(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a, err := newWithClient(ctx, &fakeLLM{}, testSpec())
	if a != nil {
		t.Errorf("expected nil assistant, got %v", a)
	}
	var se *session.SessionError
	if !errors.As(err, &se) || se.Kind != session.SessionContextDone {
		t.Fatalf("err = %v, want *session.SessionError{SessionContextDone}", err)
	}
}

func TestSendHappy(t *testing.T) {
	t.Parallel()
	a, err := newWithClient(context.Background(), &fakeLLM{chunks: []content.Chunk{textChunk("hello")}}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	ev, err := a.Send(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	done, ok := ev.(loop.TurnDone)
	if !ok {
		t.Fatalf("event = %T, want loop.TurnDone", ev)
	}
	if got := textOf(done.Message); got != "hello" {
		t.Errorf("message text = %q, want hello", got)
	}
}

func TestSendProviderFailure(t *testing.T) {
	t.Parallel()
	a, err := newWithClient(context.Background(), &fakeLLM{streamErr: errFakeProvider}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	ev, err := a.Send(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Send() Go error = %v, want nil", err)
	}
	failed, ok := ev.(loop.TurnFailed)
	if !ok {
		t.Fatalf("event = %T, want loop.TurnFailed", ev)
	}
	if !errors.Is(failed.Err, errFakeProvider) {
		t.Errorf("TurnFailed.Err = %v, want errors.Is errFakeProvider", failed.Err)
	}
}

func TestSendBlankInput(t *testing.T) {
	t.Parallel()
	a, err := newWithClient(context.Background(), &fakeLLM{}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	for _, in := range []string{"", "   ", "\t\n"} {
		ev, err := a.Send(context.Background(), in)
		if ev != nil {
			t.Errorf("Send(%q) event = %v, want nil", in, ev)
		}
		var ee *EmptyInputError
		if !errors.As(err, &ee) {
			t.Errorf("Send(%q) err = %v, want *EmptyInputError", in, err)
		}
	}
}

func TestStreamOrderedEvents(t *testing.T) {
	t.Parallel()
	a, err := newWithClient(context.Background(), &fakeLLM{chunks: []content.Chunk{textChunk("a"), textChunk("b")}}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	sr, err := a.Stream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer func() { _ = sr.Close() }()

	var kinds []string
	for {
		ev, err := sr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next() error = %v", err)
		}
		switch ev.(type) {
		case loop.TurnStarted:
			kinds = append(kinds, "started")
		case loop.TokenDelta:
			kinds = append(kinds, "delta")
		case loop.TurnDone:
			kinds = append(kinds, "done")
		default:
			t.Fatalf("unexpected event %T", ev)
		}
	}
	want := []string{"started", "delta", "delta", "done"}
	if !equalStrings(kinds, want) {
		t.Errorf("events = %v, want %v", kinds, want)
	}
}

func TestStreamBlankInput(t *testing.T) {
	t.Parallel()
	a, err := newWithClient(context.Background(), &fakeLLM{}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	sr, err := a.Stream(context.Background(), "  ")
	if sr != nil {
		t.Errorf("Stream() reader = %v, want nil", sr)
	}
	var ee *EmptyInputError
	if !errors.As(err, &ee) {
		t.Errorf("Stream() err = %v, want *EmptyInputError", err)
	}
}

// TestStreamCloseEventuallyReusable proves the contract: sr.Close() interrupts
// asynchronously, so a subsequent Send may briefly see *loop.TurnBusyError and
// must be retried; the session is eventually reusable.
func TestStreamCloseEventuallyReusable(t *testing.T) {
	t.Parallel()
	hold := make(chan struct{})
	a, err := newWithClient(context.Background(), &fakeLLM{chunks: []content.Chunk{textChunk("partial")}, hold: hold}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	sr, err := a.Stream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	// read at least one event so the turn is genuinely running before we close
	if _, err := sr.Next(); err != nil {
		t.Fatalf("Next() error = %v", err)
	}
	if err := sr.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	// allow any future turn to complete via EOF
	close(hold)

	deadline := time.Now().Add(2 * time.Second)
	for {
		ev, err := a.Send(context.Background(), "again")
		if err == nil {
			if _, ok := ev.(loop.TurnDone); !ok {
				t.Fatalf("Send event = %T, want loop.TurnDone", ev)
			}
			return
		}
		var busy *loop.TurnBusyError
		if !errors.As(err, &busy) {
			t.Fatalf("Send err = %v, want nil or *loop.TurnBusyError", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("session not reusable within deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCloseThenSend(t *testing.T) {
	t.Parallel()
	a, err := newWithClient(context.Background(), &fakeLLM{chunks: []content.Chunk{textChunk("x")}}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	// Close is safe to call twice
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}

	ev, err := a.Send(context.Background(), "hi")
	if ev != nil {
		t.Errorf("Send event = %v, want nil", ev)
	}
	var se *session.SessionError
	if !errors.As(err, &se) || se.Kind != session.SessionLoopExited {
		t.Fatalf("Send err = %v, want *session.SessionError{SessionLoopExited}", err)
	}
}

// TestCtxIndependenceFromSession proves the session root is not the caller ctx:
// cancelling the construction ctx must not kill the session.
func TestCtxIndependenceFromSession(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	a, err := newWithClient(ctx, &fakeLLM{chunks: []content.Chunk{textChunk("ok")}}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	cancel() // cancel the construction ctx; the session must survive

	ev, err := a.Send(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Send() error = %v, want nil (session should outlive ctx)", err)
	}
	if _, ok := ev.(loop.TurnDone); !ok {
		t.Fatalf("event = %T, want loop.TurnDone", ev)
	}
}
```

**Step 3: Run the tests, verify they fail**

Run: `go test -race ./agents/personal-assistant/ -v`
Expected: build failure — `undefined: newWithClient`, `Assistant`, etc.

**Step 4: Write the minimal implementation**

`agents/personal-assistant/agent.go` (NOTE: no `New`, no `os`/`auto` imports yet — those land in Task 6):
```go
// Package personalassistant is a conversational personal-assistant agent built
// on the session engine. It wraps a session.AgentSession with a fixed persona
// and a named model, exposing a small text-in / event-out surface.
package personalassistant

import (
	"context"
	"strings"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/session"
)

// Assistant is a persona-bearing wrapper over a session.AgentSession. The
// caller owns it and must call Close to release the underlying actor goroutine.
type Assistant struct {
	session *session.AgentSession
	cancel  context.CancelFunc // cancels the session's root context; called by Close
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
// whose Err carries the original provider/engine cause, not as a Go error. The
// Go error is non-nil only when no turn completed (transport failures: the loop
// exited, or ctx done), and the event is then nil. Cancel ctx to interrupt the
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
// read until EOF or call sr.Close(). sr.Close() abandons the stream and
// interrupts the turn asynchronously, so an immediately following Send may
// briefly observe *loop.TurnBusyError until the cancelled turn unwinds.
func (a *Assistant) Stream(ctx context.Context, text string) (*llm.StreamReader[loop.Event], error) {
	blocks, err := userBlocks(text)
	if err != nil {
		return nil, err
	}
	return a.session.Stream(ctx, blocks)
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

**Step 5: Run the tests, verify they pass**

Run: `go test -race ./agents/personal-assistant/ -v`
Expected: `PASS` for all Task 4 + Task 5 tests.

**Step 6: Commit**

```bash
git add agents/personal-assistant/agent.go agents/personal-assistant/fake_test.go agents/personal-assistant/agent_test.go
git commit -m "$(cat <<'EOF'
feat(agents): personal-assistant Assistant wrapper

Thin pass-through over session.AgentSession: Send/Stream/Close plus userBlocks.
Session runs under an assistant-owned root context so lifetime is Close-bound,
not tied to the caller ctx. Tested via a fake llm.LLM through newWithClient.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: `agents/personal-assistant` — `New` (env gate) + package definitions

**Files:**
- Modify: `agents/personal-assistant/agent.go` (add imports `os`, `auto`; add `model`, `personaPrompt`, `envAPIKey`; add `New`)
- Create: `agents/personal-assistant/new_test.go`

**Step 1: Write the failing test**

`agents/personal-assistant/new_test.go`:
```go
package personalassistant

import (
	"context"
	"errors"
	"testing"
)

// These tests use t.Setenv, which forbids t.Parallel(); they run serially. They
// read the package-level `model` (chutes, key-required) but do not mutate it.

func TestNewMissingKey(t *testing.T) {
	t.Setenv("LLM_API_KEY", "")
	a, err := New(context.Background())
	if a != nil {
		_ = a.Close(context.Background())
		t.Fatalf("New() returned non-nil assistant, want nil")
	}
	var me *MissingEnvError
	if !errors.As(err, &me) || me.Var != "LLM_API_KEY" {
		t.Fatalf("err = %v, want *MissingEnvError{Var: LLM_API_KEY}", err)
	}
}

func TestNewWhitespaceKey(t *testing.T) {
	t.Setenv("LLM_API_KEY", "   ")
	a, err := New(context.Background())
	if a != nil {
		_ = a.Close(context.Background())
		t.Fatalf("New() returned non-nil assistant, want nil")
	}
	var me *MissingEnvError
	if !errors.As(err, &me) {
		t.Fatalf("err = %v, want *MissingEnvError", err)
	}
}

func TestNewHappy(t *testing.T) {
	t.Setenv("LLM_API_KEY", "test-key-not-used-offline")
	a, err := New(context.Background())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if a == nil {
		t.Fatal("New() returned nil assistant")
	}
	// Construction performs no network I/O; Close stops the real session actor.
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}
```

**Step 2: Run the test, verify it fails**

Run: `go test -race ./agents/personal-assistant/ -run 'TestNew(MissingKey|WhitespaceKey|Happy)' -v`
Expected: build failure — `undefined: New`.

**Step 3: Edit `agent.go` — add imports, package definitions, and `New`**

Change the import block from:
```go
import (
	"context"
	"strings"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/session"
)
```
to:
```go
import (
	"context"
	"os"
	"strings"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/auto"
	"github.com/inventivepotter/urvi/internal/session"
)
```

Add these package-level definitions immediately after the import block (before the `Assistant` type):
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

Add `New` immediately above `newWithClient`:
```go
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
```

**Step 4: Run the test, verify it passes**

Run: `go test -race ./agents/personal-assistant/ -run 'TestNew(MissingKey|WhitespaceKey|Happy)' -v`
Expected: `PASS`.

**Step 5: Run the whole package**

Run: `go test -race ./agents/personal-assistant/`
Expected: `ok  github.com/inventivepotter/urvi/agents/personal-assistant`.

**Step 6: Commit**

```bash
git add agents/personal-assistant/agent.go agents/personal-assistant/new_test.go
git commit -m "$(cat <<'EOF'
feat(agents): personal-assistant New env gate

New selects the named chutes model, reads only LLM_API_KEY (whitespace-only
treated as missing), fails secure on an unclassified provider, and builds the
client via auto.New.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: Full verification (build, race suite, security)

No new files. This task proves the whole tree is green and secure.

**Step 1: Build everything (trimpath, CGO off)**

Run: `CGO_ENABLED=0 go build -trimpath ./...`
Expected: no output, exit 0. (This compiles the new `agents/personal-assistant` package even though `cmd/urvi` does not import it yet.)

**Step 2: Run the full race suite**

Run: `make test`  (= `go test -race ./...`)
Expected: all packages `ok`. If a pre-existing package fails for environmental reasons unrelated to this change, note it explicitly — do not "fix" unrelated code; the new packages (`internal/llm`, `agents/personal-assistant`) must be `ok`.

**Step 3: Security + static analysis**

Run: `make secure`  (= `go vet ./...`, `staticcheck ./...`, `gosec ./...`, `go mod verify`, `govulncheck ./...`)
Expected: clean. Likely nits to watch for and fix if raised:
- `gosec` G104 on an unchecked `Close`/`sr.Close()` — use `_ = x.Close()` (the plan's test code already does).
- `staticcheck` on the package-name vs directory mismatch is **not** expected; if any linter flags it, it is acceptable and intended (the directory name is fixed as `personal-assistant`).

**Step 4: Commit only if Step 3 required code changes**

```bash
# only if make secure forced edits:
git add -A
git commit -m "$(cat <<'EOF'
chore(agents): satisfy make secure for personal-assistant

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task dependency graph

```
Task 1 (Model/Spec) ─┐
Task 2 (RequiresKey) ─┼─> Task 6 (New) ─> Task 7 (verify)
Task 3 (catalog) ─────┘        ^
Task 4 (errors) ─> Task 5 (Assistant core) ─┘
```

Tasks 1–4 are mutually independent. Task 5 depends on Task 4. Task 6 depends on Tasks 1, 2, 3, 4, 5. Task 7 is last.

## Out of scope (do not implement)

- `cmd/urvi` CLI/REPL wiring — stays `func main() {}`.
- Tools, memory, journal, additional catalog entries, event sinks.
- Modifying `internal/session`, `internal/agent/loop`, or any provider package.
