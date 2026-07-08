# Gates Package - session-authoritative "machine asks, caller answers" primitive

**Date:** 2026-06-18
**Status:** Draft - promoted to a first-class harness primitive (revised 2026-07-06)
**Related:** `docs/plans/loop-machine-design.md` (the turn-scoped permission/user-input gates this
generalizes; the stranded-message decision it would later host);
`docs/plans/2026-07-06-serve-http-session-api-design.md` (a consumer that should resolve gates by
opaque gate id, not by loop id)

> **Revision 2026-07-06.** Promoted from an exploratory loop-local extraction to a
> **session-authoritative gate router**. The public handle is an opaque `GateID`, not a
> `ToolExecutionID` and not `(LoopID, ToolExecutionID)`. The session owns the open-gate directory and
> internal route; producers own their local blockers; transports such as `looprig/serve` post answers
> to `/sessions/{sid}/gates/{gate_id}` and never need to know which loop, foreign engine, or session
> handler resolves the answer.

## 1. Motivation

The loop already has a gate mechanism, but it is hard-wired to one corner of the design space:
turn-scoped permission and user-input questions that park a tool runner and resume it with
`ApproveToolCall`, `DenyToolCall`, or `ProvideUserInput`.

That is too narrow for the harness surface we want:

- a gate may come from a tool call today, from a loop-level question tomorrow, and from a session or
  foreign engine after that;
- a `ToolExecutionID` is useful correlation for tool-backed gates, but it is not the normalized gate
  id;
- clients should not have to route gate replies by `LoopID`;
- a reconnecting or restored process needs an authoritative open-gate directory, not a transport-side
  event-derived shadow map;
- no-answer behavior must be explicit per gate kind: a tool permission prompt may fail closed after
  a short timeout, while a human question may suspend the session for restore or let the model pick a
  non-critical default;
- some gates resume a parked computation, while others initiate or control future work.

The primitive is therefore not just a generic map extracted from `loop.pendingGates`. It is a small
session-owned router with a stable gate envelope, private routes, and explicit blocker/restore
semantics.

## 2. Core Concepts

### Gate ID

`GateID` is the public handle for every gate. It is a fresh opaque id minted for the gate itself.
For tool-backed gates, `ToolExecutionID` moves into `Subject` as correlation data.

The stable public control shape is:

```text
POST /sessions/{sid}/gates/{gate_id}
```

The client never supplies `LoopID`, `ToolExecutionID`, or any other resolver-specific route.

### Resolver

`Resolver` names who owns answer dispatch. It is internal routing ownership, not UI display.

Examples:

- `loop` - a native loop actor owns a parked local gate;
- `session` - the session owns a decision such as stranded-input resend/drop;
- `foreign` - a foreign engine exposes a prompt through the same session channel;
- `extension` - a consumer/tool extension owns the answer validator and continuation.

### Blocks

Every gate blocks something, but not always the same thing. The blocker target is what becomes unable
to proceed until the gate is answered or abandoned.

Examples:

- `tool_call` - a permission check or `AskUser` call is parked;
- `step` - a loop cannot finish or continue a step before a choice is made;
- `turn` - a whole turn is paused on a model/runtime decision;
- `loop` - a loop-level operation is waiting;
- `session` - the session is holding work such as a stranded input decision.

This is not event durability. Gate prompt events are enduring by definition. `Blocks` describes the
live blocker and its cleanup boundary.

### Effect

The answer's effect is resolver-specific:

- **resume** - deliver an answer to a parked goroutine/actor continuation;
- **initiate** - start new work, such as resubmitting a stranded input;
- **control** - update session/loop configuration, cancel, switch model, etc.

### Subject

`Subject` is optional correlation. It tells humans, transcripts, and UIs what object the gate is
about without making that object the gate id.

Examples:

- `ToolExecutionID` / provider `ToolUseID` for tool gates;
- `TurnID` / `StepID` for loop-step gates;
- `InputID` for queued/stranded input gates;
- model ids for switch-model gates.

### Prompt and Payload

Known harness gates should use typed prompt/payload structs. Extension gates should use a constrained
dynamic prompt envelope for display, while resolver-owned code validates the answer. Dynamic JSON can
describe UI controls; it must not decide harness behavior by itself.

### Response Policy

`ResponsePolicy` says what the session should do if nobody answers in time. It is gate behavior, not
transport behavior: in-process callers, `serve`, restore, and future consumers all observe the same
policy.

The policy does not change what a gate is. A policy may synthesize a normal `GateResponse`, suspend
the live session while leaving the gate open and restorable, or delegate a non-critical choice to a
configured policy responder/model. Any synthesized answer must travel through the same session
`RespondGate` path as a human answer, so races are resolved exactly once.

## 3. Gate Envelope

The common envelope is stable and transport-safe. The route is private.

```go
package gate

type ID uuid.UUID
type Kind string

const (
	KindPermission  Kind = "harness.permission"
	KindAskUser     Kind = "harness.ask_user"
	KindSwitchModel Kind = "harness.switch_model"
	KindResumeInput Kind = "harness.resume_input"
)

type ResolverKind string

const (
	ResolverLoop      ResolverKind = "loop"
	ResolverSession   ResolverKind = "session"
	ResolverForeign   ResolverKind = "foreign"
	ResolverExtension ResolverKind = "extension"
)

type Blocks string

const (
	BlocksToolCall Blocks = "tool_call"
	BlocksStep     Blocks = "step"
	BlocksTurn     Blocks = "turn"
	BlocksLoop     Blocks = "loop"
	BlocksSession  Blocks = "session"
)

type Effect string

const (
	EffectResume   Effect = "resume"
	EffectInitiate Effect = "initiate"
	EffectControl  Effect = "control"
)

type CloseReason string

const (
	CloseAnswered           CloseReason = "answered"
	ClosePolicyResponse     CloseReason = "policy_response"
	CloseAbandoned          CloseReason = "abandoned"
	CloseOwnerClosed        CloseReason = "owner_closed"
	CloseRestoreUnavailable CloseReason = "restore_unavailable"
)

type Criticality string

const (
	GateCritical    Criticality = "critical"
	GateNonCritical Criticality = "non_critical"
)

type ResponseRequest struct {
	Action string
	Values map[string]json.RawMessage
}

type GateResponse struct {
	GateID ID
	Action string
	Values map[string]json.RawMessage
	Source ResponseSource // server-filled for user, policy, or model-generated answers
}

type ResponseSourceKind string

const (
	ResponseFromUser   ResponseSourceKind = "user"
	ResponseFromPolicy ResponseSourceKind = "policy"
	ResponseFromModel  ResponseSourceKind = "model"
)

type ResponseSource struct {
	Kind   ResponseSourceKind
	Reason string // e.g. "timeout", "recommended_default"
}

type ResponsePolicy struct {
	Timeout   time.Duration // zero means no deadline
	OnTimeout PolicyAction
}

type PolicyActionKind string

const (
	PolicyWait           PolicyActionKind = "wait"
	PolicyRespond        PolicyActionKind = "respond"
	PolicySuspendSession PolicyActionKind = "suspend_session"
	PolicyModelDecide    PolicyActionKind = "model_decide"
)

type PolicyAction struct {
	Kind          PolicyActionKind
	Response      *ResponseTemplate     // for PolicyRespond
	ModelDecision *ModelDecisionPolicy  // for PolicyModelDecide
}

type ResponseTemplate struct {
	Action string
	Values map[string]json.RawMessage
}

type ModelDecisionPolicy struct {
	Instruction       string
	RecommendedAction string
	RecommendedValues map[string]json.RawMessage
}

type Gate struct {
	ID             ID
	Kind           Kind
	Resolver       ResolverKind
	Blocks         Blocks
	Effect         Effect
	Criticality    Criticality // empty defaults to critical
	Subject        Subject
	Prompt         Prompt
	ResponsePolicy ResponsePolicy
	Restorable     bool
}
```

`gate.Gate` is a **pure public envelope — no routing.** The private route is not a field on it: an
unexported field could not be set from `pkg/session`/`pkg/loop`, and an exported one would leak
internal routing to clients. The session keeps the route (and the typed payload + live state) in a
private `directoryEntry{gate, route, payload, state}` keyed by `GateID`; `gate.Gate` is only its
transport-safe projection.

`Subject` should be typed but sparse:

```go
type Subject struct {
	ToolExecutionID uuid.UUID
	ToolUseID       string
	TurnID          uuid.UUID
	StepID          uuid.UUID
	InputID         uuid.UUID
	FromModel       string
	ToModel         string
}
```

`Prompt` should be normalized enough for generic clients:

```go
type Prompt struct {
	Title    string
	Body     string
	Controls []Control
	Schema   *PromptSchema // optional, for extension/custom display
}

type Control struct {
	Action string // "approve", "deny", "answer", "resend", "drop", ...
	Label  string
	Style  string // optional hint: primary, danger, neutral
	Fields []Field
}

type FieldKind string

const (
	FieldText        FieldKind = "text"
	FieldSelect      FieldKind = "select"
	FieldMultiSelect FieldKind = "multi_select"
)

type Field struct {
	Name     string // key under ResponseRequest.Values
	Kind     FieldKind
	Required bool
	Options  []Option
	Default  json.RawMessage
}

type Option struct {
	Value       json.RawMessage // machine value returned by the client
	Label       string          // human display label
	Description string          // optional display detail
}
```

The same `Action` strings are what a caller sends back in `ResponseRequest`. Gate-specific fields belong
under `Values`; for example `{"scope":"session"}` for an approval, `{"answer":"blue"}` for an
ask-user response, or no values for a deny/drop.

For permission gates, the normalized prompt must include the full response shape a generic client
needs:

- the `approve` control has a `scope` select whose values are stable strings (`once`, `session`,
  `workspace`) derived from `PermissionRequest.AllowedScopes()`;
- if the request contains escalation grants, `approve` also has an `accepted_grants` multi-select;
  each option's `Value` is the grant token and its label/description come from
  `tool.GrantDisplay.Description`;
- the UI renders labels/descriptions, but the response echoes the selected machine values under
  `Values`; the session validates them against the private `PermissionPayload.Request` before
  dispatching.

`ResponseRequest` is the public input shape. `GateResponse` is the internal, server-attributed shape
delivered to the session gate router. `GateResponse.Source` is server-owned provenance, so public
clients cannot set it.

**Two layers, cleanly split.** Everything a UI renders is the **normalized `Prompt`** on the `Gate`
envelope — `Title`, `Body` (context/description), `Controls` (options) — kind-agnostic, so one
renderer handles every gate. The per-kind **payload is *not* display**: it is the resolver's
server-private data for *validating and applying* the response. It is **not** on the public
`GateOpened` event (which fans out to SSE/history) — it is persisted as the **private projection
inside `GatePreparedRecord`** (restore-visible but never fanned out), kept in the
`directoryEntry`. A permission gate therefore populates `Prompt` (title,
the command + grant descriptions, and approve/deny + scope + grant-choice controls) for the human, and
keeps only `tool.PermissionRequest` in its payload — **reusing `tool.MarshalPermissionRequest`** — with
**no `ToolName`** (that is `Prompt`/`Subject`, and already inside the request).

```go
// Server-private: validate the response (accepted grants ⊆ Request.Grants) and apply the approval.
// Never shipped to clients — the human-facing display is the normalized Prompt on the Gate.
type PermissionPayload struct {
	Request tool.PermissionRequest // canonical source of grants (e.g. BashRequest.Grants); no duplicated ToolName
}

type AskUserPayload struct {
	Question string
	Choices  []string
}

type SwitchModelPayload struct {
	FromModel string
	ToModel   string
	Reason    string
}

type ResumeInputPayload struct {
	InputID  uuid.UUID
	Preview string
}
```

**Permission responses echo accepted grants, validated against request grant tokens.** A permission
approval carries the operator's **accepted** escalation grant tokens — a typed `AcceptedGrants
[]string` (under `Values["accepted_grants"]` or a permission response struct) — which the session
**validates is a subset of the `GrantDisplay.Token` values carried in the permission *request*** (e.g.
`BashRequest.Grants` — the single canonical source, not labels/descriptions or a duplicated field)
before dispatch. These map
1:1 to `command.ApproveToolCall.AcceptedGrants` (`pkg/command/approve.go`), which the runner places
on the first spawn's ctx and hands to `Permission.Grant` for MAC-verified persistence
(`pkg/loop/runner.go`); dropping them silently breaks pre-ask escalation and grant persistence.

Extension gates should be namespaced and display-oriented:

```go
type ExtensionPayload struct {
	Namespace string
	Schema    PromptSchema
	Metadata  map[string]string // small display/audit fields only
}
```

The extension resolver, not the schema, owns answer validation and side effects.

## 4. Concrete Gate Examples

Permission gate:

```go
Gate{
	ID:          gateID,
	Kind:        KindPermission,
	Resolver:    ResolverLoop,
	Blocks:      BlocksToolCall,
	Effect:      EffectResume,
	Criticality: GateCritical,
	Subject: Subject{
		TurnID:          turnID,
		StepID:          stepID,
		ToolExecutionID: toolExecutionID,
		ToolUseID:       "toolu_abc",
	},
	Prompt: Prompt{Title: "Approve tool call", Body: "Run `go test ./...`?", Controls: approveDeny()},
	ResponsePolicy: ResponsePolicy{
		Timeout: 5 * time.Minute,
		OnTimeout: PolicyAction{
			Kind:     PolicyRespond,
			Response: &ResponseTemplate{Action: "deny"},
		},
	},
}
```

AskUser gate that is not critical to forward progress:

```go
Gate{
	ID:          gateID,
	Kind:        KindAskUser,
	Resolver:    ResolverLoop,
	Blocks:      BlocksToolCall,
	Effect:      EffectResume,
	Criticality: GateNonCritical,
	Subject:     Subject{TurnID: turnID, StepID: stepID, ToolExecutionID: toolExecutionID},
	Prompt:      Prompt{Title: "Question", Body: "Which migration path?", Controls: choices("A", "B")},
	ResponsePolicy: ResponsePolicy{
		Timeout: 2 * time.Minute,
		OnTimeout: PolicyAction{
			Kind: PolicyModelDecide,
			ModelDecision: &ModelDecisionPolicy{
				Instruction:       "Choose the safest reversible option if the user does not answer.",
				RecommendedAction: "answer",
				RecommendedValues: map[string]json.RawMessage{"answer": json.RawMessage(`"A"`)},
			},
		},
	},
}
```

AskUser gate that must survive process loss — **future / session-hosted only.** A *current*
loop-`AskUser` gate is **non-restorable** (its parked goroutine is not persisted — see §8 Restore
rules), so `Restorable: true` implies a session-resolved variant that reissues the question after
restore, not today's `ResolverLoop` gate:

```go
Gate{
	ID:          gateID,
	Kind:        KindAskUser,
	Resolver:    ResolverSession, // future: a loop AskUser is non-restorable (parked goroutine not persisted)
	Blocks:      BlocksSession,
	Effect:      EffectResume,
	Criticality: GateCritical,
	Subject:    Subject{TurnID: turnID, StepID: stepID},
	Prompt:     Prompt{Title: "Clarification needed", Body: "Which production cluster?", Controls: freeText()},
	Restorable: true,
	ResponsePolicy: ResponsePolicy{
		Timeout:   10 * time.Minute,
		OnTimeout: PolicyAction{Kind: PolicySuspendSession},
	},
}
```

Loop-level switch-model gate:

```go
Gate{
	ID:          gateID,
	Kind:        KindSwitchModel,
	Resolver:    ResolverLoop,
	Blocks:      BlocksStep,
	Effect:      EffectControl,
	Criticality: GateNonCritical,
	Subject:     Subject{TurnID: turnID, StepID: stepID, FromModel: "gpt-x", ToModel: "gpt-y"},
	Prompt:      Prompt{Title: "Switch model?", Body: "The current model is unavailable.", Controls: yesNo()},
	ResponsePolicy: ResponsePolicy{
		Timeout:   15 * time.Second,
		OnTimeout: PolicyAction{
			Kind: PolicyRespond,
			Response: &ResponseTemplate{
				Action: "switch",
				Values: map[string]json.RawMessage{"to_model": json.RawMessage(`"gpt-y"`)},
			},
		},
	},
}
```

Session stranded-input gate:

```go
Gate{
	ID:          gateID,
	Kind:        KindResumeInput,
	Resolver:    ResolverSession,
	Blocks:      BlocksSession,
	Effect:      EffectInitiate,
	Criticality: GateCritical,
	Subject:    Subject{InputID: inputID},
	Prompt:     Prompt{Title: "Resume queued input?", Controls: resendDrop()},
	Restorable: true,
	ResponsePolicy: ResponsePolicy{
		OnTimeout: PolicyAction{Kind: PolicyWait},
	},
}
```

Consumer extension gate:

```go
Gate{
	ID:          gateID,
	Kind:        Kind("consumer.deploy_approval"),
	Resolver:    ResolverExtension,
	Blocks:      BlocksToolCall,
	Effect:      EffectResume,
	Criticality: GateCritical,
	Subject:     Subject{TurnID: turnID, StepID: stepID, ToolExecutionID: toolExecutionID},
	Prompt:      Prompt{Title: "Deploy to staging?", Controls: approveDeny()},
	ResponsePolicy: ResponsePolicy{
		Timeout: 5 * time.Minute,
		OnTimeout: PolicyAction{
			Kind:     PolicyRespond,
			Response: &ResponseTemplate{Action: "deny"},
		},
	},
}
```

## 5. Response Policies

`ResponsePolicy` is the normalized no-answer layer for each gate. Defaults should be configured by
`gate.Kind` at the session/composition root, then copied onto each opened gate, so restore sees the
exact policy that was active when the prompt was shown. A producer may request a per-gate override,
but the session should validate it against the gate kind before appending `GatePreparedRecord`.

`Criticality` is the policy guardrail. Empty criticality means `critical`. A critical gate may still
have a fail-closed timeout response such as tool `deny`, but it may not use `model_decide`. Only
`GateNonCritical` gates can delegate a missing answer to a policy responder/model.

Default policies:

| Gate scope | Default response policy |
|---|---|
| Tool-execution permission gates (`fetch`, `bash`, `read`, `write`, `glob`, and equivalent consumer tools) | `Timeout=5m`, then `PolicyRespond{Action:"deny"}` |
| Critical `AskUser` gates with restorable continuation | timeout chosen by session config, then `PolicySuspendSession` |
| Non-critical `AskUser` gates with explicit autonomy | timeout chosen by producer/session config, then `PolicyModelDecide` |
| Session resume/input gates | `PolicyWait` unless the session owner configures a narrower policy |

Policy actions:

- **`wait`** - no automatic answer; the gate remains open until a caller responds, the owner closes
  it, or restore determines it cannot be reattached.
- **`respond`** - after the timeout, the session synthesizes `GateResponse{Action, Values}` from the
  template and submits it through `RespondGate`. Tool permission gates should normally use this with
  `Action="deny"` so unattended no-answer behavior fails closed. The default for tool-execution
  permission gates, including fetch, bash, read, write, and glob-style tools, is 5 minutes then deny.
- **`suspend_session`** - after the timeout, the session checkpoints/releases the in-process session
  while leaving the gate open. A later response resumes by acquiring/restoring the whole session from
  durable history and dispatching the response to the restored resolver route. This is valid only for
  `Restorable=true` gates whose resolver can rebuild the blocked continuation.
- **`model_decide`** - after the timeout, the session invokes a configured policy responder/model
  outside the blocked continuation, using the policy instruction and optional recommended
  action/values. This is only for non-critical gates where the user has explicitly allowed
  best-effort autonomy. The generated answer still becomes a `GateResponse` with `Source.Kind=model`
  and closes exactly once through the same path.

Validation rules:

- `PolicyModelDecide` requires `Criticality=GateNonCritical`, a configured policy responder/model,
  and an action that is valid for the gate's prompt/kind. It is invalid for permission gates and any
  gate whose answer grants external side effects.
- `PolicySuspendSession` requires `Restorable=true` and a resolver restore hook that can rebuild the
  blocked continuation. It is invalid for current parked-goroutine-only tool gates.
- `PolicyRespond` requires a positive timeout and a response action accepted by the gate kind. For
  tool permission gates, the default response must be `deny`.
- `PolicyWait` is the only policy action that may have no timeout. Any timeout action with
  `Timeout=0` is rejected during gate registration.

Timeout is relative to the durable `GateOpened.Header.CreatedAt` timestamp. The timeout deadline is
therefore restorable without trusting any in-memory timer. On restore, the session recomputes whether
the policy is already expired:

- expired `respond` policies submit the synthetic response before the gate is listed as open;
- expired `model_decide` policies ask the configured policy responder/model to decide if it is
  available;
- expired `suspend_session` policies leave the gate open and keep the session suspended until a
  response arrives or an owner closes it;
- if a policy action requires a resolver that cannot be restored, the session closes with
  `restore_unavailable`.

Do not make `valid_until` the authoritative gate field. A UI or read DTO may expose a projected
deadline for display, but the durable source of truth is the opened timestamp plus the persisted
policy timeout.

Human response, model response, and timeout response all race on the same session directory entry.
The winner marks the gate claimed/closed under the directory lock; every loser sees a stale gate id
and becomes a no-op or typed rejection. This avoids a separate timer race path.

### Scalability Constraints

The design scales if gates stay session-local:

- Keep the authoritative directory per live session. Do not introduce a global gate service or a
  transport-owned open-gate index.
- Keep timers as an execution detail, not the source of truth. A simple per-session timer loop is
  fine for v1; for many open gates, use one min-heap/timer-wheel per session instead of a permanent
  goroutine per gate. Restore still recomputes from `GateOpened.Header.CreatedAt + Timeout`.
- Bound live gate entries and maximum timeout duration in session config. The cap counts
  `preparing + open + claiming`, so failed activations cannot accumulate invisible prepared entries.
  Hitting the bound should fail closed or reject the producer before appending `GatePreparedRecord`.
- Session suspension is the scalability path for long human waits. It releases the in-process
  session but does not answer or close the gate.

## 6. Session-Owned Directory and Events

The session owns the authoritative open-gate directory:

```go
type Directory struct {
	open map[gate.ID]directoryEntry // gate + private route + typed payload + live state
}

// directoryEntry is session-private: the public Gate envelope PLUS the internal route, the typed
// resolver payload (never shipped to clients), and the live state.
type directoryEntry struct {
	gate    gate.Gate
	route   gate.Route // set only after owner activation for parked loop gates
	payload gate.Payload
	state   gateState // preparing | open | claiming | closed
}
```

The directory is not rebuilt by `serve` watching events. It is the source of truth that `serve` calls
into. The transport can offer `GET /sessions/{sid}/gates` by asking the session for a snapshot, and
can resolve via `POST /sessions/{sid}/gates/{gate_id}`. Only `open` entries are listable and
answerable. `preparing` entries are durable but not live-announced yet; they exist only while the
owning actor is installing its local blocker.

Open and resolve are **enduring** events. Per `pkg/event`, every event embeds exactly one lifecycle
mixin and one scope mixin (compiler-enforced). **Scope tracks the producer, not the directory:** a
loop-produced gate's events are **`loopScoped`** — they carry `Header.LoopID` and are per-loop
filterable (subagent delivery, per-loop SSE). They are **not** `sessionScoped`, because
`ShouldDeliver` delivers session-scoped events to *every* subscriber and the `Header` convention
forces `LoopID/TurnID/StepID` to zero on them (`pkg/event/filter.go`, `event.go`). The session
directory is still the source of truth — it is populated by the durable tap, which sees every event
regardless of scope. Session-owned gates (stranded input) get `sessionScoped` variants when they land.

**Prepare is private; open is public.** `PrepareGateOpen` must durably commit the public envelope and
private payload before any local blocker is installed, but it must not expose a public prompt yet. The
prepare append writes one private journal record whose durable content is:

```go
type GatePreparedRecord struct {
	Prepared GatePrepared   // private/internal projection; not eligible for SSE/history
	Payload GateOpenPayload // private projection; restore/validation only
}
```

After the owner installs its local blocker, `ActivateGate` appends the public `GateOpened` event and
only then makes the gate listable/answerable and live-fans it out. This creates the durable ordering
restore needs:

1. `GatePreparedRecord` without `GateOpened` is a prepared-but-never-public gate; it is not listed,
   not answerable, and can be skipped or internally abandoned on restore.
2. `GateOpened` with a matching `GatePreparedRecord` is a public open gate and has the private payload
   needed for validation/restore.
3. `GateOpened` without a matching private payload is corrupt/legacy data and restores as
   `restore_unavailable`, never as an answerable gate.

`Payload` is never sent to public event subscribers. If `GatePreparedRecord` cannot be appended,
registration fails before a local blocker is installed. If `GateOpened` cannot be appended during
activation, the actor removes/abandons the local blocker and the gate remains non-public.
Implementation guardrail: `GatePrepared` must not be appended through the normal public
`EventRecord`/`hub.PublishEvent` path. It belongs only inside the private `GatePreparedRecord`, which
ordinary event replay/SSE/history skip.

**Resolve is a *single* durable event.** `GateResolved` carries the answer **and** the close
together, so restore never sees an answered-but-open gate (two events would not be atomic — the hub
appends one at a time and can die between). `Reason` distinguishes an actual answer from an
abandon/owner/policy/timeout close.

```go
// GatePrepared is the private/internal prepared projection inside GatePreparedRecord. It is durable
// so restore can validate a later GateOpened, but it is not fanned out to SSE/history and does not make
// the gate answerable.
type GatePrepared struct {
	Header event.Header
	enduring
	loopScoped // session gates use a sessionScoped variant; scope follows the producer
	Gate gate.Gate // public envelope stored privately until ActivateGate appends GateOpened
}

type GateOpened struct {
	Header event.Header
	enduring
	loopScoped // session gates use a sessionScoped variant; scope follows the producer
	Gate gate.Gate // pure public envelope; route omitted from marshal — NO payload (fans out to SSE/history)
}

// GateOpenPayload is the PRIVATE projection inside GatePreparedRecord (NOT an enduring event by itself
// and never fanned out to SSE) carrying the server-side typed payload the resolver needs for restore
// + response validation. Restore joins it to GateOpened by GateID.
type GateOpenPayload struct {
	GateID  gate.ID
	Payload gate.Payload // sealed union (see codecs below)
}

// GateResolved is the SINGLE atomic close-with-answer record. Decision fields (Action, Scope) stay
// in the clear; Reason is the close reason; per-kind Audit is redaction-aware. A non-answer close
// (abandon/owner) sets Reason with Action="".
type GateResolved struct {
	Header event.Header
	enduring
	loopScoped
	GateID gate.ID
	Reason gate.CloseReason
	Action string             // approve / deny / answer / drop — "" for a non-answer close
	Scope  tool.ApprovalScope `json:",omitzero"` // permission gates — in the clear
	Source gate.ResponseSource
	Audit  gate.ResponseAudit // per-kind sealed union (see codecs) — grant DELTA DESCRIPTIONS, not tokens
}
```

**Payload/audit wire contract (required for restore-safety).** `gate.Payload` and
`gate.ResponseAudit` are **interface fields on durable events** — so, exactly like
`tool.PermissionRequest`, each is a **sealed union with a `kind` discriminator, explicit
marshal/unmarshal functions, and size caps**; a bare interface is not restore-safe (JSON cannot
decode into an interface without a discriminator). Permission payloads reuse
`tool.MarshalPermissionRequest`; a permission `Audit` records the **accepted grant DESCRIPTIONS** the
operator chose (after the accepted tokens are validated against `GrantDisplay.Token`) — **not** the raw single-mint tokens, and
**not** the persisted grant deltas, which do not exist yet: `GateResolved` commits *before* the
answer is dispatched, and the runner computes/persists the deltas later inside `ApproveToolCall`
handling. Auditing the *persisted* deltas would require a resolver ack-back (a later record). An
unknown discriminator fails closed on decode.

Existing `PermissionRequested` and `UserInputRequested` can either be migrated to carry `GateID` and
fit this model, or be replaced by `GateOpened` variants with typed payloads. The important invariant
is that every public open gate has a durable `GateOpened` event, matching private prepared payload,
and an explicit close record.

**Every decision is durable — gated or not.** Today only a *gate* leaves a record; an auto-approve
or auto-deny (Stages 1&ndash;6.5 of the checker) is silent, so the transcript cannot show *"Trusted
auto-approved `Bash(npm i)`"*, *"ZeroTrust auto-approved a read"*, or *"HardDeny blocked `.env`"*.
Close the gap with a companion enduring event, **`PermissionDecided`**, emitted for every checker
decision that did **not** open a gate:

```go
type PermissionDecided struct {
	Header event.Header
	enduring
	loopScoped
	ToolExecutionID uuid.UUID          // correlation to the tool call
	ToolName        string
	Effect          Effect             // approve | deny — never ask (an ask opens GateOpened instead)
	Reason          DecisionReason     // containment | hard_deny | effect_checker | hard_approve |
	                                   //   persisted_ws | persisted_user | session_policy | posture_auto
	Subject         gate.ResponseAudit // redacted path/command preview — per-kind, same rules as GateResolved
}
```

The audit invariant becomes: **every permission decision has exactly one durable record** — an auto
decision as `PermissionDecided`, a gated one as `GateResolved` (the gate's `GateOpened` +
`GateResolved` already cover the Ask path). No approve or deny is silent; `PermissionDecided ∪
GateResolved` is the complete decision log.

- **Emitter:** the loop, when it acts on a checker `Effect` that is not `Ask` — appended (enduring)
  via the hub *before* the tool runs (approve) or *before* the denied tool result (deny).
- **Redaction:** `Subject` is redaction-aware like `GateResolved.Audit`; `Effect`/`Reason` stay in the
  clear (that is the audit value), the path/command is previewed/redacted. `loopScoped`, like the gate
  events.

Two calls to settle when implementing:

- **Volume vs. coverage.** Recording *every* decision includes every auto-approved read (`Grep`/
  `ReadFile` on the ReadGuard fast path), which is high-frequency. Default: record **all denies** and
  **all non-read approvals**; make read-fast-path approvals opt-in if journal volume bites.
- **Append fail-mode.** A **deny** is a **required** enduring append (a security event, fault-secure
  on loss). An **approve** may be best-effort audit (logged on loss, never blocking the tool) —
  mirroring how the gate-reply command append never blocks a human's decision.

Policy application is auditable the same way: a policy- or model-generated answer is captured by
`GateResolved{Reason: policy_response, Source: policy|model}`, so the actual
decision — not just its provenance — reaches the transcript. A `suspend_session` action produces no
answer: it is recorded by the session lifecycle event that releases the in-process session, not by
closing the gate.

## 7. Open and Resolve Protocol

The race-safe open sequence is **commit-before-install-before-announce**:

1. The producer builds the envelope + private payload and asks the owning actor to open a gate.
2. The actor calls `PrepareGateOpen`. The session mints `GateID`, validates the policy, appends the
   private `GatePreparedRecord`, and records a non-listable `directoryEntry{state:preparing}`. This uses a
   strict gate-prepare appender, not plain `hub.PublishEvent`: the append error is returned to the
   actor, while `hub.PublishEvent` still keeps its current fault-and-return-nil contract.
3. If the append fails, the actor installs **no** local blocker and the producer fails closed.
4. If the append succeeds, the actor installs its local blocker (`pendingGates`) while still on the
   actor goroutine.
5. The actor calls `ActivateGate(gateID, route)`. The session fills the private route, appends the
   public durable `GateOpened` event, flips the entry to `open`, makes it listable/answerable, and
   live-fans out that just-committed projection.

This resolves the two required ordering properties without contradiction: a failed durable prepare
never installs a parked local blocker, a failed durable activation never exposes the gate publicly, and
a public answer cannot be processed before the actor has a
blocker. A guessed response to a `preparing` gate is rejected as not-ready/not-found; normal clients
cannot see the gate until activation because it is neither listed nor live-announced.

If the actor dies between prepare and activate, the session owns a durable but non-listable
`preparing` gate that was never public. Owner cleanup should abandon it internally when possible;
after process loss, restore skips the prepared-only record because no public `GateOpened` was ever
committed.

**Required refactor (P1), not an existing invariant.** Today the parked *runner/tool* goroutine
registers via the `gateReg` handshake and the **actor** installs `pendingGates` (`pkg/loop/gate.go`).
Gate preparation, blocker installation, and activation must be woven into that actor-side install
and serialized on the actor goroutine. The new test is the activate race: no HTTP/list response is
possible before activation, and an answer immediately after `GateOpened` delivers exactly once.

Resolve flow:

1. `serve` parses `{gate_id}` and a public `ResponseRequest{Action, Values}` body.
2. The session stamps the request as `GateResponse{GateID, Action, Values, Source:user}`. Public
   callers do not control `Source`.
3. A policy timer, if it expires, also sends a `GateResponse` to the session for `respond` and
   `model_decide` policies. It does not dispatch directly to the resolver.
4. The session looks up the gate, checks the action against the gate kind/prompt, and **claims** it
   under the directory lock (a duplicate is a no-op) — an in-memory `claiming` state, not yet
   durably closed.
5. **Durable-first, single event:** the session appends the one `GateResolved` (fail-secure — the
   hub's enduring invariant). If the append faults, deliver nothing; the gate is neither resolved in
   the journal nor consumed, so restore/list correctly still see it open (re-answerable).
6. **Deliver via the resolver's command path, not event fan-out.** Only after the durable commit
   does the session dispatch the answer to the loop over the actor command channel (`routeGate`) —
   **never** via hub subscription fan-out, which is best-effort and *fails* an Enduring subscriber on
   egress overflow (`pkg/hub/hub.go`), so it cannot be a reliable resume path. Post-commit dispatch
   uses a **session-owned context, not the HTTP request context**: `routeGate` aborts on `ctx.Done()`
   (`pkg/session/session.go`), so a client that disconnects *after* the durable commit must not cancel
   delivery of an already-committed answer.

For v1 loop gates, `GateResolved{Reason:answered|policy_response}` means the answer was accepted and
committed by the session, **not** that the parked runner consumed it. The current loop command path
has no ack. If the owner closes before the session claims the gate, `owner_closed`/`abandoned` wins
the same claim race. If the owner exits after the answer commit but before command delivery, the close
record remains the accepted answer; the session may log or emit a later diagnostic
`GateDeliveryFailed`, but it must not rewrite the close reason or reopen the gate. A future resolver
ack can strengthen specific resolvers without changing the public gate id model.

**Gate states:** `preparing` (durable `GatePreparedRecord`, no public `GateOpened`, not
listed/answerable) → `open` (durable `GateOpened`, listed) → `claiming` (in-memory only, between the
lock-claim and the durable commit; **never persisted**, so a process death here leaves the gate `open`
on restore — correctly re-answerable) → `closed` (durable `GateResolved`). `ListGates` shows only
`open`; `preparing` and `claiming` are not client-visible states.

The session must not hold the directory lock while calling a loop/foreign/extension resolver. Mark
or remove the gate under lock, then dispatch. Duplicate answers, stale ids, wrong actions, and kind
mismatches are fail-secure no-ops or typed rejections at the session boundary.

HTTP success should be `202 Accepted` for v1 loop gates: the answer is accepted and durably committed
by the session gate router, not proven consumed. A resolver with an explicit ack path may return a
stronger success later, but generic clients must rely on subsequent events for the downstream effect.

`suspend_session` is different: the timer does not produce an answer and does not close the gate. It
asks the session owner to stop the live in-memory session after durable state is checkpointed. The
open gate remains in the journal and in the read-side projection. When a response later arrives, the
consumer/infra must restore the session and route the response to the restored session directory.

## 8. Restore Semantics

Prompt events are durable, but not every open gate is answerable after restore. The event proves the
question existed; the open blocker may not.

Restore should replay `GatePreparedRecord`, `GateOpened`, and `GateResolved` to reconstruct candidate
open gates, then ask the gate's resolver whether the gate can be restored:

```go
type Resolver interface {
	// Gets the opened record — envelope PLUS the private payload — not just the envelope: the
	// resolver needs the typed payload to rebuild its continuation/validation state.
	RestoreGate(ctx context.Context, g gate.Gate, payload gate.Payload) (route gate.Route, ok bool, err error)
}
```

Rules:

- `Restorable=false` gates are historical after process/session restore unless their owning live
  resolver is still present. Current turn-scoped permission and `AskUser` gates fall here because the
  parked goroutine is not persisted.
- `Restorable=true` gates must carry enough durable subject/payload state for the resolver to rebuild
  the private route. Stranded-input and pre-turn/session decisions are good candidates.
- `PolicySuspendSession` requires `Restorable=true`. It is invalid for current parked-goroutine-only
  gates because there would be no durable continuation to answer after the process is gone.
- The session directory remains authoritative. Restore must not let loop and session independently
  rebuild two open records for the same gate. The session replays the private `GatePreparedRecord`,
  joins it to the public `GateOpened`, then calls the resolver restore hook to reattach a private
  route.
- Response policies are restored from the durable `GateOpened` timestamp. The session recomputes
  deadlines from `GateOpened.Header.CreatedAt` and applies already-expired policy actions before
  exposing the open gate snapshot.
- If a gate cannot be restored, the session should publish or derive a close with
  `Reason=restore_unavailable` so read-side projections do not list it as open forever.
- A durable `GatePreparedRecord` that never got a matching public `GateOpened` is not exposed as open
  on restore. Current parked loop gates skip it because no client could have seen or answered it.

## 9. Security and Fail-Secure Rules

- **Opaque gate ids:** public clients answer by `GateID`; internal routes never cross the API.
- **Commit-before-install-before-announce:** append private `GatePreparedRecord` first, install the
  local blocker second, then activate by appending public `GateOpened` and list/live-announce. Failed
  durable prepare installs no blocker; failed activation removes/abandons the local blocker; unactivated
  gates are not answerable.
- **Kind/action checked:** a response action must match the gate kind and prompt controls.
- **At most once, delivery best-effort:** the session claims a gate atomically, so duplicates cannot
  fire twice. *Delivery* to the resolver is not proven — gate commands carry no ack and `routeGate`
  is fire-and-route (`pkg/session/session.go`), so HTTP success means **durably accepted by the
  session gate router** (§7), not consumed. Owner close wins only if it claims the gate before the
  answer; after an answer is durably committed, later delivery failure is diagnostic, not a close
  rewrite.
- **Policy uses the same response path:** timeout/model responses call `RespondGate`; they do not
  bypass action validation or the at-most-once claim.
- **Server-owned provenance:** callers cannot choose `GateResponse.Source`; the session sets user,
  policy, or model provenance at the trust boundary.
- **Model autonomy is explicit:** `PolicyModelDecide` is rejected unless the gate is
  `GateNonCritical`, has a configured policy responder/model, and cannot grant external side effects.
- **Suspend requires restore *and* a not-yet-designed lifecycle contract:** a suspend policy is
  rejected unless the gate is restorable and the resolver can rebuild the continuation.
  `PolicySuspendSession` also needs a concrete `SessionSuspended` lifecycle event + resume contract
  that does **not exist today** (lifecycle is active/idle/stopped/restore) — a hard P3 prerequisite,
  not assumed.
- **Bounded resource use:** session config caps open gates, timeout duration, and timer work; hitting
  a cap rejects registration before any prompt is published.
- **No transport shadow authority:** `serve` may display snapshots from the session, but it does not
  infer open gates from its own event subscription.
- **Typed core, constrained extension:** harness-owned gates use typed payloads; extension gates get
  dynamic display schema only, with behavior validated by their resolver.
- **Explicit close:** generic restore/listing uses `GateResolved`, not inferred tool/turn events.
- **No external deps:** stdlib plus internal `uuid`/event/tool types.

## 10. API Sketch

```go
// PrepareGateOpen durably commits the public envelope plus private payload as one private
// GatePreparedRecord. It returns a GateID but does not expose the gate to clients yet; the entry is
// state=preparing.
type Registrar interface {
	PrepareGateOpen(ctx context.Context, g gate.Gate, payload gate.Payload) (gate.ID, error)
	// ActivateGate is called by the owner after its local blocker/continuation exists. It appends the
	// public GateOpened event, flips the entry to open, and live-fans out the prompt.
	// The private route never lands on gate.Gate and is not durable unless a resolver
	// can reconstruct it during restore.
	ActivateGate(ctx context.Context, id gate.ID, route gate.Route) error
	CloseGate(ctx context.Context, id gate.ID, reason gate.CloseReason) error
}

// Non-parked resolvers whose continuation already exists may use a helper that performs
// PrepareGateOpen + ActivateGate back-to-back. Parked loop gates must use the two-step form.

// ListGates returns the public envelopes. All display — including grant-choice controls — is
// normalized into Gate.Prompt, so a reconnecting client renders and answers from Gate alone; the
// typed payload stays server-private (directoryEntry), used only by RespondGate for validation.
type Resolver interface {
	RespondGate(ctx context.Context, response gate.GateResponse) error
	ListGates(ctx context.Context) ([]gate.Gate, error)
}
```

The loop should receive a narrow registrar interface from the session rather than importing the
session package. That preserves the existing dependency direction: loop owns local continuation
state; session owns fan-in, routing, persistence, and API-facing gate ids.

The old session methods (`Approve`, `Deny`, `ProvideUserInput`) should become compatibility wrappers
or test helpers during migration. They should not remain the primary session or `serve` API, because
each new gate kind would otherwise require another session method and another transport branch.

## 11. Phasing

- **P1 - session-authoritative tool gates:** introduce `GateID`, the session directory, private
  `GatePreparedRecord`, public activation-time `GateOpened`, the single `GateResolved` event (no
  separate close/respond pair), and the prepare/install/activate handshake for existing
  permission/AskUser gates. `serve` resolves by
  `GateID` only and returns `202 Accepted` for v1 loop gates. **Owner-abandon cleanup:** when a turn
  is interrupted or a parked producer
  goroutine exits, the loop must `CloseGate(gateID, abandoned|owner_closed)` for every gate it opened
  — replacing today's turn-local `clearGates()`, since the directory is now session-owned, not
  turn-scoped. **Typed permission payload lands in P1, not P2:** opaque `GateID` for permission gates
  needs the payload server-side to validate accepted grants, so `PermissionPayload` + its
  sealed-union codec ship here. **Every-decision audit lands in P1:** emit `PermissionDecided` for
  auto-approve/auto-deny (denies required, read-fast-path approvals configurable), so
  `PermissionDecided ∪ GateResolved` is the complete decision log from day one.
- **P2 - normalize prompt model:** migrate `PermissionRequested`/`UserInputRequested` to the envelope
  or make them wrappers over `GateOpened`; add the remaining kind prompts/payloads and display-safe
  `Subject` (the permission payload already landed in P1).
- **P3 - response policies:** add per-kind default policies, persist the selected policy in the
  prepared/opened gate envelope, route timeout-generated responses through `RespondGate`, and support
  `PolicySuspendSession` only for restorable gates. Include policy validation, open-gate/timeout
  caps, and the 5-minute deny default for tool-execution permission gates.
- **P4 - restore/list:** rebuild open gate snapshots from `GatePreparedRecord`/`GateOpened`/
  `GateResolved`; mark current turn-scoped tool gates non-restorable; implement restorable session
  gates for stranded input.
- **P5 - new producers:** add loop-level questions such as switch-model and foreign/extension gates
  through the same registrar/resolve path.

## 12. Testing

- Table-driven, `-race`.
- Open ordering: `PrepareGateOpen` append failure installs no local blocker; a prepared-but-not-
  activated gate is not listed, not live-announced, and not answerable.
- Activation race: an answer immediately after `GateOpened` is accepted once and reaches the actor's
  installed local blocker.
- Prepared/open restore safety: `GatePreparedRecord` commits before `GateOpened`; prepared-only data is
  not listed/answerable, and `GateOpened` without a matching payload restores as
  `restore_unavailable`, not as an answerable gate.
- API route hiding: `POST /sessions/{sid}/gates/{gate_id}` resolves without client `LoopID`.
- Kind/action mismatch fails secure and does not dispatch.
- Duplicate answer delivers once.
- HTTP resolve success for loop gates is `202 Accepted` and means durably accepted, not runner
  consumed; downstream consumption is observed through later events.
- Timeout-generated responses use the same `RespondGate` path and lose cleanly to an earlier human
  response.
- Tool-execution permission gates for fetch/bash/read/write/glob-style tools timeout at 5 minutes,
  deny exactly once, and emit `GateResolved{Reason: policy_response}`.
- `PolicySuspendSession` keeps the gate open, records session suspension, and resumes only after
  restore reattaches a resolver route.
- Non-critical `PolicyModelDecide` responses are marked `Source.Kind=model` and remain auditable.
- `PolicyModelDecide` registration fails for critical gates, permission gates, side-effect-granting
  gates, and sessions without a configured policy responder/model.
- Policy registration enforces configured caps for open gates and timeout duration before appending
  `GatePreparedRecord`; the open-gate cap includes `preparing + open + claiming`.
- Close/abandon removes from `ListGates`.
- Every checker `Effect` yields exactly ONE durable record: `Ask` → `GateOpened` (+ `GateResolved`);
  `AutoApprove`/`Deny` → `PermissionDecided` (never both, never neither). `HardDeny` on `.env` emits
  `PermissionDecided{Effect: deny, Reason: hard_deny}`; a Trusted-posture Bash auto-approve emits
  `PermissionDecided{Effect: approve, Reason: posture_auto}`; a ZeroTrust auto-approved read emits
  `PermissionDecided{Effect: approve, Reason: posture_auto}` (or is coalesced per the volume rule).
- Gate-prepare append failure leaves no local blocker; activation failure removes/abandons the local
  blocker and leaves no public open gate.
- Restore reopens only `Restorable=true` gates with successful resolver restore hooks.
- Restore closes/skips non-restorable turn/tool gates so stale prompts do not remain answerable.
- Restore recomputes expired policy deadlines from the durable open timestamp.
- Extension gate schema is display-only; resolver still validates action and values.
