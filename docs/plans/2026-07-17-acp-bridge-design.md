# ACP bridge for Looprig

**Status:** approved design, not implemented. Created 2026-07-17.

This design adds a bidirectional Agent Client Protocol bridge without placing ACP
inside Harness:

- **Agent side:** expose a Looprig-backed product as an ACP agent.
- **Client side:** host a foreign ACP agent through the extracted foreign-loop
  backend so existing Harness and TUI projections remain unchanged.

The foreign-loop module extraction is specified independently in
`2026-07-17-foreignloop-module-extraction-design.md` and contains no ACP work.

## Goals

- Implement stable ACP protocol version 1 over stdio first.
- Keep Harness protocol-neutral and free of ACP dependencies.
- Let CodeRig expose its sessions over ACP through a product-owned composition
  adapter.
- Let CodeRig drive a foreign ACP agent through the same foreign-loop backend used
  by other external agents.
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

## Module topology

```text
github.com/looprig/harness
        ▲              ▲
        │              │
github.com/looprig/acp  │
        ▲              │
        └──── github.com/looprig/foreignloop
```

- `harness` owns protocol-neutral session, loop, event, gate, persistence, and
  foreign-builder contracts.
- `acp` owns protocol types, JSON-RPC, stdio transport, the Looprig agent facade,
  and a client for foreign ACP agents. Its agent facade depends on public Harness
  value types and consumer-defined interfaces.
- `foreignloop` owns the optional foreign backend. Its future `driver/acp`
  package depends on `acp/client` and emits `foreignloop/driver.Event` values.
- CodeRig is the composition root for workspace, policy, persistence, TUI, auth,
  filesystem, terminal, and optional external-capability wiring.

Harness never imports either optional module. The graph remains acyclic.

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

## Protocol-version policy

The first release targets stable ACP wire protocol version 1 and pins a reviewed
schema artifact revision for generated types, validation fixtures, and golden
tests. Artifact package versions and wire protocol versions are recorded
separately.

Every optional method or update is capability-gated. The implementation maintains
an explicit support matrix rather than claiming undifferentiated “full ACP.” At a
minimum the matrix covers:

- initialization and authentication advertisement;
- `session/new`, `session/load`, and `session/resume`;
- `session/list` and `session_info_update`;
- `session/prompt`, `session/cancel`, and `session/close`;
- session updates for content, tools, plans, commands, usage, and configuration;
- `session/set_config_option`;
- permission requests;
- filesystem and terminal client capabilities;
- optional deletion and logout.

Unsupported optional features are omitted from capabilities and rejected with the
protocol-defined error if called. `session/delete`, logout, remote transport, and
draft features are not advertised merely because their wire types exist.

## Agent-side host boundary

ACP setup cannot depend directly on `serve.Rig`. A Harness rig's option type is
opaque to ACP, workspace placement is fixed when a rig is defined, and ACP setup
includes product concerns such as `cwd`, MCP servers, replay, catalogs, and runtime
configuration.

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

`LiveSession` contains the narrow data-plane methods needed for prompt handling:

- stable session ID;
- submit;
- filtered event subscription;
- gate response;
- interrupt;
- shutdown through a segregated close capability.

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
calling the injected host. Harness `identity` describes runtime provenance and is
not reused as network authentication.

The stdio first release may rely on the launching process's local trust boundary,
but still validates all protocol inputs and does not silently broaden workspace or
session access.

## Prompt correlation and event translation

ACP `session/prompt` is a request whose response completes at a turn terminal.
Harness submit is fire-and-forget and returns a command ID. The agent facade uses
the established two-phase correlation rule:

1. Subscribe before submitting.
2. Match `TurnStarted.Header.Cause.CommandID` to the submitted command ID.
3. Capture that event's `LoopID` and `TurnID`.
4. Match progress and terminal events using both captured identifiers.
5. Complete the ACP response only on the correlated `TurnDone`, `TurnFailed`, or
   `TurnInterrupted`.

Interleaved activity from other loops or prompts is ignored. Subscription loss
before a terminal becomes a typed prompt failure rather than a successful empty
answer.

The live translator maps:

| Harness event | ACP update or result |
|---|---|
| text `TokenDelta` | `agent_message_chunk` |
| thinking `TokenDelta` | `agent_thought_chunk` |
| `ToolCallStarted` | `tool_call` |
| tool progress/completion | `tool_call_update` |
| plan state, when supplied | full `plan` update |
| safe available commands | `available_commands_update` |
| runtime option change | `config_option_update` |
| catalog title/activity change | `session_info_update` |
| usage/context observation | `usage_update` when representable |
| `TurnDone` | `stopReason: end_turn` |
| `TurnInterrupted` | `stopReason: cancelled` |
| `TurnFailed` | sanitized ACP prompt error unless a typed cause maps exactly |

Message and tool-call IDs are deterministically derived from Harness coordinates
and tool execution IDs. Complete enduring messages are not re-emitted after their
live deltas in a way that duplicates client-visible content.

## Load replay versus live streaming

Live ephemeral token and tool progress events are not durable. `session/load`
therefore uses a distinct replay translator over public enduring history. It
reconstructs user and assistant messages, completed tool calls, and session
metadata from durable events instead of pretending the live token stream can be
replayed.

CodeRig supplies a narrow event replayer backed by the session store. The adapter
must apply the same public visibility filter used for live delivery before
producing ACP notifications. Internal events, private reasoning, raw errors,
secrets, and non-public gate payloads never cross the boundary.

`session/resume` restores the live controller without replaying history, matching
ACP's distinct resume semantics.

## Cancellation and close

`session/cancel` calls the live session's interrupt capability. The prompt handler
continues draining the correlated turn until it observes its terminal, then returns
`stopReason: cancelled`. Cancellation does not remove the live session.

`session/close` is an orchestrated lifecycle operation, not a direct registry
delete:

1. Mark the ACP session closing and reject new prompts.
2. Cancel in-flight work with behavior equivalent to `session/cancel`.
3. Resolve or cancel outstanding permission requests owned by the connection.
4. Ensure every outstanding prompt completes with cancellation or a typed error.
5. Call the injected bounded shutdown capability.
6. Remove the controller from the live registry only after teardown finishes.

Durable history is preserved. A later load or resume may restore it. Delete remains
separate and is advertised only when a host supplies explicit storage and
authorization semantics.

## Permissions and host-owned gates

Harness permission and ask-user gates remain event-driven: a public gate-open event
causes the agent facade to issue the matching ACP client request, and the validated
client response is returned through `RespondGate`.

The July 17 Harness `session.GateHost` capability is segregated from the ordinary
session controller and supports host-owned form and open-URL gates. ACP integrations
may use it for external services such as MCP elicitation or authentication only
when ACP has a faithful negotiated client interaction. The facade must not flatten
host-owned forms, open-URL actions, permission gates, and loop ask-user gates into
one generic approval operation.

Sensitive open-URL targets remain live-only. Only their validated display origin
may enter durable events or client-visible metadata under the Harness gate contract.

## Session listing and metadata

When a `SessionCatalog` is supplied, the agent advertises `session/list`. The
adapter maps Harness catalog metadata to ACP session information:

- Harness session UUID to `sessionId`;
- canonical workspace root to `cwd`;
- title to `title`;
- last activity to `updatedAt`.

ACP pagination cursors remain opaque even if the current Harness catalog returns a
complete deterministic list. The facade owns bounded page construction and cursor
validation; callers cannot depend on Harness catalog key layout.

Live title or activity changes may produce `session_info_update` after the
underlying durable/catalog update is observable. ACP metadata is a projection, not
a second source of truth.

## Session configuration

ACP session config options are the preferred configuration surface. Legacy modes
may be sent alongside config options for older clients, with both kept consistent.

Configuration is supplied through optional product interfaces:

- Harness `loop.ModeCatalog` and `loop.Controller` for mode.
- CodeRig model and effort catalogs/controllers.
- CodeRig security access choices, when the host explicitly exposes them.

The facade validates config IDs and values against the latest catalog before
applying a change. It returns the complete resulting option state so dependent
choices remain coherent. It does not expose arbitrary internal controls or assume
that every product supports the same model, effort, or access choices.

## MCP and external capabilities

ACP setup contains MCP server descriptors, but Harness MCP support remains a
separate feature. The ACP agent accepts or advertises MCP setup only when CodeRig
supplies a reviewed MCP composition capability. Otherwise it rejects or omits the
feature according to ACP rather than silently ignoring requested servers.

When external MCP capabilities are composed, CodeRig computes the external
capability revision included in the Harness configuration fingerprint. Restoring a
session under a changed external tool/server identity follows Harness config-drift
policy; the ACP bridge must not bypass or overwrite that check.

Draft additional workspace directories are not part of the stable first release.
If later implemented, they are capability-gated, canonicalized, explicitly
authorized, and included in every load/resume request that activates them. Omitted
directories never implicitly regain access.

## Client side and foreign-loop driver

`acp/client` owns the ACP connection, lifecycle, typed calls, update delivery, and
client capability dispatch. It does not know about Harness events or the TUI.

After the foreign-loop extraction, the external module adds:

```text
github.com/looprig/foreignloop/driver/acp
```

That package implements `foreignloop/driver.Agent`. Its flow is:

```text
foreign ACP agent
      │ ACP JSON-RPC
      ▼
acp/client
      │ typed ACP updates and prompt result
      ▼
foreignloop/driver/acp
      │ driver.Event
      ▼
foreignloop/backend
      │ harness event.Event
      ▼
Harness session and TUI
```

The ACP driver never creates Harness events. It converts ACP text/thought chunks,
tool calls, plan updates, session identity, and prompt terminals into normalized
driver events. The backend remains the sole owner of Harness event stamping,
correlation, transcript commit, and quiescence.

The driver event contract must represent ACP terminal reasons without collapsing
every non-success result into an untyped string. The backend maps those reasons to
the closest existing Harness terminal while retaining typed diagnostic causes.
When a prompt completes, the backend emits the same terminal and idle boundary
required of every foreign loop so the Harness hub can reach `SessionIdle`.

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
teardown. Permission handlers preserve Harness gate identity and never grant more
authority than CodeRig's configured security limit.

## Slash commands and compaction

ACP-native methods are used whenever their semantics match. Product operations
without a native method may be exposed as explicitly registered safe slash
commands.

When a compaction capability is supplied, the agent may advertise `/compact`.
Receiving that exact command invokes the session's focused/active-loop compaction
control rather than submitting it as model text. The prompt completes only after
the matching compaction outcome. Internal rejection details and summary content
are sanitized before crossing the ACP boundary.

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

## Validation, errors, and security

All JSON-RPC envelopes and ACP values are validated before reaching product code.
The protocol layer enforces:

- bounded message size and nesting;
- valid JSON-RPC version, IDs, methods, and request/response direction;
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
  union, and error;
- capability-matrix tests proving unsupported methods are not advertised;
- correlation tests with interleaved sessions, loops, prompts, gates, and tools;
- replay tests proving durable reconstruction does not duplicate live deltas;
- cancel/close race tests and connection-loss tests;
- fuzz tests for JSON-RPC framing, union decoding, schema validation, IDs, paths,
  and extension metadata;
- subprocess integration tests for stdio framing and graceful teardown;
- cross-module tests proving `driver/acp` emits driver events and never Harness
  events;
- interoperability tests against at least one maintained ACP client and one
  maintained ACP agent, with Zed as the initial agent-side client target.

Process-boundary tests are tagged `integration`, race-enabled, and use bounded
contexts. The module follows the same build, formatting, vet, staticcheck, gosec,
govulncheck, module verification, vendor, and secure checks as Harness.

## Delivery phases

1. **Protocol and stdio:** pinned v1 types, validation, JSON-RPC, stdio, limits,
   fuzzing, and conformance fixtures.
2. **Agent core:** initialization, new session, prompt/update correlation,
   cancellation, permission gates, and close through an injected CodeRig host.
3. **Durable sessions:** load replay, resume, list, metadata updates, and optional
   delete when a real deletion capability exists.
4. **Runtime controls:** config options, legacy mode compatibility, safe commands,
   compaction, and usage projections.
5. **Foreign client:** `acp/client`, injected client capabilities, and
   `foreignloop/driver/acp` over stdio.
6. **Interoperability and release:** maintained client/agent matrices, failure
   injection, dependency guards, and module releases.
7. **Later amendments:** MCP composition after Harness support exists; remote
   HTTP/WebSocket after its ACP binding and Looprig security design are approved.

## Acceptance criteria

- Harness has no ACP imports, protocol types, or transport code.
- ACP agent setup goes through a typed product `SessionHost`, not arbitrary rig
  options or direct workspace mutation.
- The agent side correctly correlates prompts, translates public events, replays
  durable history, cancels, closes, and advertises only supplied capabilities.
- The client side emits normalized foreign-driver events; only the foreign backend
  mints Harness events.
- CodeRig, not the protocol packages, owns filesystem, terminal, auth, workspace,
  MCP, and security authority.
- Stable stdio behavior passes schema, fuzz, race, security, process, and external
  interoperability tests.
- Draft remote and additional-directory behavior is not presented as stable ACP.
- Existing CodeRig/TUI session browsing, runtime controls, replay, gates, and
  external-capability drift rules are reused rather than duplicated.
