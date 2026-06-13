# Agent Loop & Session Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Each task is one fresh subagent; review between tasks.

**Goal:** Build `internal/uuid`, the additive `internal/llm` provider fields, `internal/llm/auto`, `internal/agent/loop`, and `internal/session` — the execution engine and runtime identity layer — strictly test-first.

**Architecture:** A single actor goroutine (`loop.listen`) owns all session state; every command is request/reply over channels; each turn streams over a request-owned `Events` channel; observability fans out to best-effort `EventSink`s. `AgentSession` wraps the loop with a UUID identity. See the design spec for the full rationale and the corrected code.

**Tech Stack:** Go 1.26.1, stdlib only (`crypto/rand`, `context`, `log/slog`, `testing`, `testing/iotest`). Module `github.com/inventivepotter/urvi`.

---

## Source of truth & how to read this plan

The corrected design spec is **`docs/plans/2026-06-13-agent-loop-session-design.md`**. It already contains the complete, reviewed implementation code (including the eight review fixes: ctx escape in `deliverAndClose`, typed `TurnFailed.Err`, rollback-on-failure, `DrainTimeout`, lossless `emit`, etc.).

To avoid divergence (DRY — one source of truth), this plan **does not re-paste large implementation bodies**. For each task it cites the exact spec section to copy the implementation from, and gives you the **full test code** (the spec only has test *tables*), the **exact commands**, and the **commit**. Small bodies are inlined where it's faster than a cross-reference.

When the plan and the spec disagree, the plan wins — the plan encodes a few testability refinements (called out inline) that supersede the spec's sketch.

## Ground rules (apply to every task)

- **TDD, no exceptions** (REQUIRED SUB-SKILL: superpowers:test-driven-development). Write the failing test, watch it fail for the *right reason*, write the minimal code, watch it pass, commit. Never write implementation before a red test.
- **Table-driven tests** per CLAUDE.md: each `TestX` uses `[]struct{name string; ...}`, every subtest calls `t.Parallel()`, and the table covers happy path, boundary, error, and domain edge cases.
- **Race detector always:** every run uses `-race`.
- **Typed errors only:** assert with `errors.As` against the concrete type, never string-match `.Error()` output in production paths.
- **Strict typing:** no `any`/`interface{}` except at the recover() site, which is immediately narrowed to a string.
- **Commit message footer** (every commit):
  ```
  Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
  ```
- **Conventional commit prefixes:** `feat:`, `test:`, `refactor:`.

## Preconditions (do once, before Task 1)

**Step 0.1 — Branch.** We are on `main`. Create a feature branch (or worktree) so no work lands on `main` directly:
```bash
git switch -c feat/agent-loop
```
Expected: `Switched to a new branch 'feat/agent-loop'`.

**Step 0.2 — Baseline green.** Confirm the repo is clean before adding anything:
```bash
go test -race ./...
```
Expected: all existing packages PASS. If not, stop and fix the baseline first.

---

## Task 1: `internal/uuid` — typed UUID v4

**Files:**
- Create: `internal/uuid/uuid.go`
- Create: `internal/uuid/uuid_test.go`

**Refinement over spec:** the spec's `New` hardcodes `rand.Reader`, which makes the "random source failure" case untestable. Split out an unexported `generate(io.Reader)` seam; `New()` calls `generate(rand.Reader)`. Same public API, now testable.

**Step 1.1 — Write the failing test.** Create `internal/uuid/uuid_test.go`:

```go
package uuid

import (
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"
)

var rfc4122v4 = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNew(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		reader  io.Reader
		wantErr bool
	}{
		{name: "happy path", reader: strings.NewReader(strings.Repeat("\x01", 16))},
		{name: "short read returns error", reader: strings.NewReader("too short"), wantErr: true},
		{name: "reader failure returns error", reader: iotest.ErrReader(errors.New("boom")), wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u, err := generate(tt.reader)
			if (err != nil) != tt.wantErr {
				t.Fatalf("generate() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var ge *GenerateError
				if !errors.As(err, &ge) {
					t.Fatalf("err = %T, want *GenerateError", err)
				}
				return
			}
			if u == (UUID{}) {
				t.Fatal("generate() returned zero UUID")
			}
			if got := u[6] & 0xf0; got != 0x40 {
				t.Errorf("version nibble = %#x, want 0x40", got)
			}
			if got := u[8] & 0xc0; got != 0x80 {
				t.Errorf("variant bits = %#x, want 0x80", got)
			}
		})
	}
}

func TestUUIDString(t *testing.T) {
	t.Parallel()
	u, err := New()
	if err != nil {
		t.Fatalf("New() err = %v", err)
	}
	if !rfc4122v4.MatchString(u.String()) {
		t.Errorf("String() = %q, not RFC 4122 v4", u.String())
	}
}

func TestNewUnique(t *testing.T) {
	t.Parallel()
	a, _ := New()
	b, _ := New()
	if a == b {
		t.Error("two New() calls returned equal UUIDs")
	}
}
```

**Step 1.2 — Run, expect failure.**
```bash
go test -race ./internal/uuid/ -run 'TestNew|TestUUIDString|TestNewUnique' -v
```
Expected: compile failure — `undefined: generate`, `UUID`, `New`, `GenerateError`.

**Step 1.3 — Implement.** Create `internal/uuid/uuid.go` from spec **`## internal/uuid`** (lines ~63–98), but replace the body of `New` with the seam:

```go
package uuid

import (
	"crypto/rand"
	"fmt"
	"io"
)

type UUID [16]byte

// GenerateError wraps failures from the randomness source.
type GenerateError struct{ Cause error }

func (e *GenerateError) Error() string {
	if e.Cause == nil {
		return "uuid: generate"
	}
	return "uuid: generate: " + e.Cause.Error()
}
func (e *GenerateError) Unwrap() error { return e.Cause }

// New returns a version-4 UUID sourced from crypto/rand.
func New() (UUID, error) { return generate(rand.Reader) }

// generate is the testable seam: it reads 16 bytes from r and stamps the v4
// version and variant bits.
func generate(r io.Reader) (UUID, error) {
	var u UUID
	if _, err := io.ReadFull(r, u[:]); err != nil {
		return UUID{}, &GenerateError{Cause: err}
	}
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // variant 10
	return u, nil
}

func (u UUID) String() string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
```

**Step 1.4 — Run, expect pass.**
```bash
go test -race ./internal/uuid/ -v
```
Expected: PASS.

**Step 1.5 — Commit.**
```bash
git add internal/uuid/
git commit -m "feat(uuid): typed UUID v4 with crypto/rand seam

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: `internal/llm` — additive provider fields

**Files:**
- Modify: `internal/llm/llm.go` (add `Provider` type + 3 fields on `ModelSpec`)
- Modify/Create: `internal/llm/llm_test.go` (one new test; keep existing tests passing)

This is additive — do not touch `Validate`'s logic. `auto.New` does provider dispatch; `Validate` stays provider-agnostic.

**Step 2.1 — Write the failing test.** Add to `internal/llm/llm_test.go`:

```go
func TestModelSpecProviderFields(t *testing.T) {
	t.Parallel()
	spec := ModelSpec{
		Provider: ProviderLMStudio,
		BaseURL:  "http://localhost:1234",
		APIKey:   "sk-test",
		Model:    "qwen",
	}
	if spec.Provider != ProviderLMStudio {
		t.Errorf("Provider = %q, want %q", spec.Provider, ProviderLMStudio)
	}
	if err := spec.Validate(); err != nil {
		t.Errorf("Validate() on a benign spec = %v, want nil", err)
	}
}
```

**Step 2.2 — Run, expect failure.**
```bash
go test -race ./internal/llm/ -run TestModelSpecProviderFields -v
```
Expected: compile failure — `undefined: ProviderLMStudio`, unknown field `Provider`.

**Step 2.3 — Implement.** In `internal/llm/llm.go`, add the `Provider` type + constants from spec **`### ModelSpec additions`** (lines ~108–114), and add the three fields to the `ModelSpec` struct:

```go
type Provider string

const (
	ProviderLMStudio Provider = "lmstudio"
	ProviderPhala    Provider = "phala"
	ProviderChutes   Provider = "chutes"
)
```
Then add to `ModelSpec` (top of the struct):
```go
	Provider Provider
	BaseURL  string
	APIKey   string
```

**Step 2.4 — Run, expect pass (whole package, to catch encoder fallout).**
```bash
go test -race ./internal/llm/... -v
```
Expected: PASS.

**Step 2.5 — Commit.**
```bash
git add internal/llm/llm.go internal/llm/llm_test.go
git commit -m "feat(llm): add Provider, BaseURL, APIKey to ModelSpec

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: `internal/llm/auto` — provider factory

**Files:**
- Create: `internal/llm/auto/auto.go`
- Create: `internal/llm/auto/auto_test.go`

**Step 3.1 — Write the failing test.** Create `internal/llm/auto/auto_test.go`:

```go
package auto

import (
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/llm"
)

func temp(f float64) *float64 { return &f }

func TestNew(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		spec     llm.ModelSpec
		wantErr  bool
		wantLLM  bool
	}{
		{name: "lmstudio", spec: llm.ModelSpec{Provider: llm.ProviderLMStudio, BaseURL: "http://x"}, wantLLM: true},
		{name: "phala", spec: llm.ModelSpec{Provider: llm.ProviderPhala, BaseURL: "http://x", APIKey: "k"}, wantLLM: true},
		{name: "chutes", spec: llm.ModelSpec{Provider: llm.ProviderChutes, BaseURL: "http://x", APIKey: "k"}, wantLLM: true},
		{name: "unknown provider", spec: llm.ModelSpec{Provider: "nope"}, wantErr: true},
		{name: "empty provider", spec: llm.ModelSpec{}, wantErr: true},
		{name: "invalid spec rejected before dispatch",
			spec:    llm.ModelSpec{Provider: llm.ProviderLMStudio, ThinkingBudget: 1, Temperature: temp(0.5)},
			wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := New(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("New() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var ve *llm.ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("err = %T, want *llm.ValidationError", err)
				}
				return
			}
			if (got != nil) != tt.wantLLM {
				t.Fatalf("New() llm = %v, wantLLM %v", got, tt.wantLLM)
			}
		})
	}
}
```

**Step 3.2 — Run, expect failure.**
```bash
go test -race ./internal/llm/auto/ -run TestNew -v
```
Expected: compile failure — `undefined: New`.

**Step 3.3 — Implement.** Create `internal/llm/auto/auto.go` from spec **`### internal/llm/auto/auto.go`** (lines ~134–152). Imports: `llm`, and the three provider packages `.../openaiapi/lmstudio`, `.../openaiapi/phala`, `.../openaiapi/chutes`. Constructor calls (verified signatures): `lmstudio.New(spec.BaseURL)`, `phala.New(spec.BaseURL, spec.APIKey)`, `chutes.New(spec.BaseURL, spec.APIKey)`.

**Step 3.4 — Run, expect pass.**
```bash
go test -race ./internal/llm/auto/ -v
```
Expected: PASS.

**Step 3.5 — Commit.**
```bash
git add internal/llm/auto/
git commit -m "feat(llm/auto): provider factory dispatching on ModelSpec.Provider

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: `internal/agent/loop` — leaf types (errors, events, sink, command, config)

These are pure type declarations and one pure function (`validateStartTurn`). Build them together (they're a few lines each and the actor in Task 6 needs all of them to compile). One subagent, one commit, but still test-first on the behavior-bearing pieces.

**Files:**
- Create: `internal/agent/loop/errors.go`, `event.go`, `sink.go`, `command.go`, `config.go`
- Create: `internal/agent/loop/types_test.go`

**Step 4.1 — Write the failing test.** Create `internal/agent/loop/types_test.go`:

```go
package loop

import (
	"errors"
	"testing"
)

func TestErrorTypesAreTyped(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"config missing client", &ConfigError{Kind: ConfigMissingClient}, "loop: config error: Config.Client is required"},
		{"turn busy running", &TurnBusyError{Reason: TurnAlreadyRunning}, "loop: turn already running"},
		{"turn busy shutdown", &TurnBusyError{Reason: SessionShuttingDown}, "loop: session shutting down"},
		{"empty response", &EmptyResponseError{}, "loop: empty response from provider"},
		{"turn panic", &TurnPanicError{Detail: "x"}, "loop: panic in turn goroutine: x"},
		{"invalid command", &InvalidCommandError{Command: CommandStartTurn, Field: StartTurnAbandoned}, "loop: invalid command: StartTurn.Abandoned is required"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfigErrorUnwrap(t *testing.T) {
	t.Parallel()
	cause := errors.New("inner")
	err := &ConfigError{Kind: ConfigInvalidModel, Cause: cause}
	if !errors.Is(err, cause) {
		t.Error("ConfigError does not unwrap to its Cause")
	}
}

func TestValidateStartTurn(t *testing.T) {
	t.Parallel()
	ctx := contextTODO()
	ev := make(chan Event)
	ab := make(chan struct{})
	ack := make(chan error)
	tests := []struct {
		name      string
		cmd       StartTurn
		wantField CommandField
		wantErr   bool
	}{
		{"valid", StartTurn{Ctx: ctx, Events: ev, Abandoned: ab, Ack: ack}, "", false},
		{"nil ctx", StartTurn{Events: ev, Abandoned: ab, Ack: ack}, StartTurnCtx, true},
		{"nil events", StartTurn{Ctx: ctx, Abandoned: ab, Ack: ack}, StartTurnEvents, true},
		{"nil abandoned", StartTurn{Ctx: ctx, Events: ev, Ack: ack}, StartTurnAbandoned, true},
		{"nil ack", StartTurn{Ctx: ctx, Events: ev, Abandoned: ab}, StartTurnAck, true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateStartTurn(tt.cmd)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateStartTurn() err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			var ice *InvalidCommandError
			if !errors.As(err, &ice) {
				t.Fatalf("err = %T, want *InvalidCommandError", err)
			}
			if ice.Field != tt.wantField {
				t.Errorf("Field = %q, want %q", ice.Field, tt.wantField)
			}
		})
	}
}
```
> Helper: add `func contextTODO() context.Context { return context.TODO() }` to the test file (or just use `context.TODO()` inline and import `context`).

**Step 4.2 — Run, expect failure.**
```bash
go test -race ./internal/agent/loop/ -run 'TestErrorTypes|TestConfigErrorUnwrap|TestValidateStartTurn' -v
```
Expected: compile failure — undefined `ConfigError`, `validateStartTurn`, etc.

**Step 4.3 — Implement.** Copy verbatim from the spec:
- `errors.go` ← spec **`### Errors`** (the full block, including the new `EmptyResponseError` and `TurnPanicError`).
- `event.go` ← spec **`### event.go`** (note `TurnFailed.Err` is `error`).
- `sink.go` ← spec **`### sink.go`**.
- `command.go` ← spec **`### command.go`**, plus the `validateStartTurn` function from spec lines ~671–684.
- `config.go` ← spec **`### config.go`** (with the `DrainTimeout` field and `import ("time"; ".../llm")`).

**Step 4.4 — Run, expect pass.**
```bash
go test -race ./internal/agent/loop/ -v
```
Expected: PASS.

**Step 4.5 — Commit.**
```bash
git add internal/agent/loop/errors.go internal/agent/loop/event.go internal/agent/loop/sink.go internal/agent/loop/command.go internal/agent/loop/config.go internal/agent/loop/types_test.go
git commit -m "feat(loop): typed errors, events, sink, commands, config

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: `internal/agent/loop/turn.go` — the streaming assembler

`runTurn` is pure given a `client llm.LLM` and an `emit func(Event)` — no actor, no channels of its own. Test it directly with a fake LLM. This is where the **rollback-on-failure** and **typed `TurnFailed.Err`** fixes get verified.

**Files:**
- Create: `internal/agent/loop/turn.go`
- Create: `internal/agent/loop/fake_test.go` (shared fake LLM — also used by Task 6)
- Create: `internal/agent/loop/turn_test.go`

**Step 5.1 — Write the fake LLM helper.** Create `internal/agent/loop/fake_test.go`:

```go
package loop

import (
	"context"
	"errors"
	"io"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
)

// fakeLLM is a controllable llm.LLM for tests.
//   - chunks:   emitted in order from Stream's reader
//   - streamErr: returned from Stream() itself (before any chunk)
//   - nextErr:   returned from Next() after all chunks are drained (instead of io.EOF)
//   - blockUntilCancel: if true, Next() blocks until ctx is cancelled, then
//     returns ctx.Err() — models a provider that honors cancellation
//   - ignoreCtx: if true with blockUntilCancel, Next() blocks forever (models a
//     provider that ignores ctx) — use only with a bounded test timeout
type fakeLLM struct {
	chunks           []content.Chunk
	streamErr        error
	nextErr          error
	blockUntilCancel bool
	ignoreCtx        bool
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
		if f.blockUntilCancel {
			if f.ignoreCtx {
				select {} // deliberate hang; only safe under a bounded test
			}
			<-ctx.Done()
			return content.Chunk{}, ctx.Err()
		}
		if f.nextErr != nil {
			return content.Chunk{}, f.nextErr
		}
		return content.Chunk{}, io.EOF
	}
	return llm.NewStreamReader(next, nil), nil
}
```

**Step 5.2 — Write the failing test.** Create `internal/agent/loop/turn_test.go`:

```go
package loop

import (
	"context"
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
)

func drainEmit(events *[]Event) func(Event) {
	return func(ev Event) { *events = append(*events, ev) }
}

func TestRunTurn(t *testing.T) {
	t.Parallel()
	cfg := Config{Model: llm.ModelSpec{Model: "m"}}
	input := []*content.Block{{Type: content.TypeText, Text: &content.TextBlock{Text: "hi"}}}

	t.Run("success appends user+assistant and returns TurnDone", func(t *testing.T) {
		t.Parallel()
		client := &fakeLLM{chunks: []content.Chunk{textChunk("hel"), textChunk("lo")}}
		var emitted []Event
		msgs, terminal := runTurn(context.Background(), input, 1, nil, cfg, client, drainEmit(&emitted))

		done, ok := terminal.(TurnDone)
		if !ok {
			t.Fatalf("terminal = %T, want TurnDone", terminal)
		}
		if len(msgs) != 2 {
			t.Fatalf("history len = %d, want 2 (user, assistant)", len(msgs))
		}
		if _, ok := msgs[0].(*content.UserMessage); !ok {
			t.Errorf("msgs[0] = %T, want *UserMessage", msgs[0])
		}
		text := done.Message.Blocks[len(done.Message.Blocks)-1].Text.Text
		if text != "hello" {
			t.Errorf("assembled text = %q, want %q", text, "hello")
		}
	})

	t.Run("stream error rolls back user message, TurnFailed carries typed cause", func(t *testing.T) {
		t.Parallel()
		boom := &llm.ValidationError{Field: "x", Reason: "boom"}
		client := &fakeLLM{streamErr: boom}
		var emitted []Event
		msgs, terminal := runTurn(context.Background(), input, 1, nil, cfg, client, drainEmit(&emitted))

		failed, ok := terminal.(TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var ve *llm.ValidationError
		if !errors.As(failed.Err, &ve) {
			t.Fatalf("TurnFailed.Err = %T, want *llm.ValidationError via errors.As", failed.Err)
		}
		if len(msgs) != 0 {
			t.Errorf("history len = %d, want 0 (user rolled back)", len(msgs))
		}
	})

	t.Run("empty response rolls back and returns EmptyResponseError", func(t *testing.T) {
		t.Parallel()
		client := &fakeLLM{chunks: nil} // stream completes with no chunks
		var emitted []Event
		msgs, terminal := runTurn(context.Background(), input, 1, nil, cfg, client, drainEmit(&emitted))

		failed, ok := terminal.(TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var ere *EmptyResponseError
		if !errors.As(failed.Err, &ere) {
			t.Fatalf("TurnFailed.Err = %T, want *EmptyResponseError", failed.Err)
		}
		if len(msgs) != 0 {
			t.Errorf("history len = %d, want 0 (user rolled back)", len(msgs))
		}
	})

	t.Run("cancelled context rolls back and returns TurnInterrupted", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		client := &fakeLLM{streamErr: context.Canceled}
		var emitted []Event
		msgs, terminal := runTurn(ctx, input, 1, nil, cfg, client, drainEmit(&emitted))

		if _, ok := terminal.(TurnInterrupted); !ok {
			t.Fatalf("terminal = %T, want TurnInterrupted", terminal)
		}
		if len(msgs) != 0 {
			t.Errorf("history len = %d, want 0 (user rolled back)", len(msgs))
		}
	})
}
```

**Step 5.3 — Run, expect failure.**
```bash
go test -race ./internal/agent/loop/ -run TestRunTurn -v
```
Expected: compile failure — `undefined: runTurn`.

**Step 5.4 — Implement.** Create `internal/agent/loop/turn.go` from spec **`### turn.go`** (the corrected version: all three failure paths return `msgs[:len(msgs)-1]`, `TurnFailed.Err` is `error`, empty response returns `&EmptyResponseError{}`). Imports: `context`, `io`, `strings`, `content`, `llm`.

**Step 5.5 — Run, expect pass.**
```bash
go test -race ./internal/agent/loop/ -run TestRunTurn -v
```
Expected: PASS.

**Step 5.6 — Commit.**
```bash
git add internal/agent/loop/turn.go internal/agent/loop/fake_test.go internal/agent/loop/turn_test.go
git commit -m "feat(loop): runTurn streaming assembler with rollback-on-failure

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: `internal/agent/loop/loop.go` — the actor

The big one: `New` + `listen`. Copy the implementation from spec **`### loop.go`** and the `listen`/`loopState`/`turnResult` blocks (lines ~415–685, the corrected versions with the `ctx.Done` escape in `deliverAndClose`, the bounded-drain `select` with `cfg.DrainTimeout`, and the backpressure `emit`). Then drive the tests in **one subagent, many red/green micro-cycles** — the file is one cohesive unit, so it stays with one owner.

**Files:**
- Create: `internal/agent/loop/loop.go`
- Create: `internal/agent/loop/loop_test.go`

**Test harness.** Add this helper to `loop_test.go` to reduce per-test boilerplate:

```go
package loop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/uuid"
)

type captureSink struct {
	mu  sync.Mutex
	got []EventEnvelope
}

func (s *captureSink) OnEvent(_ context.Context, e EventEnvelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, e)
}
func (s *captureSink) events() []EventEnvelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]EventEnvelope(nil), s.got...)
}

// startLoop spins up a loop with the given client and returns it plus a cancel.
func startLoop(t *testing.T, client llm.LLM, sinks ...EventSink) (*Loop, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	l, err := New(ctx, id, Config{Client: client, Model: llm.ModelSpec{Model: "m"}, Sinks: sinks, DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(cancel)
	return l, cancel
}

// runOneTurn sends a StartTurn and collects events until terminal.
func runOneTurn(t *testing.T, l *Loop) (events []Event, terminal Event) {
	t.Helper()
	ev := make(chan Event, 64)
	ack := make(chan error, 1)
	ab := make(chan struct{})
	defer close(ab)
	input := []*content.Block{{Type: content.TypeText, Text: &content.TextBlock{Text: "hi"}}}
	l.Commands <- StartTurn{Ctx: context.Background(), Input: input, Events: ev, Abandoned: ab, Ack: ack}
	if err := <-ack; err != nil {
		t.Fatalf("StartTurn ack = %v, want nil", err)
	}
	for e := range ev {
		events = append(events, e)
		switch e.(type) {
		case TurnDone, TurnFailed, TurnInterrupted:
			terminal = e
			return events, terminal
		}
	}
	t.Fatal("events channel closed without terminal")
	return nil, nil
}
```
> Add `import "sync"` to the harness file.

Implement `loop.go` now (copy from spec), then run the table below as individual `t.Run` subtests. **Write each subtest, watch it fail, confirm the actor handles it, commit in logical groups.** Required cases (mirrors the spec's loop test table, now including the fix cases):

| Subtest | Assert |
|---|---|
| `New` missing client | `*ConfigError{Kind: ConfigMissingClient}` |
| `New` invalid model | `*ConfigError{Kind: ConfigInvalidModel}`, `errors.As` to inner `*llm.ValidationError` |
| startup event | `captureSink` observes `SessionStarted{SessionID}` (TurnIndex 0) |
| single turn | `TurnStarted` → ≥1 `TokenDelta` → `TurnDone`; idle after (a second turn is accepted) |
| two turns serial | second `StartTurn` accepted only after first terminal |
| start while running | second `StartTurn.Ack` = `*TurnBusyError{TurnAlreadyRunning}`; its Events closed |
| start while shutting down | `*TurnBusyError{SessionShuttingDown}`; Events closed |
| invalid start missing abandoned | `*InvalidCommandError{Field: StartTurnAbandoned}`; actor still usable |
| interrupt mid-turn | `Interrupt.Ack` true; terminal `TurnInterrupted`; idle after |
| interrupt idle | `Interrupt.Ack` false |
| interrupt missing ack | logs, no deadlock, still usable |
| ctx cancel mid-turn | turn ctx cancelled (fake `blockUntilCancel`); `TurnInterrupted`; idle after |
| shutdown idle | `Shutdown.Ack` nil; `Loop.Done` closes |
| shutdown mid-turn | turn cancelled; terminal delivered; `Shutdown.Ack` nil |
| shutdown while already shutting down | both acks nil after one cleanup |
| shutdown missing ack | actor exits, no block |
| **`TurnFailed.Err` typed** | fake `streamErr` a `*llm.ValidationError`; terminal `TurnFailed`; `errors.As` succeeds |
| **failed turn rolls back** | after `TurnFailed`, next turn's `req.Messages` has no trailing user msg (assert via a recording fake that captures `req`) |
| **slow Stream consumer keeps all deltas** | consumer reads with a delay; no delta dropped; assembled `TurnDone.Message` == concatenated deltas |
| **leaked reader + root-ctx cancel** | start a turn with an `Abandoned` you never close and never read Events; cancel root ctx; `Loop.Done` closes within `DrainTimeout`+slack; no wedge |
| **ctx-ignoring provider on hard kill** | fake `blockUntilCancel+ignoreCtx`; cancel root ctx; `Loop.Done` closes within `DrainTimeout`+slack |
| loop ctx cancel `LoopTerminatedError` | direct `Shutdown` then cancel root ctx; `Shutdown.Ack` = `*LoopTerminatedError`, `errors.As` succeeds |
| turn goroutine panic | fake whose `Stream` panics; terminal `TurnFailed` with `*TurnPanicError`; idle after |
| send after exit | after `Loop.Done`, a `select` send on `Commands` with `Done` returns immediately |
| event sink panic | panicking sink recovered; turn still completes |

> For the "rolls back" and panic cases you'll need two more tiny fakes: a `recordingLLM` that stores the last `req` it received, and a `panicLLM` whose `Stream` panics. Add them to `fake_test.go`.

Representative full subtest for the **leaked-reader escape** (the issue-#1 regression guard) — write this one out in full since it's the subtlest:

```go
func TestLeakedReaderDoesNotWedgeActor(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	id, _ := uuid.New()
	l, err := New(ctx, id, Config{
		Client:       &fakeLLM{chunks: []content.Chunk{textChunk("hi")}},
		Model:        llm.ModelSpec{Model: "m"},
		DrainTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ev := make(chan Event)       // unbuffered, never read
	ack := make(chan error, 1)
	ab := make(chan struct{})    // never closed -> simulates a leaked Stream reader
	l.Commands <- StartTurn{Ctx: context.Background(), Input: nil, Events: ev, Abandoned: ab, Ack: ack}
	if err := <-ack; err != nil {
		t.Fatalf("ack = %v", err)
	}

	cancel() // root-ctx kill must rescue the actor even though nobody reads ev/closes ab

	select {
	case <-l.Done:
		// good: actor exited despite the leaked reader
	case <-time.After(2 * time.Second):
		t.Fatal("actor wedged: Loop.Done never closed after root-ctx cancel")
	}
}
```

**Run after each group:**
```bash
go test -race ./internal/agent/loop/ -v
```
Expected: PASS (and crucially, no `-race` warnings and no `go test` timeout — a timeout means a wedge regression).

**Commit (group the actor work, e.g. 2–3 commits):**
```bash
git add internal/agent/loop/loop.go internal/agent/loop/loop_test.go internal/agent/loop/fake_test.go
git commit -m "feat(loop): actor goroutine with bounded drain and leak-safe delivery

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: `internal/session/agent.go` — AgentSession

**Files:**
- Create: `internal/session/agent.go`
- Create: `internal/session/agent_test.go`

Implementation is a verbatim copy of spec **`## internal/session/agent.go`** (lines ~779–976), including the `sync.Once`-guarded `abandoned` close in `Stream` and the `defer close(abandoned)` in `Invoke`. Tests need a fake `llm.LLM`; reuse the pattern from Task 5 (define a small fake in `agent_test.go`, or export a constructor — keep it package-local).

Required cases (from spec's session test table):

| Subtest | Assert |
|---|---|
| `NewAgent` non-zero SessionID | not zero `uuid.UUID` |
| `NewAgent` ctx cancelled | `*SessionError{Kind: SessionContextDone}` |
| `Invoke` returns `TurnDone` | terminal event, nil error |
| `Invoke` ctx cancel returns `TurnInterrupted` | event (not Go error) |
| `Stream` yields ordered events | `TurnStarted` → `TokenDelta`s → `TurnDone` → EOF |
| `Stream` `sr.Close()` cancels turn | sink observes `TurnInterrupted`; session usable again |
| `Stream` drain contract | read-to-EOF releases; early `Close` releases |
| `Interrupt` during `Invoke` | `(true, nil)`; `Invoke` returns `TurnInterrupted` |
| `Interrupt` ctx cancelled before send | `(false, *SessionError{SessionContextDone})` |
| concurrent `Invoke` | second returns `*loop.TurnBusyError` |
| `Shutdown` waits for exit | nil only after actor done |
| `Shutdown` after shutdown | nil immediately |
| methods after shutdown | `*SessionError{Kind: SessionLoopExited}`, no deadlock |

Write a representative full subtest for **`Invoke` happy path** to anchor the file:

```go
func TestInvokeReturnsTurnDone(t *testing.T) {
	t.Parallel()
	cfg := loop.Config{Client: &fakeLLM{chunks: []content.Chunk{textChunk("hello")}}, Model: llm.ModelSpec{Model: "m"}}
	s, err := NewAgent(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	ev, err := s.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke err = %v", err)
	}
	if _, ok := ev.(loop.TurnDone); !ok {
		t.Fatalf("event = %T, want loop.TurnDone", ev)
	}
}
```
> The `fakeLLM`/`textChunk` helpers are in package `loop`'s test files, not importable. Re-declare a minimal fake in `internal/session/agent_test.go` (package `session`).

**Run / Commit:**
```bash
go test -race ./internal/session/ -v
git add internal/session/
git commit -m "feat(session): AgentSession with Invoke, Stream, Interrupt, Shutdown

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 8: Whole-tree verification & security gate

**Files:** none (verification only). REQUIRED SUB-SKILL: superpowers:verification-before-completion — paste real command output, no "should pass".

**Step 8.1 — Full race test.**
```bash
go test -race ./...
```
Expected: every package PASS, no race warnings, no timeout.

**Step 8.2 — Build with required flags.**
```bash
CGO_ENABLED=0 go build -trimpath ./...
```
Expected: clean build.

**Step 8.3 — Security + static analysis gate.**
```bash
make secure
```
Expected: `go vet`, `staticcheck`, `gosec`, `go mod verify`, `govulncheck` all clean. Address any finding before declaring done (e.g. gosec on the recover()/`fmt.Sprintf` site — should be fine, but verify).

**Step 8.4 — Goroutine-leak spot check.** Confirm no test leaked a goroutine: a `go test` that hangs or a `-race` "leaked" note is a failure. (Optional: add a `TestMain` with a leak check later — out of scope here.)

**Step 8.5 — Final commit (only if verification changed anything).**
```bash
git add -A
git commit -m "chore(loop,session): pass make secure and -race across tree

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Import layering (sanity check while building)

```
internal/uuid            (no internal imports)
internal/content         (no internal imports)
internal/llm          →  internal/content
internal/llm/auto     →  internal/llm, internal/llm/openaiapi/{lmstudio,phala,chutes}
internal/agent/loop   →  internal/uuid, internal/content, internal/llm
internal/session      →  internal/uuid, internal/agent/loop, internal/llm
```
If anything tries to import "upward" (e.g. `loop` importing `session`), stop — it's a layering violation.

## Out of scope (do not build here)

Tools / `runToolBatch`, conversation-history compaction, journal/WAL, checkpoint/resume, turn queuing, `agents/coding` wiring, `cmd/urvi` integration, registry. These are the spec's "Explicitly deferred" list.
