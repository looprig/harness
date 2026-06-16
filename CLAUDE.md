# CLAUDE.md — Development Guidelines

## SOLID Principles (strictly enforced)

**Single Responsibility** — Every struct, function, and package has exactly one reason to change. If you can't describe what it does in one sentence without "and", split it.

**Open/Closed** — Extend behavior via interfaces and composition. Never modify a working type to add new behavior; add a new type or wrap it.

**Liskov Substitution** — Every implementation of an interface must honor the full contract. If a concrete type can't satisfy a method without panicking, returning errors the caller doesn't expect, or silently doing less, redesign the interface.

**Interface Segregation** — Interfaces are small and focused. A caller should never be forced to depend on methods it doesn't use. Prefer many small interfaces over one large one.

**Dependency Inversion** — Depend on interfaces, not concrete types. High-level packages must not import low-level packages directly. Wire dependencies at the composition root (main or a factory), never inside business logic.

## Security — First-Class, Not an Afterthought

**Validate at every boundary.** All external input (HTTP, CLI args, env vars, files, queues) is untrusted until validated. Validate before it enters business logic, not inside it.

**Least privilege always.** Every component, goroutine, and service gets only the permissions it needs. Never pass a full config or god-object when a narrow interface suffices.

**No secrets in code.** No hardcoded tokens, passwords, keys, or connection strings — ever. Use environment variables or a secrets manager. Fail loudly on startup if required secrets are missing.

**Authenticate before authorize, authorize before act.** Every action that crosses a trust boundary must check identity first, then permission, then execute. Never assume a caller is trusted.

**Sanitize before use.** Never interpolate external data into queries, shell commands, file paths, or log messages without sanitization. Use parameterized queries, exec with argument lists, and filepath.Clean.

**Fail secure.** On error or ambiguity, deny by default. A failed permission check must block the action, not fall through.

**Log security events, not secrets.** Audit auth failures, permission denials, and unexpected inputs. Never log credentials, tokens, or PII.

## Dependencies

**Prefer stdlib.** Always reach for the Go standard library first. If a need can be met with stdlib — even with a bit more code — use stdlib.

**External packages require explicit user approval.** Before adding any external dependency, stop and ask the user. State what the package is, why stdlib is insufficient, and what the package adds. Do not `go get` or add to `go.mod` without a clear "yes" from the user in the current conversation.

**Amend this file when approved.** Once a package is approved, add it here so future sessions know it is sanctioned:

<!-- Approved external packages -->
- `github.com/securego/gosec/v2` — security static analysis tool (dev/tool only)
- `golang.org/x/vuln/cmd/govulncheck` — official Go vulnerability scanner (dev/tool only)
- `honnef.co/go/tools/cmd/staticcheck` — extended static analysis (dev/tool only)
- `github.com/google/go-tdx-guest` — Intel TDX quote parsing and verification; required by internal/llm/tee for phala and chutes TEE attestation
- `golang.org/x/crypto` — ChaCha20-Poly1305 AEAD; required by internal/llm/e2e for ML-KEM E2E envelope (stdlib has no chacha20poly1305)
- `github.com/charmbracelet/bubbletea` — Elm-architecture TUI runtime; required by tui + cmd/cli (stdlib has no terminal raw-mode/TUI framework). **v2 approved (2026-06-15):** the TUI is migrating to Bubble Tea v2 (and co-versioned Bubbles/Lipgloss v2) for the Kitty keyboard protocol (true Shift+Enter newline) and other v2 features. Use v2 APIs throughout — v1.3.10 cannot distinguish Shift+Enter from Enter. **Migration landed (2026-06-15): the v2 import path is the `charm.land/...` vanity module, i.e. `charm.land/bubbletea/v2` (NOT `github.com/charmbracelet/...`).**
- `github.com/charmbracelet/bubbles` — textarea + viewport widgets for the TUI. **v2 approved (2026-06-15)** (co-required by Bubble Tea v2). v2 import path: `charm.land/bubbles/v2` (subpackages `.../textarea`, `.../viewport`, `.../key`).
- `github.com/charmbracelet/lipgloss` — terminal styling/layout for the TUI. **v2 approved (2026-06-15)** (co-required by Bubble Tea v2). v2 import path: `charm.land/lipgloss/v2`.
- `github.com/charmbracelet/glamour` — markdown → ANSI rendering for the TUI transcript; pin a version compatible with Lipgloss v2. v2 import path: `charm.land/glamour/v2` (styles subpackage `charm.land/glamour/v2/styles`).
- `github.com/atotto/clipboard` — transitive (indirect) dep of `bubbles/textarea`, which imports it unconditionally for paste; approved as part of textarea, not chosen directly
- `golang.org/x/net/html` — HTML tokenizer; required by the `WebSearch` tool's DuckDuckGo HTML-scrape `SearchProvider` (stdlib has no HTML parser)
- `golang.org/x/net/idna` — IDNA/punycode host normalization (same `golang.org/x/net` module as above); required by the `Fetch` tool's persisted-approval host matching to defeat unicode homographs (stdlib has no IDNA)

### Vendored patches (forked dependencies)

When a sanctioned dependency has a confirmed upstream bug we cannot wait on, we
vendor a **minimally patched** copy under `third_party/<module-path>/` and wire
it in via a `replace <module> => ./third_party/<module-path>` directive in
`go.mod`. The copy is byte-for-byte the pinned upstream version except for the
documented patch; each fork carries a `PATCH.md` recording the upstream version,
the exact change, why, and how to re-sync. A local `replace => ./dir` is not
checksum-verified by `go mod verify` (expected); `govulncheck` still scans the
local code. **Re-evaluate every fork on dependency bumps — drop it if upstream
fixes the bug.**

- `github.com/charmbracelet/ultraviolet` (transitive dep of `charm.land/bubbletea/v2`) — vendored as `./third_party/github.com/charmbracelet/ultraviolet`, pinned to `v0.0.0-20260525132238-948f4557a654`. **One-line patch** in `(*TerminalRenderer).Render` (`terminal_renderer.go`): the post-resize `curbuf` sync loop `for i := curHeight - 1; i < newHeight` → `for i := 0; i < newHeight`, so **all** rows (not just the grown tail) are synced from the new frame after a dimension change. **Why:** upstream left `curbuf` holding stale content from the previous, different-width frame (old narrow row survives on width growth; nothing synced at all on height shrink), so the next diff partial-redrew the bordered composer box's `┌…┐` / `└…┘` borders at an absolute column offset (`CSI <col> C`) and desynced the relative cursor-up count — a Bubble Tea **v2 inline-renderer resize scrollback leak** that stranded the prior separator + box-top rows in native scrollback on every resize step. Locked by `tui/resize_scrollback_leak_test.go`; full detail in `third_party/github.com/charmbracelet/ultraviolet/PATCH.md`. **Must be re-synced (or dropped) when bubbletea / ultraviolet updates.**

## Secure Coding Patterns

**Randomness** — Use `crypto/rand` for anything security-sensitive (tokens, nonces, IDs). Never use `math/rand` for secrets.

**Queries** — Always use parameterized queries via `database/sql`. Never format SQL with `fmt.Sprintf` or string concatenation.

**HTTP server** — Always set explicit timeouts. No naked `http.ListenAndServe` with default server:
```go
srv := &http.Server{
    ReadTimeout:    5 * time.Second,
    WriteTimeout:   10 * time.Second,
    IdleTimeout:    60 * time.Second,
    MaxHeaderBytes: 1 << 20,
}
```

**TLS** — Never set `InsecureSkipVerify: true`. Never use TLS versions below 1.2. Default to `tls.Config{MinVersion: tls.VersionTLS12}`.

**Context** — Every I/O call (HTTP, DB, file, external service) must use a `context.Context` with a timeout or deadline. No unbounded blocking.

**Shell commands** — Never pass user input to `exec.Command` as a shell string. Always pass args as separate parameters.

> **Documented exception — the `Bash` tool (`tools/bash.go`).** `Bash` runs a single command via `sh -c <command>` — a deliberate violation of the rule above, because a coding agent genuinely needs shell features (pipes, globs, `&&`, redirects) an argv list can't express. The security boundary is the **permission gate**, not the argv shape: `Bash` defaults to **Ask**, so a human reads and approves each command before it runs. `DeniedBashPrefixes` is **advisory** defense-in-depth only (trivially bypassable — `/usr/bin/sudo`, `env sudo`, …) and must never be relied on as a boundary. The real hard boundary — OS-level sandboxing (seccomp/landlock/nsjail) — is **out of scope** and is the prerequisite for ever auto-approving `Bash` broadly; until then `Bash` must stay human-gated. The exec call carries a `// #nosec G204` with this rationale.

**File paths** — Always call `filepath.Clean` and verify the result stays within the expected root before opening files from user-supplied paths.

## Build & Testing Requirements

**Build** — Always build with `CGO_ENABLED=0 go build -trimpath`. Never ship a binary without `-trimpath` (leaks local paths).

**Tests** — Always run with `-race`: `go test -race ./...`. A test that passes without `-race` but not with it is not passing.

**Table-driven tests (mandatory).** Every test function uses a `[]struct{ name string; ... }` table. Each table must cover:
- Happy path (valid, expected input → expected output)
- Boundary values (zero, empty, max, minimum valid)
- Error cases (invalid input, missing required fields, wrong types)
- Edge cases specific to the domain (e.g. nil blocks, empty message threads, unknown block types)

```go
func TestFoo(t *testing.T) {
    tests := []struct {
        name    string
        input   Bar
        want    Baz
        wantErr bool
    }{
        {name: "happy path", ...},
        {name: "empty input", ...},
        {name: "nil field returns error", ..., wantErr: true},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel()
            got, err := Foo(tt.input)
            if (err != nil) != tt.wantErr {
                t.Fatalf("Foo() error = %v, wantErr %v", err, tt.wantErr)
            }
            if !tt.wantErr && got != tt.want {
                t.Errorf("Foo() = %v, want %v", got, tt.want)
            }
        })
    }
}
```

**Integration tests** — Write integration tests (tagged `//go:build integration`) for any code that crosses a process boundary: HTTP providers, database queries, filesystem operations, TEE attestation. Integration tests live in `*_integration_test.go` files and are excluded from the default `go test ./...` run. Run them explicitly with `go test -tags integration -race ./...`.

**Fuzzing** — For any function that parses external input, write a fuzz target: `go test -fuzz=FuzzXxx ./pkg -fuzztime=30s`.

**Security checks** — Run `make secure` before every commit. It runs `lint` (vet + staticcheck + gosec) and `vuln` (go mod verify + govulncheck).

## Code Rules

- **Strict typing everywhere.** Never use `any` or `interface{}` except at explicit serialization boundaries (JSON unmarshal, plugin APIs). Immediately narrow to a concrete type; never pass `any` deeper into business logic. No untyped magic numbers or strings — use named constants or typed enums. Prefer named types (`type UserID string`) over bare primitives when the value has domain meaning.
- All domain concepts are typed structs — no `map[string]interface{}` for domain data.
- Return errors explicitly; never swallow them with `_`.
- **All errors must be typed.** Define a concrete error struct for every distinct failure mode. Never return `errors.New("...")` or `fmt.Errorf("...")` from package-level APIs — those lose type identity at the call site. Callers must be able to `errors.As` to the concrete type to inspect cause and context. Sentinel errors (`var ErrFoo = errors.New(...)`) are permitted only for leaf errors with no additional context fields.
- Keep packages shallow and cohesive; avoid circular imports.
- Write the interface first, then the implementation.
- If a function exceeds ~30 lines, ask whether it violates SRP before adding more.
