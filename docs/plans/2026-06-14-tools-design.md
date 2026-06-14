# Tools — Design

Date: 2026-06-14 · Status: approved (brainstorm)

## Scope

The agent **tools** subsystem: an agent-agnostic set of built-in tools, the
seven-stage permission model that governs them, workspace containment, the
agentic tool-use turn loop, and the two agent manifests that consume them
(`agents/personal-assistant`, the safe read/web subset; `agents/coding`, the full
set).

It is a port of `docs/old/7.md` (the "Nexus" coding-agent tools spec) onto the
current "urvi" codebase, which has diverged substantially. It depends on the
identity model in `docs/plans/2026-06-14-identity-correlation-design.md`
(referenced here as *identity doc*) for `TurnID`, `CallID`, the command `Header`,
and the event envelope.

**Tool count: 11.** `ReadFile, WriteFile, EditFile, Bash, Glob, Grep, Fetch,
WebSearch, AskUser, Todo, Subagent`. `ShellSession` is deferred (see *Out of
scope*).

### Decisions locked in (this brainstorm)

| Decision | Choice |
|---|---|
| Tool set | Agent-agnostic; manifests select. Full set ported (ShellSession deferred) |
| Coding agent | New `agents/coding` manifest with the full set; reuses `llm.ChutesKimiK2()` for v1 |
| Prompt UX | Loop emits typed events; TUI surfaces them. This doc defines the event/command contract + an integration section; detailed TUI rendering deferred to a TUI-update doc |
| Persistence | Full seven-stage model incl. disk-persisted approvals; build `internal/hashcache` |
| Identity | Per-message IDs via the *identity doc*; tools consume `TurnID` + `CallID` |
| Impl package | Root-level `tools/`; contracts in `internal/tool/` |
| Glob `**` | Stdlib `WalkDir` + per-segment matcher (no new dep) |
| WebSearch | `golang.org/x/net/html` approved for the DuckDuckGo scrape |
| `StreamableTool` | Dropped — no `internal/graph` in this codebase |

---

## Key divergences from 7.md

| 7.md assumption | Current reality | Consequence |
|---|---|---|
| `content.Block` struct with `Type` field; `FunctionToolResult` | Sealed `Block` interface; `ToolUseBlock{ID,Name,Input}`, `ToolResultBlock{ToolUseID,Content []Block,IsError}` exist | `ToolResult.Content` is `[]content.Block`; reuse existing tool blocks |
| `client/` holds prompt view-models | No `client/` layer; TUI drives the loop directly | View-model building moves to `tui/`; this doc defines the contract only |
| `loop.Loop` has always-open `Commands` + `Events` | Actor loop: `Commands chan<- command.Command`; per-turn `StartTurn.Events`; `loopState` | Approve/Deny/Provide are new commands; `listen` relays them to the active turn |
| `internal/graph.ToolsNode` needs `StreamableTool` | No graph package | Drop `StreamableTool`; only `InvokableTool` |
| `llm` tool defs `map[string]any` | `llm.Request.Tools []Tool{Name,Description,Schema json.RawMessage}` exists | Tool defs already plumbed + typed; `ToolInfo.Schema` is `json.RawMessage` |
| `ToolDeps.Events` session-wide channel | events flow per-turn | tool-emitted events injected via `ctx` (see §4a) |
| `.nexus/…` paths | project is "urvi" | `.urvi/approvals.json` + `~/.urvi/approvals.json` |
| `internal/hashcache` already in repo | does not exist | built here |
| `Stream` surfaces tool calls | `Stream` yields only `TextChunk`/`ThinkingChunk` | add `content.ToolUseChunk` (§2b) |

---

## §1 — Package layout & layering

Strictly low→high, additive, no cycles:

```
internal/content/                  Block (+ ToolUseBlock, ToolResultBlock), Chunk (+ ToolUseChunk)
    ↑                  ↑
internal/tool/      internal/llm/  contracts: BaseTool, InvokableTool, ToolResult, ToolInfo,
    ↑                              PermissionRequest (sealed), PermissionPrompter, ApprovalScope,
    │                              Sequential, ToolMiddleware  — imports content only
internal/agent/loop/event/         + PermissionRequested(tool.PermissionRequest), UserInputRequested,
    ↑                                ToolCallStarted, ToolCallCompleted
internal/agent/loop/               runner.go, deps.go (ToolDeps, PermissionGate, Effect, ToolPolicy),
    ↑                ↑               EmitFromContext; command/ + Approve/Deny/Provide; turn.go agentic loop
tools/            internal/hashcache/   11 impls, PermissionChecker, SearchProvider; hashcache backs approvals
    ↑
agents/coding/   agents/personal-assistant/   manifests select tool set + prompt + model
    ↑
cmd/cli/                           composition root (registry → agent → TUI)
```

- **`internal/tool` imports only `internal/content`** — never `loop`.
  `PermissionPrompter.BuildRequest` returns `tool.PermissionRequest`, a sealed
  interface with no loop dependency.
- **`EffectChecker` lives in `tools/`** (returns `loop.Effect`), keeping contracts
  loop-free.
- **`loop` never imports `tools/`.** The concrete `*tools.PermissionChecker` and
  `[]tool.InvokableTool` registry are injected via `loop.Config.Tools` (`ToolDeps`)
  at the composition root; the runner dispatches through interfaces only.
- **`tools/` is root-level** (alongside `internal/`, `agents/`, `cmd/`); it imports
  `urvi/internal/...` freely. `Subagent` imports `internal/agent/session`
  (session wraps loop; acyclic).

---

## §2 — The agentic turn loop

### §2a. `runTurn` becomes a loop

Today `runTurn` (`internal/agent/loop/turn.go:21`) streams once and returns. It
becomes the agentic loop, re-streaming after each tool batch until the model stops
calling tools:

```
append user message to msgs; emit TurnStarted
for {
    req := llm.Request{Model: cfg.Model, Messages: msgs, Tools: toolDefs(cfg.Tools.Registry)}
    stream := client.Stream(ctx, req)
    // accumulate: TextChunk/ThinkingChunk → emit TokenDelta (unchanged)
    //             ToolUseChunk           → fold by Index into []ToolUseBlock
    assemble AIMessage{Blocks: [thinking…, text, toolUse…]}; append to msgs
    if no tool_use blocks { emit TurnDone(msg); return }
    results := runner.RunBatch(ctx, toolUseBlocks, cfg.Tools, replies, emit)
    for each result: append content.ToolMessage{ToolUseID, Blocks:[ToolResultBlock]} to msgs
    if any result.Terminate { emit TurnDone(lastAIMessage); return }
}
```

- `toolDefs` maps each registered tool's `Info(ctx)` → `llm.Tool{Name,
  Description, Schema}`. `ToolInfo.Schema` is `json.RawMessage` (1:1 with
  `llm.Tool.Schema`; no `map[string]any`).
- **Thinking blocks are preserved** in the assistant message before the tool
  results — required for extended-thinking models. Never stripped.
- One `content.ToolMessage{ToolUseID, …}` is appended per tool result (the content
  model's `ToolMessage` carries a single `ToolUseID`).
- The existing **history-rollback** contract is unchanged for failures; a
  *completed* tool-using turn advances history with the full assistant↔tool
  exchange.
- The only turn-enders remain `ctx` cancellation and `Interrupt`.

### §2b. Streaming tool calls — `content.ToolUseChunk`

`Stream` yields only `TextChunk`/`ThinkingChunk` today, so tool calls never reach
the loop. Add one sealed `Chunk` variant:

```go
// ToolUseChunk is a streaming delta of a tool call. Providers emit these as they
// parse function-call deltas; the runner accumulates by Index into a ToolUseBlock.
type ToolUseChunk struct {
    Index     int    // tool call's position in the response
    ID        string // tool_use id (may arrive only on the first delta for this Index)
    Name      string // tool name (likewise)
    InputJSON string // partial JSON delta of the arguments
}
func (*ToolUseChunk) isChunk() {}
```

`internal/llm/openaiapi` emits these; `runTurn` folds them by `Index` into
`content.ToolUseBlock{ID, Name, Input: json.RawMessage}`. Because `ToolUseChunk`
rides the existing `TokenDelta{Chunk}` event, the in-flight TUI needs **zero
change** — it already type-switches on `Chunk` and skips non-`TextChunk` variants.

### §2c. Permission / AskUser reply plumbing (actor-model remap)

Three additions remap 7.md's `activeTurn.replies` onto the current actor.

**New control commands** (`internal/agent/loop/command/`, each embeds the identity
doc's `Header`):

```go
type ApproveToolCall  struct { Header; CallID uuid.UUID; Scope tool.ApprovalScope }
type DenyToolCall     struct { Header; CallID uuid.UUID }
type ProvideUserInput struct { Header; CallID uuid.UUID; Answer string }
// each: isCommand()  — fire-and-forget control commands (no Ack)
```

**`loopState` gains a reply channel.** When the actor accepts a `StartTurn`, it
creates `replies := make(chan command.Command, 1)`, keeps the write side in
`loopState.turnReplies`, and passes the read side to `runTurn`. `listen` forwards
the three control commands:

```go
case command.ApproveToolCall, command.DenyToolCall, command.ProvideUserInput:
    if state.status == loopRunning && state.turnReplies != nil {
        select {
        case state.turnReplies <- cmd: // relay to the parked runner
        default:                       // runner already unblocked / ctx done — drop
        }
    }
```

**The runner** generates a `CallID` (`uuid.New`) per tool call, injects `CallID` +
the replies read-channel into `ctx` (package-private context keys), then:

1. `effect := cfg.Tools.Permission.Check(ctx, t, name, argsJSON)`.
2. If `EffectAsk`: build the typed request
   (`t.(tool.PermissionPrompter).BuildRequest` or fallback `UnknownRequest`), emit
   `event.PermissionRequested{CallID, Request}`, block on `<-replies` /
   `<-ctx.Done()`, **validate `cmd.CallID == CallID`** (stale/unknown dropped).
3. `AskUser` blocks on the *same* `replies` channel for `ProvideUserInput`;
   `CallID` validation is what lets the two share the channel.

The `loopState` invariant is preserved: only `listen` touches `loop.Commands` and
`loopState`; it *routes* control commands to the parked runner via `turnReplies`.

#### Two channels, opposite directions

The request goes **out** as an event on the per-turn `Events` channel; the decision
comes **in** as a command on the permanent `Commands` channel; `listen` bridges the
command to the parked runner via the internal `turnReplies`.

```
                          ┌──────────────────── Loop actor ────────────────────┐
TUI ──Approve(callID)──► session ──► loop.Commands (inbound, always open) ─► listen()
                                                                               │ owns loopState
                                                                               │ turnReplies (internal, buf 1)
                                                                               ▼
                                                                            runTurn goroutine
                                                                            (runner blocked on a gate)
TUI ◄── r.Next() ── session ◄── StartTurn.Events (outbound, per-turn) ◄──── emit(PermissionRequested)
```

End-to-end: TUI key `w` → `agent.Approve(callID, ScopeWorkspace)` →
`session.Approve` → `loop.Commands <- ApproveToolCall` → `listen` → `turnReplies`
→ runner unblocks. `PermissionRequested` is just another event interleaved with
`TokenDelta`; the runner then parks while `listen` keeps draining.

### §2d. Runner batching, middleware, panic recovery

`internal/agent/loop/runner.go`:

- **Permission is resolved sequentially across all calls first** — a session-scope
  grant on call *N* is visible to call *N+1*'s `Check`.
- Execution then splits into a **serial batch** (tools where `Sequential()==true`)
  that drains before a **semaphore-bounded parallel batch**. (No built-in
  implements `Sequential` yet; it is the documented seam for ShellSession.)
- The `tool.ToolMiddleware` chain wraps each `InvokableRun` (first listed =
  outermost). Cross-cutting concerns (OTel spans, rate limiting, audit, caching,
  per-tool timeout) live here, not in the runner body.
- `defer/recover` turns a panic into `"error: tool panicked: <detail>"`.
- **All tool failures become tool-result strings** (invalid args, permission
  denied, execution error, panic) — never turn-terminating. Only `ctx`
  cancellation and `Interrupt` end the turn.

**Observability events** (auto-approved tools execute silently otherwise):
`event.ToolCallStarted{CallID, ToolName, ArgsJSON}` before run,
`event.ToolCallCompleted{CallID, IsError}` after. Both carry `CallID`; today's TUI
ignores unknown event types (additive).

### §2e. Tool events via `ctx` (replaces 7.md's `ToolDeps.Events`)

Since events flow per-turn, the runner injects the active turn's emit func into
`ctx` (alongside `CallID` + `replies`). Event-emitting tools retrieve it via a
helper in `loop` (not `internal/tool`, which must stay `event`-free):

```go
// in internal/agent/loop/
func EmitFromContext(ctx context.Context) (func(event.Event), bool)
```

`AskUser` uses it to emit `UserInputRequested`. `ToolDeps` therefore keeps
`SessionCtx` (needed at construction by background goroutines) but **drops the
`Events` field**. With ShellSession deferred, no tool emits events that outlive a
turn.

---

## §3 — Tool contracts, permission model, `hashcache`

### §3a. Contracts — `internal/tool/` (imports only `content`)

```go
type ToolInfo struct {
    Name   string
    Desc   string
    Schema json.RawMessage // JSON Schema; maps 1:1 to llm.Tool.Schema
}
type BaseTool      interface { Info(ctx context.Context) (*ToolInfo, error) } // never widened (Rule 1)
type InvokableTool interface { BaseTool; InvokableRun(ctx context.Context, argsJSON string) (*ToolResult, error) }

type ToolResult struct {
    Content   []content.Block // ≥1 block; runner injects "error: empty result" if nil
    Terminate bool
}
func TextResult(s string) *ToolResult // one TextBlock, Terminate false

// Optional capability interfaces (added, never folded into BaseTool — Rule 1):
type Sequential        interface { Sequential() bool }
type PermissionPrompter interface { BuildRequest(argsJSON string) (PermissionRequest, error) }

type ToolMiddleware func(ctx context.Context, t InvokableTool, argsJSON string, next ToolExecuteFunc) (*ToolResult, error)
type ToolExecuteFunc func(ctx context.Context, argsJSON string) (*ToolResult, error)
```

Sealed `PermissionRequest` + `ApprovalScope`:

```go
type PermissionRequest interface {
    permissionRequest()
    ToolName() string        // prompt header
    Description() string     // prompt body
    AllowedScopes() []ApprovalScope
}
type ApprovalScope uint8
const (
    ScopeOnce ApprovalScope = iota // approve this call only; nothing persisted
    ScopeSession                   // session policy (in-memory)
    ScopeWorkspace                 // <ws>/.urvi/approvals.json
)
```

Concrete requests live in `tools/permission.go`: `FileWriteRequest`,
`BashRequest`, `FetchRequest`, `WebSearchRequest`, and the fallback
`UnknownRequest{Tool, ArgsJSON}` (used when a tool implements no
`PermissionPrompter` but `Check` returns `EffectAsk`). `StreamableTool` is
**dropped**.

### §3b. Consumer surface — `internal/agent/loop/deps.go`

```go
type Effect uint8 // EffectAutoApprove | EffectAsk | EffectDeny

type ToolPolicy struct { Tool string; Effect Effect; Paths, Commands []string }

type PermissionGate interface {
    Check(ctx context.Context, t tool.InvokableTool, toolName, argsJSON string) Effect
    AddSessionPolicy(p ToolPolicy)
}

type ToolDeps struct {
    WorkspaceRoot string
    SessionCtx    context.Context      // wired by loop.New (root ctx)
    Permission    PermissionGate
    Registry      []tool.InvokableTool // runner looks up by Info().Name
    Middlewares   []tool.ToolMiddleware
}
```

`loop.Config` gains `Tools ToolDeps`. `loop.New` wires `SessionCtx`. `EffectChecker`
(returns `loop.Effect`) lives in `tools/`, kept out of `internal/tool`.

### §3c. The seven-stage `PermissionChecker` — `tools/permission.go`

Ported verbatim from 7.md with `.urvi` naming. Stages run top-to-bottom; first
definitive effect wins:

```
Stage 0  EffectChecker      — optional per-call override from the tool (e.g. future ShellSession send)
Stage 1  ContainmentCheck   — containedPath: resolve under WorkspaceRoot, Clean, verify still inside
Stage 2  HardApproveRules   — operator always-allow ("*" = all)
Stage 3  HardDenyRules      — denied read/write globs, bash prefixes, MaxReadBytes
Stage 4  PersistedApprovals — <ws>/.urvi/approvals.json then ~/.urvi/approvals.json (first match wins)
Stage 5  SessionPolicies    — in-memory ToolPolicy list; extended at runtime by 's'/'w'
Stage 6  DefaultEffect      — EffectAsk
```

```go
type EffectChecker interface { CheckEffect(argsJSON string) (effect loop.Effect, handled bool) }

type HardApproveRules struct { Tools []string }
type HardDenyRules struct {
    DeniedReadPaths    []string // ~/.ssh/**, **/.env, **/*.pem, **/id_rsa, … (defaults)
    DeniedWritePaths   []string // same + **/.git/config, **/go.sum
    DeniedBashPrefixes []string // "rm -rf /", "sudo", "curl | bash", "dd if=", … (defaults)
    MaxReadBytes       int64    // default 1 MiB
}
type ApprovalRecord struct {
    Tool          string `json:"tool"`
    PathPattern   string `json:"path_pattern,omitempty"`
    CommandPrefix string `json:"command_prefix,omitempty"`
    Effect        Effect `json:"effect"`
}
type ApprovalsFile struct { Version int `json:"version"`; Approvals []ApprovalRecord `json:"approvals"` }

type PermissionPolicy struct {
    WorkspaceRoot string
    HardApprove   HardApproveRules
    HardDeny      HardDenyRules
    Policies      []ToolPolicy // extended in place for session-scope grants
}

type PermissionChecker struct { /* mu; policy; wsCache, userCache *hashcache.Cache[ApprovalsFile] */ }
func NewPermissionChecker(policy PermissionPolicy) *PermissionChecker
// satisfies loop.PermissionGate: Check runs the seven stages; AddSessionPolicy appends under mu.
```

`Check` re-reads the approval files on **every** call (so a `w` grant during one
gate is visible to the next call's `Check` immediately); `hashcache` skips the
JSON unmarshal when the file bytes are unchanged.

### §3d. New helper — `internal/hashcache/`

```go
type Cache[T any] struct { /* mu sync.Mutex; sum [32]byte; val T; ok bool; parse func([]byte)(T,error) */ }
func New[T any](parse func([]byte) (T, error)) *Cache[T]
func (c *Cache[T]) Load(content []byte) (T, error) // sha256(content)-keyed; re-parses only on change
```

Pure stdlib (`crypto/sha256`, `sync`); concurrency-safe. `PermissionChecker` holds
two instances (workspace + user approvals files).

---

## §4 — The 11 tools & the two manifests

### §4a. Tool construction (Rule 2)

Every tool takes a single `loop.ToolDeps` at construction and ignores fields it
doesn't use; adding a shared dependency = one new field, no constructor changes.
Errors → tool-result strings; secrets never logged; `crypto/rand` IDs
(`internal/uuid`).

### §4b. The tools (`ToolResult.Content` is `[]content.Block`)

| Tool | Args | Behaviour | Default |
|---|---|---|---|
| `ReadFile` | path, start/end line | `containedPath` + `DeniedReadPaths` + `MaxReadBytes`; line-numbered text; truncation notice | AutoApprove |
| `WriteFile` | path, content | `containedPath` + `DeniedWritePaths`; `MkdirAll`; atomic tmp+`Rename` | Ask (`FileWriteRequest`) |
| `EditFile` | path, old/new, replace_all | str-replace; 0→error, 2+ & !replace_all→ambiguous, else replace; diff preview | Ask (`FileWriteRequest`) |
| `Bash` | command, workdir, timeout(≤120s) | `DeniedBashPrefixes`; `exec.CommandContext`; combined output; 32 KiB cap | Ask (`BashRequest`) |
| `Glob` | pattern, root | `containedPath`; **`**` via stdlib `WalkDir` + per-segment `path.Match`**; ≤500 results | AutoApprove |
| `Grep` | pattern, path, recursive, ignore_case, context_lines, include_all | `rg` if present (binary, not a Go dep) else `WalkDir`+`regexp`; skip noise dirs; ≤200 matches | AutoApprove |
| `Fetch` | url, method(GET/POST), headers, body, timeout(≤60s) | `net/http` w/ explicit timeouts + `tls.Config{MinVersion: TLS1.2}`; 64 KiB cap | Ask (`FetchRequest`) |
| `WebSearch` | query, results(≤10) | `SearchProvider` iface; DuckDuckGo HTML scrape via **`golang.org/x/net/html`** | Ask (`WebSearchRequest`) |
| `AskUser` | question, choices | emits `UserInputRequested` via `EmitFromContext`; blocks on `replies` (CallID-validated); answer validated against choices | AutoApprove |
| `Todo` | action(create/update/list), … | in-memory `sync.Mutex` map on the tool; `uuid` IDs; session-scoped | AutoApprove |
| `Subagent` | skill, message | **synchronous** child `session.AgentSession` (`Invoke` to completion, returns final text); `Skill` selects a persona via `internal/registry` | AutoApprove |

`Sequential` and `EffectChecker` are defined as extensibility seams but no built-in
implements them yet (ShellSession, deferred, is their first user).

### §4c. Default-policy table

`ReadFile/Glob/Grep/Todo/AskUser/Subagent → AutoApprove` (within workspace);
`WriteFile/EditFile/Bash/Fetch/WebSearch → Ask`.

### §4d. The two manifests

Each `New(ctx)` self-wires (matching today's `personalassistant.New(ctx)`):
`os.Getwd()` → workspace root → build `PermissionPolicy` (default hard rules) →
`tools.NewPermissionChecker(policy)` → build the `[]tool.InvokableTool` it wants →
read API key from env → `auto.New(spec)` → seal `loop.Config{Client, Model, Tools}`
→ `session.NewAgent(rootCtx, cfg)`. The wrapper satisfies `tui.Agent` plus the new
`Approve`/`Deny`/`ProvideAnswer` trio (§5c).

- **`agents/personal-assistant`** (Kimi K2, text-only) → safe subset:
  `ReadFile, Glob, Grep, Fetch, WebSearch, AskUser, Todo`. No write/exec tools.
  (Easily tuned in the manifest.)
- **`agents/coding`** (new) → all 11 tools; coding system prompt; reuses
  `llm.ChutesKimiK2()` for v1 (strong agentic-coding model, already in the catalog,
  text-only — model swap is a one-line manifest change). Registered as a second
  `tui.Agent` in `cmd/cli`, selected by name arg.

---

## §5 — Loop command/event additions, TUI contract, testing

### §5a. `command/` additions

`ApproveToolCall`, `DenyToolCall`, `ProvideUserInput` — see §2c. Handled by
`listen` as control commands.

### §5b. `event/` additions

```go
type PermissionRequested struct { CallID uuid.UUID; Request tool.PermissionRequest }
type UserInputRequested  struct { CallID uuid.UUID; Question string; Choices []string }
type ToolCallStarted     struct { CallID uuid.UUID; ToolName, ArgsJSON string }
type ToolCallCompleted   struct { CallID uuid.UUID; IsError bool }
// each: isEvent()
```

`event` imports `internal/tool` for `PermissionRequest` — no cycle (`tool` imports
only `content`). `ShellSession*` events are deferred with the tool.

### §5c. `session` + `tui.Agent` additions

`AgentSession` gains `Approve(callID uuid.UUID, scope tool.ApprovalScope)`,
`Deny(callID uuid.UUID)`, `ProvideUserInput(callID uuid.UUID, answer string)` —
each sends the command to `loop.Commands` with a fresh `Header.ID` (same pattern as
`Interrupt`/`Shutdown`). The TUI's consumer-defined `Agent` interface gains a
matching `Approve`/`Deny`/`ProvideAnswer` trio; both agent wrappers delegate to the
session.

### §5d. TUI integration — contract only (rendering deferred)

7.md's dropped `client/` view-models reland in `tui/`:

- New events ride the same per-turn stream `readNext` already drains.
- `PermissionRequested` → store; render an approval box (`Request.ToolName()` /
  `Request.Description()` / keys from `AllowedScopes()` + always `[n]`); single
  keypress `y/s/w` → `agent.Approve(callID, scope)`, `n` → `agent.Deny(callID)`;
  cleared on any terminal event.
- `UserInputRequested` → assistant-style question; choices → key list + "other…";
  free text → `agent.ProvideAnswer(callID, text)`.
- `ToolCall{Started,Completed}` → optional tool-call cards; today's TUI ignores
  unknown event types, so this is additive.

The pixel-level work is a follow-up TUI-update doc, since that TUI is mid-flight.

### §5e. Testing (table-driven, `-race`, fuzz for parsers — CLAUDE.md)

- `hashcache` — parse-skip on unchanged bytes, change detection, concurrent `Load`.
- `PermissionChecker` — each of the seven stages, containment escape, hard-deny
  globs, persisted-approval precedence (ws over user), `AddSessionPolicy`.
- Each tool — happy/boundary/error/edge; `FuzzContainedPath`; `FuzzGlobMatch`;
  `EditFile` occurrence rules; `Bash` exit/timeout/truncation; `Fetch` method +
  truncation; `WebSearch` via a fake `SearchProvider`; `AskUser` answer validation.
- `runner` — serial-then-parallel batching (fake `Sequential` tool), N→N+1
  session-grant visibility, middleware ordering, panic→error result, ctx-cancel
  terminates.
- `runTurn` — `ToolUseChunk` accumulation, tool round-trips, `Terminate`, thinking
  preserved, no-tool-call exit; fake `llm.LLM`.
- control commands — Approve/Deny/Provide forwarded to `turnReplies`, stale `CallID`
  dropped, dropped when idle.
- manifests — coding registers 11 tools, PA registers the 7-tool subset,
  `AcceptsImages` false.
- (identity-model tests live in the identity doc.)

---

## Out of scope (this iteration)

- **ShellSession** — persistent/async shell; needs a session-wide event path so
  `ShellSessionEnded` reaches the UI between turns. Its own follow-up. (`Sequential`
  + `EffectChecker` seams are in place so it lands with no runner change.)
- **Subagent** beyond the synchronous stub — streaming child events, depth limits,
  skill catalog.
- WebSearch providers beyond DuckDuckGo.
- Command dedup/idempotency **cache** (IDs are the substrate — identity doc).
- Detailed TUI rendering of prompts/cards (contract here; rendering in a TUI-update
  doc).
- Bash OS-level sandboxing, Fetch prompt-injection sanitization, tool-result
  caching, tool versioning (as 7.md).
