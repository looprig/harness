# Modular MCP client and Harness integration

**Status:** approved design; not implemented.

**Date:** 2026-07-16.

## Purpose

Looprig needs an MCP client so agents built with Harness can consume existing MCP
servers. A Session may connect to multiple servers over stdio or network
transports. Individual bindings may be shared across the Session or owned by one
Loop.

This is a client design only. Looprig will not expose Harness agents as MCP
servers in this work.

The implementation must meet or exceed the practical MCP client support in
Codex. It also adds Looprig-specific ownership, delegation, safe-boundary
catalog adoption, and durable configuration identity.

## Decisions

- MCP is a separate module, `github.com/looprig/mcp`.
- The module contains no Go package at its root.
- Harness does not import MCP.
- The TUI does not own MCP configuration, connections, discovery, or auth.
- `mcp/pkg/harness` is the optional adapter from MCP capabilities to Harness
  tools, gates, events, and configuration identity.
- Applications such as CodeRig compose MCP definitions, Harness, and the TUI or
  another presentation client.
- One owner may have multiple independently named MCP bindings.
- Every binding is either Session-scoped or Loop-scoped.
- A Session-scoped binding owns one logical MCP connection shared by the Loops
  allowed to use it.
- A Loop-scoped binding owns a connection private to one Loop.
- Delegation never copies or transfers the parent Loop's bindings.
- A delegate may have its own Loop-scoped bindings and may use Session-scoped
  bindings allowed by their selectors.
- Catalog notifications fetch a candidate catalog immediately.
- Model-facing toolsets adopt candidates only at a safe Loop boundary.
- Active turns keep the immutable catalog generation with which they started.
- Removed or incompatibly changed tools produce structured unavailable results;
  they do not make the Session unrestorable.
- Required MCP startup failures prevent the owning Session or Loop from becoming
  ready. Optional failures degrade only that binding.
- Sampling is supported only through an explicit application-supplied handler.
  It is not advertised when no handler and policy are installed.

## Terminology

This design avoids the overloaded term "MCP host."

- **Client:** one initialized MCP protocol peer connected to one server.
- **Server definition:** immutable, secret-free connection and policy
  configuration.
- **Binding:** a named server definition attached to a Session or Loop scope.
- **Client set:** the collection of live clients owned by one scope.
- **Catalog generation:** an immutable, validated snapshot of a server's tools,
  prompts, resources, templates, instructions, and negotiated capabilities.
- **Candidate generation:** a refreshed snapshot not yet visible to a Loop.
- **Adopted generation:** the snapshot used to construct a Loop's model-facing
  capabilities.
- **Turn snapshot:** the fixed tool view captured when a model turn starts.

## Module topology

```text
                          application / CodeRig
                           /                 \
                          v                   v
               github.com/looprig/mcp      github.com/looprig/harness
                          \                   /
                           \                 /
                            v               v
                              mcp/pkg/harness
                                      |
                                      v
                            composed Harness Rig
                                      |
                                      v
                              TUI / HTTP / headless
```

The dependency graph is acyclic:

```text
mcp/pkg/client             -> stdlib + mcp/internal
mcp/pkg/auth               -> stdlib
mcp/pkg/transport/*        -> mcp/pkg/client (+ auth where needed)
mcp/pkg/harness            -> mcp/pkg/client + harness
harness                    -> no MCP dependency
tui                        -> harness, no MCP dependency
application                -> harness + mcp/pkg/harness + chosen transports
```

The standalone module layout is:

```text
mcp/
├── go.mod
├── README.md
├── CLAUDE.md
├── pkg/
│   ├── client/
│   ├── auth/
│   ├── transport/
│   │   ├── stdio/
│   │   ├── streamablehttp/
│   │   └── sse/
│   └── harness/
└── internal/
    ├── protocol/
    ├── lifecycle/
    ├── catalog/
    ├── content/
    └── limits/
```

`pkg/transport/sse` is an opt-in compatibility transport for legacy servers.
Stdio and Streamable HTTP are the standard transports and are mandatory.

There are no `.go` files at `mcp/` itself. Consumers import a focused public
package rather than a root facade.

## Public package responsibilities

### `pkg/client`

The protocol-neutral public client API owns:

- MCP initialization and capability negotiation;
- protocol version selection;
- client and server identity;
- lifecycle and health state;
- requests, responses, notifications, progress, and cancellation;
- tools, prompts, resources, templates, and server instructions;
- elicitation and optional sampling callbacks;
- catalog generations and change notifications;
- request and result bounds;
- typed errors;
- explicit shutdown.

It does not know about Harness Sessions, Loops, tools, gates, or journals.

The conceptual API is:

```go
type Client struct { /* private */ }

type Definition struct {
	Name         Name
	Transport    TransportFactory
	Timeouts     Timeouts
	Limits       Limits
	Capabilities ClientCapabilities
	ToolFilter   ToolFilter
}

type Handlers struct {
	Elicitation ElicitationHandler
	Sampling    SamplingHandler
	Log         LogHandler
	Event       EventHandler
}

func Connect(
	context.Context,
	Definition,
	Handlers,
) (*Client, error)
```

The exact Go shapes may change during implementation planning, but the
responsibility boundary is fixed. `Definition` is immutable after validation.
Credentials are obtained through narrow providers at connection time and are
not retained in a printable configuration value.

### `pkg/auth`

`auth` owns reusable OAuth and bearer-token contracts:

- protected-resource and authorization-server discovery;
- dynamic client registration where supported;
- authorization-code flow with PKCE;
- access-token refresh;
- token-store interfaces;
- browser/open-URL callback interfaces;
- redacted auth status and typed failures.

Token persistence is application supplied:

```go
type TokenStore interface {
	Load(context.Context, Key) (TokenSet, error)
	Store(context.Context, Key, TokenSet) error
	Delete(context.Context, Key) error
}
```

The module must not assume a specific keyring, database, CLI, or TUI. Token
values, client secrets, authorization codes, verifiers, and bearer headers never
enter events, catalogs, fingerprints, or logs.

Stdio credentials are injected through an explicit environment allowlist.
Network credentials are injected through auth or header providers. A complete
process environment or header map is not copied into audit data.

### `pkg/transport/stdio`

The stdio transport:

- launches a direct executable plus argv, never a shell command string;
- accepts an explicit working directory;
- builds an allowlisted environment;
- keeps stdout exclusively for MCP framing;
- captures bounded stderr for diagnostics;
- owns the child process and its process group where the platform permits;
- terminates and reaps the process on shutdown;
- cancels startup and in-flight work through context;
- reports premature process exit as a typed transport failure.

The application may inject a process launcher or confinement wrapper. The MCP
module does not import Looprig's sandbox or confinement modules.

### `pkg/transport/streamablehttp`

The Streamable HTTP transport:

- uses explicit request, response-header, and idle timeouts;
- requires TLS verification for HTTPS;
- supports OAuth and application-provided headers;
- handles MCP session identifiers and resumable streams where negotiated;
- bounds response bodies and event frames;
- applies protocol-defined retry behavior only to safe lifecycle operations;
- never retries a tool call unless the protocol and application prove the call
  idempotent;
- exposes redacted origin and connection diagnostics.

### `pkg/transport/sse`

The legacy SSE transport exists only for interoperability with older servers.
It is opt-in, documented as compatibility-only, and does not weaken validation,
auth, limits, or cancellation requirements.

### `pkg/harness`

The Harness adapter owns:

- binding MCP server definitions to Session or Loop scope;
- creating and closing client sets with their owners;
- selecting which Loops may use Session-scoped bindings;
- converting adopted MCP tool definitions into immutable Harness
  `tool.Definition` values;
- exposing bounded resource and prompt access when configured;
- routing elicitation through protocol-neutral Harness gates;
- routing MCP tool calls through Harness permission policy;
- converting MCP content into Harness content blocks;
- translating failures into safe tool results and operational events;
- supplying secret-free MCP configuration and catalog identity to Harness
  configuration manifests;
- coordinating candidate catalog adoption at safe Loop boundaries.

The adapter does not add MCP concepts to Harness core types. Where Harness needs
a new seam, that seam is protocol-neutral and usable by other integrations.

## Binding model and scope

An application creates any number of named bindings:

```text
Session 8f...
├── github          [Session]
├── documentation   [Session]
├── database        [Loop: researcher]
└── browser         [Loop: operator]
```

A binding name is stable within a Session and is part of capability identity.
Two bindings may target the same server when separate credentials, working
directories, state, or isolation are required.

The conceptual Harness adapter configuration is:

```go
type Scope uint8

const (
	ScopeSession Scope = iota + 1
	ScopeLoop
)

type Binding struct {
	Name       string
	Server     client.Definition
	Scope      Scope
	Loop       uuid.UUID
	Visibility LoopSelector
	Required   bool
}
```

Harness has no `loop.ID` named type; Loop identity is a bare `uuid.UUID`
(`identity.Coordinates`, `tool.Bindings.LoopID`). The adapter uses `uuid.UUID`
unless Harness later introduces an alias.

For Session scope, `Loop` is empty and `Visibility` decides which Loops may
consume the binding. For Loop scope, `Loop` identifies exactly one owner and
`Visibility` is not used.

Scope determines connection ownership. Visibility determines capability access.
They are separate because a Session may share one stateful connection while
showing its capabilities to only selected Loops.

Required/optional posture also belongs to the binding rather than the raw MCP
client definition. The same server definition may be required in one product or
Loop role and optional in another.

### Delegation

A child does not inherit the parent's Loop-scoped bindings:

```text
parent Loop
└── parent-private browser

delegate Loop
├── delegate-private database
└── Session-scoped documentation, if selector permits
```

The delegate definition or application composition must assign its private
bindings explicitly. This prevents accidental transfer of credentials,
connection state, network reachability, and tool authority.

### Binding reconfiguration

Applications may add, remove, enable, disable, or replace bindings while an
owner is live. Reconfiguration creates new immutable binding definitions; it
does not mutate a definition already used by an active turn.

- Adding or enabling starts a new client and discovers a candidate generation.
- Replacing transport, auth, limits, filters, or server identity starts a new
  logical client before the old route is retired.
- Removing or disabling marks the binding retiring and removes it from future
  Loop generations.
- Active turns keep their existing route until their calls finish or the
  configured retirement deadline cancels them.
- A Session-scoped replacement is adopted independently by each permitted Loop
  at its safe boundary.
- The old connection closes after no active turn generation references it.
- A failed replacement leaves the prior binding active unless policy explicitly
  requires fail-closed removal.

The client-set control surface supports status, connect, disconnect, refresh,
and shutdown without requiring a TUI. Applications decide which controls they
expose to users or operators.

## Lifecycle and readiness

Each binding has a typed lifecycle:

```text
configured
    |
    v
starting -> authenticating -> discovering -> ready
    |             |              |
    +-------------+--------------+-> failed
                                      |
                                      v
                                  reconnecting

ready -> degraded -> reconnecting -> ready
  |
  v
closing -> closed
```

Status contains safe metadata only:

- binding name and scope;
- lifecycle state;
- negotiated protocol version;
- server name and version when known;
- transport kind and redacted origin;
- adopted and candidate catalog digests;
- bounded, classified failure;
- timestamps and retry state.

### Concurrent startup

Bindings start concurrently. One slow optional server must not delay discovery
from unrelated servers.

The owner publishes a reachable client-set handle before waiting for required
servers. Initialization may itself trigger OAuth or elicitation, so callbacks
must be routable while startup is still in progress.

### Required and optional servers

- A required Session-scoped binding must become ready before the Session is
  returned as ready.
- A required Loop-scoped binding must become ready before that Loop is admitted.
- An optional failure leaves the owner usable and marks the binding failed or
  degraded.
- Failures are aggregated after concurrent startup so users see every required
  failure in one report.
- An optional binding may reconnect independently without restarting other
  bindings.

### Shutdown

Closing an owner closes its bindings:

- Session shutdown closes Session-scoped clients.
- Loop shutdown closes that Loop's clients.
- Closing a parent Loop does not close independent delegate bindings.
- Session shutdown eventually closes all remaining Loop-scoped clients.
- Shutdown cancels startup, in-flight requests, pending elicitations, background
  readers, reconnect work, and stdio subprocesses.
- Shutdown is idempotent and waits for owned resources to finish within a bound.

### Shared-connection concurrency

A Session-scoped connection may receive calls from multiple Loops. The client
set therefore owns a per-binding request scheduler.

- Protocol request IDs allow independent request/response correlation.
- Lifecycle, auth, and catalog refresh operations are serialized where their
  state transitions require ordering.
- Tool calls are serialized by default.
- An application may explicitly allow bounded parallel calls for a binding that
  is known to support them.
- Per-binding and per-owner concurrency limits apply even when parallelism is
  enabled.
- Cancelling one request does not cancel unrelated calls on the same connection.
- Shutdown cancels the whole binding after new work is rejected.

Server tool annotations may inform concurrency policy but cannot broaden it
without application approval.

## Capability support

The client must support at least:

- tools: list, call, pagination, and list-change notifications;
- resources: list, read, subscribe/update where negotiated, and list-change
  notifications;
- resource templates;
- prompts: list, get, pagination, and list-change notifications;
- server instructions;
- server logging;
- progress notifications;
- request cancellation;
- form and URL elicitation;
- OAuth for network transports;
- stdio credential injection;
- protocol extensions through a bounded, namespaced escape hatch.

### Optional sampling

Server-requested sampling gives a server authority to initiate model work and
spend. It is therefore capability-gated:

- `pkg/client` supports a `SamplingHandler`.
- Sampling is not advertised when the handler is nil.
- The application supplies model selection, budget, permission, recursion,
  tool-use, and content policies.
- The client applies independent request and output limits.
- Sampling never receives a Harness Session controller or unrestricted tool
  registry.
- Nested sampling depth and concurrent requests are capped.
- Sampling requests and outcomes are audited without recording secrets.

This provides full client capability without enabling implicit model spend.

### Roots

Roots may be advertised only when an application installs a root provider.
Returned roots are canonical, bounded, and restricted to the binding's approved
workspace view. A server never learns arbitrary host filesystem roots by
default.

## Catalog model

Each ready client maintains immutable generations:

```text
generation 4 [adopted]
        |
tools/list_changed
        |
        v
generation 5 [candidate, validated]
        |
Loop A reaches idle ----------------> generation 5 [adopted by A]
        |
Loop B remains active
        |
Loop B reaches idle ----------------> generation 5 [adopted by B]
```

A generation contains:

- binding identity;
- negotiated protocol and capabilities;
- raw server identity;
- tools and their input/output schema digests;
- prompts;
- resources and resource templates, or bounded discovery metadata;
- bounded server instructions;
- generation number;
- canonical catalog digest;
- discovery warnings and compatibility decisions.

Catalogs are immutable after publication. Mutable connection state points to
the latest valid candidate. Each Loop view points to its adopted generation.

### Discovery

Initial discovery:

1. initialize and negotiate capabilities;
2. fetch every advertised initial catalog with pagination;
3. reject duplicate cursors and page cycles;
4. enforce page, item, schema, and byte limits;
5. validate names and schemas;
6. preserve raw protocol identity;
7. construct stable model-visible identity;
8. publish the first candidate;
9. adopt it before the owner becomes ready.

Discovery failure does not partially replace a prior valid generation.

### Change notifications

When a list-change notification arrives:

1. mark the relevant catalog family stale;
2. coalesce duplicate notifications;
3. fetch a complete candidate;
4. validate and digest it;
5. compare it with the current generation;
6. publish a candidate event;
7. schedule adoption for each affected Loop's next safe boundary.

If refresh fails, the prior generation remains adopted. The binding reports
stale/degraded status and retries according to bounded policy.

Resources and prompts may refresh without changing model-facing tools. A tool
catalog change requires a new immutable Harness toolset generation.

## Harness safe-boundary integration

Harness definitions and bound tool registries are immutable. The adapter must
not mutate a live tool's name, schema, or implementation in place.

None of this exists in Harness today and it is net-new Harness core work:
`loop.BoundDefinition` is sealed and read-only, and the only post-construction
change Harness supports is selecting a predeclared mode or inference at the
next turn boundary (`SetLoopMode`, `ChangeLoopInference`). The boundary
*detection* primitive does exist — `event.LoopIdle` and the hub idle
machinery — and the apply-at-next-turn-boundary pattern is established. What
is missing is the replacement capability itself.

The integration therefore needs a protocol-neutral Harness capability for
atomic external-toolset replacement:

```text
candidate MCP catalog
        |
        v
snapshot-specific []tool.Definition
        |
        v
Loop safe-boundary request
        |
        v
validate + bind replacement
        |
        v
atomic toolset generation swap
```

The initial safe boundary is Loop idle:

- no model inference request is active;
- no tool call from the prior turn is executing;
- no permission or elicitation gate tied to that turn is unresolved;
- no compaction or other operation depends on the current tool registry.

The Harness owns boundary detection and the atomic swap. The MCP adapter owns
candidate construction. The application owns policy for enabling bindings and
filters.

A failed replacement leaves the prior generation installed and reports the
failure. It never produces a partially changed toolset.

## Tool identity and calls

Raw server and tool names are preserved for protocol calls. Model-facing names
are qualified by binding identity and sanitized for provider constraints.

Conceptually:

```text
binding: github
raw tool: search_issues
model identity: mcp__github__search_issues
```

Name construction must:

- be deterministic;
- avoid collisions after sanitization;
- remain within inference-provider limits;
- append a digest suffix when truncation or collision requires it;
- preserve a reverse mapping to the raw binding and tool names;
- never route by reparsing the display name.

Each adapted tool closes over:

- binding ID;
- raw server tool name;
- adopted generation;
- input schema digest;
- connection route;
- limits and permission identity.

### Calling a tool

1. Harness validates model arguments against the adopted input schema.
2. Harness permission policy evaluates the qualified capability.
3. The adapter verifies that the connection still corresponds to the binding.
4. If a newer candidate proves the tool removed or incompatibly changed, the
   call returns `ToolUnavailable` without invoking a replacement definition.
5. Otherwise the adapter calls the raw MCP tool with cancellation, progress,
   deadline, and result limits.
6. MCP content is validated and translated into Harness content blocks.
7. A protocol-level tool error becomes a structured tool result.
8. Transport, auth, or connection failures are classified and may mark the
   binding degraded.

An active turn may call a tool from its old turn snapshot after a notification
but before adoption. If the server has already removed it, the server may return
unknown-tool. The adapter translates that response to the same structured
`ToolUnavailable` result. The Session and Loop remain healthy.

An old schema is never silently applied to a new tool definition. If the raw
name remains but its schema digest changes, calls from the old generation fail
as `ToolSchemaChanged` unless the server still explicitly identifies and
supports the old version.

## Permissions

MCP tools cross the same Harness permission boundary as native tools.

Permission identity includes the binding and raw tool:

```text
mcp:<binding-name>:<raw-tool-name>
```

Applications may define:

- binding-wide allow, ask, or deny;
- tool-specific overrides;
- Session- or Loop-scoped grants;
- restrictions based on server origin, transport, or auth posture;
- stricter policies for sampling, resources, and prompts.

The MCP server cannot self-declare that a call is approved. Tool annotations and
metadata are untrusted policy input.

The adapter implements `tool.PermissionPrompter` to provide a redacted MCP
request summary. It must not place credentials, full request bodies, resource
contents, or unbounded arguments into a gate or audit event.

`tool.PermissionRequest` is currently sealed to `pkg/tool`: an external module
cannot implement it, and the only available fallback is `tool.UnknownRequest`,
which permits once-only approval scopes. The Harness stage of this work must
add a protocol-neutral seam — a constructor or a generic external-capability
request type — so an external adapter can supply a redacted summary with
session- and workspace-scoped approvals. The seam must preserve the sealed
contract's redaction guarantees.

## Elicitation

Elicitation is a server request for user or policy input during initialization
or another MCP operation. It is not a TUI-owned protocol feature.

Flow:

```text
MCP server
    |
    v
pkg/client ElicitationHandler
    |
    v
mcp/pkg/harness
    |
    v
protocol-neutral Harness form/URL gate
    |
    +--> TUI renders and answers
    +--> HTTP client renders and answers
    +--> headless policy answers or declines
    |
    v
validated MCP elicitation response
```

Harness needs protocol-neutral gate payloads for:

- bounded forms with typed fields, labels, descriptions, defaults, required
  flags, and safe validation constraints;
- confirmation-only requests;
- URL/open-browser requests with a durable display origin, an ephemeral action
  target, and explicit completion;
- accept, decline, and cancel responses.

These payloads belong in Harness because any integration may require structured
human input. They contain no MCP-specific wire types.

They are additive to `pkg/gate`: today the package defines only the
`harness.permission` and `harness.ask_user` kinds, and while `gate.PromptSchema`
already models typed form fields, there is no form-elicitation payload and no
URL/open-browser payload. New `gate.Kind` and sealed `gate.Payload` variants are
required, following the existing `{kind,data}` discriminator codec and
fail-closed unknown-kind handling.

The adapter translates between MCP elicitation schemas and Harness gates.
Unsupported or unsafe schema constructs are declined with a classified error.

While an elicitation is pending:

- the originating MCP request remains cancellable;
- active-time request timeout may pause, but an overall wall-clock deadline
  remains;
- shutdown or interrupt resolves the request as cancelled;
- responses are correlated to binding, MCP request, Harness gate, and owner;
- late or duplicate responses are rejected;
- form requests that solicit passwords, tokens, private keys, or other
  credentials are rejected;
- sensitive authorization is performed through URL elicitation or the auth
  package, not through durable form values;
- the full action URL and query parameters are not written to journals or
  ordinary events.

Pending elicitation is tied to a live connection. Restore does not attempt to
resume a stale server request from journal data; the old gate is resolved as
unavailable, and a reconnected server may issue a new request.

## Resources, prompts, and instructions

### Resources

The client library provides direct typed resource APIs. The Harness adapter may
also expose bounded generic tools such as list-resources, list-templates, and
read-resource.

Resource contents are external, untrusted data:

- text is bounded and labeled with provenance;
- binary data is allowed only for configured MIME types and sizes;
- unsupported binary data is summarized rather than injected;
- resource URIs are opaque protocol identifiers, not host paths;
- resource subscriptions and updates never mutate model context mid-turn.

### Prompts

The client supports prompt discovery and retrieval. MCP prompts are not
automatically inserted into the Harness system prompt. Applications may expose
them through UI commands or bounded tools.

Prompt messages returned to an agent are treated as external content unless an
explicit application policy promotes a specific trusted prompt source.

### Server instructions

Server instructions are retained in the catalog and available to applications,
but they are not silently concatenated into Harness system instructions.
Automatic injection would let a remote server acquire instruction authority
merely by being connected.

An application may explicitly install a bounded, trusted instruction policy for
selected bindings. That choice becomes part of configuration identity.

## Content conversion and limits

All MCP input is untrusted. Limits apply before allocation where possible.

Every definition supplies defaults and may tighten them per binding:

- startup timeout;
- request timeout;
- overall elicitation timeout;
- maximum concurrent requests;
- maximum catalog pages and items;
- maximum JSON frame and HTTP body size;
- maximum tool schema size and depth;
- maximum text result size;
- maximum structured-content size;
- maximum binary item size and count;
- maximum log message size;
- maximum prompt and resource count;
- maximum sampling depth, tokens, and concurrency.

The adapter supports MCP text, image, audio, embedded resource, and structured
content only when Harness has a safe corresponding block. Unknown content types
become bounded opaque metadata or a classified unsupported-content result; they
do not panic or disappear silently.

Large tool results use the application's artifact or truncation facilities when
available. The model receives a bounded summary and reference, never an
unbounded payload.

## Compatibility

Compatibility is based on negotiated protocol versions and capabilities, not on
assuming that every server implements the latest specification perfectly.

The client:

- sends only capabilities it can actually fulfill;
- checks server capabilities before using a method;
- ignores unknown optional fields while preserving required raw metadata where
  useful;
- rejects unknown required semantics;
- accepts missing optional fields;
- detects unsupported protocol versions with a typed error;
- supports safe, named compatibility profiles for known deviations;
- reports every applied tolerance in diagnostics and catalog identity;
- never widens permissions or schemas as a compatibility fallback.

Safe tolerances may include:

- ignoring an invalid optional output schema while retaining a valid input
  schema and reporting a warning;
- accepting a legacy SSE transport only when explicitly configured;
- normalizing non-provider-compatible display names while preserving raw names.

Unsafe tolerances include:

- replacing an invalid input schema with unconstrained arguments;
- disabling TLS verification;
- treating malformed framing as a valid message;
- retrying non-idempotent tool calls;
- treating an auth failure as anonymous success.

Compatibility profiles are versioned and included in the binding's secret-free
configuration identity.

## Reconnection and failure isolation

One binding's failure does not disconnect other bindings.

The client may reconnect automatically when:

- the owner is still live;
- the failure is classified transient;
- policy permits reconnection;
- retry count, delay, and total time remain bounded.

After reconnect:

1. initialize a new logical MCP connection;
2. discover a complete candidate catalog;
3. compare server and catalog identity;
4. leave existing Loop generations active;
5. adopt the new generation at each Loop's safe boundary.

Pending requests from the failed connection are not replayed automatically.
Tool calls may have caused effects before disconnection, so their outcome is
classified as indeterminate when no definitive response exists.

## Error model

Public failures are typed for classification:

- invalid configuration;
- unsupported protocol or capability;
- startup timeout or required-server failure;
- authentication required, denied, expired, or failed;
- transport closed, framing failure, or remote HTTP failure;
- server protocol error;
- request deadline or cancellation;
- catalog invalid, stale, or over limit;
- binding or tool not found;
- tool unavailable or schema changed;
- remote tool error;
- result or content limit exceeded;
- elicitation declined, cancelled, invalid, or timed out;
- sampling denied or over budget;
- indeterminate execution after connection loss;
- shutdown failure.

Operational errors do not expose tokens, headers, raw environment values, or
unbounded server text.

For model-facing calls, expected remote and availability failures become
bounded tool results. Construction failures, invariant violations, and required
startup failures remain control-plane errors.

## Events and observability

`pkg/client` emits typed callbacks for:

- startup and readiness;
- auth state;
- connection loss and reconnection;
- catalog stale, candidate, rejected, and refreshed;
- request progress;
- server logging;
- elicitation lifecycle;
- shutdown.

`mcp/pkg/harness` translates useful events into protocol-neutral Harness
integration events. The TUI subscribes to the normal Harness event stream and
does not call the MCP client directly.

Event fields are bounded and redacted. Raw tool arguments, resource contents,
authorization URLs containing secrets, headers, tokens, and stdio environment
values are excluded.

Metrics should include startup duration, discovery duration, request latency,
timeouts, failures by class, catalog generation changes, reconnect attempts,
content truncation, and pending elicitation duration.

## Session restore and configuration identity

MCP connections are live resources and are never restored from journal bytes.
On Session restore:

1. reconstruct the Session from durable Harness records;
2. recreate Session- and Loop-scoped client sets from current application
   configuration;
3. initialize connections and discover current catalogs;
4. compare current MCP binding/catalog identity with the Session's latest
   adopted configuration manifest;
5. report drift and follow the configured restore decision;
6. adopt the current configuration epoch when accepted.

Changing servers, tools, schemas, auth posture, transports, or filters does not
make old journal records unreadable. Historical MCP calls remain data.

The configuration manifest contains only safe identity:

- binding name and scope;
- Loop selector or owner identity;
- transport kind and redacted origin identity;
- required/optional posture;
- capability, filter, limits, and compatibility policy digests;
- negotiated server identity;
- adopted catalog digest and tool schema digests.

Credentials, tokens, raw headers, full environment values, resource contents,
prompt bodies, and tool results are excluded.

The detailed adoption and migration rules are defined in
`2026-07-16-session-versioning-migration-design.md`. That design is not
implemented: Harness today has only the one-shot `event.ConfigFingerprint`
stamped at `SessionStarted` and the boolean `WithAllowConfigMismatch` restore
override. The MCP module must not hard-depend on manifest, epoch, or drift
types. Until the versioning work lands, the adapter degrades to contributing
its secret-free identity digests to the existing fingerprint model, and the
manifest integration stage is sequenced after (or alongside) the versioning
implementation.

## Security model

- Every server, catalog, notification, result, log, prompt, resource, and
  elicitation schema is untrusted input.
- A connected MCP receives only the authority explicitly granted by transport,
  roots, credentials, permissions, and confinement.
- Session sharing never implies visibility to every Loop.
- Delegation never implies MCP inheritance.
- Stdio uses argv execution and an allowlisted environment.
- Remote transports verify TLS and apply explicit timeouts and size bounds.
- OAuth state and PKCE are validated.
- Tool and sampling authority are checked before execution.
- Server instructions and prompt content do not automatically gain system
  authority.
- Compatibility fallbacks never broaden arguments or permissions.
- Secrets never enter catalogs, journals, events, errors, or fingerprints.
- Shutdown and cancellation fail closed.

## Testing strategy

### Unit tests

- configuration and name validation;
- deterministic qualified names, truncation, and collisions;
- capability negotiation;
- catalog pagination, duplicate cursors, cycles, and bounds;
- schema validation and safe compatibility profiles;
- catalog digest stability;
- candidate/adopted generation transitions;
- typed error classification and redaction;
- content conversion and limits;
- auth state and token-store contracts;
- selector and scope rules;
- delegate non-inheritance;
- sampling capability advertisement.

### Fuzz tests

- JSON-RPC message decoding;
- protocol envelopes;
- schemas and catalog entries;
- content conversion;
- elicitation schemas and responses;
- name normalization;
- legacy compatibility decoding.

### Transport integration tests

Tagged integration tests use local fixture servers:

- stdio initialize, discovery, calls, cancellation, stderr, crash, and cleanup;
- Streamable HTTP initialize, session IDs, streaming, OAuth, cancellation, and
  reconnect;
- legacy SSE compatibility;
- malformed frames and oversized bodies;
- process-group cleanup;
- progress-aware timeouts;
- elicitation during initialization.

### Harness integration tests

- multiple MCPs connected to one Session;
- mixed Session- and Loop-scoped bindings;
- selectors on Session-scoped capabilities;
- delegate bindings without parent inheritance;
- required and optional startup behavior;
- permission gates for MCP tools;
- form and URL elicitation through generic Harness gates;
- active turns retaining their tool snapshot;
- automatic candidate adoption at idle;
- removed and schema-changed tools returning structured results;
- failed refresh preserving the last valid generation;
- Session restore with catalog drift;
- independent failure and reconnect of one binding;
- Session and Loop shutdown cleanup.

All Go tests run with the race detector. Process, HTTP, and OAuth integration
tests are tagged `integration`. External-input parsers receive fuzz coverage.

## Implementation staging

The implementation plan should split the work into reviewable stages:

1. standalone module skeleton, contracts, typed errors, and protocol SDK choice;
2. stdio transport and base lifecycle;
3. Streamable HTTP and auth;
4. discovery, catalogs, tools, resources, prompts, and limits;
5. notifications, candidate generations, reconnect, and compatibility;
6. protocol-neutral Harness seams: form/URL gates, the external
   permission-request seam, and external-toolset replacement;
7. `mcp/pkg/harness` Session/Loop ownership and tool adapter;
8. TUI rendering for the new generic gates and status events;
9. restore/configuration-manifest integration (blocked on, or degraded until,
   the session-versioning implementation; fingerprint-level identity in the
   interim);
10. optional sampling and legacy SSE compatibility.

No stage should place MCP protocol types in Harness or make the TUI own client
lifecycle.

## Dependency decision

Codex uses the official Rust MCP SDK, and OpenCode uses the official TypeScript
SDK. The preferred Go implementation path is the official
`github.com/modelcontextprotocol/go-sdk`, wrapped behind `pkg/client` and
`internal/protocol` so SDK types do not leak into Looprig APIs.

Harness development policy requires explicit user approval before adding an
external Go dependency. Writing this design does not approve or add that
dependency. The implementation plan must place an explicit approval checkpoint
before modifying `go.mod`.

If the SDK is not approved, the alternative is a stdlib protocol
implementation. That increases protocol, transport, compatibility, and test
surface but does not change the public architecture in this design.

## Acceptance criteria

The design is satisfied when:

- an application can attach multiple MCP servers to one Harness Session;
- each binding can be Session- or Loop-scoped;
- delegates receive only their own bindings and allowed Session bindings;
- stdio and Streamable HTTP meet the security and lifecycle requirements;
- tools, resources, templates, prompts, instructions, logging, progress,
  cancellation, elicitation, auth, and optional sampling are represented;
- required and optional failures behave independently;
- catalogs refresh automatically and adopt only at safe boundaries;
- active turns use immutable tool generations;
- removed tools fail safely without invalidating the Session;
- Harness and TUI remain MCP-agnostic;
- Session restore reports MCP configuration drift without treating it as
  journal corruption;
- tests demonstrate cleanup, bounds, isolation, compatibility, and race safety.
