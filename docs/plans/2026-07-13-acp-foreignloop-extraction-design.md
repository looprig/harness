# looprig/acp + foreignloop extraction — ACP bridge & pluggable foreign agents

**Status:** draft spec, not built. Created 2026-07-13; updated 2026-07-14.

Two co-designed pieces of work:

1. A bidirectional [Agent Client Protocol](https://agentclientprotocol.com) bridge
   (`acp` module).
2. Extraction of `foreignloop` out of the harness core into its own module, making
   "host an external agent as a first-class loop" an **optional** capability with
   pluggable drivers (`claude`, `codex`, `acp`).

The ACP bridge is bidirectional:

- **Agent side** — expose looprig-backed agents over ACP so any compatible ACP
  client can drive a looprig agent. Zed is an initial interoperability target,
  not a dependency or special case in the API.
- **Client side** — let the looprig `cli` act as an ACP *client* so it can drive
  **any** foreign ACP server, rendering it through the existing TUI (via a
  foreign-loop driver — see below).

## Module topology

```
harness (core)  ◀── acp  ◀── foreignloop
   ▲                            │
   └────────────────────────────┘        acp → harness ;  foreignloop → harness + acp
```
Acyclic. Three modules:

- **`harness` (core)** — keeps the loop abstraction and the foreign-loop **contract
  seam** (`Builder`, `RestoredBuilder`, `RestoredForeign`, `EventPublisher`,
  promoted out of `pkg/foreignloop`) plus the `WithForeignBuilders(...)` injection
  options. Ships **no** concrete foreign drivers. A consumer that never wires a
  builder pays nothing — foreign-agent hosting is opt-in.
- **`acp`** — one sibling module with three focused packages:
  `acp/protocol` (ACP schema + JSON-RPC transport), `acp/agent` (looprig → ACP
  server facade), and `acp/client` (client for a foreign ACP server). The module
  depends on harness because `acp/agent` adapts its public session contracts.
- **`foreignloop` (new repo)** — the concrete foreign-loop backend + drivers
  `claude`, `codex`, and `acp`. Depends on harness (seam) and acp (client).
  Optional; only imported by consumers hosting foreign agents.

There is deliberately **no `harness/pkg/acp` package**. Harness remains unaware
of ACP; importing `github.com/looprig/acp/agent` is the opt-in that adds the ACP
facade to a harness-backed application.

```text
github.com/looprig/acp
├── protocol   ACP messages and JSON-RPC transports
├── agent      adapt a harness Rig/LiveSession into an ACP agent
└── client     connect to an external ACP agent
```

**Naming trap — "agent" points two ways:**
- `acp/agent` = *we are* the ACP agent (expose looprig outward).
- `foreignloop/acp` = *host a remote* ACP agent as a foreign loop, built on
  `acp/client`. Sibling to `foreignloop/claude` and `foreignloop/codex`.

## Decisions (locked)

- **`acp` ships as a separate module, not inside harness.** New repo
  `github.com/looprig/acp`, sibling to `cli`, `natsstore`, etc. Keeps the ACP
  schema + JSON-RPC transport (both evolving; remote transport is WIP upstream)
  out of the dependency-clean core.
- **`acp` is one module, both halves.** `acp/agent` (looprig → ACP) and
  `acp/client` (foreign ACP → looprig). Shared schema/transport/translation.
- **Agent side is a pure facade library.** Ships **no agents**. Depends only on
  the harness public seam (`serve.LiveSession` / `serve.Rig` shapes) plus the ACP
  protocol/transport. A consumer (e.g. `swe`) wires a concrete rig + agent and
  gets ACP endpoints. Mirrors how `harness/pkg/serve` is a facade over the same
  session machinery — `acp/agent` is a *second* facade alongside it.
- **`foreignloop` is extracted into its own optional module.** The core keeps only
  the contract seam; concrete backend + `claude`/`codex`/`acp` drivers move out.
  Rationale: not every consumer needs foreign-agent hosting, and the code is
  already composition-root-injected (`Builder`/`RestoredBuilder` are function-type
  seams over core types — the authors' `Spec` comment confirms this was intended).
- **Client side is a `foreignloop/acp` driver.** The ACP client translates incoming
  ACP updates into `event.*` and drives a harness foreign loop, so the existing
  `cli` TUI renders a foreign agent **unchanged**. `cli` depends on `foreignloop`
  (+ `acp`); no second render path.
- **Scope: full ACP + remote transport.** Local stdio (editor subprocess) *and*
  ACP's HTTP/WebSocket remote transport. Full method set, not a minimal shim.

## Why it's feasible

The harness already proves the pattern. `serve.LiveSession` / `serve.Rig` is a
narrow, protocol-agnostic control surface that ACP maps onto almost 1:1:

| ACP surface | Harness seam |
|---|---|
| `initialize` (capability negotiation) | static + rig capabilities |
| `session/new` | `Rig.NewSession(ctx, opts…)` |
| `session/load` | `Rig.RestoreSession(ctx, id)` |
| `session/prompt` | `LiveSession.Submit(ctx, blocks)` (outcome observed on event stream) |
| `session/update` (streaming) | `LiveSession.SubscribeEvents(filter)` → `event.*` |
| `session/cancel` | `LiveSession.Interrupt(ctx)` |
| `session/close` | `SessionCloser.Shutdown(ctx)`; preserve durable history |
| `session/delete` | not advertised in the first version |
| `session/request_permission` | harness **gate**: gate-open event → ACP permission request; client reply → `RespondGate` |
| `fs/read_text_file`, `fs/write_text_file` | **not used** — harness agents run tools against their own workspace; these client caps are ignored |

The gate direction is a *natural* match: ACP has the agent ask the client for
permission, which is exactly what the harness gate already does.

## Session identity and lifecycle

ACP session IDs are the canonical harness `SessionID` UUID encoded as strings;
the facade does not mint a second identifier or persist an ACP-to-harness lookup
table. `session/new` returns the new harness UUID, while `session/load` and
`session/resume` parse the supplied ACP ID as a harness UUID before crossing the
rig boundary. An ID identifies a session but never authenticates or authorizes a
caller.

One live ACP session owns one live harness session controller in `acp/agent`'s
in-memory registry. The facade requires the narrow lifecycle capability that the
harness `session.SessionController` already satisfies structurally:

```go
type SessionCloser interface {
	Shutdown(context.Context) error
}
```

ACP [`session/close`](https://agentclientprotocol.com/rfds/session-close) maps to
`SessionCloser.Shutdown(ctx)`, then removes the stopped controller from the live
registry. `Shutdown` is already the stronger lifecycle operation: it stops and
drains active loops and releases session resources, so the facade does not call
`Interrupt` first. Close preserves durable history; a later explicit
`session/load` or `session/resume` may restore it.

The first version does not advertise
[`session/delete`](https://agentclientprotocol.com/protocol/v1/session-delete).
ACP permits either soft or hard deletion and leaves active-session behavior to
the implementation, while harness does not yet expose a narrow deletion contract
with those semantics. Deletion remains out of scope until that storage and
authorization design exists.

## Controls and advertised commands

The facade distinguishes protocol-native controls from agent-specific commands.
It does not expose every internal harness command automatically; only an
explicitly registered, externally safe capability crosses the ACP boundary.

### Interrupt is a native ACP control

An ACP [`session/cancel`](https://agentclientprotocol.com/protocol/v1/prompt-turn#cancellation)
notification maps directly to
`LiveSession.Interrupt(ctx)`. The facade continues draining the turn's terminal
events and then completes the outstanding `session/prompt` response with
`stopReason: cancelled`. Cancellation is not `session/close`: close additionally
releases the live session and belongs to lifecycle cleanup.

```text
ACP session/cancel
        │
        ▼
LiveSession.Interrupt
        │
        ▼
session/prompt → stopReason: cancelled
```

### Compact is an ACP slash command

ACP has no standard compaction method. When compaction is wired for a live
session, `acp/agent` advertises `compact` through ACP's
[`available_commands_update`](https://agentclientprotocol.com/protocol/v1/slash-commands).
A client invokes it by sending the exact text `/compact` in a normal
`session/prompt` request. The facade recognizes the advertised command and calls
the harness compaction control; it does **not** pass `/compact` to inference as
user text.

The facade depends on a narrow optional capability supplied at composition:

```go
type Compactor interface {
	Compact(context.Context) (uuid.UUID, error)
}
```

If that capability is absent, the facade does not advertise `compact`. Generic
ACP has no loop-focus concept, so this command targets the harness session's
primary/active loop. A future Looprig-specific focused-loop operation would need
a negotiated extension rather than changing standard `/compact` semantics.

The prompt remains outstanding until the matching `CompactionCommitted` or
`CompactionRejected` event. On success, the facade publishes ACP `usage_update`
with the post-compaction context measurement and returns `end_turn`. ACP has no
standard compaction-progress update, so the first interoperable version does not
invent one; the pending prompt already communicates activity. Richer progress
can be added later through a negotiated
[`_looprig.dev/...` extension](https://agentclientprotocol.com/protocol/v1/extensibility)
if a real client needs it.

```text
available_commands_update: compact
        │
session/prompt: "/compact"
        │
        ▼
Compactor.Compact
        │
        ├── CompactionCommitted → usage_update → end_turn
        └── CompactionRejected  → sanitized prompt error
```

The exact ACP error-code mapping for typed compaction rejection reasons remains
an error-design decision; no raw internal cause or summary content crosses the
wire.

This establishes the general mapping rule:

1. Use a native ACP method when its semantics match (`session/cancel`).
2. Use ACP advertised slash commands for user-invoked harness capabilities
   (`/compact`).
3. Use a negotiated underscore-prefixed ACP extension only for a Looprig-specific
   machine control that cannot be represented faithfully by the standard protocol.

## Architecture (agent side: looprig → ACP)

```
ACP client ──JSON-RPC (stdio | HTTP/WS)──▶ acp.Server
                                              │  depends only on:
                                              │   - Rig[LiveSession]     (session factory)
                                              │   - LiveSession          (per-session control)
                                              │   - event.* leaf types, gate.*, content.*
                                              ▼
                                        harness session machinery
                                        (wired by the consumer at composition root)
```

- `acp.Server` owns the JSON-RPC dispatch, one transport binding (stdio or
  remote), and session multiplexing across connections.
- Per ACP session → one harness `LiveSession` (from `Rig.NewSession`/`RestoreSession`).
- Same Dependency-Inversion contract as `serve`: `acp` imports **no** concrete
  session/LLM/store types; the consumer injects them.

## The one piece of real design work: event → SessionUpdate translation

Everything else is transport + config plumbing. The translator maps the harness
event stream onto ACP `SessionUpdate` variants. Rough mapping to nail down:

| Harness event | ACP `session/update` |
|---|---|
| assistant content chunks | `agent_message_chunk` (grouped by messageId) |
| tool call started | `tool_call` (name, input, status=pending/in_progress) |
| tool call progress/result | `tool_call_update` |
| turn/loop lifecycle (`LoopStarted`, `TurnDone`, `SessionIdle`) | drives `stopReason` on the `session/prompt` response |
| plan/todo state (if surfaced) | `plan` (full list each update) |
| available commands | `AvailableCommandsUpdate` |

`session/prompt` is request/response in ACP but fire-and-forget + stream in the
harness: the handler calls `Submit`, then consumes the subscription until the
turn reaches a terminal event, and returns the corresponding `stopReason`
(`end_turn`, `max_tokens`, `max_turn_requests`, `refusal`, or `cancelled`).
Correlate by the input id `Submit` mints.

## Client side (`cli` → any foreign ACP server)

The inverse of the agent side. The `cli` TUI is built to render the harness
**event stream**, so the client half is an inverse translator that never touches
the TUI:

```
foreign ACP server ──JSON-RPC──▶ acp.Client ──translate──▶ event.* ──▶ foreignloop ──▶ cli TUI
```

- Implemented as the **`foreignloop/acp` driver** (sibling to `claude`/`codex`).
  One foreign ACP session → one harness **foreign loop**, driven by the remote
  agent rather than looprig inference.
- Inverse translation table (mirror of the agent side):
  `agent_message_chunk` → assistant content event; `tool_call` /
  `tool_call_update` → tool events; `plan` → plan/todo state.
- **`stopReason` closes the quiescence gap.** The foreign loop currently never
  reaches `LoopIdle`/`SessionIdle` (no clean turn boundary). ACP's
  `session/prompt` returns a `stopReason` — an explicit turn-done signal. Driving
  the foreign loop through the ACP client gives it the terminal boundary it
  otherwise lacks. Map `stopReason` → the foreign loop's turn-done / idle event.

### Reversed responsibilities (the client must *implement* client caps)

On the agent side we could ignore ACP's client capabilities. On the client side a
foreign agent will call *us*:

- `fs/read_text_file` / `fs/write_text_file` — serve from the cli's workspace
  (advertise the capability; wire to the same workspace the foreign loop targets).
- `session/request_permission` — surface through the cli's existing gate/TUI
  approval flow, then reply to the foreign agent.
- Terminal capability — decide whether to advertise it (foreign agents may want to
  run commands in our terminal).

## Config mapping (`session/new`)

- ACP `cwd` + workspace roots → harness workspace / rig `SessionOption`s.
- ACP-provided MCP servers → harness tool config (passthrough).
- ACP modes (ask / architect / code) → optional; map to rig/agent presets if the
  consumer defines them, else advertise no modes.

## Remote transport notes

- Reuse the same `acp.Server` + translator; swap the stdio binding for HTTP/WS.
- Remote adds: auth (`authenticate` + advertised auth methods), multi-connection
  session ownership, and connection-loss handling (detach subscription, keep
  session alive for resume). Spec upstream is WIP — pin a version and gate remote
  behind a capability flag.

## foreignloop extraction (work items)

The cut is mostly mechanical because the seam already exists (`sessionruntime`
holds `foreignloop.Builder` / `RestoredBuilder` as injected fields, wired via
`WithForeignBuilders`). Steps:

1. **Promote the contract seam to a public harness package** (e.g. `pkg/loop` or a
   new `pkg/foreign`): `Builder`, `RestoredBuilder`, `RestoredForeign`,
   `EventPublisher`. These reference only `loop.*`, `event.*`, `core/*` — no churn.
2. **Promote the shared helper** `internal/runtimecontract/managed_queue.go` to a
   public package (small; used by `foreignloop/turn.go`).
3. **Move concrete impl + drivers** out to the new module: `foreignloop.go`,
   `loop.go`, `turn.go`, `mapper.go`, `restored.go`, `decode_*`, `snapshot.go`,
   plus `claude/` and `codex/`.
4. **Keep injection options in harness** (`WithForeignBuilders`,
   `WithLifecycleForeignBuilders`) — they accept the interface types.
5. **Relocate rig foreign options** (`pkg/rig/options.go`): generic option
   constructors stay; per-driver presets move to the module.
6. **Add the `foreignloop/acp` driver.**

**The one risk to verify first (durability-sensitive):** `command_journal.go` folds
durable history into `RestoredForeign{ForeignSID, TurnIndex, Msgs}`. Confirm that
*folding/decoding* stays on the core/sessionruntime side and the module only
*consumes* `RestoredForeign`. If any foreign-specific journal decoding lives in the
module, that boundary needs care before the cut.

## Open questions (for next iteration)

1. Is there a usable Go ACP library, or do we hand-roll JSON-RPC over stdlib the
   way `serve` hand-rolls its HTTP surface? (Prefer stdlib-only if the schema is
   small enough.)
2. Does the harness already emit token-level assistant content chunks on the
   event stream, or only whole messages? (Determines `agent_message_chunk`
   granularity — see token-usage/stream-trailer design.)
3. `session/load` replay: ACP wants history replayed as notifications on load.
   Can we reconstruct that from `RestoreSession` + the durable journal?
4. Where does the reference ACP binary live — `swe` (`cmd/acp`) or a consumer?
5. Auth model for remote: reuse harness `identity` package or ACP-native methods?
6. **Client side:** does `foreignloop` accept externally-minted `event.*` cleanly,
   or does driving it from an ACP stream need new plumbing? Confirm the
   `stopReason` → turn-done mapping actually satisfies the quiescence gap.
7. **Client side:** which foreign ACP server do we validate against first
   (e.g. Claude Code / Gemini / a reference ACP agent)?

## Phasing

The `acp` bridge and the `foreignloop` extraction are separable and can proceed in
parallel — but the client side (phase 4) depends on the extraction being done, so
sequence the extraction alongside the agent-side work.

**Agent side (looprig → ACP):**

1. **Facade + stdio, core methods:** initialize, new, prompt, cancel, update,
   permission gate. Validate protocol interoperability initially against Zed;
   no Zed-specific behavior enters the facade.
2. **Full local:** load, modes, plan, available-commands, MCP passthrough.
3. **Remote transport:** HTTP/WS binding + auth + resume.

**foreignloop extraction (parallel to 1–3):**

E. **Extract `foreignloop` into its own module** per the work-items above; verify
   the journal/restore boundary; move `claude`/`codex`; confirm existing consumers
   still wire foreign loops via `WithForeignBuilders`.

**Client side (`cli` → ACP, needs E):**

4. **`foreignloop/acp` driver, stdio:** connect to a foreign ACP server, translate
   updates → `event.*`, drive a foreign loop, render in the existing TUI.
   Implement `fs/*` + `request_permission` client caps. Validate turn boundaries
   via `stopReason`.
5. **Remote client + parity:** HTTP/WS client transport; feature parity with the
   agent side (load/resume, modes).

**Later (YAGNI now):** rewire `claude`/`codex` to drive their ACP servers through
`foreignloop/acp`, collapsing the bespoke transcript drivers toward one protocol.
