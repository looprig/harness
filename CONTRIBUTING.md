# Contributing to looprig/harness

Thanks for considering a contribution. `harness` is part of a multi-module
Go ecosystem (see [`docs/ECOSYSTEM.md`](docs/ECOSYSTEM.md)); this file is the
short guide for working in *this* repository.

## Before you write code

1. Read [`CLAUDE.md`](CLAUDE.md) (a.k.a. `AGENTS.md`). It is the authoritative
   source for the design, security, dependency, build, and code rules the
   whole module follows. PRs that contradict it will be asked to change.
2. Skim [`docs/architecture/agent-loop.md`](docs/architecture/agent-loop.md)
   for the actor/channel/event-flow picture and a couple of recent files in
   [`docs/plans/`](docs/plans/) for the design-doc style the project uses.
3. Open an issue for anything non-trivial so we can agree on direction
   before you spend the time. The [`docs/TODO.md`](docs/TODO.md) backlog is
   a good source of known work.

## Design and security rules (the short version)

- **Strict typing everywhere.** No `any`/`interface{}` past a serialization
  boundary. Named types (`type UserID string`) over bare primitives when the
  value has domain meaning. All domain concepts are typed structs, never
  `map[string]interface{}`.
- **All errors are typed.** Sentinel or typed errors for public failures
  callers classify with `errors.As`; wrapped ordinary errors for contextual
  failures callers only report. Never swallow with `_`.
- **Security is first-class.** Validate at every boundary. Authenticate
  before authorize, authorize before act. Fail secure: on error or
  ambiguity, deny by default. Never log secrets, tokens, or PII. Use
  `crypto/rand` for anything security-sensitive.
- **Prefer stdlib.** External packages require explicit user approval in
  the conversation that adds them. Once approved, the package is added to
  the approved list in `CLAUDE.md`. Never `go get` without that approval.
- **Open-Closed + Interface Segregation.** Small focused interfaces; never
  widen an existing interface — add a separate optional capability
  interface and let the runner probe for it via type assertion. Liskov
  substitution: every implementation honors the full contract.

## Build, test, and secure

Run these before pushing. CI runs the same.

```sh
make fmt       # gofmt the whole module in place
make test      # go test -race ./...           (always -race)
make secure    # fmt-check + vendor-check + vet + staticcheck + gosec
               # + go mod verify + govulncheck
```

Build with `CGO_ENABLED=0 go build -trimpath` so binaries never leak local
paths. Integration tests are tagged `//go:build integration` and run
explicitly: `go test -tags integration -race ./...`. Fuzz any parser of
external input: `go test -fuzz=FuzzXxx ./pkg -fuzztime=30s`.

The module **vendors** its dependency tree. Do not run `go get` casually;
`make vendor` refreshes `vendor/`, scrubs the VCS metadata of local-replace
dependencies, and verifies no forbidden metadata leaked in.

## Tests

- **Table-driven tests, mandatory** when several cases share setup and
  assertion shape. Each subtest calls `t.Parallel()`. Cover the happy path,
  boundary values (zero/empty/max), error cases (invalid/missing/wrong
  type), and domain edge cases.
- A test that passes without `-race` but fails with it is **not passing**.
- Never assume a test framework or script. The `Makefile` is the source of
  truth; if you change how tests run, update it.

## Design docs and plans

Non-trivial work goes through a short design doc in `docs/plans/` named
`YYYY-MM-DD-<topic>-design.md` (and, when ready,
`YYYY-MM-DD-<topic>-implementation.md`). Date the file the day you start;
one topic per file. Keep them small and readable — they are how future
contributors (human and agent) understand why the code is the way it is.

## Pull requests

- Branch from `main`, name the branch something descriptive.
- One logical change per PR. If a change spans modules, open a PR per
  module and stack them; the `replace` directives let each module build
  against the others' local checkout.
- Write a clear description: what, why, the design alternative you
  rejected, and how you verified. `make secure` output is welcome in the
  PR body.
- Don't force-push after review; add commits and let the reviewer squash.
- Don't commit secrets, tokens, or credentials. Don't add a new external
  dependency without prior approval (see `CLAUDE.md`).
- Don't update `CLAUDE.md`, `Makefile`, or `go.mod` `replace` directives
  unless the change is the point of the PR.

## Reporting security issues

See [`SECURITY.md`](SECURITY.md). Do **not** open a public issue for a
security vulnerability.

## Code of conduct

Be excellent to each other. Discussions stay technical and respectful;
personal attacks, harassment, and discrimination are not welcome.

## License

By contributing, you agree that your contributions are licensed under the
Apache License 2.0, as described in [`LICENSE`](LICENSE).
