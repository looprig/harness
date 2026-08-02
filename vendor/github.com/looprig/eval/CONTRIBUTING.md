# Contributing to looprig/eval

Thanks for considering a contribution. `eval` is an application-neutral
evaluation framework for agentic systems, part of a multi-module Go
ecosystem. This file is the short guide for working in *this* repository.

## Before you write code

1. Read [`CLAUDE.md`](CLAUDE.md) (a.k.a. `AGENTS.md`). It is the authoritative
   source for the design, security, dependency, build, and code rules the
   whole module follows. PRs that contradict it will be asked to change.
2. Skim recent files in [`docs/plans/`](docs/plans/) for the design-doc style
   the project uses.
3. Open an issue for anything non-trivial so we can agree on direction
   before you spend the time.

## Design and security rules (the short version)

- **Strict typing everywhere.** No `any`/`interface{}` except at explicit
  serialization boundaries, narrowed immediately. Named types
  (`type Score float64`, `type Verdict string`) over bare primitives when the
  value carries domain meaning. No untyped magic numbers/strings.
- **All errors are typed.** Every distinct failure mode is a concrete struct
  with an `Error()` method (and `Unwrap()` when it carries a cause). Never
  return `errors.New(...)`/`fmt.Errorf(...)` from a package-level API.
  Callers classify with `errors.As`, never by string. Never swallow an error
  with `_`.
- **Diagnostic strings must never echo untrusted content** — conversation
  text, tool output, judge explanations. Bound and redact anything that
  leaves the process in an error, log, or report field.
- **Fail secure.** Missing evidence or unavailable enforcement is
  `unverified`, never a passing score. On error or ambiguity, deny by
  default; never fall through.
- **Contracts first.** Write the interface, then the implementation. Keep
  interfaces small and segregated; a caller never depends on methods it does
  not use.
- `crypto/rand` for anything security-sensitive; never `math/rand`. Every
  I/O method takes a `context.Context`; callers set deadlines; no unbounded
  blocking.
- **Dependencies are narrow and additive.** The only third-party dependency
  today is `github.com/looprig/core` (via a local `replace`).
  `github.com/looprig/inference` may be added later, and only in `judge/` and
  `target/inference/` — the root package must never import `inference`. No
  other third-party dependency without explicit approval in the conversation
  that adds it; don't `go get` without that approval. (The staticcheck/gosec/
  govulncheck toolchain wired via `tool (...)` in `go.mod` is a sanctioned
  dev-only exception — never linked into the library.)

## Build, test, and secure

Run these before pushing. CI runs the same. Every Go command in this module
runs with `GOWORK=off` so it proves it resolves through its own
`require`/`replace` graph, not a parent workspace.

```sh
make fmt         # gofmt the whole module in place
make fmt-check   # fail if any tracked Go file is not gofmt-clean
make lint        # fmt-check + go vet + staticcheck + gosec
make vuln        # go mod verify + govulncheck
make secure      # lint + vuln
make test        # go test -race ./...
```

## Tests

- **Table-driven tests, mandatory**, each with `t.Parallel()`. Cover the
  happy path, boundary values (zero/empty/max), error cases, and domain edge
  cases (absent evidence, nil/empty conversations, unknown kinds).
- **Always `-race`:** `GOWORK=off go test -race ./...`. A test that needs
  `-race` to fail is not passing.
- **Fuzz any parser** of external input, including serialized envelopes and
  evaluator descriptors: `go test -fuzz=FuzzXxx ./path/to/pkg -fuzztime=30s`
  (`make fuzz` prints the invocation).
- **Integration tests** that reach real models, HTTP endpoints, or processes
  go behind a build tag and are skipped by default; unit tests never require
  the network.

## Design docs and plans

Non-trivial work goes through a short design doc in `docs/plans/` named
`YYYY-MM-DD-<topic>-design.md` (and, when ready,
`YYYY-MM-DD-<topic>-implementation.md`). Date the file the day you start;
one topic per file. Keep them small and readable — they are how future
contributors (human and agent) understand why the code is the way it is.

## Pull requests

- Branch from `main`, name the branch something descriptive.
- One logical change per PR. If a change spans modules, open a PR per
  module and stack them; the `replace` directive lets this module build
  against a local `core` checkout.
- Write a clear description: what, why, the design alternative you
  rejected, and how you verified. `make secure` output is welcome in the
  PR body.
- Don't force-push after review; add commits and let the reviewer squash.
- Don't commit secrets, tokens, or credentials. Don't add a new external
  dependency without prior approval (see `CLAUDE.md`).
- Don't update `CLAUDE.md`, `Makefile`, or `go.mod` `replace` directives
  unless the change is the point of the PR.

## Code of conduct

Be excellent to each other. Discussions stay technical and respectful;
personal attacks, harassment, and discrimination are not welcome.

## License

By contributing, you agree that your contributions are licensed under the
Apache License 2.0, as described in [`LICENSE`](LICENSE).
