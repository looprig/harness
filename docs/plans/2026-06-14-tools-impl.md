# Tools Subsystem — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build the agent tools subsystem — 11 agent-agnostic tools, a fail-secure 7-stage permission model with out-of-repo persisted approvals, the agentic tool-use turn loop, and two agent manifests (`personal-assistant` subset, new `agents/coding` full set).

**Architecture:** Contracts in `internal/tool/` (imports only `content`); implementations in root `tools/`; the loop (`internal/agent/loop`) gains a runner, a `ToolSet` consumer surface, tool control commands/events, and an agentic `runTurn`. The loop never imports `tools/`; concrete `*tools.PermissionChecker` + the tool registry are injected via `loop.Config.Tools` at the composition root. Streaming tool calls flow through a new `content.ToolUseChunk`.

**Tech Stack:** Go, stdlib-first. New deps (already approved in CLAUDE.md): `golang.org/x/net/html` + `golang.org/x/net/idna` (WebSearch / Fetch host matching).

**Design reference:** `docs/plans/2026-06-14-tools-design.md` (cited below as **§N**). **Read it in full first** — this plan is the execution choreography; the design doc is the authoritative spec for every struct, default, and behavior. Do not re-derive specs; implement what §N states.

**Prerequisite:** `docs/plans/2026-06-14-identity-correlation-impl.md` must be complete (command `Header`, `EventEnvelope` correlation fields, loop `TurnID`).

**Conventions (CLAUDE.md):** write the interface first; table-driven tests covering happy/boundary/error/edge; `go test -race ./...`; typed errors (concrete structs, `errors.As`-able); fuzz targets for parsers; integration tests tagged `//go:build integration` in `*_integration_test.go`; `make secure` before each phase-closing commit; `CGO_ENABLED=0 go build -trimpath`.

**TDD rhythm (applies to every task below):** (1) write the failing table-driven test; (2) run it, confirm it fails for the right reason; (3) write the minimal implementation; (4) run `-race`, confirm pass; (5) commit. Commit messages: `feat(<pkg>): …` / `test(<pkg>): …`.

---

## Phase 1 — `internal/tool/` contracts (imports only `content`)

> Full target source: design §3a. Build the interface set first; no `tools/` package yet.

### Task 1.1: core contracts

**Files:** Create `internal/tool/tool.go`; Test `internal/tool/tool_test.go`.

- Define `ToolInfo{Name, Desc string; Schema json.RawMessage}`, `BaseTool`, `InvokableTool`, `ToolResult{Content []content.Block}`, `func TextResult(s string) *ToolResult`. **No `Terminate` field** (§3a, Out of scope).
- Test: `TextResult("x")` yields one `*content.TextBlock{Text:"x"}`; a fake tool satisfies `InvokableTool`.
- Commit.

### Task 1.2: optional capability interfaces

**Files:** Append to `internal/tool/tool.go`; test in `tool_test.go`.

- Define `Sequential`, `PermissionPrompter`, `Auditable`, `WriteTarget`, `ToolMiddleware`, `ToolExecuteFunc` exactly as §3a. (No `StreamableTool`.)
- Test: a fake type implementing each is assignable to the interface; a tool that implements none still satisfies `InvokableTool`.
- Commit.

### Task 1.3: `PermissionRequest` (sealed) + `ApprovalScope` + concrete request types

**Files:** Create `internal/tool/permission_request.go`; Test `internal/tool/permission_request_test.go`.

- `PermissionRequest interface { permissionRequest(); ToolName() string; Description() string; AllowedScopes() []ApprovalScope }` and `ApprovalScope` (`ScopeOnce/ScopeSession/ScopeWorkspace`).
- **All concrete types live here** (§3a — sealing requires it): `FileWriteRequest`, `BashRequest`, `FetchRequest`, `WebSearchRequest`, `UnknownRequest{Tool, Summary string}`. Each implements `permissionRequest()` + the three methods.
- Test (table-driven): each type's `ToolName()`/`Description()`/`AllowedScopes()`; `UnknownRequest` uses `Summary` (never raw args).
- Commit.

**Phase 1 close:** `go test -race ./internal/tool/`; `make secure`.

---

## Phase 2 — `internal/hashcache/` (generic, domain-free)

> Target: design §3d. Generic `Cache[T any]` is intentional (precedent: `StreamReader[T any]`); see §3d note.

### Task 2.1: `Cache[T]`

**Files:** Create `internal/hashcache/cache.go`; Test `internal/hashcache/cache_test.go`.

- `type Cache[T any] struct { mu sync.Mutex; sum [32]byte; val T; ok bool; parse func([]byte)(T,error) }`, `New[T](parse)`, `(*Cache[T]).Load(content []byte) (T, error)` — `sha256.Sum256(content)`; if equal to cached `sum` and `ok`, return cached `val`; else call `parse`, store, return.
- Test (table-driven + a parse-call counter): first `Load` parses; second `Load` of identical bytes does **not** parse (counter unchanged); changed bytes re-parse; a `parse` error propagates and is not cached. Add a concurrent-`Load` test under `-race`.
- Commit.

**Phase 2 close:** `go test -race ./internal/hashcache/`; `make secure`.

---

## Phase 3 — `tools/permission.go` (the PermissionChecker)

> The largest single file. Target: design §3b (`Effect`, `ToolPolicy`, `PermissionGate`, `ReadGuard`) and §3c (stages, `HardDenyRules`, `containedPath`, `Match` semantics, `Grant`, fs hardening, malformed handling). The `loop`-side types (`Effect`, `ToolPolicy`, `PermissionGate`, `ReadGuard`, `ToolSet`) are created in Phase 5 (`internal/agent/loop/deps.go`); **Phase 3 depends on them**, so do Phase 5 Task 5.1 (deps.go types) first, then return here. (Marked as a cross-dependency; see Phase 5.)

### Task 3.1: `Effect` JSON (string) round-trip

**Files:** `internal/agent/loop/deps.go` already defines `Effect` (Phase 5 Task 5.1). Add `MarshalJSON`/`UnmarshalJSON` here or in deps.go; Test in `deps_test.go`.

- `Effect` (un)marshals `"allow"/"ask"/"deny"` (§3b note). Unknown string → error (fail-secure).
- Test: round-trip each; unknown `"yolo"` → error; numeric input → error.
- Commit.

### Task 3.2: `containedPath` (symlink-aware) + fuzz

**Files:** Create `tools/contained_path.go`; Test `tools/contained_path_test.go` + `tools/contained_path_fuzz_test.go`.

- Implement per §3c steps 1–5: `EvalSymlinks(root)` once; `Clean`+`Join` input under root; `EvalSymlinks` full path (existing) or deepest existing parent (new write target); `filepath.Rel` containment check; the `O_NOFOLLOW` open is the *tool's* concern (note for Phase 6).
- Test table: in-workspace path OK; `..` escape rejected; symlink-inside-workspace→`/tmp` rejected; non-existent write target under a real parent OK; absolute path outside rejected.
- `FuzzContainedPath`: rewrite every fuzz path under a per-run `t.TempDir()` (never touch host paths — mirror the TUI fuzz discipline in `docs/plans/2026-06-13-tui-design.md`); assert no panic and the result is always inside the root or a typed error.
- Commit.

### Task 3.3: matchers (path glob / exact Bash / Fetch `METHOD scheme://host`)

**Files:** Create `tools/match.go`; Test `tools/match_test.go`.

- Per-tool match per §3c "Match semantics": file glob over the workspace-relative canonical path (reuse the Phase 6 `**` matcher — extract a shared `matchGlob(pattern, relPath)` helper now); `Bash` exact-normalized (trim + collapse whitespace) unless `prefix`; `Fetch` `METHOD scheme://host[path]` — `strings.ToLower(u.Hostname())`, `idna.Lookup.ToASCII`, exact-or-`.suffix` host, scheme default `https`, method exact, optional path-prefix.
- Test table per §5e "Match semantics": `example.com` grant rejects `example.com.evil.com`, accepts `.example.com`, strips port, requires https, rejects POST for a GET grant; Bash exact rejects a suffix unless prefix; path glob rejects `..`.
- Commit.

### Task 3.4: `HardDenyRules` defaults + `ApprovalRecord`/`ApprovalsFile` + `PermissionPolicy`

**Files:** `tools/permission.go`; Test `tools/permission_test.go`.

- Structs per §3c. `DeniedReadPaths`/`DeniedWritePaths`/`DeniedBashPrefixes` defaults incl. **`**/.urvi/**` + `~/.urvi/**` in write-deny** and `~/.urvi/**` in read-deny (§3c F2). `ApprovalRecord{Tool, Match, Prefix bool, Effect}`. A `DefaultHardDeny()` constructor.
- Test: defaults contain the secret globs + the `.urvi` policy-store entries.
- Commit.

### Task 3.5: `PermissionChecker.Check` — the 7 stages (fail-secure) + `ReadGuard`

**Files:** `tools/permission.go`; Test `tools/permission_test.go`.

- `NewPermissionChecker(policy)`; satisfies `loop.PermissionGate`. Implement the **fail-secure order** (§3c): 1 Containment → 2 HardDeny → 3 EffectChecker → 4 HardApprove → 5 PersistedApprovals → 6 SessionPolicies → 7 default `EffectAsk`. Persisted approvals read both files via two `hashcache.Cache[ApprovalsFile]`; **deny beats allow** across both; malformed file → stage behaves empty (fail open to `EffectAsk`, never `AutoApprove`) + warn; bad record skipped (§3c F4). Implement `DeniedRead`/`MaxReadBytes` so `*PermissionChecker` satisfies `loop.ReadGuard`.
- Test table (the security core — be exhaustive): HardDeny path beats an `EffectChecker`/`HardApprove "*"`/persisted allow; containment escape → Deny; deny-beats-allow across ws+user files; session policy add via `Grant`; malformed file → `EffectAsk`; default → `EffectAsk`.
- Commit.

### Task 3.6: `Grant` (persist) + out-of-repo store + fs hardening

**Files:** `tools/permission.go` (+ a small `tools/store.go` for path resolution); Test `tools/permission_test.go` (+ integration test Task 3.7).

- `Grant(ctx, toolName, argsJSON, scope)`: `ScopeSession` appends an in-memory `ToolPolicy` under `mu`; `ScopeWorkspace` writes an `ApprovalRecord` to `~/.urvi/workspaces/<sha256(resolvedRoot)>/approvals.json` — **never the repo** (§3c F1). Derive the record's `Match` via the Task 3.3 matchers from `toolName`+`argsJSON` (Bash → exact). fs hardening (§3c F4): dirs `0700`, file `0600`, temp `O_CREATE|O_EXCL|O_WRONLY|O_NOFOLLOW`@`0600` then `Rename`; reject symlinked policy-path components; loader rejects group/world-writable files.
- Test (unit, `t.TempDir()` as a fake home via an injectable home-dir func): `Grant(ScopeWorkspace)` writes under `workspaces/<hash>/`, not the workspace; the next `Check` sees it; an in-repo `<ws>/.urvi/approvals.json` is **ignored**.
- Commit.

### Task 3.7: permission integration tests

**Files:** Create `tools/permission_integration_test.go` (`//go:build integration`).

- Real-filesystem: symlinked policy dir rejected; world-writable approvals file rejected; deny-beats-allow with two real files. Run: `go test -tags integration -race ./tools/`.
- Commit. **Phase 3 close:** `make secure`.

---

## Phase 4 — streaming tool calls (`content` + provider)

### Task 4.1: `content.ToolUseChunk`

**Files:** Modify `internal/content/chunk.go`; Test `internal/content/chunk_test.go`.

- Add `ToolUseChunk{Index int; ID, Name, InputJSON string}` with `func (*ToolUseChunk) isChunk()` (§2b). It must satisfy the sealed `Chunk` interface.
- Test: a `*ToolUseChunk` is assignable to `content.Chunk`.
- Commit.

### Task 4.2: OpenAI streaming tool-call decode

**Files:** Modify `internal/llm/openaiapi/types.go` (add `sseToolCallDelta`, change `sseMessageDelta.ToolCalls` to `[]sseToolCallDelta`); Modify `internal/llm/openaiapi/stream.go:14` (`NewStream` — emit `content.ToolUseChunk` per delta); Test `internal/llm/openaiapi/stream_test.go`.

- Per §2b "Provider-layer change": `sseToolCallDelta{Index int; ID string; Function{Name, Arguments string}}`; in the `NewStream` loop, after the text/thinking checks, if `len(delta.ToolCalls) > 0` return a `*content.ToolUseChunk{Index, ID, Name, InputJSON: Arguments}` for the first (loop emits one chunk per `Next()`; carry remaining deltas across calls or emit one per SSE line — keep it simple: one chunk per non-empty delta entry).
- Test: feed an SSE script with a tool-call delta sequence (id+name on first, argument fragments after); assert the decoder yields `*content.ToolUseChunk`s with the right `Index`/fragments.
- Commit. **Phase 4 close:** `go test -race ./internal/content/ ./internal/llm/openaiapi/`; `make secure`.

---

## Phase 5 — loop integration (the heart)

> Targets: §2a–§2e, §3b, §5a, §5b, §5c. **Do Task 5.1 before Phase 3.** This is the most intricate phase — go slow, one sub-step per commit.

### Task 5.1: `deps.go` consumer types (prerequisite for Phase 3)

**Files:** Create `internal/agent/loop/deps.go`; Test `internal/agent/loop/deps_test.go`.

- Define `Effect` (+ string (un)marshal, or in Task 3.1), `ToolPolicy{Tool string; Effect Effect; Match []string}`, `PermissionGate{Check; Grant}`, `ReadGuard{DeniedRead; MaxReadBytes}`, `ToolSet{Permission; Registry; Middlewares; MaxToolIterations; MaxToolCallsPerTurn; MaxParallelToolCalls}` (§3b). Add `Config.Tools ToolSet` to `config.go`; `loop.New` applies defaults (25/100/8) when zero.
- Test: zero-value `ToolSet` → `New` applies the three defaults.
- Commit.

### Task 5.2: control commands

**Files:** Create `internal/agent/loop/command/approve.go`, `deny.go`, `provide_user_input.go`; Test alongside.

- `ApproveToolCall{Header; CallID uuid.UUID; Scope tool.ApprovalScope}`, `DenyToolCall{Header; CallID}`, `ProvideUserInput{Header; CallID; Answer}`; each `isCommand()` + a shared `GateCallID() uuid.UUID` accessor (§2c). No `Ack`.
- Test: each satisfies `Command`; `GateCallID()` returns `CallID`.
- Commit.

### Task 5.3: events + `Redactable` sink projection

**Files:** Create `internal/agent/loop/event/tool.go`; Modify the loop sink-publish path; Test `event/tool_test.go` + `loop_test.go`.

- `PermissionRequested{CallID; Request tool.PermissionRequest}`, `UserInputRequested{CallID; Question; Choices}`, `ToolCallStarted{CallID; ToolName, Summary string}`, `ToolCallCompleted{CallID; IsError bool; ResultPreview string}` — each `isEvent()` (§5b). `ResultPreview` is the capped tool output for the TUI; **stream-only** (dropped on the sink path).
- `Redactable interface { SinkProjection() Event }` (§5b): `PermissionRequested`→`{CallID,ToolName}`; `UserInputRequested`→`{CallID,len(Choices)}`; `TokenDelta` with `*content.ToolUseChunk`→drop `InputJSON`; `TurnDone`→redact every `ToolUseBlock.Input` to `{}`; `ToolCallCompleted`→drop `ResultPreview`. The loop's sink path calls `SinkProjection()` before enveloping; the per-turn stream stays full-fidelity.
- Test: a recording sink receives **no** `Description`/`Question`/raw-args; the stream reader receives full payloads. Set the envelope `CallID` from `PermissionRequested`/`UserInputRequested`/`ToolCall*`.
- Commit.

### Task 5.4: `EmitFromContext` + `RequestUserInput` + gate plumbing types

**Files:** Create `internal/agent/loop/gate.go`; Test `gate_test.go`.

- `gateKind` (`gatePermission`/`gateUserInput`), `gate{reply chan<- command.Command; kind gateKind}`, `gateRegistration{callID uuid.UUID; reply chan<- command.Command; kind gateKind; ack chan<- struct{}}` (§2c). Package-private ctx keys for: emit func, `CallID`, and the `gateReg chan<- gateRegistration` handle.
- `func EmitFromContext(ctx) (func(event.Event), bool)`; `func RequestUserInput(ctx, question string, choices []string) (string, error)` — reads emit + `CallID` + `gateReg` from ctx, registers a `gateUserInput` gate (ctx-aware send + ack), emits `UserInputRequested`, blocks on `<-reply`/`<-ctx.Done()`, returns the answer (§2e).
- Test: a fake actor that acks registrations and replies; `RequestUserInput` returns the delivered answer; ctx-cancel mid-register returns `ctx.Err()` (no wedge).
- Commit.

### Task 5.5: `loopState` gate routing in `listen`

**Files:** Modify `internal/agent/loop/loop.go` (`loopState.pendingGates map[uuid.UUID]gate`; `gateReg` channel the actor selects on; `listen` handles `gateRegistration` and routes `Approve/Deny/ProvideUserInput` by `CallID`+kind); Test `loop_test.go`.

- Per §2c `listen` snippet: install on registration + `close(ack)`; on a control command, deliver once to the matching gate (`accepts(kind, cmd)`) and `delete`; else drop. Clear `pendingGates` on turn end.
- Test (the gate concurrency core): register two `gateUserInput` gates (distinct `CallID`), deliver two `ProvideUserInput` — each reaches its own reply; a wrong-`CallID` or wrong-kind command is dropped; a duplicate after delivery is dropped; no valid reply is ever dropped by a preceding stale command.
- Commit.

### Task 5.6: the runner — `RunBatch`

**Files:** Create `internal/agent/loop/runner.go`; Test `runner_test.go`.

- `RunBatch(ctx, toolUseBlocks, ts ToolSet, gateReg chan<- gateRegistration, emit func(event.Event)) []result` per §2d: resolve each call's `Name` in `ts.Registry` (**unknown → tool-result error**, no panic); **sequential permission resolution** first (open a `gatePermission` gate on `EffectAsk` via `gateReg`, validate `CallID`, on approval `ts.Permission.Grant` for non-`ScopeOnce`, `Grant` error → proceed + warn); then split serial (`Sequential()==true`) / parallel; **parallel bounded by a semaphore of width `ts.MaxParallelToolCalls`**; **group same-`WriteTarget` calls serially** (`WriteTarget` err → invalid-args result); wrap each `InvokableRun` in the `Middlewares` chain; `defer/recover`→`"error: tool panicked: …"`; emit **all** `ToolCallStarted{Summary}` (via `Auditable`) for the approved batch **before executing any call** (so every start precedes every completion — the TUI groups a batch race-free), then `ToolCallCompleted{IsError, ResultPreview}` per call as it finishes (preview = `flattenToText(result.Content)` capped ~2 KiB/20 lines, truncation-marked); inject `CallID`+emit+`gateReg` into each call's ctx.
- Test (fake tools): unknown tool → error result, loop continues; a `Sequential` fake drains before the parallel batch; N→N+1 session-grant visibility; `MaxParallelToolCalls=2` caps peak concurrency (a fake that records concurrent count); two same-`WriteTarget` calls serialize; a panicking fake → error result not a crash; `ToolCallStarted.Summary` carries no secret.
- Commit.

### Task 5.7: `runTurn` becomes the agentic loop

**Files:** Modify `internal/agent/loop/turn.go:21`; Test `turn_test.go` (fake `llm.LLM`).

- Rewrite per the §2a pseudocode exactly: `base := len(msgs)` snapshot; loop {stream (accumulate `TextChunk`/`ThinkingChunk`→`TokenDelta`; fold `ToolUseChunk` by `Index`); **validate** each assembled `ToolUseBlock` (ID/Name non-empty, `json.Valid(Input)`; invalid → sanitize `Input` to `{}` + synthetic tool-result error, §2b); assemble `AIMessage`, append; **no tool calls → `TurnDone`** (always wins); `iters++`/`calls+=`; **cap check → `TurnFailed{ToolLimitError}` + `msgs=msgs[:base]`**; `RunBatch`; append `ToolMessage{Blocks: flattenToText(result.Content)}`}. On ctx-cancel/interrupt anywhere → `msgs=msgs[:base]` + `TurnInterrupted`. Add `flattenToText` (text passes; non-text → `[unsupported …]` placeholder, §2a).
- Add `ToolLimitError{Iterations, MaxIterations, Calls, MaxCalls}` to `internal/agent/loop/event/errors.go`.
- Test (fake `llm.LLM` scripting tool calls): one tool round-trip completes with `TurnDone`; a final text-only response on the limit iteration → `TurnDone` (not failed); a model that always calls tools → `TurnFailed{ToolLimitError}` with `msgs` back at `base` (assert the next encode has no unpaired `tool_use`); malformed tool args → `{}` + tool-result error; `ToolUseChunk` fragments fold by `Index`; interrupt mid-loop → whole-turn rollback.
- Commit.

### Task 5.8: `session` + `tui.Agent` command methods

**Files:** Modify `internal/agent/session/agent.go` (add `Approve(ctx, callID, scope) error`, `Deny(ctx, callID) error`, `ProvideUserInput(ctx, callID, answer) error` — send with fresh `Header.ID`, select on `ctx.Done()`/loop `Done`, §5c); Test `agent_test.go`.

- Test: each sends the right command with a non-zero `Header.ID`; a cancelled ctx → error (no block); after loop exit → error.
- Commit. **Phase 5 close:** `go test -race ./internal/...`; `make secure`.

---

## Phase 6 — the 11 tools (`tools/`)

> Targets: §4a (narrow per-family deps), §4b (the table), §4c (default policy). Each tool is **one task** following the TDD rhythm: write the arg struct + `Info` + `InvokableRun`, a table-driven test (happy/boundary/error/edge per CLAUDE.md), commit. Construct each with its **narrow deps** (§4a) — never `ToolSet`. All errors → tool-result strings. Implement `PermissionPrompter`/`Auditable`/`WriteTarget` where §4b/§5b/§2d call for it.

**Shared first:** extract the `**` glob matcher (`tools/glob_match.go`, stdlib `WalkDir` + per-segment `path.Match`) + `FuzzGlobMatch`, used by `Glob` and the path matchers (Task 3.3). Commit.

| Task | Tool | Key deps & behavior (full spec: §4b) | Notable tests |
|---|---|---|---|
| 6.1 | `ReadFile` | `root`,`ReadGuard`; `containedPath`+`DeniedRead`+`O_NOFOLLOW`; `LimitReader(MaxReadBytes)`; line-numbered; truncation notice | range, denied path, oversize cap, symlink rejected |
| 6.2 | `WriteFile` | `root`; `containedPath`+denied-write; `MkdirAll`; temp `O_EXCL\|O_NOFOLLOW`@0600 + `Rename`; `PermissionPrompter`(`FileWriteRequest`), `Auditable`(no content), `WriteTarget` | new file, nested dirs, denied path, atomicity |
| 6.3 | `EditFile` | `root`; str-replace occurrence rules (0→err, 2+&!all→ambiguous, 1/all→replace); diff preview; same opt-interfaces as WriteFile | not-found, ambiguous, replace-all, single |
| 6.4 | `Bash` | `root`; `sh -c` (documented exception §4b); `DeniedBashPrefixes` (advisory); `exec.CommandContext`; 32KiB cap; timeout≤120s; `PermissionPrompter`(`BashRequest`), `Auditable` | exit code, timeout, truncation, workdir containment |
| 6.5 | `Glob` | `root`,`ReadGuard`; `**` matcher; **exclude `DeniedRead` from results**; ≤500 | `**`, denied-path excluded, limit |
| 6.6 | `Grep` | `root`,`ReadGuard`; `rg` arg-list `--regexp <p> -- <path>` (flag-injection) **OR** `WalkDir`+`regexp`; skip noise+denied; ≤200 | pattern, `-x` not a flag, denied skipped, recursive |
| 6.7 | `Fetch` | `*http.Client`(timeouts+TLS1.2); GET/POST; 64KiB cap; `PermissionPrompter`(`FetchRequest`), `Auditable`(method+host only) | method validation, truncation, header redaction in summary |
| 6.8 | `WebSearch` | `*http.Client`; `SearchProvider` iface + DuckDuckGo (`x/net/html`); `PermissionPrompter`(`WebSearchRequest`) | fake provider; real scrape in integration test |
| 6.9 | `AskUser` | none; calls `loop.RequestUserInput(ctx,…)`; validate answer ∈ choices(+"other") | free-text, choices, invalid answer |
| 6.10 | `Todo` | none; `sync.Mutex` map; `uuid` IDs; create/update/list | create→update→list, unknown id |
| 6.11 | `Subagent` | child-agent `Factory`+root `ctx`; `Invoke` to completion, return final text; **max-depth (2) via ctx**, exceed→error; `Skill`→`internal/registry` persona | depth-cap error, child round-trip (fake factory) |

**Integration tests** (`tools/*_integration_test.go`, `//go:build integration`, §5e): filesystem tools under `t.TempDir()` (containment, symlink rejection, `MaxReadBytes`, `.env` excluded from Glob/Grep, atomic write); `Fetch`/`WebSearch` against `httptest.Server` (timeouts, TLS floor, truncation, host-match enforcement). Commit.

**Phase 6 close:** `go test -race ./tools/` and `go test -tags integration -race ./tools/`; `make secure`.

---

## Phase 7 — manifests + composition root

### Task 7.1: `agents/coding` manifest

**Files:** Create `agents/coding/agent.go`; Test `agents/coding/agent_test.go` (fake client, matching `agents/personal-assistant` test style).

- `New(ctx)` self-wires per §4d: `os.Getwd()`→root; `DefaultHardDeny` policy; `tools.NewPermissionChecker`; construct **all 11** tools with narrow deps; assemble `loop.ToolSet{Permission, Registry, Middlewares:nil}`; `llm.ChutesKimiK2()`; `session.NewAgent`. Wrapper satisfies `tui.Agent` + `Approve`/`Deny`/`ProvideAnswer` (delegate to session, ctx+error).
- Test: registers 11 tools; `AcceptsImages()==false`; the trio delegates.
- Commit.

### Task 7.2: `agents/personal-assistant` gains the safe subset

**Files:** Modify `agents/personal-assistant/agent.go`; Test alongside.

- Wire the 7-tool subset (`ReadFile, Glob, Grep, Fetch, WebSearch, AskUser, Todo`) into its `loop.ToolSet`; add the `Approve`/`Deny`/`ProvideAnswer` trio. Keep it immutable (TUI design).
- Test: registers exactly the 7; no write/exec tools; trio delegates.
- Commit.

### Task 7.3: register `coding` at the composition root

**Files:** Modify `cmd/cli/main.go`; Test (or smoke build).

- `reg.Register("coding", func(c)(tui.Agent,error){ return coding.New(c) })` alongside `personal-assistant`. Selected by the name arg (TUI design).
- `CGO_ENABLED=0 go build -trimpath ./...`; run the CLI with each agent name to smoke-test wiring.
- Commit. **Phase 7 close:** `go test -race ./...`; `go test -tags integration -race ./...`; `make secure`.

---

## Out of scope (separate plans)

- **TUI rendering** of `PermissionRequested`/`UserInputRequested`/tool-call cards + the CallID-keyed prompt queue (design §5d is a contract only — a TUI-update plan extends the in-flight `tui/`). This plan stops at the session boundary: the loop emits the events and accepts the commands; the `tui.Agent` interface gains the methods; pixel-level rendering is deferred.
- **ShellSession**, full **Subagent** (streaming/budget/skill-catalog), WebSearch providers beyond DuckDuckGo, the idempotency dedup cache, Bash OS-sandboxing (all per design Out of scope).

---

## Suggested execution order (dependency-correct)

1. Identity plan (prerequisite).
2. Phase 1 (contracts) → Phase 5 Task 5.1 (deps.go) → Phase 2 (hashcache) → Phase 3 (permission).
3. Phase 4 (streaming) → rest of Phase 5 (5.2–5.8, loop heart).
4. Phase 6 (tools) → Phase 7 (manifests + cli).
5. (Later) TUI rendering plan.
