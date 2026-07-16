# Modular MCP Client Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (or
> superpowers:subagent-driven-development) to implement this plan task-by-task.

**Goal:** Build `github.com/looprig/mcp` — a standalone MCP client module with
stdio/Streamable HTTP/SSE transports, OAuth, immutable catalog generations, and
a `pkg/harness` adapter — plus the protocol-neutral Harness seams (form/URL
gates, external permission requests, external-toolset replacement) and TUI
rendering it needs.

**Architecture:** New sibling module `mcp/` wrapping the official
`github.com/modelcontextprotocol/go-sdk` behind `pkg/client` and
`internal/protocol` so SDK types never leak into Looprig APIs. Harness gains
three protocol-neutral seams in its own repo; the adapter `mcp/pkg/harness`
composes the two. Harness never imports MCP; the TUI never calls the MCP
client.

**Tech Stack:** Go 1.26 stdlib; `github.com/modelcontextprotocol/go-sdk`
v1.6.1 (**approved by user 2026-07-16 for the mcp module only** — record in
`mcp/CLAUDE.md`); existing looprig modules via local `replace` + vendoring.

**Design Reference:** `docs/plans/2026-07-16-mcp-client-module-design.md`
(canonical; read the referenced section before each phase). Companion:
`2026-07-16-session-versioning-migration-design.md` (unimplemented — Phase 9
degrades to fingerprint-level identity).

**Repos touched:** `looprig/mcp` (new git repo), `looprig/harness`
(Phase 6, 9), `looprig/tui` (Phase 8). Commit per task in the repo the task
touches. No `Co-Authored-By` trailers. Run `make fmt && make secure` and
`go test -race ./...` before every commit; integration tests are tagged
`integration` and run with `go test -tags integration -race ./...`.

**Executor ground rules:**
- TDD every task: failing test → minimal code → pass → commit.
- SDK types (`mcp.*` from the go-sdk) may appear ONLY in `internal/protocol`,
  `internal/*`, and transport internals — never in any `pkg/...` exported API.
  Add a lint task guard (Task 1.7) enforcing this.
- All external input is untrusted; enforce limits before allocation where
  possible. No secrets in errors, events, logs, or fingerprints.
- When the plan gives a type sketch, field names are binding but you may add
  unexported fields. When behavior is ambiguous, the design doc section named
  in the task wins.
- Read the vendored SDK source (`mcp/vendor/github.com/modelcontextprotocol/go-sdk/mcp`)
  for exact SDK APIs instead of guessing; the SDK is v1.6.1.

---

## Phase 0: Module bootstrap

### Task 0.1: Create the `mcp` module skeleton

**Files:**
- Create: `/Users/ipotter/code/looprig/mcp/go.mod`, `README.md`, `CLAUDE.md`,
  `Makefile`, `.gitignore`, `docs/` (symlink-free copy note referencing the
  harness design doc path).

**Steps:**
1. `git init /Users/ipotter/code/looprig/mcp` (new repo, default branch main).
2. `go mod init github.com/looprig/mcp`; set `go 1.26.4`.
3. `go get github.com/modelcontextprotocol/go-sdk@v1.6.1`.
4. Copy the Makefile shape from `../storage/Makefile` (fmt, fmt-check, lint,
   vuln, secure, test targets; tool directives for gosec, govulncheck,
   staticcheck as in harness `go.mod` `tool (...)` block).
5. `CLAUDE.md`: copy the harness CLAUDE.md security/testing/code-rules
   sections, then an approved-dependencies section listing
   `github.com/modelcontextprotocol/go-sdk` v1.6.1 (approved 2026-07-16,
   wrapped behind `pkg/client`/`internal/protocol`, must not leak from `pkg/`).
6. `go mod vendor`; commit `chore: bootstrap mcp module`.

**Verify:** `go build ./...` succeeds (empty module OK), `make secure` passes.

---

## Phase 1: `pkg/client` contracts, typed errors, limits (design §Public package responsibilities, §Error model, §Content conversion and limits)

### Task 1.1: Typed error taxonomy

**Files:** Create `pkg/client/errors.go`, `pkg/client/errors_test.go`.

Define a sealed classification enum + error type covering every class in
design §Error model:

```go
type FailureClass uint8

const (
    FailureInvalidConfig FailureClass = iota + 1
    FailureUnsupportedProtocol
    FailureStartupTimeout
    FailureAuthRequired
    FailureAuthDenied
    FailureAuthExpired
    FailureAuthFailed
    FailureTransportClosed
    FailureFraming
    FailureRemoteHTTP
    FailureServerProtocol
    FailureDeadline
    FailureCancelled
    FailureCatalogInvalid
    FailureCatalogStale
    FailureCatalogOverLimit
    FailureNotFound
    FailureToolUnavailable
    FailureToolSchemaChanged
    FailureRemoteToolError
    FailureLimitExceeded
    FailureElicitationDeclined
    FailureElicitationCancelled
    FailureElicitationInvalid
    FailureElicitationTimeout
    FailureSamplingDenied
    FailureSamplingOverBudget
    FailureIndeterminate
    FailureShutdown
)

type Error struct { /* Class FailureClass; Binding Name; Op string; redacted Msg; wrapped err */ }
func (e *Error) Error() string
func (e *Error) Unwrap() error
func ClassOf(err error) (FailureClass, bool)
```

Tests: classification via `errors.As`/`ClassOf`; `Error()` never contains a
string registered as a secret (add a `redact_test.go` helper asserting a
canary token passed as wrapped-error text is not echoed — implement by
bounding/normalizing wrapped text). Commit `feat(client): typed error model`.

### Task 1.2: Names, definitions, limits, timeouts

**Files:** Create `pkg/client/definition.go`, `pkg/client/limits.go`,
`pkg/client/definition_test.go`.

Implement per design §pkg/client:

```go
type Name string                       // validated: non-empty, [a-z0-9_-]{1,64}
type Timeouts struct{ Startup, Request, Elicitation time.Duration }
type Limits struct {
    MaxConcurrentRequests, MaxCatalogPages, MaxCatalogItems int
    MaxFrameBytes, MaxBodyBytes, MaxSchemaBytes, MaxSchemaDepth int
    MaxTextResultBytes, MaxStructuredBytes, MaxBinaryItemBytes, MaxBinaryItems int
    MaxLogMessageBytes, MaxPromptCount, MaxResourceCount int
    MaxSamplingDepth, MaxSamplingConcurrency, MaxSamplingTokens int
}
func DefaultLimits() Limits            // every field non-zero, documented
type ClientCapabilities struct{ Elicitation, Sampling, Roots bool }
type ToolFilter struct{ Allow, Deny []string } // exact raw-name match sets
type Definition struct{ Name Name; Transport TransportFactory; Timeouts Timeouts; Limits Limits; Capabilities ClientCapabilities; ToolFilter ToolFilter }
func (d Definition) Validate() error   // FailureInvalidConfig on any violation
type TransportFactory interface {
    Kind() string
    RedactedOrigin() string
    connect(ctx context.Context, deps transportDeps) (protoConn, error) // unexported seam
}
```

`Definition` immutable after `Validate`; zero limits replaced by defaults into
a private normalized copy. Table-driven validation tests (happy, empty name,
bad chars, nil transport, negative limits). Commit
`feat(client): definitions, limits, validation`.

### Task 1.3: `internal/limits` — bounded readers and JSON depth checks

**Files:** Create `internal/limits/limits.go`, `internal/limits/limits_test.go`,
`internal/limits/fuzz_test.go`.

Helpers: `BoundedReader(r, max)` (typed over-limit error),
`CheckJSONDepth(raw []byte, maxDepth int) error`,
`TruncateText(s string, max int) (string, bool)`. Fuzz `CheckJSONDepth`.
Commit `feat(limits): bounded input helpers`.

### Task 1.4: `internal/protocol` — SDK boundary

**Files:** Create `internal/protocol/protocol.go`, `conv.go`, `conv_test.go`.

This is the ONLY package importing `github.com/modelcontextprotocol/go-sdk/mcp`
outside transports. Read the vendored SDK first. Define our neutral protocol
types and conversions:

- `protocol.ServerIdentity{Name, Version string}`, `protocol.ProtocolVersion string`
- `protocol.ToolSpec{RawName, Title, Description string; InputSchema, OutputSchema json.RawMessage; Annotations map[string]json.RawMessage}`
- `protocol.PromptSpec`, `protocol.ResourceSpec`, `protocol.ResourceTemplateSpec`
- `protocol.Content` union (text/image/audio/embedded-resource/structured) with
  bounded decode from SDK content.
- Conversions `FromSDKTool`, `FromSDKPrompt`, … each enforcing byte/depth
  limits (take a `limits.Limits`-narrow struct, not the whole config).

Fuzz the schema/content converters. Commit
`feat(protocol): SDK boundary types and conversions`.

### Task 1.5: Client lifecycle state machine (`internal/lifecycle`)

**Files:** Create `internal/lifecycle/state.go`, `state_test.go`.

States exactly per design §Lifecycle and readiness: `configured, starting,
authenticating, discovering, ready, degraded, reconnecting, failed, closing,
closed` with a transition table `CanTransition(from, to) bool` and a
`Machine` (mutex-guarded, `Watch(ch)` or callback on change). Reject illegal
transitions with typed error. Full transition-table test. Commit
`feat(lifecycle): binding state machine`.

### Task 1.6: `pkg/client.Client` skeleton + `Handlers`

**Files:** Create `pkg/client/client.go`, `pkg/client/handlers.go`,
`pkg/client/status.go`, tests.

```go
type Handlers struct {
    Elicitation ElicitationHandler // nil => decline capability
    Sampling    SamplingHandler    // nil => capability not advertised
    Roots       RootsProvider      // nil => not advertised
    Log         LogHandler
    Event       EventHandler       // typed client events (Task 5.4 enumerates)
}
type Client struct{ /* private: definition, conn, lifecycle machine, catalog holder */ }
func Connect(ctx context.Context, def Definition, h Handlers) (*Client, error)
func (c *Client) Status() Status     // design §Lifecycle: safe metadata only
func (c *Client) Close(ctx context.Context) error // idempotent
```

`Connect` validates, moves configured→starting, calls the transport factory,
runs MCP initialize via `internal/protocol`, negotiates capabilities
(advertise elicitation/sampling/roots only when the handler is non-nil —
design §Optional sampling, §Roots), then discovery (stubbed until Phase 4;
return a client in `ready` with empty catalog for now behind a build-internal
flag is NOT acceptable — instead land Connect in this task wired to a fake
protoConn in tests, and integration-wire it in Phase 2). Unit tests use an
in-memory fake `protoConn`. Commit `feat(client): Connect lifecycle skeleton`.

### Task 1.7: SDK-leak guard test

**Files:** Create `internal/protocol/leakguard_test.go`.

Test walks `go list -deps -json ./pkg/...` (exec `go` via argv) and fails if
any `pkg/...` package's *exported* API mentions the SDK path, and if any
package outside `internal/protocol` + `pkg/transport/...` imports the SDK.
Commit `test: guard against SDK type leakage`.

---

## Phase 2: stdio transport + live lifecycle (design §pkg/transport/stdio)

### Task 2.1: Fixture MCP server binary

**Files:** Create `internal/mcptest/server.go`, `internal/mcptest/cmd/fixture/main.go`.

Use the SDK's *server* side to build a configurable fixture: flags/env choose
tools (`echo`, `slow`, `fail`, `big`), prompts, resources, instructions,
list-change emission on SIGUSR1 (or stdin command), crash-on-demand, stderr
noise, elicitation-during-initialize mode. Built by tests with
`go build -o` into `t.TempDir()`. Commit `test(mcptest): fixture server`.

### Task 2.2: `pkg/transport/stdio`

**Files:** Create `pkg/transport/stdio/stdio.go`, `stdio_test.go`,
`stdio_integration_test.go` (tagged `integration`).

```go
type Config struct {
    Command string        // absolute path or PATH-resolved executable, never a shell string
    Args    []string
    Dir     string
    Env     EnvAllowlist  // explicit allowlist: []Var{Name, Value} + PassThrough []string
    Launcher ProcessLauncher // optional injection (confinement wrapper); default os/exec
    StderrLimit int
}
func New(cfg Config) (client.TransportFactory, error)
```

Requirements (all tested): argv exec only; allowlisted env built from scratch
(never `os.Environ()` wholesale); stdout reserved for framing; bounded stderr
ring buffer surfaced in failures; process group set where platform allows;
`Close` terminates (SIGTERM→deadline→SIGKILL) and reaps; premature exit
becomes `FailureTransportClosed` with redacted detail; context cancels
startup. Integration tests: initialize+list against the fixture, crash
mid-call, cleanup leaves no orphan (poll `os.FindProcess`/kill(0)). Commit
`feat(stdio): stdio transport`.

### Task 2.3: Wire Connect end-to-end over stdio

**Files:** Modify `pkg/client/client.go`; create
`pkg/client/client_integration_test.go` (tagged).

Full path: Connect → initialize → negotiated version + server identity in
`Status()` → Close idempotent → typed startup-timeout when the fixture stalls.
Commit `feat(client): live stdio lifecycle`.

**PHASE CHECKPOINT:** `make secure && go test -race ./... && go test -tags integration -race ./...`.

---

## Phase 3: Streamable HTTP + `pkg/auth` (design §pkg/transport/streamablehttp, §pkg/auth)

### Task 3.1: `pkg/auth` contracts

**Files:** Create `pkg/auth/auth.go`, `tokenstore.go`, `status.go`, tests.

`TokenStore` exactly as design §pkg/auth (`Load/Store/Delete` with typed
`Key{ServerOrigin, ClientID string}` and `TokenSet{Access, Refresh string; Expiry time.Time; Scopes []string}`);
`BrowserOpener interface{ OpenURL(ctx, url string) error }`;
`Status` (redacted: state enum + expiry, never token text). Redaction tests.
Commit `feat(auth): token store and status contracts`.

### Task 3.2: OAuth flow (discovery, DCR, PKCE, refresh)

**Files:** Create `pkg/auth/oauth.go`, `oauth_test.go`,
`oauth_integration_test.go` (tagged, uses `httptest` authorization server).

Read the vendored SDK's auth/oauth support first and wrap it if usable
(preferred); otherwise implement with stdlib per RFC 8414/7591/7636. Verify:
protected-resource discovery, PKCE S256 only, state validated, refresh
rotation persisted through `TokenStore`, typed `FailureAuth*` mapping. TLS
min 1.2, no `InsecureSkipVerify`. Commit `feat(auth): oauth PKCE flow`.

### Task 3.3: `pkg/transport/streamablehttp`

**Files:** Create `pkg/transport/streamablehttp/streamablehttp.go`, tests +
tagged integration tests (SDK server over `httptest`).

Config: endpoint URL (https required unless loopback), explicit request/
response-header/idle timeouts, `HeaderProvider`, optional `auth` wiring,
body/frame bounds via `internal/limits`, session-ID + resumable-stream support
as negotiated (delegate to SDK transport, wrapped), retry ONLY safe lifecycle
ops (never tool calls — test this), redacted origin. Commit
`feat(streamablehttp): streamable HTTP transport`.

**PHASE CHECKPOINT** as Phase 2.

---

## Phase 4: Discovery, catalogs, calls, content (design §Catalog model, §Capability support, §Content conversion and limits)

### Task 4.1: `internal/catalog` — generations and digests

**Files:** Create `internal/catalog/generation.go`, `digest.go`, tests.

`Generation` holds everything in design §Catalog model (binding identity,
negotiated caps, raw server identity, tools+schema digests, prompts,
resources, templates, bounded instructions, generation number, canonical
digest, warnings). Canonical digest: SHA-256 over a length-delimited,
domain-tagged, deterministically-ordered encoding (mirror the fingerprint
guidance in the versioning design). Digest-stability test with shuffled input
maps. Immutable after `Publish()` (copy-on-build). Commit
`feat(catalog): immutable generations`.

### Task 4.2: Discovery with pagination and bounds

**Files:** Create `internal/catalog/discover.go`, tests, fuzz for page decode.

Implement design §Discovery steps 1–9: paginate every advertised family,
reject duplicate/cyclic cursors, enforce page/item/schema/byte limits, name +
schema validation (valid JSON schema doc ≤ MaxSchemaBytes/Depth), preserve raw
identity, publish candidate, adopt-before-ready. Failure never partially
replaces a prior generation. Commit `feat(catalog): bounded discovery`.

### Task 4.3: Client capability surface

**Files:** Modify `pkg/client/client.go`; create `pkg/client/catalog.go`,
`calls.go`, tests + tagged integration tests against the fixture.

Public, SDK-free API:

```go
func (c *Client) Catalog() Catalog                       // adopted generation view (public mirror of internal/catalog)
func (c *Client) CallTool(ctx, RawToolName, args json.RawMessage, CallOpts) (ToolResult, error)
func (c *Client) GetPrompt(ctx, name string, args map[string]string) (Prompt, error)
func (c *Client) ReadResource(ctx, uri string) (Resource, error)
func (c *Client) Subscribe(ctx, uri string) error        // where negotiated
```

`CallOpts{Progress func(Progress), Deadline}` — progress notifications reset
no deadline by default; cancellation propagates protocol-level cancel.
`ToolResult` uses `internal/content`-converted bounded content
(text/image/audio/embedded/structured + `Unsupported{Kind, Bytes int}`).
Server logging → `Handlers.Log` bounded. Commit
`feat(client): catalog surface and calls`.

### Task 4.4: `internal/content` conversion + fuzz

**Files:** Create `internal/content/convert.go`, tests, fuzz.

MCP content → neutral `client.Content` union with every limit from design
§Content conversion; unknown types → bounded opaque metadata (never panic or
drop silently). Commit `feat(content): bounded conversion`.

**PHASE CHECKPOINT.**

---

## Phase 5: Notifications, candidates, reconnect, compatibility, scheduler (design §Change notifications, §Reconnection, §Compatibility, §Shared-connection concurrency)

### Task 5.1: List-change notifications → candidate generations

**Files:** Modify `internal/catalog`, `pkg/client`; tests incl. tagged
integration (fixture emits list_changed).

Design §Change notifications steps 1–7: stale-mark, coalesce, full refetch,
validate+digest, compare, publish candidate event, expose
`Client.Candidate() (Catalog, bool)` + `Client.Adopt(generation uint64) error`
(caller — the harness adapter — decides *when*; the client only enforces that
adoption targets a validated candidate). Failed refresh keeps prior adopted
generation, status degraded, bounded retry. Commit
`feat(catalog): candidate generations`.

### Task 5.2: Request scheduler

**Files:** Create `internal/lifecycle/scheduler.go`, tests (race-focused).

Per design §Shared-connection concurrency: serialize tool calls by default;
`Definition.Limits.MaxConcurrentRequests` bounds everything; opt-in bounded
parallelism flag on Definition (`AllowParallelCalls bool` — add to Task 1.2
type if missing); lifecycle/auth/refresh ops serialized; per-request cancel
independence; shutdown rejects new work then cancels. Commit
`feat(lifecycle): per-binding scheduler`.

### Task 5.3: Reconnect

**Files:** Create `pkg/client/reconnect.go`, tests + tagged integration
(fixture crash → auto-reconnect → new generation candidate).

Design §Reconnection: classify-transient gate, bounded retries
(count/delay/total), new logical connection + full rediscovery, prior
generations stay adopted, in-flight calls at disconnect →
`FailureIndeterminate`. Commit `feat(client): bounded reconnect`.

### Task 5.4: Typed client events + compatibility profiles

**Files:** Create `pkg/client/events.go`, `pkg/client/compat.go`, tests.

Events per design §Events and observability (startup, readiness, auth state,
connection lost/restored, catalog stale/candidate/rejected/refreshed,
progress, server log, elicitation lifecycle, shutdown) delivered via
`Handlers.Event`; every payload redaction-tested. Compatibility profiles per
design §Compatibility: named, versioned profile values on `Definition`
implementing only the safe tolerances listed; every applied tolerance recorded
in generation warnings + catalog identity. Commit
`feat(client): events and compatibility profiles`.

**PHASE CHECKPOINT** — full standalone client done. Tag nothing yet.

---

## Phase 6: Harness seams (in `../harness`; design §Elicitation, §Permissions, §Harness safe-boundary integration)

> Work in the harness repo. Study before writing: `pkg/gate/payload.go`
> (sealed payload + `{kind,data}` codec), `pkg/gate/prompt.go`,
> `pkg/tool/permission_request.go` (sealed), `pkg/command/loop_change.go` +
> its loopruntime handling (`internal/loopruntime`, `internal/sessionruntime`)
> — the SetLoopMode next-turn-boundary path is the template for Task 6.3.

### Task 6.1: Form and open-URL gate payloads

**Files:** Modify `pkg/gate/gate.go` (kinds), `pkg/gate/payload.go`; create
`pkg/gate/form_payload_test.go`, `pkg/gate/openurl_payload_test.go`; extend
codec round-trip tables.

- `KindForm = "harness.form"`, `KindOpenURL = "harness.open_url"`.
- `FormPayload{Title, Body string; Schema PromptSchema}` (reuse existing field
  model; add `FieldKindConfirm` if absent for confirmation-only forms).
- `OpenURLPayload{DisplayOrigin string; URL string; RequiresCompletion bool}`
  — `DisplayOrigin` is the durable, journal-safe origin; full `URL` is the
  ephemeral action target and MUST be excluded from durable records: follow
  how existing payloads marshal, and mark these gates `Restorable: false` at
  open time (adapter responsibility, but add a validation hook rejecting a
  restorable open-url gate).
- Response side: form answers as `map[string]string` validated against the
  schema (bounded count/length); accept/decline/cancel per existing
  `ResponsePolicy` machinery.

Fail-closed unknown-kind behavior must still hold (extend existing codec
tests). Commit in harness: `feat(gate): form and open-url payloads`.

### Task 6.2: External permission-request seam

**Files:** Modify `pkg/tool/permission_request.go`; create
`pkg/tool/external_request_test.go`.

```go
// NewExternalRequest builds a PermissionRequest for capabilities implemented
// outside this module (e.g. MCP tools). description must already be redacted;
// it is bounded and normalized here.
func NewExternalRequest(toolName, description string, scopes []ApprovalScope) (PermissionRequest, error)
```

Returns a package-private impl satisfying the sealed interface. Validation:
non-empty tool name, description bounded (reuse existing bounds), scopes
non-empty subset of valid scopes, defensive-copied. Tests: session/workspace
scopes now reachable by an external caller; empty scopes error. Commit
`feat(tool): external permission requests`.

### Task 6.3: External-toolset replacement at the turn boundary

The single riskiest task. Mirror the `SetLoopMode`/`ChangeLoopInference`
pattern end to end.

**Files (expect):** Modify `pkg/command/loop_change.go` (or new
`pkg/command/loop_tools.go`), `pkg/event/event.go` + `validate.go` +
`marshal_test.go`, `pkg/loop/definition.go` (external-slot support),
`internal/loopruntime`, `internal/sessionruntime`; create tests alongside
each.

Contract:

```go
// pkg/command
type ReplaceLoopExternalTools struct {
    LoopID      uuid.UUID
    Source      string            // e.g. "mcp"; namespaces the slot
    Generation  string            // opaque identity digest for the durable record
    Definitions []tool.Definition // live factories; NOT serialized
}

// pkg/event (Enduring, loop-scoped)
type LoopExternalToolsetChanged struct {
    Source     string
    Generation string
    Tools      []ExternalToolIdentity // {Name, SchemaDigest string}
}
```

Semantics (design §Harness safe-boundary integration):
- A `loop.Definition` gains an *external tool slot* per `Source`: bound tools =
  immutable declared tools + current external generation's tools.
- The command validates against the live loop, stashes the pending
  replacement, and applies it atomically at the next turn boundary (same hook
  where mode changes apply); a turn in flight keeps its snapshot.
- Never a partial swap: build+bind ALL replacement `InvokableTool`s first;
  on any Build error the prior generation stays and a typed failure is
  reported.
- The durable event records identity only (names + schema digests +
  generation), never factories or schemas-with-secrets.
- Restore: the slot starts empty; the composing application re-installs
  external tools after restore (matches design §Session restore — connections
  are live resources).

Tests: apply-at-idle (drive a loop to LoopIdle, assert next turn sees new
tools), mid-turn snapshot retention, atomicity on Build failure, durable
event round-trip, restore-with-empty-slot. Commit
`feat(loop): external toolset replacement at turn boundary`.

### Task 6.4: Harness phase checkpoint

`make fmt && make secure && go test -race ./...` in harness; fix fallout
(event registry tables, serve/SSE marshaling of new gate kinds). Commit any
residue as `test(harness): cover external seams`.

---

## Phase 7: `mcp/pkg/harness` adapter (design §pkg/harness, §Binding model, §Tool identity and calls, §Permissions, §Elicitation)

> Back in the mcp repo. Add `require github.com/looprig/harness` + `replace
> github.com/looprig/harness => ../harness` (and transitive local replaces for
> `core`/`inference`/`storage` mirroring harness's own go.mod), `go mod vendor`.
> The adapter package name is `mcpharness` (dir `pkg/harness`).

### Task 7.1: Bindings, scopes, selectors

**Files:** Create `pkg/harness/binding.go`, `selector.go`, tests.

`Scope`, `Binding` exactly per the (fixed) design §Binding model
(`Loop uuid.UUID`); `LoopSelector` with `AllLoops()`, `Loops(ids...)`,
`Named(names...)` constructors and a `Permits(loopID uuid.UUID, name string) bool`;
`Binding.Validate()` (session scope ⇒ no Loop, loop scope ⇒ Loop set and no
selector). Commit `feat(harness-adapter): bindings and selectors`.

### Task 7.2: Manager — client sets, concurrent startup, required/optional

**Files:** Create `pkg/harness/manager.go`, `manager_test.go`, tagged
integration test with two fixture servers.

```go
type Manager struct{ /* private */ }
func NewManager(bindings []Binding, deps Deps) (*Manager, error)
// Deps: gate opener, hub event publisher, permission wiring, token store,
// browser opener, clock — all narrow interfaces defined HERE (consumer side).
func (m *Manager) Start(ctx context.Context) error   // concurrent; returns after required ready, aggregating all required failures
func (m *Manager) SessionTools(loopID uuid.UUID, loopName string) []tool.Definition
func (m *Manager) LoopTools(loopID uuid.UUID) []tool.Definition
func (m *Manager) Status() []BindingStatus
func (m *Manager) Reconfigure(ctx context.Context, ops []BindingOp) error // add/remove/enable/disable/replace per design §Binding reconfiguration
func (m *Manager) Close(ctx context.Context) error
```

Startup per design §Concurrent startup + §Required and optional servers
(handle published before required-wait; aggregated failure report; optional
failure degrades only that binding). Shutdown ordering per design §Shutdown.
Commit `feat(harness-adapter): manager lifecycle`.

### Task 7.3: Tool adaptation — names, calls, unavailability

**Files:** Create `pkg/harness/tools.go`, `names.go`, tests (name fuzz too).

- Qualified name `mcp__<binding>__<raw>` with deterministic sanitization,
  provider length bound (64), digest suffix on truncation/collision, reverse
  map retained; NEVER route by parsing display names (closure carries binding
  ID + raw name + generation + schema digest).
- Build via `tool.NewBundleDefinition`: one bundle per (binding, adopted
  generation) producing `[]tool.InvokableTool`.
- Call path implements design §Calling a tool steps 1–8 including
  `ToolUnavailable`/`ToolSchemaChanged` structured results (bounded
  `tool.ToolResult{IsError: true}` texts, session stays healthy) and
  degraded-marking on transport failures.
- `tool.PermissionPrompter` implemented with `tool.NewExternalRequest`
  (Task 6.2), identity `mcp:<binding>:<raw-tool>`, redacted summary.
- MCP content → `content.Block` via a translation layer over
  `client.Content` (text/image/audio/document; structured → bounded JSON
  text block; unsupported → labeled placeholder text). Large results →
  bounded summary per design §Content conversion.

Commit `feat(harness-adapter): tool adaptation and calls`.

### Task 7.4: Catalog adoption at LoopIdle

**Files:** Create `pkg/harness/adoption.go`, tests + tagged integration.

Subscribe to hub `event.LoopIdle`; when a binding holds a validated candidate,
for each permitted Loop at ITS idle boundary: build snapshot
`[]tool.Definition`, issue `command.ReplaceLoopExternalTools` (Task 6.3),
`client.Adopt` on success; per-Loop independent adoption (design §Catalog
model diagram); failed replacement leaves prior generation + reports. Commit
`feat(harness-adapter): safe-boundary adoption`.

### Task 7.5: Elicitation → gates; events → hub

**Files:** Create `pkg/harness/elicitation.go`, `events.go`, tests.

- Elicitation handler translates MCP form schemas → `gate.FormPayload`
  (unsupported/unsafe constructs → classified decline; credential-soliciting
  fields rejected per design §Elicitation), URL elicitation →
  `gate.OpenURLPayload` (non-restorable, full URL kept out of durable
  records); pending elicitation cancellable; shutdown resolves cancelled;
  late/duplicate responses rejected.
- Client events (Task 5.4) → protocol-neutral harness events published via
  the hub publisher dep; bounded + redacted; includes binding status changes
  for TUI consumption. If `pkg/event` lacks a suitable generic integration
  event, add ONE in harness (`event.IntegrationStatus{Source, Name, State,
  Detail}` — loop-scoped or session-scoped, Ephemeral) as a small harness
  commit first.

Commit `feat(harness-adapter): elicitation and event bridging`.

### Task 7.6: Adapter integration suite

**Files:** Create `pkg/harness/integration_test.go` (tagged).

Cover the design §Harness integration tests list end-to-end with fixture
servers + a real in-process rig/session: mixed scopes, selectors, delegate
non-inheritance, required/optional startup, permission gate flow, form + URL
elicitation, mid-turn snapshot retention, idle adoption, removed/changed tool
structured results, refresh failure retention, one-binding reconnect
isolation, session+loop shutdown cleanup. Commit
`test(harness-adapter): integration suite`.

**PHASE CHECKPOINT** in mcp repo.

---

## Phase 8: TUI rendering (in `../tui`)

### Task 8.1: Render form and open-URL gates

**Files:** Study `tui/components` gate rendering (grep `KindPermission`,
`AskUserPayload`); extend for `KindForm` (field editor over
`gate.PromptSchema`: text/select/multi-select/confirm, required-field
validation, accept/decline/cancel) and `KindOpenURL` (show display origin,
offer open-in-browser + explicit "completed" confirmation; never print the
full URL into scrollback if the payload marks it sensitive — display origin
only, with keybound "open" action).

Update tui `go.mod` replace to local harness; vendor. Tests follow existing
component test conventions (render-driving tests). Commit in tui:
`feat(gates): render form and open-url gates`.

### Task 8.2: MCP/integration status surface

**Files:** tui session status area component.

Render `IntegrationStatus` events (binding name, state, redacted origin,
failure class) in the session status/footer area per existing status
patterns. Commit `feat(status): integration binding status`.

---

## Phase 9: Restore + configuration identity (degraded; design §Session restore and configuration identity)

### Task 9.1: Secret-free binding/catalog identity

**Files (mcp):** Create `pkg/harness/identity.go`, tests.

`Manager.ConfigIdentity() []BindingIdentity` — per binding: name, scope,
selector identity, transport kind + redacted origin, required posture,
capability/filter/limits/compat digests, negotiated server identity, adopted
catalog digest + tool schema digests (design §Session restore manifest list).
Deterministic encoding + digest (same canonical scheme as Task 4.1). Commit
`feat(harness-adapter): configuration identity`.

### Task 9.2: Fingerprint integration + restore drift report

**Files (harness):** extend `event.ConfigFingerprint` with one field:
`ExternalCapabilityRev string` (digest over application-supplied external
capability identity; empty = none, preserving old fingerprints' equality).
**Files (mcp):** Manager exposes the digest; restore flow: application
recreates the Manager, compares its current identity digest against the
restored fingerprint field, and reports drift through the existing
`WithAllowConfigMismatch` decision (typed `*ConfigMismatchError` today —
do NOT build manifests/epochs; that is the deferred versioning design).
Removed-tool calls after restore already yield `ToolUnavailable` (Task 7.3) —
add a restore-focused integration test proving an old journal restores
cleanly under a changed catalog. Commits in each repo.

---

## Phase 10: Optional sampling + legacy SSE (design §Optional sampling, §pkg/transport/sse)

### Task 10.1: Sampling handler plumbing

**Files (mcp):** `pkg/client/sampling.go`, adapter `pkg/harness/sampling.go`,
tests.

`SamplingHandler` invoked only when installed; capability not advertised when
nil (test both); depth/concurrency/token caps from Limits enforced client-side;
adapter routes through an application-supplied policy dep (model selection,
budget, permission) and audits outcomes sans secrets; sampling NEVER receives
session controller or tool registry (API shape makes it impossible — handler
receives only neutral request/response types). Commit
`feat(client): gated sampling support`.

### Task 10.2: `pkg/transport/sse`

**Files:** `pkg/transport/sse/sse.go`, tests + tagged integration.

Wrap the SDK SSE client transport; explicitly documented compatibility-only;
same validation/limits/auth/cancellation as streamablehttp (share internals
where clean). Commit `feat(sse): legacy SSE transport`.

### Task 10.3: Final acceptance sweep

Walk design §Acceptance criteria item by item; for each, name the test that
proves it (add any missing); run in all three repos:
`make fmt && make secure && go test -race ./... && go test -tags integration -race ./...`.
Write `mcp/README.md` (module overview, composition example, transport matrix,
security posture). Final commits.

---

## Execution notes

- **Order:** Phases 0–5 are strictly sequential in mcp. Phase 6 (harness) can
  start any time after Phase 0 and MUST finish before Phase 7. Phase 8 needs
  6; 9 needs 7; 10 last.
- **Dependency policy:** ONLY the go-sdk is approved for mcp. Anything else
  (including test libs) requires stopping and asking the user. Harness and
  tui gain NO new dependencies.
- **Linux-only behaviors** (process-group kill semantics): keep tests
  portable; guard platform assertions; flag anything requiring the Linux box.
- **Every commit message**: conventional prefix, no co-author trailer.
