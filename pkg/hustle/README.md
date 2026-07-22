# pkg/hustle

`pkg/hustle` defines **parallel background work** the session can run
alongside its loops. A hustle is a named, secret-free, replayable
inference invocation that runs in one of two participation lanes —
**blocking** (the loop waits for it) or **background** (it publishes
results when done). The scheduler runtime lives in
`internal/hustleruntime`; this package owns the immutable definitions,
the typed audit taxonomy, and the secret-free descriptor projection.

## What is hustle?

- **`Definition`** — an immutable hustle recipe built with
  `hustle.Define(opts...)`: a stable `Name`, a participation lane, a
  model source (the originating loop's binding or a named model), a
  system prompt, optional structured output schema, timeout, and
  payload limits.
- **`BoundDefinition`** — a hustle bound to a session: the runtime
  view a `Controller` schedules against.
- **`DefinitionDescriptor`** — the complete secret-free behavioral
  projection used by rig identity and durable audit records (the
  `Name`, `Participation`, `ModelSource`, prompt SHA-256, output-schema
  SHA-256, policy revisions, timeout, limits). It is what an audit
  record carries; the raw prompt and inference client never leave the
  runtime.
- **`RunID` / `Stage` / `ReasonCode` / `TerminalStatus`** — the typed
  audit taxonomy. `Stage` names the bounded execution phase in which a
  hustle failed (Queue → ModelResolution → Inference → Output → Terminal
  → Finalization). `ReasonCode` is the bounded, security-safe
  classification. `ReasonAllowed(stage, reason)` closes the matrix so
  impossible stage/reason audit records cannot be written.
- **`Participation`** — `Blocking` or `Background`, the session-global
  execution lane.
- **`ModelSource`** — `CurrentLoop` (use the originating loop's live
  binding) or `Named` (use a named model). The two are mutually exclusive
  in a descriptor.

## How to use

Hustles are composed into a rig, not invoked directly:

```go
summarizer, err := hustle.Define(
    hustle.WithName("summarize-transcript"),
    hustle.WithParticipation(hustle.ParticipationBackground),
    hustle.WithModelSource(hustle.ModelSourceCurrentLoop),
    hustle.WithSystem("You summarize agent transcripts."),
    hustle.WithTimeout(30*time.Second),
    hustle.WithLimits(hustle.Limits{InputBytes: 1<<20, OutputBytes: 1<<20}),
    hustle.WithPromptRevision("2026-07-21.1"),
    hustle.WithPolicyRevision("2026-07-21.1"),
    // optional structured output:
    // hustle.WithOutputSchema(name, schema, strict, revision),
)
if err != nil { return err }

r, err := rig.Define(
    rig.WithLoops(operator),
    rig.WithHustles(summarizer),
    rig.WithHustleLimits(rig.HustleLimits{
        BlockingConcurrent: 1, BlockingQueued: 8,
        BackgroundConcurrent: 2, BackgroundQueued: 16,
        AuditTimeout:        5*time.Second,
        FinalizationTimeout: 30*time.Second,
        WorkerDrainTimeout:  5*time.Second,
    }),
    /* ... */
)
```

A loop invokes a hustle through the hustle tool (built by the
composition root) and observes the outcome as a typed event on the
session stream (`HustleStarted`, `HustleCompleted`, `HustleFailed`).

## Sibling packages

- [`pkg/rig`](../rig/README.md) — `rig.WithHustles` and
  `rig.WithHustleLimits` register hustles and bound their lanes.
- [`pkg/event`](../event/README.md) — `event.HustleStarted` /
  `HustleCompleted` / `HustleFailed`, the durable lifecycle events.
- [`pkg/identity`](../identity/README.md) — `identity.AgentName` used by
  the originating loop's binding.
- `github.com/looprig/inference` — `inference.Client`, `model.Model`,
  `inference.OutputSchema`.

## How it is designed

```
       hustle.Definition (immutable recipe)
                │
                │  rig.Define + Bind
                ▼
       hustle.BoundDefinition (runtime view)
                │
                │  a loop invokes via the hustle tool
                ▼
   ┌────────────────────────────────────────────────────┐
   │ internal/hustleruntime.Controller                   │
   │  two lanes (Blocking | Background)                  │
   │  per-lane: Concurrent in-flight + Queued waiting    │
   │  RunIDFactory mints candidate ids before commit    │
   │  AuditPublisher  ──► durable HustleStarted/...     │
   │  FaultReporter   ──► typed controller faults        │
   │  ActivityTracker ──► session-active-set accounting │
   └────────────────────────────────────────────────────┘
                │
                ▼
        inference.Client  →  HustleCompleted / HustleFailed (durable, stage+reason)
```

### Two lanes, bounded by limits

`rig.HustleLimits` bounds both lanes independently: each has a
`Concurrent` in-flight cap and a `Queued` waiting-capacity cap; their
sum is the total ownership cap (`MaxHustleQueued` is the largest
configured waiting capacity either lane may take). Blocking runs hold
the loop's turn; background runs publish results when complete. A
background hustle that outlives its session drains on shutdown within
`WorkerDrainTimeout`.

### Secret-free audit by construction

The `DefinitionDescriptor` is the only thing an audit record carries.
Raw prompts and inference clients never leave the runtime; the prompt
is hashed (`PromptSHA256`), the output schema is hashed
(`OutputSchemaSHA256`), and the model is captured as a `model.ModelKey`
plus a policy revision. An audit record therefore cannot leak a prompt
or a model's secret configuration, and a re-deployment that changes
either is detectable by comparing descriptors.

### Closed stage/reason matrix

`Stage` and `ReasonCode` are closed enums, and `ReasonAllowed(stage,
reason)` closes the **matrix**: an impossible combination (e.g. a
`Finalization` stage with a `ReasonInference` reason) is rejected
before it ever reaches an audit record. The closed matrix is the
invariant that makes hustle usage aggregates trustworthy.
