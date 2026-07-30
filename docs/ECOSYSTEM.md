# The looprig ecosystem

`looprig/harness` is one module in a multi-module Go ecosystem that together
build, run, observe, and evaluate agentic systems. Each module is its own
Go repository with its own `go.mod`, versioning, and release cadence; the
`replace` directives in `harness/go.mod` are how a developer checkout composes
the siblings locally. CI and releases build against the published module
versions, not the replaces.

## Module map

```
                       +--------------------------+
                       |       looprig/core       |
                       |  content blocks, uuid,    |
                       |  the shared primitives    |
                       |  every module depends on  |
                       +------------+-------------+
                                    |
       +------------+---------------+---------------+------------+
       |            |               |               |            |
       v            v               v               v            v
+-----------+ +-----------+ +---------------+ +-----------+ +-----------+
| inference | |  storage  | |    harness    | |   eval    | |   tui     |
| LLM clients,| | Ledger,   | | agent runtime | | eval frame-| | terminal  |
| model specs,| | Leaser,   | | (this module)  | | work, runs | | presenter |
| streaming, | | KV, Blobs | | Rig/Session/   | | under      | | over a    |
| context    | | contracts | | Loop/Hub/Gate  | | go test    | | Session    |
+-----+------+ +-----+-----+ +-------+--------+ +-----+-----+ +-----+-----+
      |              |                |                 |           |
      |              |                |                 |           |
      +--------------+----------------+-----------------+           |
                     |                |                             |
                     v                v                             |
              +-----------+    +-------------+                       |
              | foreignloops|   |   tools     |                       |
              | codex/claude|   | bash/web/   |                       |
              | subprocess  |   | etc tools  |                       |
              | backends    |   |             |                       |
              +-----+------+   +------+------+                        |
                    |                 |                             |
                    +--------+---------+                             |
                             |                                       |
                             v                                       v
                     +-------------+                          +-----------+
                     |  sandbox    |  OS confinement         |   mcp     |
                     |  profiles + |  for tool execution      | MCP client|
                     |  enforcement|                          |           |
                     +-------------+                          +-----------+

                     +-------------+
                     | classifiers |  permission auto-review classifiers
                     | (gate.*     |  (prompts, wire codecs, evidence-tool
                     |  reviewers) |  catalogs, evaluation corpus) over
                     +-------------+  harness's public pkg/gate + pkg/hustle

  Storage backends (each its own module, satisfies storage.Composite):
    looprig/fsstore  · looprig/natsstore · looprig/rclonestore
```

## Module roles

| Module | Owns | Depends on |
| --- | --- | --- |
| [`looprig/core`](https://github.com/looprig/core) | `content.Block`/`AgenticMessages`, `uuid`, the shared primitives | stdlib only |
| [`looprig/inference`](https://github.com/looprig/inference) | `inference.Client`, `model.Model`, `stream.Reader`, output schemas, context counters | `looprig/core` |
| [`looprig/storage`](https://github.com/looprig/storage) | `Ledger`, `Leaser`, `KV`, `Blobs` leaf contracts + in-memory `memstore` + conformance suite | stdlib only |
| [`looprig/harness`](https://github.com/looprig/harness) (this module) | the agent runtime: `Rig`, `Session`, `Loop`, `Hub`, `Gate`, `Event`, `Command`, `Tool`, durable journal + restore | `core`, `inference`, `storage`, `eval` |
| [`looprig/eval`](https://github.com/looprig/eval) | the evaluation framework: conversations, evaluators (`exact`/`judge`), rubrics, reports | `core`, `inference` (judge only) |
| [`looprig/classifiers`](https://github.com/looprig/classifiers) | the permission auto-review classifier product: prompts, wire codecs, deterministic risk/authorization policy, evidence-tool catalogs, and evaluation corpus for `harness`'s `pkg/gate` permission-review domain and `pkg/hustle` tool-use loop. Ships `gate.command-safety` (`pkg/commandsafety`) as its initial classifier | `harness` (public packages only), `core`, `inference` |
| [`looprig/foreignloops`](https://github.com/looprig/foreignloops) | `codex`, `claude`, and `acp` (Task 5.2) subprocess/protocol backends behind harness's `pkg/foreign` seams | `harness`, `core`, `inference`, `acp` (`acp/client` only, for `driver/acp`) |
| [`looprig/acp`](https://github.com/looprig/acp) | Agent Client Protocol bridge, split into a stdlib-only wire layer and one harness-facing facade: `acp/protocol` (generated ACP types + JSON-RPC envelope/errors) and `acp/transport/stdio` (NDJSON framing, subprocess spawn/supervision) have **no** harness or core dependency; `acp/client` drives a foreign ACP agent as a typed connection/session runtime (also harness-free — `foreignloops/driver/acp` is the only thing that turns its output into Harness events); `acp/agent` is the sole package that imports harness's public `session`/`event`/`gate`/`loop`/`identity` contracts (plus `core/content`, `core/uuid`) to expose a harness-backed product as an ACP agent through consumer-owned host interfaces, never importing product code | `acp/protocol`, `acp/transport/stdio`, `acp/client`: stdlib only. `acp/agent` only: `harness` (public pkgs), `core` |
| [`looprig/tools`](https://github.com/looprig/tools) | `bash`, `web`, and the other standard tool implementations + their `CallPreparer` | `harness` (`pkg/tool`), `sandbox` |
| [`looprig/sandbox`](https://github.com/looprig/sandbox) | OS access profiles and enforcement; satisfies `gate.AccessSource`/`GrantIssuer` without importing harness | stdlib + OS APIs |
| [`looprig/mcp`](https://github.com/looprig/mcp) | MCP client and the harness integration that publishes `IntegrationStatus` | `harness`, `core` |
| [`looprig/tui`](https://github.com/looprig/tui) | the interactive terminal presenter (charm.land stack) over a live `Session` | `harness`, `core` |
| [`looprig/fsstore`](https://github.com/looprig/fsstore) | filesystem `storage.Composite` backend | `looprig/storage` |
| [`looprig/natsstore`](https://github.com/looprig/natsstore) | NATS JetStream `storage.Composite` backend | `looprig/storage` |
| [`looprig/rclonestore`](https://github.com/looprig/rclonestore) | rclone `storage.Composite` backend | `looprig/storage` |

## How harness relates to each

`harness` defines the contracts and the runtime; it deliberately **does not**
implement most leaf capabilities — those live in their own modules and are
wired at the consumer's composition root.

- **Inference** — `pkg/loop` depends on `inference.Client` and `model.Model`.
  Harness ships no provider SDK in its build graph; a consumer wires the
  concrete client (Anthropic, OpenAI, Google, …) at the rig.
- **Storage** — `pkg/sessionstore` and `pkg/workspacestore` operate on
  `storage.Composite` (Ledger/Leaser/KV/Blobs). Harness ships no backend; a
  consumer opens `fsstore` / `natsstore` / `rclonestore` and passes it in.
- **Tools** — `pkg/tool` is the dependency-free contract surface
  (`BaseTool`, `InvokableTool`, `CallPreparer`). Harness's runner drives any
  tool that satisfies it; the standard implementations live in
  `looprig/tools`.
- **Sandbox** — `pkg/gate` defines the three-state evaluator (Deny/Gated/Allow)
  and the `AccessSource`/`GrantIssuer` seams; `looprig/sandbox` satisfies them
  with OS confinement. Harness never imports a sandbox package.
- **Foreign loops** — `pkg/foreign` exposes `Builder`/`RestoredBuilder` seams
  so `looprig/foreignloops` can back a loop with the `codex` or `claude` CLI,
  or (via `foreignloops/driver/acp`) a foreign Agent Client Protocol agent,
  without harness knowing any of the three protocols.
- **ACP bridge** — `looprig/acp` is asymmetric by design: its wire layer
  (`protocol`, `transport/stdio`, `client`) is a stdlib-only ACP
  implementation usable against any ACP peer with zero harness dependency,
  and only `acp/agent` crosses into harness's public packages to adapt the
  protocol onto a real session. Harness itself carries no ACP import in
  either direction — a harness-backed product depends on `acp/agent` to
  *be* an ACP agent, and `looprig/foreignloops`'s `driver/acp` depends on
  `acp/client` to *drive* one, exactly mirroring the existing `pkg/foreign`
  seam so harness never needs to know the ACP wire format.
- **Eval** — `pkg/evalmigration` is the build-tagged migration proof that the
  legacy harness eval examples re-express cleanly against `looprig/eval`. New
  evaluation code lives in `looprig/eval`, not here.
- **Permission auto-review classifiers** — `pkg/gate` owns the neutral,
  mechanism-level review domain (subject/basis/context envelope, local
  decision policy, evidence-tool access seams, audit events) and `pkg/hustle`
  owns the bounded tool-use loop a classifier runs inside; `harness` never
  imports `looprig/classifiers`. `looprig/classifiers` imports only harness's
  public packages and owns the actual classifiers — prompts, wire codecs,
  risk policy, evidence-tool catalogs, and evaluation corpus. A consumer
  imports both and explicitly registers which classifiers to run via
  `rig.WithPermissionClassifiers`; zero registered classifiers preserves
  harness's pre-classifier gate behavior exactly. See
  [`pkg/gate/README.md`](../pkg/gate/README.md#permission-review) for the
  full enable/disable, evidence-boundary, audit/privacy, policy-tuning, and
  restore-behavior story.
- **TUI** — `pkg/serve` and the `Session` contracts are the surface a
  terminal presenter binds against; `looprig/tui` does the rendering.

## Versioning and replaces

Sibling modules version independently. The `replace` directives in
`harness/go.mod` point at local checkouts so a developer can build the whole
ecosystem from one workspace:

```
replace github.com/looprig/core      => ../core
replace github.com/looprig/storage    => ../storage
replace github.com/looprig/inference  => ../inference
replace github.com/looprig/eval       => ../eval
```

A release cuts each sibling module at its own tag; consumers depend on the
published versions and never see the replaces.

## Where to read next

- [`README.md`](../README.md) — what `harness` is and how to use it.
- [`docs/architecture/agent-loop.md`](architecture/agent-loop.md) — the loop
  actor's goroutine, channel, and event-flow picture.
- [`docs/releases/`](releases/) — per-release notes.
- [`docs/TODO.md`](TODO.md) — the cross-cutting product backlog.
- Each `pkg/*/README.md` for the package-level contracts.
