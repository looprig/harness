# Togo Prompt + Reusable `internal/eval` Framework — Design

**Date:** 2026-06-15
**Status:** Approved (brainstorming)
**Scope:** Two of the four coding-agent updates identified against `docs/old/5.md`
(Coding Agent Design): (1) expand and extract the system prompt, (2) add the
missing package structure — reframed by the user as a reusable evaluation
framework plus a rename of the agent's identity to **Togo**.

Explicitly **out of scope** (deferred): the model swap from Kimi K2 to a Claude
model (#3) and the Nexus-gateway vs direct-provider architecture question (#4).

---

## Background

The coding agent lives in `agents/coding` (`coding.New(ctx) (*Coding, error)`,
type `Coding`) and runs on `llm.ChutesKimiK2()` (the only model in the catalog).
Two gaps versus its spec (`docs/old/5.md`):

1. **System prompt is thin.** The spec designed a ~80-line structured prompt
   (`5.md:62-145`); the code ships an ~8-line inline `const codingPersonaPrompt`
   (`agents/coding/agent.go:36-43`). The spec also wanted the prompt in its own
   `prompts/` subpackage; there is no such package today.
2. **No evaluation structure.** The spec called for `evals/` and `golden-set/`
   placeholders (`5.md:18-28`); neither exists. There is currently no way to
   measure whether a prompt or model change improves or regresses the agent.

`codingPersonaPrompt` is referenced in three places: its definition and use in
`agent.go` (`:36`, `:80`), and once in `agent_test.go:174` (passed through to
`model.Spec`, with no assertion on its content). The module is
`github.com/inventivepotter/urvi`, so the platform is **Urvi**; this agent within
it is being named **Togo**.

---

## Decisions (from brainstorming)

| Question | Decision |
|---|---|
| Prompt approach | **Adapt the spec's section structure to the real agent** — keep 5.md's sections but rewrite content to accurately describe Togo's real 11 tools and Ask/auto-approve permission model. Tuned for the current Kimi K2 model. |
| Rename scope | **Identity/name only** — keep Go package `coding` and type `Coding`; change only the display/banner name and the system-prompt identity to "Togo". |
| Eval structure | **Seed a working scaffold**, not bare placeholders. |
| Framework shape | **Reusable `internal/eval` package** (deepeval-style), usable by any agent — not a one-off under `agents/coding`. |
| Framework depth | **Deterministic + LLM-judge** — a deterministic metric that runs offline in unit tests, plus a GEval-style LLM-judge metric exercised in an integration test. |

---

## Part A — System prompt → `prompts/` subpackage

New file `agents/coding/prompts/system.go`:

- `package prompts`
- exported `const SystemPrompt string` — verbatim text, **never constructed at
  runtime, never interpolated** (matches the spec, and satisfies CLAUDE.md's
  "no external data into prompts" rule). No logic, no formatting helpers.

Content adapts the 5.md sections to *this* agent (identity = "Togo"):

- **Identity** — "You are Togo, an interactive CLI tool that helps users with
  software-engineering tasks. You work through tools."
- **Personality** — concise, direct, friendly.
- **Doing tasks** — including the spec's rule: exploratory questions
  ("what could we do about X?") get a 2–3 sentence recommendation with the main
  trade-off, presented as redirectable; don't implement until the user agrees.
- **Communicating while you work** — brief preambles before tool calls; the user
  sees only your text output, not the tool calls.
- **Writing code** — fix root cause over surface patches, smallest change that
  works, read a file before editing it.
- **Tools & permissions** *(the accuracy fix)* — name the real tools and the
  real gate: `ReadFile`, `Glob`, `Grep`, `Todo`, `AskUser`, `Subagent` run
  automatically; `WriteFile`, `EditFile`, `Bash`, `Fetch`, `WebSearch` require
  the user's approval — so state your plan before invoking those.
- **Validating your work** — run tests / build commands when available; start
  with the narrowest test.
- **Reversibility & risky actions** — maps to the approval gate; confirm before
  hard-to-reverse actions.
- **Security & secrets** — do not read, display, or transmit credentials,
  secrets, or PII; note their presence, never their values (aligns with
  CLAUDE.md security-first).

**Wiring:** `agent.go` imports `github.com/inventivepotter/urvi/agents/coding/prompts`,
replaces both uses of `codingPersonaPrompt` with `prompts.SystemPrompt`, and
deletes the inline const. `agent_test.go:174` switches to `prompts.SystemPrompt`.

**Identity/name wiring:** the agent's display name (the `AgentBanner.Name`
constructed at the `cmd/cli` composition root, and any catalog/registration
name string) is set to "Togo". Exact wiring point to be confirmed during
implementation. No Go symbol renames (`coding` / `Coding` stay).

---

## Part B — `internal/eval` (deepeval-style, stdlib-only)

A reusable evaluation framework. It sits **low** in the layering: it imports
**only the standard library** — no agent packages, and not even `internal/llm`
(see the `Completer` interface below). Agents import it.

### Core types

```go
// TestCase is one golden input/output pair under evaluation.
type TestCase struct {
    Name           string
    Input          string   // the prompt/task given to the agent
    ActualOutput   string   // produced by the agent at run time
    ExpectedOutput string   // optional reference output
    Context        []string // optional grounding material
}

// Score is one metric's verdict on one TestCase.
type Score struct {
    Metric    string
    Value     float64 // 0..1
    Threshold float64
    Passed    bool
    Reason    string
}

// Result groups a case with all its metric scores.
type Result struct {
    Case   TestCase
    Scores []Score
    Passed bool // true iff every metric passed
}
```

### Interfaces (small, segregated)

```go
// Metric scores a single TestCase.
type Metric interface {
    Name() string
    Measure(ctx context.Context, tc TestCase) (Score, error)
}

// Runner produces a TestCase's ActualOutput from its Input
// (an agent adapter implements this).
type Runner interface {
    Run(ctx context.Context, input string) (string, error)
}

// Completer is the minimal model surface the LLM-judge metric needs.
// Keeping it here means internal/eval never imports internal/llm.
type Completer interface {
    Complete(ctx context.Context, prompt string) (string, error)
}
```

### Functions

- `RunCases(ctx, Runner, []TestCase) ([]TestCase, error)` — fills `ActualOutput`
  for each case (SRP: producing output is separate from scoring).
- `Evaluate(ctx, []TestCase, []Metric) ([]Result, error)` — scores each case
  against each metric.
- `LoadCases(dir string) ([]TestCase, error)` — reads golden cases from JSON
  files on disk.

### Metrics (v1)

- **`Contains`** — deterministic. Checks `ActualOutput` contains the required
  substrings/keywords; `Value` = fraction matched, `Passed` = `Value >= Threshold`.
  Pure stdlib; runs fully offline.
- **`Judge`** — GEval-style LLM-as-judge. Fields `{ Criteria string; Threshold
  float64; Completer Completer }`. `Measure` builds a judging prompt from the
  criteria + input + actual output, calls `Completer.Complete`, and parses a
  0–1 score and a reason. Depends only on the `Completer` interface, so it is
  unit-testable with a fake and integration-tested with a real model.

### Typed errors

`LoadError{ Path, Cause }`, `MeasureError{ Metric, Case, Cause }`,
`JudgeParseError{ Raw, Cause }`. No package-level `errors.New`/`fmt.Errorf`
escaping the API (CLAUDE.md).

---

## Part C — Togo eval wiring

- `agents/coding/golden-set/cases/hello.json` — one sample golden case (a simple
  task `Input` with `ExpectContains`/`ExpectedOutput`).
- `agents/coding/golden-set/README.md` — states the directory's purpose (golden
  input/output pairs consumed by `internal/eval`).
- **Integration test** `agents/coding/eval_integration_test.go`
  (`//go:build integration`): constructs the real Togo agent via `coding.New`,
  adapts it to `eval.Runner` (stream a turn, project the terminal
  `TurnDone.Message` to a string — mirroring the existing
  `childSubsession.Invoke` projection), provides a real `eval.Completer` backed
  by the model, loads `golden-set/`, and runs `Contains` + `Judge`. Excluded
  from the default `go test ./...`; run with `-tags integration` and
  `LLM_API_KEY` set.

---

## Testing & layering

- **Unit tests** (table-driven, `-race`, fully offline) live in `internal/eval`:
  - `Contains` against a fake `Runner`.
  - `Judge` against a **fake `Completer`** returning canned responses — covers
    happy path, parse error, and below-threshold. The LLM-judge logic is thus
    fully tested without network.
  - `LoadCases` against temp JSON fixtures (happy, missing dir, malformed JSON).
- **Integration test** (`-tags integration`) exercises the live Togo agent +
  real model.
- `go test -race ./...` stays green offline; `make secure` clean.
- **Layering:** `internal/eval` (stdlib only) ← `agents/coding` (wires the
  `Runner` + `Completer` adapters). No high-level → low-level imports; no DIP
  violations.

---

## File inventory

**New:**
- `agents/coding/prompts/system.go`
- `internal/eval/eval.go` (types, `Runner`, `Evaluate`, `RunCases`)
- `internal/eval/metric.go` (`Metric` interface, `Contains`)
- `internal/eval/judge.go` (`Completer`, `Judge`)
- `internal/eval/load.go` (`LoadCases`)
- `internal/eval/errors.go` (typed errors)
- `internal/eval/eval_test.go`, `metric_test.go`, `judge_test.go`, `load_test.go`
- `agents/coding/golden-set/cases/hello.json`
- `agents/coding/golden-set/README.md`
- `agents/coding/eval_integration_test.go` (`//go:build integration`)

**Edited:**
- `agents/coding/agent.go` (import `prompts`, use `prompts.SystemPrompt`, delete
  inline const)
- `agents/coding/agent_test.go` (use `prompts.SystemPrompt`)
- `cmd/cli` composition root (set the Togo display/banner name) — exact file TBD

---

## Out of scope (additive later)

- Model swap to a Claude model (#3) and the gateway architecture question (#4).
- Richer metrics (faithfulness, answer-relevancy, tool-correctness,
  hallucination) — the `Metric` interface makes these additive.
- Multi-agent golden-sets / a shared golden-set location.
- A CLI runner / report formatter for evals (the framework currently runs via
  `go test`).
