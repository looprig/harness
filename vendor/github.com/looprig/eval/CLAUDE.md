# CLAUDE.md — eval

`eval` is an **application-neutral evaluation framework** for agentic systems.
The root package models conversations, expectations, evidence, evaluators,
rubrics, findings, measurements, reports, and sinks on top of the
`github.com/looprig/core` content model. It runs as ordinary Go code under
`go test`.

## Dependencies

- **Approved dependencies are narrow and additive.** The **only** third-party
  dependency today is `github.com/looprig/core` (via `require` + a local
  `replace github.com/looprig/core => ../core`).
- `github.com/looprig/inference` may be added **later, and only** in the
  `judge/` and `target/inference/` packages. The **root package must never
  import `inference`.**
- **No other third-party dependency without explicit approval.** If a task
  seems to need one, stop and confirm before running `go get`.

## Code rules

- **Strict typing.** No `any`/`interface{}` except at explicit serialization
  boundaries, narrowed immediately. Named types (`type Score float64`,
  `type Verdict string`) over bare primitives when the value carries domain
  meaning. No untyped magic numbers/strings.
- **All errors are typed.** Every distinct failure mode is a concrete struct
  with an `Error()` method (and `Unwrap()` when it carries a cause). Never
  return `errors.New(...)`/`fmt.Errorf(...)` from a package-level API. Callers
  classify with `errors.As` — never by string.
- **Diagnostic strings must never echo untrusted content** — conversation text,
  tool output, judge explanations. Bound and redact anything that leaves the
  process in an error, log, or report field.
- **Contracts first.** Write the interface, then the implementation. Keep
  interfaces small and segregated; a caller never depends on methods it does
  not use.
- Return errors explicitly; never swallow with `_`.
- Functions over ~30 lines invite an SRP check before growing further.

## Security

- **Fail secure.** Missing evidence or unavailable enforcement is `unverified`,
  never a passing score. On error or ambiguity, deny/deny-by-default; never
  fall through.
- Validate all external input at the boundary before it reaches evaluators or
  sinks.
- `crypto/rand` for anything security-sensitive; never `math/rand`.
- Every I/O method takes a `context.Context`; callers set deadlines. No
  unbounded blocking.

## Testing

- **Table-driven tests, mandatory**, each with `t.Parallel()`. Cover happy
  path, boundary values (zero/empty/max), error cases, and domain edge cases
  (absent evidence, nil/empty conversations, unknown kinds).
- **Always `-race`:** `GOWORK=off go test -race ./...`. A test that needs
  `-race` to fail is not passing.
- **Fuzz any parser** of external input, including serialized envelopes and
  evaluator descriptors (`go test -fuzz=FuzzXxx -fuzztime=30s ./...`).
- **Integration tests** that reach real models, HTTP endpoints, or processes go
  behind a build tag and are skipped by default; unit tests never require the
  network.
- `gofmt`-clean and `go vet`-clean at all times.

## Build & workspace

- **Every Go command runs with `GOWORK=off`** — a parent `go.work` must not
  capture this module; each module proves it resolves through its own
  `require`/`replace` graph.
- `make secure` (lint + vuln) and `-race` tests before every commit.
