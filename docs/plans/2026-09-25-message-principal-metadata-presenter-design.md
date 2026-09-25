# Design: message principal, message metadata, and the Message Presenter

Status: **approved by the owner 2026-09-25**, not yet implemented. Cross-module
(core, sessionstore, harness, host, factory, wui, tests). This file lives in
`harness` because the presenter is the central new API; it is the single source
of truth for the feature. Evidence is cited at core v0.11.0, sessionstore
v0.13.1, harness v0.40.2, host v0.10.3, factory v0.11.1, wui v0.3.0.

## Goal

Every command a user sends may carry:

- a **principal**: the verified sender, stamped only by Factory, for audit and
  multiplayer attribution;
- **metadata**: optional, app-defined custom fields supplied by the client.

A harness **Message Presenter** may read both and prepend or append blocks to a
user message so the model sees selected context (for example
`[from: Alex (parent) · space: Family]`). Nothing else ever routes principal or
metadata to the model.

## Owner rulings (binding)

1. `principal` and `metadata` are **sibling typed fields**. The principal is not
   a reserved key inside metadata.
2. `principal` is a **new Core `sessionwire/v1` type**
   `Principal{Tenant, Subject, Kind}`. Only `TenantID` exists in Core today;
   `Subject` and `Kind` exist only inside Factory (`factory identity/principal.go:54-79`).
   Factory's `identity.Principal` keeps its API and converts to and from it.
3. The principal is **optional** at the Core wire and in harness (harness runs
   without Factory; machine-originated messages have no human sender; no-auth
   deployments). Absent means byte-identical wire and journal to today.
4. In Factory, stamping is an **explicit per-deployment opt-in**
   (`factory.WithPrincipalStamping()`), only meaningful with a verified
   credential. When enabled it is **mandatory on every command Factory admits**
   (no per-command omission, so the audit trail has no gaps).
5. A client may **never** supply `principal`; a request carrying it is refused
   whether stamping is on or off.
6. The stamped principal holds only verified facts (tenant, subject, kind).
   Display names are resolved by the app's presenter from the subject and are
   never stored as identity.
7. The principal is never placed on `core content.Message` / `UserMessage`
   (the provider-facing model message).
8. The presenter may only **prepend and append**; the user's blocks are never
   modified. It runs **once** when the message enters the session; the
   rendering is journaled so replay, restore and compaction reproduce exactly
   what the model saw. Changing the presenter affects only new messages.
   Metadata the presenter does not render is audit-only. No presenter means
   current behaviour.
9. **Amendment (owner, 2026-09-25): stamp every command kind now.** The
   principal is stamped on `create`, `input`, `interrupt`, `restore` and
   `gate_response`, so the audit shows who stopped an agent or answered a gate.
   **Metadata stays on `create` and `input` only** (the only kinds with a
   message to present).
10. Approved defaults: metadata values are strings only; the `looprig` key
    prefix is refused (held, no semantics); custom fields are audit-only in the
    public journal unless rendered; machine-originated messages are never
    presented; a presenter failure refuses the command (`refused` disposition).

## 0. Current behaviour the design rests on

| Fact | Evidence |
|---|---|
| `CreateRequest`/`InputRequest` are `{version, command_id, session_id, [agent_id], blocks}` with declared member lists; any undeclared member is `unknown_field` | core `sessionwire/v1/commands.go:12-13`, `:457-467`, `:71-98`, `:121-144` |
| `InterruptRequest`/`RestoreRequest` are `{envelope, session_id}`; `GateResponseRequest` adds `gate_id, action, values, expected_open_event_id` | core `commands.go:148-151`, `:189-192`, `:231-237` |
| These requests marshal via struct tags (only `UnmarshalJSON` is custom), so an `omitempty` member is byte-invisible when absent | `commands.go:43-48`, `:102-106`; factory `internal/admission/service.go:635` `canonicalCommand = json.Marshal(request)` |
| IDs: 1..256 bytes, valid UTF-8 | core `ids.go:8`, `:86-97` |
| Capability tokens ride `hostlink_methods` on the tolerant negotiation reply; unknown tokens retained; `Supports` is exact match | core `hostlink.go:1256-1332`, `:1344-1356`; `hostlink_framing.go:100-105` |
| Factory admission reads only `principal.Tenant()` and passes the principal opaquely to `AuthorizeControl` | factory `internal/admission/service.go:318-360`, `:484-491`; `identity/principal.go:56-127` |
| Descriptor payload = canonical JSON of the decoded request; digest = SHA-256 of those bytes; a retry whose kind/digest/size differs is `command_mismatch` | factory `service.go:534-559`, `create.go:118`; sessionstore `disposition_inbox.go:153-169`, `:206`, `:233` |
| Descriptor has no actor; disposition inbox record version is 2; decoders are exact-version + `DisallowUnknownFields` + canonical re-encode check | sessionstore `disposition_inbox.go:24`, `:40-57`, `:494-510`; `catalog.go:1100-1133` |
| Host receives only a `command_id` over HostLink and reads payload bytes via `GetCommand`; create body is checked pre-attempt (`unknown_field`/`unsupported_version` → block, malformed → reject); input has no pre-attempt readability check | host `internal/realtime/hostlink/bindings.go:760`, `internal/sessionstoreadapter/inbox.go:40`, `internal/createbody/createbody.go:113-140`, `internal/commands/disposition_applier.go:422-439` |
| Host's product `BlockDecoder func([]byte) ([]content.Block, error)` receives the whole InputRequest JSON | host `internal/harnessadapter/rig.go:73`, `createbody.go:~90`, `internal/harnessadapter/session.go:~350-464` |
| `hostlink_methods` = fixed method list + `Config.Capabilities`, validated against `advertisedCapabilities` | host `internal/realtime/hostlink/centrifuge.go:130`, `bindings.go:1004-1024` |
| harness `runtimecommand.Admitted{CommandID, RuntimeCommandID, Kind, LeaseEpoch, Blocks, GateResponse, AttemptID}` | harness `pkg/runtimecommand/command.go:183-224`, `:358-378` |
| A disposition input becomes `command.UserInput{Header{..., Agency: AgencyUser}, Blocks}`; its intent record is the durable copy restore re-offers | harness `internal/sessionruntime/runtime_command.go:785-821`; `pkg/journal/record.go:154` |
| The loop wraps blocks into `content.UserMessage`; `TurnStarted.Message` is that message; restore appends it verbatim; requests are `cloneMessages(transcript)` | harness `internal/loopruntime/loop.go:1765-1767`, `:1431-1452`, `:1263-1266`; `internal/sessionruntime/restore.go:805-889` |
| `SubagentResult` and `UserInput` share the wrap path; machine inputs are `AgencyMachine` | `loop.go:2591-2615`; `internal/sessionruntime/delegation.go:2702` |
| Event decode is `DisallowUnknownFields`, fails closed on unknown type / newer `v`; additive members (precedent `Header.AgentName`) keep old journals readable | harness `pkg/event/marshal.go:26`, `:405-436`, `:836,882,926,1134`; `pkg/event/event.go:93-99` |
| Public redaction is per event type; `TurnStarted`/`TurnFoldedInto`/`InputCancelled` flow verbatim; Host's `publicbody.Project` rewrites only ids | harness `pkg/sessionwire/privacy.go:41-81`; host `internal/publicbody/publicbody.go:1-38,113` |
| wui journal envelope schema is `additionalProperties: true`; request schemas are `additionalProperties: false`, hand-mirrored in `schema.ts`; own commands matched by `cause.command_id` | wui `contract/schema/journal_event.schema.json`, `packages/protocol/src/fold.ts:918-1208,1497`, `enduring.ts:463` |

Two consequences drive the design:

- Factory marshals the decoded Core struct into the descriptor payload, so **any
  new Core member automatically enters the content digest**; idempotent
  admission needs no store change to cover principal and metadata.
- Host decodes the same struct strictly, so **an old Host cannot read a payload
  carrying the new members**, and for `input` there is no pre-attempt check, so
  the command would wedge after the attempt began. The capability token and the
  Factory-side gate are therefore mandatory.

## 1. Types, wire shapes, limits, enforcement

### 1.1 core `sessionwire/v1` (v0.12.0, additive)

```go
// ids.go
type SubjectID string            // validateID: 1..256 bytes, UTF-8
func (id SubjectID) Validate() error

// principal.go (new)
type PrincipalKind string
const (
    PrincipalKindActor   PrincipalKind = "actor"
    PrincipalKindService PrincipalKind = "service"
)
// Principal is the VERIFIED sender of a command as a Factory established it.
// It is never client-supplied; a Factory stamps it after credential verification.
type Principal struct {
    Tenant  TenantID      `json:"tenant"`
    Subject SubjectID     `json:"subject"`
    Kind    PrincipalKind `json:"kind"`
}
func (p Principal) Validate() error                // all three required; kind ∈ {actor, service}
func (p *Principal) UnmarshalJSON(b []byte) error  // strict: refuses unknown members
func (p Principal) MarshalJSON() ([]byte, error)   // member order tenant, subject, kind

// metadata.go (new)
// MessageMetadata is the client-supplied, app-defined bag on a create/input command.
type MessageMetadata map[string]string
const (
    MaxMetadataFields     = 16
    MaxMetadataKeyBytes   = 64
    MaxMetadataValueBytes = 1024
    MaxMetadataBytes      = 4096 // canonical encoding of the whole object
)
// Key grammar ^[a-z][a-z0-9_]{0,63}$; keys beginning "looprig" are refused (held).
// Values: valid UTF-8, no U+0000, no C0 controls other than \t \n.
func (m MessageMetadata) Validate() error          // *MetadataValidationError{Code, Key}
type MetadataValidationCode string // too_many_fields, invalid_key, reserved_key, value_too_large, invalid_value, too_large
```

Optional members appended to the declared member lists:

| Request | New members |
|---|---|
| `CreateRequest` | `metadata`, `principal` |
| `InputRequest` | `metadata`, `principal` |
| `InterruptRequest` | `principal` |
| `RestoreRequest` | `principal` |
| `GateResponseRequest` | `principal` |

```go
Metadata  MessageMetadata `json:"metadata,omitempty"`  // create, input only
Principal *Principal      `json:"principal,omitempty"` // all five
```

```json
{"version":1,"command_id":"cmd-7","session_id":"s-1",
 "blocks":[{"type":"text","text":"Add milk to the list"}],
 "metadata":{"space":"family","client":"oxy-ios"},
 "principal":{"tenant":"acme","subject":"user_01H…","kind":"actor"}}
```

- Each request's `Validate()` calls `Metadata.Validate()` and `Principal.Validate()`.
  `metadata` values must be JSON strings; `null` for either member is `invalid_field`
  (matching `commands.go:585-590`).
- Schemas: the five request schemas gain the optional properties (still
  `additionalProperties:false`); new `principal.schema.json`. Existing fixtures stay
  byte-identical; new fixtures `*_request_principal.json` for all five kinds and
  `create_request_metadata.json` / `input_request_metadata.json`.
- Limits are enforced identically at every ingress because every decoder uses Core:
  Factory HTTP (`controls.go:262,316`), Factory ClientLink (`clientlink/command.go:127`),
  Host decode (§1.4).
- Capability token (rule `hostlink.<area>.<feature>`):

  ```go
  const HostLinkCapabilityAttributionPrincipal = "hostlink.attribution.principal"
  ```

  One token covers principal on all five kinds **and** metadata on create/input:
  they ship in one Host release and one harness release, so a split token would
  describe no real fleet state. The name follows the `hostlink.<area>.<feature>`
  rule; `hostlink.command.<kind>` is reserved for runtime command kinds and
  "principal" is not one (`hostlink_framing.go:72-79`). Earlier draft names
  `hostlink.input.metadata` and `hostlink.command.principal` are superseded.

### 1.2 sessionstore (v0.14.0, additive)

```go
type DispositionCommandDescriptor struct {
    // ... existing ...
    Principal *sessionwire.Principal      `json:"principal,omitempty"` // Factory's recorded assertion
    Metadata  sessionwire.MessageMetadata `json:"metadata,omitempty"`  // create/input only
}
type AdmitDispositionCommandRequest struct { /* ... */; Principal *sessionwire.Principal; Metadata sessionwire.MessageMetadata }
```

- Validation is shape only (`InboxErrorInvalid`, field `principal`/`metadata`).
  **The store proves nothing about who the principal is**: it is a recorded,
  caller-asserted value under the workspace rule "a value the store merely
  records may be caller-asserted", like `PlacementTermination.Kind`.
- `Principal` is permitted on all five kinds; `Metadata` only on `create`/`input`.
- The retry comparison (`disposition_inbox.go:233`) also compares `Principal` and
  `Metadata` by value (`command_mismatch`, field `principal`/`metadata`), because a
  by-reference payload (`PayloadObject`) could otherwise let two descriptors with
  different columns share one object digest.

### 1.3 harness (v0.41.0, additive)

```go
// pkg/present (new package; no rig import, so products can test presenters alone)
type Input struct {
    SessionID uuid.UUID
    LoopID    uuid.UUID
    AgentName identity.AgentName
    Kind      runtimecommand.Kind            // KindCreate or KindInput
    Principal *sessionwire.Principal         // NIL when absent; every presenter MUST handle nil
    Metadata  sessionwire.MessageMetadata    // nil when absent
    Blocks    []content.Block                // a clone; mutations are discarded
}
type Frame struct{ Prefix, Suffix []content.Block }
type Presenter interface{ Present(ctx context.Context, in Input) (Frame, error) }
const (
    MaxFrameBlocks    = 8    // prefix+suffix combined
    MaxFrameTextBytes = 8192 // sum of TextBlock bytes
)
// Frame.Validate(): only *content.TextBlock in v1; nil entries refused; limits above.
type Error struct{ Kind ErrorKind; Cause error } // presenter_failed | frame_invalid

// pkg/rig
func WithMessagePresenter(p present.Presenter) Option // refuses nil and a duplicate (capture.go:44-59 pattern)

// pkg/runtimecommand
type Admitted struct { /* ... */; Principal *sessionwire.Principal; Metadata sessionwire.MessageMetadata }
// Validate(): Principal allowed on all five kinds; Metadata only on KindInput/KindCreate.

// pkg/command
type UserInput struct {
    Header
    Blocks    []content.Block             `json:"blocks,omitempty"`
    // NoFold ...
    Principal *sessionwire.Principal      `json:"principal,omitzero"` // intent record
    Metadata  sessionwire.MessageMetadata `json:"metadata,omitempty"`
    Presented *Presented                  `json:"presented,omitzero"` // the ONE rendering
}
type Presented struct{ Prefix, Suffix []content.Block }
// Interrupt / restore / gate-response runtime commands carry Principal on their
// command records too (omitzero).

// pkg/event: TurnStarted, TurnFoldedInto, InputCancelled gain
//     Input *MessageInput `json:"input,omitzero"`
type MessageInput struct {
    Principal *sessionwire.Principal      `json:"principal,omitzero"`
    Metadata  sessionwire.MessageMetadata `json:"metadata,omitempty"`
    Prefix    int                         `json:"prefix,omitzero"` // presenter blocks at the head
    Suffix    int                         `json:"suffix,omitzero"` // presenter blocks at the tail
}
// The interrupt event and GateResolved gain Principal *sessionwire.Principal (omitzero),
// so "who stopped it" and "who answered the gate" are in the journal.

// pkg/session: additive in-process entry point (Submit keeps its signature)
type Input struct { Blocks []content.Block; Principal *sessionwire.Principal; Metadata sessionwire.MessageMetadata }
func (s *Session) SubmitInput(ctx context.Context, in Input) (uuid.UUID, error)
```

`TurnStarted.Message.Blocks` = `Prefix ++ user blocks ++ Suffix`, the assembled
message: restore appends `TurnStarted.Message` verbatim (`restore.go:869-876`) and
compaction reads `state.msgs`, so neither needs a reassembly step that could drift.
`MessageInput.Prefix/Suffix` let a reader recover the user's own blocks exactly
(`Blocks[Prefix : len-Suffix]`), which is what "never modified" is tested against.

### 1.4 host (v0.11.0, additive)

```go
// department/definition.go
type RuntimeCommand struct { /* ... */; Principal *sessionwire.Principal; Metadata sessionwire.MessageMetadata }
// internal/realtime/hostlink/bindings.go
const CapabilityAttributionPrincipal = sessionwire.HostLinkCapabilityAttributionPrincipal
var advertisedCapabilities = []string{CapabilityGateResponse, CapabilityAttributionPrincipal}
```

- Host decodes every payload with Core's strict decoder (create re-presented as an
  InputRequest by `createbody.FirstMessage`, which now copies `Metadata`/`Principal`)
  **before** handing the unchanged bytes to the product `BlockDecoder`; the decoder
  signature does not change.
- New `internal/inputbody.Check` mirrors `createbody.Check`; a new pre-attempt
  `checkInput` in `disposition_applier.go` applies the same taxonomy
  (`unknown_field`/`unsupported_version` → **block**, malformed → **reject**), and the
  same pre-attempt check covers interrupt/restore/gate_response payloads. This closes
  for input the hole host v0.5.0 closed for create.
- The token is advertised **unconditionally**: there is no product seam to wire; the
  Host binary's harness pin (≥ v0.41.0) is the whole capability.

### 1.5 factory (v0.12.0, additive)

```go
// identity
func (p Principal) Wire() sessionwire.Principal        // KindActor→"actor", KindService→"service"
func PrincipalFromWire(sessionwire.Principal) (Principal, error)
var ErrClientPrincipal     = errors.New("admission: principal is Factory-stamped; a client may not supply it")
var ErrMetadataUnsupported = errors.New("admission: the session's Host does not apply command principal or metadata")

// composition
func WithPrincipalStamping() Option // explicit opt-in; never a default; Compose refuses it without a Verifier
```

Admission for **every** kind, in order, before any authorizer or store call:

1. `req.Principal != nil` → `400 invalid_request` (field `principal`) wrapping
   `ErrClientPrincipal`, **whether or not stamping is on**.
2. Stamping on → `req.Principal = &wire` from the verified principal (tenant is
   `principal.Tenant()` by construction).
3. The canonical payload now carries it, so the digest binds sender to command id:
   a retry of the same `command_id` by a different verified subject is
   `command_mismatch` (spoof-by-retry refused, not re-attributed).

Metadata limits are enforced by Core's decoder at the edge. Capability plumbing
copies the `gate_response` shape: `hostlink.PrincipalCapable(reply)`
(`capability.go:39` pattern), `Pool.AcceptsCommandPrincipal` (`capability.go:149`),
admission `ownerAppliesPrincipal` (`service.go:462`), placement `appliesPrincipal` +
`classifyCapability` reuse (`attach.go:527-562`).

## 2. Durable encoding, versions, one-way rules

| Record | Change | Version | Old reader on a new record |
|---|---|---|---|
| sessionstore disposition inbox row | `principal`, `metadata`, `omitempty` | **Write v2 when both absent (bytes identical); v3 only when either is present.** Reader accepts {2, 3}. | v0.13.1 refuses a v3 row with `InboxErrorVersion` ("upgrade"), not `InboxErrorMalformed` ("corrupt"). Canonical re-encode holds (Go sorts map keys). |
| harness intent/command records | `principal`, `metadata`, `presented` | additive members, no record version | harness ≤ v0.40.2 fails `DisallowUnknownFields` (`record_json.go:57,132`) → restore refuses |
| harness `TurnStarted`/`TurnFoldedInto`/`InputCancelled` (`input`), interrupt event and `GateResolved` (`principal`) | additive | `v` stays 1 (precedent `AgentName`) | harness ≤ v0.40.2 fails closed. Bumping `v` was rejected: it would make every new event unreadable. |
| sessionstore `Envelope` | none | 1 | — |

- **Content identity**: unchanged mechanism (SHA-256 of canonical payload), which now
  covers both members; explicit column compare for by-reference payloads (§1.2).
  The intent record's delivery fingerprint (`record.go:130-148`) covers
  `principal`/`metadata`/`presented` automatically.
- **One-way upgrade**: once a journal holds a stamped or presented record, **never
  roll a Host (or Carbon/TUI process) back below harness v0.41.0**. Once a store holds
  a v3 inbox row, every Factory and Host must be on sessionstore ≥ v0.14.0; a rollback
  wedges every session with a stamped command. Pre-feature journals and rows decode
  with nil members and behave identically.

## 3. Compatibility and rollout

- **Old Host protection**:
  1. Core token `hostlink.attribution.principal`.
  2. Factory refuses to admit a principal- or metadata-bearing command to a session
     whose owner's reply does not `Supports` it: `409 runtime_unavailable` wrapping
     `ErrMetadataUnsupported`, before any durable write; an owner it cannot ask is
     `503` retryable (`ErrGateResponderUnavailable` pattern, `service.go:76`).
  3. Creates and pending inputs on unplaced sessions are placed only on a capable Host
     (`OutcomeNoCapacity` otherwise), copying `appliesGateResponses`
     (`attach.go:553-562`).
  4. New Host's pre-attempt checks block, not reject, an unreadable payload.
  5. Never infer the capability from `CatalogRecord.LeaseEpoch` or Host version
     strings (Fable ruling 2026-09-19).
- **Mixed pools**: with stamping off and no client metadata, an incapable Host behaves
  as today. With stamping on, every command carries a principal, so an incapable Host
  can accept none. **Operator rule: upgrade every Host, then Factory, then enable
  `WithPrincipalStamping`.** Factory logs WARN at Start for each registered Host lacking
  the token while stamping is on. Sessions resident on an incapable Host get
  `runtime_unavailable` until re-placed (drain old Hosts).
- **Old readers**: harness ≤ v0.40.2 fails closed; `publicbody.Project` is a byte
  carrier; wui v0.3.0 renders the assembled message (prefix shown as plain user text)
  and ignores `input`. A client that sends `metadata` to Factory v0.11.1 gets
  `unknown_field`; clients feature-detect via `/v1/capabilities`
  (`message_metadata: true`, `command_principal: true`).

## 4. Visibility

Rule: **a viewer sees what the model saw plus who sent it; custom fields are
audit-only unless the presenter renders them.**

- **Public journal / live tail** (`privacy.go:41-81`): new cases keep
  `input.principal`, `input.prefix`, `input.suffix`, and the `principal` on the
  interrupt event and `GateResolved`; they **strip `input.metadata`**
  (mirrors `GateResolved.audit` stripping).
- **Audit readers**: harness `ReadRuntimeJournal` (full `input`); sessionstore
  `GetDispositionCommand` (descriptor with both); Factory adds
  `GET /v1/sessions/{sid}/commands/{cid}` returning `CommandStatus` plus, when
  authorized, `principal` and `metadata`. Authorization via an assertion-discovered
  optional seam (precedent `session.LeaseEpochReporter`), so no `Authorizer`
  implementer breaks:

  ```go
  type AuditAuthorizer interface {
      AuthorizeAuditRead(ctx context.Context, principal identity.Principal, session sessionwire.SessionID) error
  }
  ```

  Absent → the route omits both members (existence not disclosed). `TenantAuthorizer`
  implements it as tenant equality. `AuthorizeSessionRead` alone never unlocks custom
  metadata.
- **The model** sees only what the presenter rendered.

## 5. Presenter semantics

- **When**: once, at `buildAndAuditUserInput` (`runtime_command.go:785-793`), before
  `appendAdmittedIntent`, so a failure leaves nothing durable naming the input. Stored
  as `UserInput.Presented`; the loop composes `Prefix ++ Blocks ++ Suffix`
  (`loop.go:1765`). Re-offer after restore reuses `Presented` verbatim; there is never a
  second presentation.
- **Only user-agency messages**: `SubagentResult`, `MessageAgent` deliveries and every
  `AgencyMachine` input bypass the presenter and carry nil principal/metadata.
  Interrupt, restore and gate responses are stamped but never presented (no message).
- **Determinism**: a function of `Input` only (bounded lookups such as subject → display
  name allowed). Not runtime-enforced, but a nondeterministic presenter breaks the
  delivery-fingerprint dedup of a redelivered command (`record.go:130`); the `tests`
  lane runs the same command twice.
- **Read-only blocks**: `Input.Blocks` is a `CloneBlocks` copy; a test proves
  `Message.Blocks[Prefix:len-Suffix]` equals the admitted blocks byte for byte.
- **Failure = refused, settled from evidence**: in the Host path the presenter runs
  inside the disposition attempt. Returning an error would leave the command `applying`
  with no disposition frame (the wedge v0.36.0 fixed), so `ApplyRuntimeCommand` returns
  `Disposition{Kind: refused, Reason: "presenter"}`, the intent record is never appended,
  and the store settles it terminal. In-process `SubmitInput` returns `*present.Error`.
  An invalid frame is treated identically (`frame_invalid`).
- **Gates**: `gate_response` is never presented; a parked gate's restore re-offers
  nothing to the presenter.
- **Compaction / prompt cache**: the frame is inside the committed `UserMessage`; the
  stable prefix is unaffected; no re-rendering on restore, so cache keys survive failover.
- **Token counting**: the frame is counted like any user text; `MaxFrameTextBytes`
  bounds the overhead.
- **Pre-feature journals**: `input` absent → nil; `Presented` absent → byte-identical
  `state.msgs` (golden test). No presenter → `Presented` never written; metadata is
  stored audit-only.
- **`pkg/serve`** (deprecated): unchanged; produces plain `Submit` with nil members.

## 6. Release order, tests, consumer migration

Leaf to root; each verified with `GOTOOLCHAIN=go1.26.8 GOWORK=off go test ./...`.

1. **core v0.12.0** (minor). Tests: strict-decoder table (unknown member inside
   `principal`, `null` members, non-string value, 17 fields, 65-byte key, `looprig_x`,
   4097-byte total, invalid UTF-8, `metadata` refused on interrupt/restore/gate_response);
   byte-stable round trips; `Supports(HostLinkCapabilityAttributionPrincipal)`; schema
   validator agrees with the Go decoder on every new fixture; exported API list.
2. **sessionstore v0.14.0** (minor). Tests: v2 bytes unchanged when absent (golden); v3
   golden; wire-DTO mirror tests extended; reader accepts v2+v3, refuses v4; retry
   mismatch on principal and metadata (inline and by-reference); metadata refused on
   non-message kinds; principal accepted on all five; `ListSessionCommands` fails closed
   at a v4 row.
3. **harness v0.41.0** (minor; pins core v0.12.0, sessionstore v0.14.0). Tests: presenter
   invoked exactly once per input across restore and re-offer; user blocks recovered
   exactly; nil-principal contract; frame limits; presenter error → `refused`, no intent
   record, no `TurnStarted`; machine hand-back never presented; interrupt and gate
   response carry principal into the journal; redaction strips metadata, keeps
   principal; pre-feature golden journal restores identically; old-decoder replay fails
   closed (documents the one-way rule); `WithMessagePresenter` nil/duplicate refused.
4. **host v0.11.0** and **factory v0.12.0** in parallel (host pins harness v0.41.0;
   factory pins core v0.12.0, sessionstore v0.14.0). Host: token advertised; pre-attempt
   block-vs-reject for all five kinds; `createbody.FirstMessage` carries members;
   `Admitted.Principal/Metadata` set; product `BlockDecoder` gets unchanged bytes.
   Factory: client principal refused with and without stamping, for every kind;
   stamping mandatory on every kind; service principal stamps `service`; `Compose`
   refuses stamping without a Verifier; different-subject retry → `409`; incapable
   owner → `runtime_unavailable`, nothing written; capable-only placement; commands
   route with/without `AuditAuthorizer`; apidiff additions only.
5. **wui v0.4.0** (needs only core v0.12.0): `make contract`, mirror `schema.ts`,
   `commands.ts` optional `metadata`, `fold.ts` reads `input.principal/prefix/suffix`
   and renders the frame dimmed with a "from" chip.
6. **tests v0.14.0**: cross-module lane with real Factory + Host + harness and a
   presenter; stamped create, input, interrupt, gate response; assembled `TurnStarted`
   matches; public journal lacks metadata, has principal; audit route gated; old-Host
   probe (host v0.10.3 in the pool with stamping on: create waits `NoCapacity`, input to a
   resident session → `runtime_unavailable`, the old Host never begins an attempt);
   restart-and-restore preserves rendering byte for byte; presenter count = 1 across
   failover; double-redelivery dedup.
7. **carbon**, then **Oxy**.

Under MVS, harness v0.41.0 lifts every harness dependent to core v0.12.0 / sessionstore
v0.14.0 on its next bump; all changes are additive. `AGENTS.md` tiers and edges are
unchanged.

**Oxy migration** (household app; stamps principal and custom fields; renders
`[from: Alex (parent) · space: Family]`):

1. Upgrade every Host to host v0.11.0, then Factory to v0.12.0 with its Verifier; check
   each registered Host advertises the token; then add `factory.WithPrincipalStamping()`.
2. Rig: `rig.WithMessagePresenter(oxy.Presenter{Directory: members})`; `Present` returns
   an empty frame for `in.Principal == nil`, else resolves `in.Principal.Subject` to a
   display name and role, reads `in.Metadata["space"]`, and returns one prefix text block.
3. Client: send `metadata` on create/input; never send `principal`; feature-detect on
   `/v1/capabilities`.
4. Viewer: `input.principal.subject` drives the "sent by" chip; hide or dim the frame.
5. Audit screen: `GET /v1/sessions/{sid}/commands/{cid}` behind Oxy's `AuditAuthorizer`.
6. Once one stamped message lands, no rollback below harness v0.41.0 / sessionstore
   v0.14.0 on any process sharing those stores.
7. Until this ships, Oxy uses an interim Oxy-side front handler (command_id → member
   audit table plus a server-written header block), retired when this lands.

## 7. Settled questions

| # | Question | Decision |
|---|---|---|
| 1 | Metadata value type | Strings only |
| 2 | `looprig` key prefix | Refused (held, no semantics) |
| 3 | Custom metadata public by default | No: audit-only unless rendered; an allowlist can be a later harness minor |
| 4 | Client feature detection | `/v1/capabilities` flags, no wire-version bump |
| 5 | Presenting machine-originated messages | No; a later separate hook if ever wanted |
| 6 | Principal on interrupt/restore/gate_response | **Yes, now** (owner amendment) |
| 7 | Presenter failure arm | `refused` |
| 8 | Host token advertisement | Unconditional |
| 9 | `SubjectID` grammar | Core's generic ID rules |
| 10 | Frame cap 8 KiB, metadata cap 4 KiB | As stated (well inside the 64 KiB inline payload) |

## 8. Implementation progress

Update this table as releases land, so work can resume from here.

| Step | Module | Version | Status |
|---|---|---|---|
| 1 | core | v0.12.0 | released (`8a6197f`, tag `f95f298`) |
| 2 | sessionstore | v0.14.0 | released (`96e4621`, tag `46b7e9b`) |
| 3 | harness | v0.41.0 | in progress on local `main` |
| 4a | host | v0.11.0 | not started |
| 4b | factory | v0.12.0 | not started |
| 5 | wui | v0.4.0 | not started |
| 6 | tests | v0.14.0 | not started |
| 7 | carbon, Oxy | — | not started |

## 9. Corrections found while planning (2026-09-25)

These override the sections above where they conflict. Source: impl-05 planning against harness v0.40.2.

1. **Old harness does not fail closed.** `record_json.go:57,132` decode lease-fence and
   gate-prepared records, not these. `UnmarshalCommand`, the plain decoders for `TurnStarted`,
   `TurnFoldedInto`, `InputCancelled`, `TurnInterrupted`, and `decodeGateResolved` use permissive
   `json.Unmarshal`, so harness ≤ v0.40.2 silently drops the new members and restores. Committed
   turns survive (the frame is inside the message); an applied-but-not-started input is
   re-offered **without** its frame. The one-way rule therefore reads **"lossy on rollback"**,
   not "fails closed". `v` stays 1 (the published event envelope schema pins it, mirrored in wui);
   the new `MessageInput` decoder is strict so the next addition fails closed. impl-05 Task 15
   measures real v0.40.2 behaviour. **Needs owner acknowledgement before release.**
2. **Refused dispositions carry no reason field.** A presenter failure writes the application
   prefix then `refused`; the reason is on the returned `*present.Error` and a WARN log line.
3. **`pkg/session.Session` is an interface**, so `SubmitInput` is a separate
   `session.InputSubmitter` capability discovered by type assertion (precedent
   `LeaseEpochReporter`), not a new method on `Session`.
4. **Restore's principal is recorded only in sessionstore's descriptor**; harness writes no
   body for a restore.
5. **Presentation anchor**: before both branches of `prepareAdmittedInput`, not
   `runtime_command.go:785-793`.
6. **Presented once**: a redelivery of an already-applied command skips the presenter via
   `ScanCommandEffect` (only when a presenter is set). Known limit: a crash between the intent
   write and the prefix write presents again, relying on presenter determinism.
7. **Capability token** is `hostlink.attribution.principal` (renamed; see §1.1).
8. **Hustle zero timeout**: see the companion design; only explicit `WithTimeout(0)` means no deadline.
9. **Incapable-owner refusal status is 422, not 409.** Factory's refusal table
   (`internal/command/refusal.go:108-125`) already maps `runtime_unavailable` to 422; it is
   kept (changing it would move an existing client-visible status). A different-subject
   retry is `command_rejected` (409) as designed.
10. **Stamping without a Verifier** is already impossible: `factory.New` requires
    `WithCredentialVerifier`; impl-07 pins that with a test instead of adding a check.
11. **Resolved (cross-plan review, 2026-09-25): the sweeper reads the create's own
    disposition inbox row, and plan 02 fills its columns.** Factory's pending sweeper makes
    one store call, `ListDueDispositionCommands` (factory v0.11.1
    `internal/placement/pending.go:267`), and classifies from
    `entry.Record.Descriptor` (`Kind`, state, deadline; `pending.go:280-312`); it never reads
    the public-create reservation, the catalog or payload bytes. `AdmitPublicCreate`
    (sessionstore v0.13.1 `public_create.go:329-365`) writes exactly one row, the create's
    `DispositionInboxRecord`, through the same `admitDispositionCommand` every other kind
    uses, so a descriptor column added in §1.2 reaches the sweeper with no further read.
    The only gap was the write side: neither `AdmitPublicCreateRequest` nor
    `PublicCreateIdentity` carried the members. impl-02 closes it by adding
    `Principal`/`Metadata` to **`AdmitPublicCreateRequest`** (not to `PublicCreateIdentity`,
    so the reservation record version is unchanged) and passing them into both
    `AdmitDispositionCommandRequest` literals; the inbox's by-value retry comparison
    covers them, and the reservation already binds them through the stamped payload's
    digest. impl-02 Task 7 pins that the due view returns them
    (`TestPublicCreateAttributionIsVisibleToTheDueView`). impl-07 sets them in
    `admitPublicCreate` and classifies `descriptor.Principal != nil || len(descriptor.Metadata) > 0`
    in `collect` (pending and claimed rows) into `Request.PrincipalCommands`, so no payload
    is parsed.

### Owner decisions, 2026-09-25 (after the cross-plan review)

- §9.1 accepted: rollback below harness v0.41.0 is **lossy and forbidden** once stamped or
  presented records exist; release notes say so; `v` stays 1; the new decoder is strict.
- §9.9 accepted: incapable-owner refusal stays **422** `runtime_unavailable`.
- `/v1/capabilities` `command_principal` means **this Factory stamps** (true only with
  `WithPrincipalStamping`); `message_metadata` is always true.
- impl-09 may add `github.com/nats-io/nats-server/v2` as a **direct test-only** import in `tests`.
