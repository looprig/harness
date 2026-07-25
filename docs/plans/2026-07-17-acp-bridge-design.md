# ACP bridge for Looprig

**Status:** approved design, not implemented. Created 2026-07-17.
**Revised 2026-07-23:** verified against the current `harness` and `foreignloops`
code; the foreign-loop module extraction is implemented and released as
`github.com/looprig/foreignloops`, so this document now names the real extracted
contracts. Incorporates techniques surveyed from mature ACP implementations
(the t3code `effect-acp` client/agent library and the grok-build ACP client).

This design adds a bidirectional Agent Client Protocol bridge without placing ACP
inside Harness:

- **Agent side:** expose a Looprig-backed product as an ACP agent.
- **Client side:** host a foreign ACP agent through the extracted foreignloops
  backend so existing Harness and TUI projections remain unchanged.

The foreign-loop module extraction was specified independently in
`2026-07-17-foreignloop-module-extraction-design.md` and has since shipped as the
`github.com/looprig/foreignloops` module (backend + claude and codex drivers).

## Goals

- Implement stable ACP wire protocol version 1 over stdio first.
- Keep Harness protocol-neutral and free of ACP dependencies.
- Let CodeRig expose its sessions over ACP through a product-owned composition
  adapter.
- Let CodeRig drive a foreign ACP agent through the same foreignloops backend used
  by the Claude and Codex drivers.
- Preserve Harness event identity, correlation, durability, permissions, workspace
  confinement, and lifecycle semantics at the bridge.
- Advertise only capabilities actually supplied by the composition root.
- Pin the ACP schema artifact used for implementation and conformance tests while
  negotiating wire compatibility through `protocolVersion` and capabilities.

## Non-goals

- Adding ACP types or handlers to Harness.
- Treating `serve.Rig` as a complete ACP session factory.
- Directly translating foreign ACP updates into Harness `event.Event` values.
- Implementing Harness MCP support as part of the bridge.
- Making draft remote transport or additional-directory proposals part of the
  stable first release.
- Making ACP session IDs authorization tokens.
- Automatically exposing every Harness or CodeRig control over ACP.
- Rewriting the Claude and Codex foreign drivers around ACP in the first release.
- Bridging interactive foreign-agent permission prompts into Harness gates in the
  first release (see "Client-side permissions" below; a fail-closed posture
  auto-responder ships first, interactive bridging is a later amendment).

## Module topology

```text
github.com/looprig/harness
        ▲              ▲
        │              │
github.com/looprig/acp  │
        ▲              │
        └──── github.com/looprig/foreignloops
```

- `harness` (currently `go 1.26.4`) owns protocol-neutral session, loop, event,
  gate, persistence, and foreign-builder contracts (`pkg/foreign`: `Builder`,
  `RestoredBuilder`, `RestoredForeign`, `EventPublisher`).
- `acp` owns protocol types, JSON-RPC, stdio transport, the Looprig agent facade,
  and a client for foreign ACP agents. Its agent facade depends on public Harness
  value types and consumer-defined interfaces.
- `foreignloops` owns the optional foreign backend and its process drivers. Its
  future `driver/acp` package depends on `acp/client` and emits
  `foreignloops/driver.Event` values.
- CodeRig is the composition root for workspace, policy, persistence, TUI, auth,
  filesystem, terminal, and optional external-capability wiring.

Harness never imports either optional module (dependency-guard tests in
`harness/pkg/foreign/deps_test.go` and `pkg/rig/optional_dependencies_test.go`
already enforce the foreignloops direction; the acp module gets the same guard).
The graph remains acyclic.

## ACP module packages

```text
github.com/looprig/acp
├── protocol/          pinned v1 types, validation, JSON-RPC envelopes and errors
├── transport/stdio/   newline-delimited stdio binding
├── agent/             Looprig-facing ACP agent facade
└── client/            ACP client connection and typed operations
```

No binary or concrete coding agent ships from the module. Products provide their
own commands and composition wiring.

The first implementation uses the standard library unless an external ACP Go SDK
is separately reviewed and explicitly approved under the repository dependency
policy. The design does not implicitly authorize adding such a dependency.

**Protocol types are generated, not hand-typed.** The upstream project publishes
versioned JSON Schema and method-metadata artifacts (`schema/*/schema*.json`,
`meta*.json`) with its GitHub releases. A repository-local generator (a Go tool in
the acp module, run manually and committed) consumes one pinned artifact revision
and emits:

- Go structs for every request, response, notification, and union member;
- a method-name constants file split into agent-served and client-served method
  sets (this split *is* the direction matrix used by the router);
- the `ProtocolVersion = 1` constant.

Generated files are committed and reviewed; regeneration against a new pinned
revision is an explicit, diffable act. The generator must normalize nullable
schema forms (`"type": ["x","null"]`) into explicit optionality — surveyed
implementations found this a real generator gotcha. Spec-declared capability
defaults are carried into the generated types so defaulting behavior matches the
spec exactly.

## Protocol-version policy

The first release targets stable ACP wire protocol version 1 and pins a reviewed
schema artifact revision for generated types, validation fixtures, and golden
tests. Upstream now ships parallel `schema/v1` and `schema/v2` artifact trees;
the artifact package version and the negotiated wire `protocolVersion` are
distinct, and only the wire version governs compatibility. Artifact revisions and
wire protocol versions are recorded separately in the module.

Every optional method or update is capability-gated. The implementation maintains
an explicit support matrix rather than claiming undifferentiated “full ACP.” At a
minimum the matrix covers:

- initialization and authentication advertisement (including `logout`);
- `session/new`, `session/load`, and `session/resume`;
- `session/list` and `session_info_update`;
- `session/prompt`, `session/cancel`, and `session/close`;
- session updates for content, tools, plans, commands, usage, and configuration;
- `session/set_config_option`, with `session/set_mode` (and `session/set_model`
  where negotiated) kept consistent as the legacy/parallel surfaces;
- permission requests (`session/request_permission`);
- elicitation (`session/elicitation`) as the client capability backing host-owned
  form gates;
- filesystem and terminal client capabilities;
- optional deletion and logout.

Unsupported optional features are omitted from capabilities and rejected with the
protocol-defined error if called. `session/delete`, `session/fork`, logout, remote
transport, and draft features are not advertised merely because their wire types
exist. `session/fork` is explicitly out of scope for the first release.

## Agent-side host boundary

ACP setup cannot depend directly on `serve.Rig` — in current code a generic
narrow factory, `serve.Rig[S serve.LiveSession, O any]` with `NewSession(ctx,
opts ...O)` and `RestoreSession(ctx, id)`. A Harness rig's option type
(`rig.SessionOption`) is opaque to ACP, workspace placement is fixed when a rig
is defined (`rig.WithExclusiveWorkspace` / `rig.WithSharedWorkspace`), and ACP
setup includes product concerns such as `cwd`, MCP servers, replay, catalogs, and
runtime configuration.

Package `agent` therefore defines small consumer-owned interfaces, conceptually:

```go
type SessionHost interface {
	NewSession(context.Context, Setup) (LiveSession, error)
	LoadSession(context.Context, SessionID, Setup) (LoadedSession, error)
	ResumeSession(context.Context, SessionID, Setup) (LiveSession, error)
}
```

`Setup` is a validated ACP-facing value containing only negotiated setup data,
including the canonical `cwd`, supported client capabilities, and MCP descriptors
when the host explicitly accepts them. It does not contain Harness rig options or
product configuration objects.

`LiveSession` contains the narrow data-plane methods needed for prompt handling.
Its natural Harness realization is the `session.Session` interface plus the
segregated lifecycle pieces of `session.SessionController`:

- stable session ID (`SessionID() uuid.UUID`);
- submit (`Submit(ctx, []content.Block) (uuid.UUID, error)` — fire-and-forget,
  returning the command ID used for correlation);
- filtered event subscription (`SubscribeEvents(event.EventFilter)
  (event.Subscription, error)`);
- gate response (`RespondGate(ctx, gate.GateResponse) error`);
- interrupt (`Interrupt(ctx) (bool, error)`);
- shutdown through a segregated close capability (Harness puts `Shutdown` on
  `SessionController`, not on `Session`; the host adapter exposes it to `agent`
  as a distinct closer interface, preserving that segregation).

Additional functions are separate optional interfaces defined where `agent`
consumes them:

- `EventReplayer`
- `SessionCatalog`
- `SessionCloser`
- `RuntimeConfigCatalog`
- `RuntimeConfigController`
- `Compactor`
- `SessionDeleter`
- `Authenticator` and `LogoutHandler`

These are consumer-owned interfaces in the acp module, not Harness types; Harness
supplies the concrete material behind them (`sessionstore` catalog and replayers,
`session.Compact`, `loop.ModeCatalog`, `loop.Controller`), and CodeRig adapts.

CodeRig implements the host adapter. It validates and canonicalizes `cwd`, chooses
or constructs the appropriate fixed-workspace rig, and supplies persistence and
policy capabilities. ACP never fabricates arbitrary Harness `rig.SessionOption`
values.

## Session identity and authorization

When CodeRig creates the underlying session, its Harness UUID is used as the ACP
session ID string. Load, resume, close, and list parse and validate that identifier
before crossing the host boundary. The facade does not maintain a second durable
identity map.

Possession of a session ID grants no authority. A remote or authenticated host
must authenticate the connection and authorize each session operation before
calling the injected host. Harness `identity` describes runtime provenance
(`identity.Coordinates`, `identity.Cause`, `identity.Agency`) and is not reused
as network authentication.

The stdio first release may rely on the launching process's local trust boundary,
but still validates all protocol inputs and does not silently broaden workspace or
session access.

## Prompt correlation and event translation

ACP `session/prompt` is a request whose response completes at a turn terminal.
Harness submit is fire-and-forget and returns a command ID. The agent facade uses
the established two-phase correlation rule:

1. Subscribe before submitting.
2. Match `TurnStarted.Header.Cause.CommandID` to the submitted command ID.
3. Capture that event's `LoopID` and `TurnID` (from `Header.Coordinates`).
4. Match progress and terminal events using both captured identifiers.
5. Complete the ACP response only on the correlated `TurnDone`, `TurnFailed`, or
   `TurnInterrupted`.

At most one prompt per ACP session is in flight at a time: a second concurrent
`session/prompt` on the same session is rejected with a typed protocol error —
never queued behind or interleaved with the running one. Interleaved activity from other
loops or prompts is ignored. Subscription loss before a terminal becomes a typed
prompt failure rather than a successful empty answer.

Harness events carry two orthogonal classifications the translator must respect:
`Class` (`Ephemeral`/`Enduring` — durability) and `EventVisibility`
(`Public`/`Internal`). Only public events are translated; ephemeral events exist
live-only and never appear in replay.

The live translator maps:

| Harness event | ACP update or result |
|---|---|
| `TokenDelta` with a text chunk | `agent_message_chunk` |
| `TokenDelta` with a thinking chunk | `agent_thought_chunk` |
| `ToolCallStarted` | `tool_call` |
| `ToolCallCompleted` | terminal `tool_call_update` |
| plan state, when supplied | full `plan` update |
| safe available commands | `available_commands_update` |
| runtime option change | `config_option_update` |
| catalog title/activity change | `session_info_update` |
| `TurnDone.Usage`, `ContextMeasured` / `ContextPressure` | `usage_update` when representable |
| `TurnDone` | `stopReason: end_turn` |
| `TurnInterrupted` | `stopReason: cancelled` |
| `TurnFailed` | sanitized ACP prompt error unless a typed cause maps exactly |

The text-versus-thinking distinction is not a field on `TokenDelta`; it lives in
the event's `content.Chunk`, and the translator classifies the chunk. Harness has
no intermediate tool-progress event — tool lifecycle is exactly
`ToolCallStarted` then `ToolCallCompleted`, so each Harness tool call produces one
`tool_call` and one terminal `tool_call_update` (with `IsError` mapped to the
failed status).

Message and tool-call IDs are deterministically derived from Harness coordinates
and tool execution IDs. Complete enduring messages are not re-emitted after their
live deltas in a way that duplicates client-visible content. Cancellation that
wins the race against a terminal is reported as `stopReason: cancelled`, never as
a transport or internal error.

## Load replay versus live streaming

Live ephemeral token and tool progress events are not durable. `session/load`
therefore uses a distinct replay translator over public enduring history. It
reconstructs user and assistant messages, completed tool calls, and session
metadata from durable events instead of pretending the live token stream can be
replayed.

CodeRig supplies a narrow event replayer backed by the session store — in current
code, `sessionstore.Store.OpenEventReplayer`, which is public-only by
construction: it filters every event whose `Visibility()` is not `Public`, so the
adapter inherits exactly the visibility filter used for live delivery. The
privileged `OpenInternalEventReplayer` / `OpenInternalRecordReplayer` variants
must never be wired into the ACP path. Internal events, private reasoning, raw
errors, secrets, and non-public gate payloads never cross the boundary.

Replayed `session/update` notifications are stamped with an `_meta` replay marker
so capable clients can distinguish reconstruction from live streaming (see
"Adopted implementation techniques").

`session/resume` restores the live controller without replaying history, matching
ACP's distinct resume semantics.

## Cancellation and close

`session/cancel` calls the live session's interrupt capability
(`session.Interrupt`). The prompt handler continues draining the correlated turn
until it observes its terminal, then returns `stopReason: cancelled`.
Cancellation does not remove the live session.

`session/close` is an orchestrated lifecycle operation, not a direct registry
delete:

1. Mark the ACP session closing and reject new prompts.
2. Cancel in-flight work with behavior equivalent to `session/cancel`.
3. Resolve or cancel outstanding permission requests owned by the connection.
4. Ensure every outstanding prompt completes with cancellation or a typed error.
5. Call the injected bounded shutdown capability (backed by
   `SessionController.Shutdown`).
6. Remove the controller from the live registry only after teardown finishes.

Durable history is preserved. A later load or resume may restore it. Delete remains
separate and is advertised only when a host supplies explicit storage and
authorization semantics.

## Permissions and host-owned gates

Harness permission and ask-user gates remain event-driven: a public
`event.GateOpened` (which carries only the public `gate.Gate` envelope, never the
private prepared payload) causes the agent facade to issue the matching ACP
`session/request_permission`, and the validated client response is returned
through `RespondGate`.

The Harness `session.GateHost` capability is segregated from the ordinary session
controller and supports host-owned form and open-URL gates (`gate.KindForm` and
`gate.KindOpenURL` with `gate.ResolverSession` only; it structurally refuses
permission and ask-user gates at open time). ACP now has a faithful negotiated
client interaction for forms: the `session/elicitation` client capability. When
the connected client advertises elicitation, host-owned form gates map to
elicitation requests; when it does not, form gates are not exposed over ACP. The
facade must not flatten host-owned forms, open-URL actions, permission gates, and
loop ask-user gates into one generic approval operation.

Sensitive open-URL targets remain live-only — the Harness gate contract already
enforces this structurally (`gate.OpenURLPayload.URL` is excluded from every
durable encoding; only the validated `DisplayOrigin` is durable, and an open-url
gate cannot be marked restorable). Only the validated display origin may enter
durable events or client-visible metadata.

## Session listing and metadata

When a `SessionCatalog` is supplied, the agent advertises `session/list`. The
adapter maps Harness catalog metadata (`sessionstore.SessionMeta`) to ACP session
information:

- Harness session UUID to `sessionId`;
- `SessionMeta.Title` to `title`;
- `SessionMeta.LastActiveAt` to `updatedAt`.

**Correction (2026-07-25):** an earlier version of this section additionally
claimed the canonical workspace root (`SessionMeta.CurrentWorkspace`) maps to
`cwd`. That is factually wrong and was never implemented that way.
`SessionMeta.CurrentWorkspace` is a `WorkspacePointer` naming a
content-addressed workspace-*snapshot* digest (`Ref`, `"v1:sha256:<64 hex>"`,
plus `EventID`/`Seq`/`Source`) — it identifies which immutable snapshot a
session's workspace was last pointed at, not a live filesystem directory path.
There is currently no field anywhere on `SessionMeta` that represents a
session's working-directory string. Because the pinned ACP schema requires
`SessionInfo.cwd` to be a non-empty absolute path, the ACP module (`acp/agent`)
resolves `cwd` from two consumer-owned sources instead: `SessionCatalog`'s own
per-entry `Cwd` field (`SessionCatalogEntry`, `acp/agent/host.go`) when a host
knows a cold session's working directory, and — always taking priority when
applicable — the already-validated `Setup.Cwd` of a session currently live in
the ACP facade's own bounded session registry. A session for which neither
source yields a cwd is omitted from the `session/list` response entirely
rather than reported with an empty or fabricated `cwd`. See
`acp/agent/list.go`'s package doc ("cwd resolution", "Pagination under cwd
omission") for the full mechanism and its pagination-slot-consumption
tradeoff, and `acp/agent/host.go`'s `SessionCatalogEntry` doc for the
contract a host must uphold.

A durable fix — persisting a real per-session cwd inside Harness's own
`sessionstore.SessionMeta`, so a host would not need to track this mapping
itself — is a legitimate future Harness-side follow-up. It is out of scope
for the ACP bridge implementation plan (Harness is read-only there).

ACP pagination cursors remain opaque even if the current Harness catalog returns a
complete deterministic list. The facade owns bounded page construction and cursor
validation; callers cannot depend on Harness catalog key layout.

Live title or activity changes may produce `session_info_update` after the
underlying durable/catalog update is observable. ACP metadata is a projection, not
a second source of truth.

## Session configuration

ACP session config options are the preferred configuration surface. ACP models
them as a discriminated union with well-known categories (`mode`, `model`,
`thought_level`) plus free-form categories; mode is therefore implemented as a
config option, with the legacy `session/set_mode` method kept consistent with it
for older clients.

Configuration is supplied through optional product interfaces:

- Harness `loop.ModeCatalog` (`Modes() []ModeName`) and `loop.Controller`
  (`SetMode`, `Change` with `loop.ChangeModel` / `loop.ChangeEffort`) for mode,
  model, and effort application.
- CodeRig model and effort catalogs/controllers (Harness deliberately has no
  model/effort catalog; the catalog is product-owned).
- CodeRig security access choices, when the host explicitly exposes them.

The facade validates config IDs and values against the latest catalog before
applying a change, and config writes are idempotent: setting an option to its
current value succeeds without side effects or spurious updates. It returns the
complete resulting option state so dependent choices remain coherent. It does not
expose arbitrary internal controls or assume that every product supports the same
model, effort, or access choices.

## MCP and external capabilities

ACP setup contains MCP server descriptors, but Harness MCP support remains a
separate feature — MCP lives in the sibling `github.com/looprig/mcp` module,
which imports Harness (never the reverse). The ACP agent accepts or advertises
MCP setup only when CodeRig supplies a reviewed MCP composition capability.
Otherwise it rejects or omits the feature according to ACP rather than silently
ignoring requested servers.

When external MCP capabilities are composed, CodeRig computes the external
capability revision included in the Harness configuration fingerprint
(`event.ConfigFingerprint.ExternalCapabilityRev`; the canonical producer is
`mcpharness.Manager.ConfigDigest`). Restoring a session under a changed external
tool/server identity follows Harness config-drift policy
(`event.AssessDrift` / `session.RestoreDecider` / `event.ConfigurationAdopted`);
the ACP bridge must not bypass or overwrite that check.

Draft additional workspace directories are not part of the stable first release.
If later implemented, they are capability-gated, canonicalized, explicitly
authorized, and included in every load/resume request that activates them. Omitted
directories never implicitly regain access.

## Client side and foreignloops driver

`acp/client` owns the ACP connection, lifecycle, typed calls, update delivery, and
client capability dispatch. It does not know about Harness events or the TUI.

The extracted module adds:

```text
github.com/looprig/foreignloops/driver/acp
```

That package implements the existing `foreignloops/driver.Agent` contract:

```go
type Agent interface {
	Spawn(context.Context, Turn) (Stream, error)
}

type Stream interface {
	Events() <-chan Event
	History() (History, error)
	Close() error
}
```

Its flow is:

```text
foreign ACP agent
      │ ACP JSON-RPC
      ▼
acp/client
      │ typed ACP updates and prompt result
      ▼
foreignloops/driver/acp
      │ driver.Event
      ▼
foreignloops/backend
      │ harness event.Event
      ▼
Harness session and TUI
```

The ACP driver never creates Harness events. It converts ACP text/thought chunks,
tool calls, session identity, and prompt terminals into normalized driver events.
ACP plan updates are dropped in the first release: neither the driver event
contract nor Harness has a plan concept for foreign loops, and inventing one is a
separate amendment — the limitation is documented, not silent. The backend
remains the sole owner of Harness event stamping, correlation, transcript commit,
and quiescence.

### Connection lifetime versus turn lifetime

The claude and codex drivers spawn one subprocess per turn; an ACP agent is a
long-lived subprocess hosting sessions across turns. The ACP driver therefore
inverts the mapping while keeping the `driver.Agent` contract:

- The `driver/acp` agent owns a persistent subprocess and `acp/client`
  connection. It is started lazily on first `Spawn` through a start-once state
  machine (not-started → starting → started, with a failed start resetting so a
  retry is possible).
- `initialize` and capability negotiation happen once at connection
  establishment, inside the start-once path — not per turn.
- Each `Spawn` is one prompt turn on that shared connection: `Turn.StartNew`
  issues `session/new` (binding the ACP session id, surfaced as `KindInit` so the
  backend's existing late-bind path applies); otherwise the existing ACP session
  is reused. If the subprocess restarted since the session was created, the
  driver issues `session/load` first — which requires the foreign agent to have
  advertised the `loadSession` capability; without it, resuming a prior session
  fails with a typed driver error rather than silently starting a fresh one.
- `Stream.Close()` cancels the in-flight prompt (`session/cancel`) and detaches
  the per-turn update routing; it does not kill the subprocess.
- Terminating the subprocess is a separate concern. The driver's agent type
  additionally implements a `Shutdown(context.Context) error` capability. The
  foreignloops backend is amended to invoke an optional
  `interface{ Shutdown(context.Context) error }` on its configured agent when the
  loop context ends, so connection teardown is owned by the loop lifecycle rather
  than leaked to the composition root. Claude and codex agents do not implement
  it and are unaffected.
- `Stream.History()` returns `History{Available: false}`: ACP has no separate
  authoritative transcript, so the backend's existing fallback (commit the live
  assistant messages) applies unchanged.

### Driver event contract amendment: typed stop reasons

The current `driver.Event` has exactly eight kinds and its only failure channel
is `KindTerminalError` with an untyped `ErrText`. ACP prompt terminals are richer
(`end_turn`, `max_tokens`, `max_turn_requests`, `refusal`, `cancelled`). The
driver contract gains a typed stop reason rather than collapsing these into a
string:

```go
type StopReason uint8

const (
	StopUnspecified StopReason = iota // zero value; claude/codex emit this
	StopEndTurn
	StopCancelled
	StopRefusal
	StopMaxTokens
	StopMaxTurnRequests
)
```

`driver.Event` gains a `Stop StopReason` field, populated on terminal kinds. The
backend maps: `StopEndTurn` (and `StopUnspecified` on `KindTerminalOK`) →
`TurnDone`; `StopCancelled` → `TurnInterrupted`; `StopRefusal`,
`StopMaxTokens`, `StopMaxTurnRequests` → `TurnFailed` with a typed diagnostic
cause. Existing claude/codex drivers are untouched (zero value preserves their
behavior). When a prompt completes, the backend emits the same terminal and idle
boundary required of every foreign loop so the Harness hub can reach
`SessionIdle`.

### Prerequisite: foreign loops must emit `LoopIdle`

Verified against current code: the Harness hub derives `SessionIdle` by tracking
an active-loop set — `TurnStarted` adds a loop key and only `event.LoopIdle`
removes it. The foreignloops backend publishes `TurnStarted` and the turn
terminals but never publishes `LoopIdle`, so a foreign **primary** loop's key is
never removed and the session can never quiesce (the known foreign-loop
`SessionIdle` gap). This predates ACP and affects claude/codex too.

Fixing it is a prerequisite for the ACP driver phase: the foreignloops backend
publishes `event.LoopIdle` after publishing a turn terminal when it parks with an
empty managed-input queue, matching the native loop's boundary semantics. The fix
lands in foreignloops with cross-module tests in `github.com/looprig/tests`
proving a foreign primary session reaches `SessionIdle`.

### Client-side permissions (first release: fail-closed posture)

A foreign ACP agent may call `session/request_permission` at any time. The
current driver contract has no interactive channel — permission is the static
`driver.PermissionPosture` passed per turn — and the first release does not widen
it. `driver/acp` instead installs a posture-derived auto-responder into
`acp/client`:

- `PostureDefault`: select a reject-class option (fail closed).
- `PostureAcceptEdits`: select an `allow_once`-class option only when the
  permission request's associated tool call is edit-class (ACP `tool_call.kind`
  edit/move/delete — file-mutation kinds); execute-, fetch-, and unknown-kind
  requests are rejected. This keeps the posture's meaning ("accept edits") exact
  instead of widening it into accept-everything.
- Never select an `allow_always`-class option automatically.

Bridging foreign permission requests into interactive Harness gates requires new
driver event kinds and a response path through the backend; that is a separate
later amendment to the foreignloops driver contract, not part of this release.

## ACP client capabilities and authority

A foreign ACP agent may call back into its client for filesystem, terminal, and
permission operations. These capabilities are injected into `acp/client`; the
protocol client does not implement them directly.

CodeRig supplies adapters backed by its existing workspace, tools, confinement,
and gate policy:

```text
CodeRig workspace/tools/confinement
          │
          ▼
safe FS, terminal, and permission adapters
          │
          ▼
acp/client capability dispatcher
```

The default advertises none. A capability is advertised only when its handler is
present and its authority is explicit. Filesystem handlers canonicalize every path,
enforce the effective root set, prevent symlink or mount escapes, and fail closed
on ambiguity. Terminal handlers use bounded contexts and supervised process-group
teardown, and bundle the follow-up operations (`output`, `wait_for_exit`, `kill`,
`release`) into one handle pre-bound to the terminal id so callers never re-thread
ids. Permission handlers preserve Harness gate identity and never grant more
authority than CodeRig's configured security limit.

## Slash commands and compaction

ACP-native methods are used whenever their semantics match. Product operations
without a native method may be exposed as explicitly registered safe slash
commands.

When a compaction capability is supplied, the agent may advertise `/compact`.
Receiving that exact command invokes the session's focused/active-loop compaction
control — in current code `session.Compact(ctx)` / `session.CompactToLoop`, which
return a command ID — rather than submitting it as model text. The prompt
completes only after the matching compaction outcome, correlated through the
compaction events (`CompactWaiterResolved` / `CompactWaiterRejected`, with
`CompactionCommitted` / `CompactionRejected` as the durable outcomes). Internal
rejection details and summary content are sanitized before crossing the ACP
boundary.

No other internal Harness command is automatically exposed.

## Transport scope

The stable first release supports newline-delimited JSON-RPC over stdio:

- the client launches the agent subprocess;
- messages are UTF-8 and contain no embedded framing newline;
- stdout contains ACP messages only;
- logs go to stderr;
- reads, writes, message sizes, queues, and shutdown are bounded.

Streamable HTTP and WebSocket remain an experimental later design because the ACP
remote binding is still draft. They are not described as a simple transport swap:
remote operation introduces authentication, connection identity, session
ownership, independent backpressure, reconnect/resume, origin policy, TLS, and
resource limits. A future remote package requires its own approved amendment and
conformance target.

## Adopted implementation techniques

These mechanisms were verified in mature ACP implementations (the t3code
`effect-acp` library and runtime, and the grok-build Rust ACP client) and are
adopted as implementation requirements where they harden the protocol layer:

- **Single outbound writer.** All encoded frames pass through one bounded queue
  drained by a single writer goroutine into stdout, so concurrent senders can
  never interleave or tear a frame.
- **Pending-request table with fail-all on termination.** Outgoing requests park
  in an ID-keyed table; connection or process death rejects every outstanding
  request with a typed transport error. No request may hang past the connection.
- **Disjoint request-ID spaces.** IDs minted by the core dispatcher and IDs
  minted by extension paths come from disjoint ranges so they cannot collide on
  one wire.
- **Buffered early notifications.** `session/update` notifications that arrive
  before a consumer subscribes are buffered (bounded) and flushed on
  subscription, eliminating the startup race that silently drops the first
  updates.
- **Typed extension passthrough.** Unknown methods and `_meta` payloads flow
  through registrable typed handlers plus a catch-all, so vendor-private methods
  are handled without polluting the core protocol tables. Extension metadata is
  validated like every other input.
- **Replay marking and dedup metadata.** The agent facade stamps every
  `session/update` `_meta` with a deterministic event ID (derived from Harness
  coordinates) and marks replayed updates (`isReplay`) during `session/load`.
  This gives capable clients an idempotency cursor and cleanly separates
  reconstruction from live streaming; incapable clients can ignore `_meta`
  entirely.
- **Replay-idle tolerance in the client.** When `acp/client` drives a foreign
  agent's `session/load`, it tolerates agents that stream replay updates but are
  slow to resolve the load call: replay updates are consumed and the load result
  is awaited under a bounded deadline. (Wall-clock-heuristic synthesis of a load
  response is not adopted; a hung load fails typed at the deadline.)
- **Cancellation-as-success.** A cancelled prompt resolves as
  `stopReason: cancelled`, never as an error, on both agent and client sides.
- **Named JSON-RPC error constructors.** One table maps semantic failures to the
  JSON-RPC codes (-32700, -32600, -32601, -32602, -32603, and ACP's -32000
  auth-required / resource-not-found), with sanitized public data and typed
  internal causes retained locally.
- **Compact schema-error diagnostics.** Validation failures are summarized
  (issue count, issue kinds, deepest path) rather than dumping raw payloads or
  full issue trees into errors or logs.
- **Start-once connection state machine** for the client's lazy subprocess
  startup, with concurrent starters awaiting one in-flight attempt and failed
  starts resetting for retry.

## Validation, errors, and security

All JSON-RPC envelopes and ACP values are validated before reaching product code.
The protocol layer enforces:

- bounded message size and nesting;
- valid JSON-RPC version, IDs, methods, and request/response direction (the
  generated agent-served/client-served method sets are the routing authority);
- duplicate-field and unknown-union handling according to the pinned schema;
- strict session ID and absolute/canonical path validation;
- capability checks before optional operations;
- sanitized public errors with typed internal causes retained locally;
- bounded concurrent prompts, sessions, subscriptions, and client requests;
- cancellation and teardown that cannot leak goroutines or subprocesses.

No prompt content, filesystem data, credentials, URLs containing secrets, private
reasoning, raw provider errors, or internal events are logged by default.

## Testing and interoperability

The ACP module includes:

- schema and JSON golden tests for every supported request, response, notification,
  union, and error, generated-fixture-driven from the pinned artifact;
- capability-matrix tests proving unsupported methods are not advertised;
- correlation tests with interleaved sessions, loops, prompts, gates, and tools;
- replay tests proving durable reconstruction does not duplicate live deltas;
- cancel/close race tests and connection-loss tests;
- fuzz tests for JSON-RPC framing, union decoding, schema validation, IDs, paths,
  and extension metadata;
- subprocess integration tests for stdio framing and graceful teardown;
- a scriptable in-repo mock ACP peer — a real subprocess built on the module's
  own agent half — exercising permissions, elicitation, plan updates, typed and
  unknown extension traffic, with environment-driven fault injection (malformed
  output, immediate exit, mid-stream death). The client test suite drives it as
  a conformance harness;
- wire-key round-trip tests guaranteeing `_meta` producer and consumer key names
  cannot drift;
- cross-module tests proving `driver/acp` emits driver events and never Harness
  events, and that a foreign ACP primary session reaches `SessionIdle`;
- interoperability tests against at least one maintained ACP client and one
  maintained ACP agent, with Zed as the initial agent-side client target.

Process-boundary tests are tagged `integration`, race-enabled, and use bounded
contexts. The module follows the same build, formatting, vet, staticcheck, gosec,
govulncheck, module verification, vendor, and secure checks as Harness.

## Delivery phases

1. **Protocol and stdio:** pinned artifact + generator, v1 types, validation,
   JSON-RPC, stdio, limits, fuzzing, conformance fixtures, and the mock peer.
2. **Agent core:** initialization, new session, prompt/update correlation,
   cancellation, permission gates, and close through an injected CodeRig host.
3. **Durable sessions:** load replay, resume, list, metadata updates, and optional
   delete when a real deletion capability exists.
4. **Runtime controls:** config options, legacy mode compatibility, safe commands,
   compaction, and usage projections.
5. **Foreign client:** `acp/client`, injected client capabilities, the
   foreignloops prerequisites (`LoopIdle` emission, typed stop reasons, optional
   agent `Shutdown`), and `foreignloops/driver/acp` over stdio.
6. **Interoperability and release:** maintained client/agent matrices, failure
   injection, dependency guards, and module releases.
7. **Later amendments:** MCP composition after Harness support exists; interactive
   foreign permission-gate bridging through a widened driver contract; remote
   HTTP/WebSocket after its ACP binding and Looprig security design are approved.

## Acceptance criteria

- Harness has no ACP imports, protocol types, or transport code.
- ACP agent setup goes through a typed product `SessionHost`, not arbitrary rig
  options or direct workspace mutation.
- The agent side correctly correlates prompts, translates public events, replays
  durable history, cancels, closes, and advertises only supplied capabilities.
- The client side emits normalized foreign-driver events; only the foreignloops
  backend mints Harness events.
- A foreign ACP primary session reaches `SessionIdle` (and the pre-existing
  claude/codex quiescence gap is closed by the same backend fix).
- ACP terminal reasons survive as typed driver stop reasons and map to the
  correct Harness terminals; cancellation maps to `TurnInterrupted`, not a
  failure.
- Client-side permission handling fails closed under `PostureDefault` and never
  auto-selects an always-allow option.
- CodeRig, not the protocol packages, owns filesystem, terminal, auth, workspace,
  MCP, and security authority.
- Stable stdio behavior passes schema, fuzz, race, security, process, and external
  interoperability tests, including the scriptable mock-peer conformance suite.
- Draft remote and additional-directory behavior is not presented as stable ACP.
- Existing CodeRig/TUI session browsing, runtime controls, replay, gates, and
  external-capability drift rules are reused rather than duplicated.
