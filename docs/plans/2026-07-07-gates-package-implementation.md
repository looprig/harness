# Gates Package Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build session-authoritative gates with opaque `GateID`, activation-time public `GateOpened`, durable-first `GateResolved`, and a thin HTTP/API response path.

**Architecture:** Add a small `pkg/gate` domain package, persist private prepared gate state outside public event fanout, let `pkg/session` own the live gate directory, and keep `pkg/loop` responsible only for local parked continuations. `pkg/api` must stop maintaining an event-derived shadow gate registry and instead call narrow `ListGates` / `RespondGate` methods exposed by the driven agent.

**Tech Stack:** Go stdlib, `pkg/gate`, `pkg/event`, `pkg/journal`, `pkg/sessionstore`, `pkg/session`, `pkg/loop`, `pkg/api`, `pkg/tool`, `pkg/command`; tests are table-driven and run with `go test -race`.

---

## Scope

Implement P1 through the restore/list foundation of the gates spec:

- `pkg/gate` public envelope, prompts, response requests, response policies, private payload/audit codecs.
- Private durable prepare record that is not exposed through SSE/history.
- Public durable `GateOpened` appended during activation.
- Single durable `GateResolved` appended before command delivery.
- Session-owned gate directory with `preparing`, `open`, `claiming`, `closed`.
- Loop prepare/install/activate handshake for permission and `AskUser` gates.
- API route by opaque `GateID`, returning `202 Accepted`.
- Restore/list reconstruction from private prepare + public open + resolve.
- Initial response-policy plumbing for wait/respond; leave model-decision and suspend-session execution behind explicit errors until their session lifecycle contracts exist.

Do not implement P5 new producers such as switch-model, foreign gates, or extension gates in this pass. Add types and validation seams so those can be added without changing API shape.

## Current Code Landmarks

- Current loop-local gates live in `pkg/loop/gate.go`, keyed by `ToolExecutionID`, with `gateRegistration` and `pendingGates`.
- Current permission gating happens in `pkg/loop/runner.go:354` (`askPermission`) and `pkg/loop/gate.go:177` (`RequestUserInput`).
- Current session gate replies are `Session.Approve`, `Session.Deny`, `Session.ProvideUserInput` in `pkg/session/session.go:1268`.
- Current HTTP gate routing is in `pkg/api/handlers_gate.go`, using a `pkg/api/supervisor.go` shadow registry derived from `PermissionRequested` / `UserInputRequested`.
- Current durable event append path is `pkg/hub.Hub.PublishEvent`; it faults and returns nil on append failure, so gate prepare/open/resolve need a strict append path.
- Current journal records are sealed in `pkg/journal/record.go` and encoded in `pkg/sessionstore/journal.go`.

## Task 1: Add `pkg/gate` Core Types

**Files:**
- Create: `pkg/gate/gate.go`
- Create: `pkg/gate/prompt.go`
- Create: `pkg/gate/response.go`
- Create: `pkg/gate/policy.go`
- Create: `pkg/gate/gate_test.go`

**Step 1: Write the failing tests**

Create table-driven tests covering:

```go
func TestGateZeroValuesAreFailSecure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		g    Gate
		want bool
	}{
		{name: "empty gate invalid", g: Gate{}, want: false},
		{name: "criticality empty defaults later", g: Gate{ID: mustID(t), Kind: KindPermission}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.g.ID != ID{} && tt.g.Kind != ""
			if got != tt.want {
				t.Fatalf("validity = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResponsePolicyDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   ResponsePolicy
		want PolicyActionKind
	}{
		{name: "zero waits", in: ResponsePolicy{}, want: PolicyWait},
		{name: "respond preserved", in: ResponsePolicy{OnTimeout: PolicyAction{Kind: PolicyRespond}}, want: PolicyRespond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.in.EffectiveAction().Kind; got != tt.want {
				t.Fatalf("EffectiveAction().Kind = %q, want %q", got, tt.want)
			}
		})
	}
}
```

**Step 2: Run tests and verify they fail**

Run: `go test -race ./pkg/gate`

Expected: FAIL because `pkg/gate` does not exist.

**Step 3: Add the minimal domain types**

Implement:

```go
package gate

import (
	"encoding/json"
	"time"

	"github.com/looprig/core/uuid"
)

type ID = uuid.UUID
type Kind string
type ResolverKind string
type Blocks string
type Effect string
type CloseReason string
type Criticality string

const (
	KindPermission Kind = "harness.permission"
	KindAskUser    Kind = "harness.ask_user"
)

const (
	ResolverLoop    ResolverKind = "loop"
	ResolverSession ResolverKind = "session"
)

const (
	BlocksToolCall Blocks = "tool_call"
	BlocksSession  Blocks = "session"
)

const (
	EffectResume   Effect = "resume"
	EffectInitiate Effect = "initiate"
	EffectControl  Effect = "control"
)

const (
	CloseAnswered           CloseReason = "answered"
	ClosePolicyResponse     CloseReason = "policy_response"
	CloseAbandoned          CloseReason = "abandoned"
	CloseOwnerClosed        CloseReason = "owner_closed"
	CloseRestoreUnavailable CloseReason = "restore_unavailable"
)

const (
	GateCritical    Criticality = "critical"
	GateNonCritical Criticality = "non_critical"
)

type Subject struct {
	ToolExecutionID uuid.UUID `json:"tool_execution_id,omitzero"`
	ToolUseID       string    `json:"tool_use_id,omitempty"`
	TurnID          uuid.UUID `json:"turn_id,omitzero"`
	StepID          uuid.UUID `json:"step_id,omitzero"`
	InputID         uuid.UUID `json:"input_id,omitzero"`
}

type Route struct {
	GateID          ID        `json:"gate_id"`
	LoopID          uuid.UUID `json:"loop_id,omitzero"`
	ToolExecutionID uuid.UUID `json:"tool_execution_id,omitzero"`
}

type Gate struct {
	ID             ID             `json:"gate_id"`
	Kind           Kind           `json:"kind"`
	Resolver       ResolverKind   `json:"resolver"`
	Blocks         Blocks         `json:"blocks"`
	Effect         Effect         `json:"effect"`
	Criticality    Criticality    `json:"criticality,omitempty"`
	Subject        Subject        `json:"subject,omitzero"`
	Prompt         Prompt         `json:"prompt"`
	ResponsePolicy ResponsePolicy `json:"response_policy,omitzero"`
	Restorable     bool           `json:"restorable"`
}

type ResponseRequest struct {
	Action string                     `json:"action"`
	Values map[string]json.RawMessage `json:"values,omitempty"`
}

type GateResponse struct {
	GateID ID                         `json:"gate_id"`
	Action string                     `json:"action"`
	Values map[string]json.RawMessage `json:"values,omitempty"`
	Source ResponseSource            `json:"source"`
}

type ResponseSourceKind string

const (
	ResponseFromUser   ResponseSourceKind = "user"
	ResponseFromPolicy ResponseSourceKind = "policy"
	ResponseFromModel  ResponseSourceKind = "model"
)

type ResponseSource struct {
	Kind   ResponseSourceKind `json:"kind"`
	Reason string             `json:"reason,omitempty"`
}

type PolicyActionKind string

const (
	PolicyWait           PolicyActionKind = "wait"
	PolicyRespond        PolicyActionKind = "respond"
	PolicySuspendSession PolicyActionKind = "suspend_session"
	PolicyModelDecide    PolicyActionKind = "model_decide"
)

type ResponsePolicy struct {
	Timeout   time.Duration `json:"timeout,omitempty"`
	OnTimeout PolicyAction  `json:"on_timeout,omitzero"`
}

type PolicyAction struct {
	Kind          PolicyActionKind   `json:"kind,omitempty"`
	Response      *ResponseTemplate  `json:"response,omitempty"`
	ModelDecision *ModelDecisionPolicy `json:"model_decision,omitempty"`
}

type ResponseTemplate struct {
	Action string                     `json:"action"`
	Values map[string]json.RawMessage `json:"values,omitempty"`
}

type ModelDecisionPolicy struct {
	Instruction       string                     `json:"instruction,omitempty"`
	RecommendedAction string                     `json:"recommended_action,omitempty"`
	RecommendedValues map[string]json.RawMessage `json:"recommended_values,omitempty"`
}

func (p ResponsePolicy) EffectiveAction() PolicyAction {
	if p.OnTimeout.Kind == "" {
		return PolicyAction{Kind: PolicyWait}
	}
	return p.OnTimeout
}
```

**Step 4: Run tests and verify they pass**

Run: `go test -race ./pkg/gate`

Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/gate
git commit -m "feat: add gate domain types"
```

## Task 2: Add Prompt Controls and Payload Codecs

**Files:**
- Modify: `pkg/gate/prompt.go`
- Create: `pkg/gate/payload.go`
- Create: `pkg/gate/payload_test.go`
- Create: `pkg/gate/response_audit.go`
- Test: `pkg/tool/permission_request_json_test.go`

**Step 1: Write prompt and payload codec tests**

Cover:

- Permission payload marshals through `tool.MarshalPermissionRequest`.
- Payload unmarshal rejects unknown `kind`.
- Accepted grant tokens are values, not labels/descriptions.
- Extension/custom prompt schemas are display-only and do not produce behavior.

Use a table:

```go
func TestPayloadRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   Payload
	}{
		{name: "permission bash request", in: PermissionPayload{Request: tool.BashRequest{Command: "echo ok"}}},
		{name: "ask user", in: AskUserPayload{Question: "continue?", Choices: []string{"yes", "no"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := MarshalPayload(tt.in)
			if err != nil {
				t.Fatalf("MarshalPayload() error = %v", err)
			}
			got, err := UnmarshalPayload(data)
			if err != nil {
				t.Fatalf("UnmarshalPayload() error = %v", err)
			}
			if reflect.TypeOf(got) != reflect.TypeOf(tt.in) {
				t.Fatalf("round trip type = %T, want %T", got, tt.in)
			}
		})
	}
}
```

**Step 2: Run tests and verify they fail**

Run: `go test -race ./pkg/gate`

Expected: FAIL on undefined payload codec symbols.

**Step 3: Implement prompt controls**

Add:

```go
type Prompt struct {
	Title    string    `json:"title"`
	Body     string    `json:"body,omitempty"`
	Controls []Control `json:"controls,omitempty"`
	Schema   *PromptSchema `json:"schema,omitempty"`
}

type Control struct {
	Action string  `json:"action"`
	Label  string  `json:"label"`
	Style  string  `json:"style,omitempty"`
	Fields []Field `json:"fields,omitempty"`
}

type FieldKind string

const (
	FieldText        FieldKind = "text"
	FieldSelect      FieldKind = "select"
	FieldMultiSelect FieldKind = "multi_select"
)

type Field struct {
	Name     string          `json:"name"`
	Kind     FieldKind       `json:"kind"`
	Required bool            `json:"required,omitempty"`
	Options  []Option        `json:"options,omitempty"`
	Default  json.RawMessage `json:"default,omitempty"`
}

type Option struct {
	Value       json.RawMessage `json:"value"`
	Label       string          `json:"label"`
	Description string          `json:"description,omitempty"`
}

type PromptSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema,omitempty"`
}
```

**Step 4: Implement sealed payload/audit codecs**

Keep `any` only at JSON boundaries. Define a sealed interface with an unexported method:

```go
type Payload interface{ isPayload() }

type OpenPayload struct {
	GateID  ID
	Payload Payload
}

type PermissionPayload struct {
	Request tool.PermissionRequest
}

type AskUserPayload struct {
	Question string
	Choices  []string
}

type ResumeInputPayload struct {
	InputID uuid.UUID
	Preview string
}
```

Add response audit as a sealed union too:

```go
type ResponseAudit interface{ isResponseAudit() }

type PermissionAudit struct {
	AcceptedGrantDescriptions []string `json:"accepted_grant_descriptions,omitempty"`
}

type AskUserAudit struct {
	AnswerPreview string `json:"answer_preview,omitempty"`
}
```

Use a wire wrapper:

```go
type payloadWire struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}
```

Permission payload marshal must call `tool.MarshalPermissionRequest`, and unmarshal must call `tool.UnmarshalPermissionRequest`.

**Step 5: Run tests and verify they pass**

Run: `go test -race ./pkg/gate`

Expected: PASS.

**Step 6: Commit**

```bash
git add pkg/gate
git commit -m "feat: add gate prompt and payload codecs"
```

## Task 3: Add Gate Events and Private Journal Record

**Files:**
- Modify: `pkg/event/tool.go`
- Modify: `pkg/event/marshal.go`
- Modify: `pkg/event/validate.go`
- Modify: `pkg/event/marshal_test.go`
- Modify: `pkg/event/validate_test.go`
- Modify: `pkg/journal/record.go`
- Modify: `pkg/journal/record_json.go`
- Modify: `pkg/journal/record_test.go`
- Modify: `pkg/sessionstore/journal.go`
- Modify: `pkg/sessionstore/replay.go`
- Modify: `pkg/sessionstore/replay_test.go`

**Step 1: Write failing event tests**

Add table-driven tests proving:

- `GateOpened` is a loop-scoped enduring event and round-trips through `event.MarshalEvent`.
- `GateResolved` is a loop-scoped enduring event and round-trips.
- `GatePrepared` validates like a tool/step event but is not exposed by `OpenEventReplayer`.
- `GateOpened` carries no private payload.

**Step 2: Write failing journal tests**

Add tests proving:

- `journal.NewGatePreparedRecord(event.GatePrepared{...}, gate.OpenPayload{...})` is a `JournalRecord`.
- `sessionstore.OpenInternalRecordReplayer` returns the private prepared record with payload.
- `sessionstore.OpenEventReplayer` skips the private prepared record.

**Step 3: Run tests and verify they fail**

Run:

```bash
go test -race ./pkg/event ./pkg/journal ./pkg/sessionstore
```

Expected: FAIL on undefined gate event/private record symbols.

**Step 4: Implement event types**

In `pkg/event/tool.go`, add:

```go
type GatePrepared struct {
	enduring
	loopScoped
	Header
	Gate gate.Gate `json:"gate"`
}

type GateOpened struct {
	enduring
	loopScoped
	Header
	Gate gate.Gate `json:"gate"`
}

type GateResolved struct {
	enduring
	loopScoped
	Header
	GateID gate.ID            `json:"gate_id"`
	Reason gate.CloseReason   `json:"reason"`
	Action string              `json:"action,omitempty"`
	Scope  tool.ApprovalScope  `json:"scope,omitzero"`
	Source gate.ResponseSource `json:"source,omitzero"`
	Audit  gate.ResponseAudit  `json:"audit,omitzero"`
}
```

Update `isEvent`, `classify`, `encodePayload`, `decodePayload`, and validation profiles.

**Step 5: Implement a concrete private prepared journal record**

In `pkg/journal/record.go`, add:

```go
type GatePreparedRecord struct {
	prepared event.GatePrepared
	payload  gate.OpenPayload
}

func NewGatePreparedRecord(prepared event.GatePrepared, payload gate.OpenPayload) GatePreparedRecord {
	return GatePreparedRecord{prepared: prepared, payload: payload}
}

func (r GatePreparedRecord) Prepared() event.GatePrepared { return r.prepared }
func (r GatePreparedRecord) Payload() gate.OpenPayload { return r.payload }
func (GatePreparedRecord) isJournalRecord() {}
func (r GatePreparedRecord) IdempotencyID() string { return r.prepared.EventHeader().EventID.String() }
```

In `pkg/sessionstore/journal.go`, add an envelope kind such as `gate_prepared`, encoded with a
`journal.MarshalGatePreparedRecord` helper that writes both the prepared projection and
`gate.MarshalPayload(payload.Payload)`.

In `pkg/sessionstore/replay.go`, make `OpenInternalRecordReplayer` decode `gate_prepared` as
`journal.GatePreparedRecord`, and make `OpenEventReplayer` skip it.

Guardrail: never append `event.GatePrepared` through `journal.NewEventRecord` or
`hub.PublishEvent`; it is only valid inside `journal.GatePreparedRecord`.

**Step 6: Run tests and verify they pass**

Run:

```bash
go test -race ./pkg/event ./pkg/journal ./pkg/sessionstore
```

Expected: PASS.

**Step 7: Commit**

```bash
git add pkg/event pkg/journal pkg/sessionstore
git commit -m "feat: add gate events and private journal records"
```

## Task 4: Add Session Gate Directory

**Files:**
- Create: `pkg/session/gates.go`
- Create: `pkg/session/gates_test.go`
- Modify: `pkg/session/session.go`
- Modify: `pkg/session/command_journal.go`

**Step 1: Write failing directory tests**

Cover:

- `PrepareGateOpen` appends private prepared record and creates `state=preparing`.
- Prepared gate is not returned by `ListGates`.
- `ActivateGate` appends public `GateOpened`, flips to `open`, and `ListGates` returns it.
- A response to `preparing` returns a typed not-ready/not-found error.
- Cap counts `preparing + open + claiming`.

**Step 2: Run tests and verify they fail**

Run: `go test -race ./pkg/session -run 'TestGatePrepare|TestGateActivate|TestGateList|TestGateCap'`

Expected: FAIL on undefined session gate methods.

**Step 3: Add directory state and strict append seams**

Add to `Session`:

```go
gatesMu sync.Mutex
gates   map[gate.ID]gateEntry
gateAppender gateAppender
gateCaps GateCaps
```

Add types:

```go
type gateState uint8

const (
	gatePreparing gateState = iota
	gateOpen
	gateClaiming
	gateClosed
)

type gateEntry struct {
	gate    gate.Gate
	route   gate.Route
	payload gate.Payload
	state   gateState
}

type gateAppender interface {
	AppendGatePrepared(ctx context.Context, ev event.GatePrepared) error
	AppendGateOpened(ctx context.Context, ev event.GateOpened) error
	AppendGateResolved(ctx context.Context, ev event.GateResolved) error
}
```

Default to a nop appender for headless tests. Real composition should adapt the journal and hub.

**Step 4: Implement `PrepareGateOpen`, `ActivateGate`, `ListGates`**

Rules:

- `PrepareGateOpen` mints `GateID`, stamps `GatePrepared`, appends private record, then inserts `preparing`.
- If append fails, return error and do not mutate the directory.
- `ActivateGate` requires `preparing`, appends public `GateOpened`, stores private route, flips to `open`, then live-fans out.
- `ListGates` returns only `open` entries.

**Step 5: Add typed errors**

Create typed errors for:

- `GateNotFoundError`
- `GateNotReadyError`
- `GateKindMismatchError`
- `GateActionError`
- `GateCapacityError`
- `GateAppendError`

**Step 6: Run tests and verify they pass**

Run: `go test -race ./pkg/session -run 'TestGatePrepare|TestGateActivate|TestGateList|TestGateCap'`

Expected: PASS.

**Step 7: Commit**

```bash
git add pkg/session
git commit -m "feat: add session gate directory"
```

## Task 5: Implement Durable-First `RespondGate`

**Files:**
- Modify: `pkg/session/gates.go`
- Modify: `pkg/session/session.go`
- Modify: `pkg/session/gates_test.go`
- Modify: `pkg/session/command_journal_test.go`

**Step 1: Write failing response tests**

Cover:

- Human response claims `open` gate, appends `GateResolved`, then dispatches command.
- Duplicate response after claim/close is no-op or typed stale rejection and does not dispatch twice.
- Append failure delivers nothing and leaves gate answerable.
- HTTP/client context cancellation after durable commit does not cancel delivery; session context is used for delivery.
- Permission approve validates `accepted_grants` against `PermissionPayload.Request` `GrantDisplay.Token` values.

**Step 2: Run tests and verify they fail**

Run: `go test -race ./pkg/session -run 'TestRespondGate|TestGateResolved|TestAcceptedGrants'`

Expected: FAIL on undefined `RespondGate`.

**Step 3: Implement response validation**

Add:

```go
func (s *Session) RespondGate(ctx context.Context, response gate.GateResponse) error
```

Behavior:

1. Lock `gatesMu`.
2. Find gate by `GateID`.
3. Require `state=open`.
4. Validate action against `gate.Prompt.Controls` and typed payload.
5. Set `state=claiming`.
6. Unlock.
7. Append `GateResolved`.
8. On append error, relock and revert `claiming` to `open`.
9. On append success, route translated command using `s.sessionCtx`, not the HTTP request context.
10. Mark/remove closed entry.

**Step 4: Translate gate responses to current commands**

For v1 loop gates:

- permission approve -> `command.ApproveToolCall{GateRoute: route, Scope: scope, AcceptedGrants: grants}`
- permission deny -> `command.DenyToolCall{GateRoute: route}`
- ask-user answer -> `command.ProvideUserInput{GateRoute: route, Answer: answer}`

Keep `Approve`, `Deny`, and `ProvideUserInput` as compatibility wrappers that build `GateResponse` only after looking up a legacy bridge during migration.

**Step 5: Run tests and verify they pass**

Run: `go test -race ./pkg/session -run 'TestRespondGate|TestGateResolved|TestAcceptedGrants|TestGateRepliesAppendCommandRecord'`

Expected: PASS.

**Step 6: Commit**

```bash
git add pkg/session
git commit -m "feat: resolve gates through session directory"
```

## Task 6: Wire Loop Prepare/Install/Activate

**Files:**
- Modify: `pkg/loop/gate.go`
- Modify: `pkg/loop/runner.go`
- Modify: `pkg/loop/loop.go`
- Modify: `pkg/loop/gate_test.go`
- Modify: `pkg/loop/gate_routing_test.go`
- Modify: `pkg/loop/runner_test.go`
- Modify: `pkg/loop/turn_test.go`

**Step 1: Write failing activation-order tests**

Cover:

- Prepare append failure installs no `pendingGates` entry.
- Gate is not emitted/listable before actor ack and activation.
- Answer immediately after `GateOpened` reaches the installed blocker exactly once.
- Activation failure removes/abandons the local blocker and leaves no public gate.

**Step 2: Run tests and verify they fail**

Run: `go test -race ./pkg/loop -run 'Test.*Gate|Test.*Permission|Test.*RequestUserInput'`

Expected: FAIL because loop still emits `PermissionRequested` / `UserInputRequested` directly after local ack.

**Step 3: Change `gateRegistration`**

Replace `callID`-only registration with a prepared gate request:

```go
type gateRegistration struct {
	gate    gate.Gate
	payload gate.Payload
	reply   chan<- command.Command
	kind    gateKind
	ack     chan<- gateInstallAck
}

type gateInstallAck struct {
	gateID gate.ID
	err    error
}
```

The actor path:

1. Calls session registrar `PrepareGateOpen`.
2. If prepare succeeds, stores `pendingGates[gateID]`.
3. Calls `ActivateGate(gateID, route)`.
4. Sends ack with `gateID` or error.

**Step 4: Keep local routing simple**

`pendingGates` should move from `map[uuid.UUID]gate` keyed by `ToolExecutionID` to `map[gate.ID]gate`. The translated command still carries `ToolExecutionID` inside `GateRoute`; the loop should route by the `GateID` once `command` grows a gate id field, or bridge through a temporary map during the migration.

Prefer adding `GateID gate.ID` to `command.GateRoute` so session dispatch and loop matching use the same opaque id.

**Step 5: Update permission and AskUser prompt builders**

`askPermission` builds:

- `gate.Gate{Kind: gate.KindPermission, Resolver: gate.ResolverLoop, Blocks: gate.BlocksToolCall, Effect: gate.EffectResume, Subject.ToolExecutionID: r.callID, Prompt: permissionPrompt(req), Restorable: false}`
- `gate.PermissionPayload{Request: req}`

`RequestUserInput` builds:

- `gate.Gate{Kind: gate.KindAskUser, ...}`
- `gate.AskUserPayload{Question: question, Choices: choices}`

**Step 6: Run tests and verify they pass**

Run:

```bash
go test -race ./pkg/loop -run 'Test.*Gate|Test.*Permission|Test.*RequestUserInput'
go test -race ./pkg/session -run 'TestRespondGate|TestGate'
```

Expected: PASS.

**Step 7: Commit**

```bash
git add pkg/loop pkg/session pkg/command
git commit -m "feat: activate gates after loop blocker install"
```

## Task 7: Replace API Shadow Gate Registry

**Files:**
- Modify: `pkg/api/api.go`
- Modify: `pkg/api/handlers_gate.go`
- Modify: `pkg/api/handlers_gate_test.go`
- Modify: `pkg/api/server.go`
- Modify: `pkg/api/supervisor.go`
- Modify: `pkg/api/supervisor_test.go`

**Step 1: Write failing API tests**

Update `TestGateResolution` expectations:

- Path is `/sessions/{sid}/gates/{gid}` where `{gid}` is opaque `gate.ID`.
- Handler decodes `gate.ResponseRequest`.
- Handler calls `Agent.RespondGate(ctx, gate.GateResponse{GateID: gid, Source: user})`.
- Success returns `202 Accepted`.
- API does not inspect gate kind and does not maintain a `serve`/API open-gate map.

Update `TestListGates` to call `Agent.ListGates`.

**Step 2: Run tests and verify they fail**

Run: `go test -race ./pkg/api -run 'TestGateResolution|TestListGates'`

Expected: FAIL because API still uses `supervisor.lookup` and returns `204`.

**Step 3: Update `api.Agent`**

Replace:

```go
Approve(ctx context.Context, loopID, callID uuid.UUID, scope tool.ApprovalScope) error
Deny(ctx context.Context, loopID, callID uuid.UUID) error
ProvideAnswer(ctx context.Context, loopID, callID uuid.UUID, answer string) error
```

with:

```go
RespondGate(ctx context.Context, response gate.GateResponse) error
ListGates(ctx context.Context) ([]gate.Gate, error)
```

Keep `pkg/api` independent of `pkg/session`.

**Step 4: Simplify route handling**

- Parse `{gid}` as UUID into `gate.ID`.
- Decode `gate.ResponseRequest`.
- Stamp `Source: gate.ResponseSource{Kind: gate.ResponseFromUser}`.
- Call `entry.agent.RespondGate`.
- Return `202 Accepted` on nil error.
- Map malformed IDs/bodies to 400, unknown sessions to 404, typed session gate rejections to 404/409/400 once exposed.

**Step 5: Retire or shrink `supervisor`**

Remove gate registry behavior from `pkg/api/supervisor.go`. If the supervisor is still needed for non-gate session lifecycle tests, keep only that responsibility. Otherwise delete it and update `sessionEntry`.

**Step 6: Run tests and verify they pass**

Run: `go test -race ./pkg/api`

Expected: PASS.

**Step 7: Commit**

```bash
git add pkg/api
git commit -m "feat: route api gates by opaque gate id"
```

## Task 8: Add Owner Cleanup and Close Semantics

**Files:**
- Modify: `pkg/session/gates.go`
- Modify: `pkg/loop/loop.go`
- Modify: `pkg/loop/gate_routing_test.go`
- Modify: `pkg/session/gates_test.go`

**Step 1: Write failing cleanup tests**

Cover:

- Turn interruption closes open gates for that loop with `CloseAbandoned`.
- Owner exit after prepare before activation abandons prepared entry without public `GateResolved`.
- Owner close before response wins the claim race.
- Owner close after `GateResolved` is diagnostic/no-op and does not rewrite reason.

**Step 2: Run tests and verify they fail**

Run:

```bash
go test -race ./pkg/session -run 'TestGateClose|TestGateOwner'
go test -race ./pkg/loop -run 'Test.*Gate.*Close|Test.*Interrupt'
```

Expected: FAIL because cleanup is still `clearGates()` local to loop.

**Step 3: Implement `CloseGate`**

Rules:

- `preparing`: remove/abandon locally; no public close event because no public prompt existed.
- `open`: claim, append `GateResolved{Reason: owner_closed|abandoned}`, remove from `ListGates`.
- `claiming` / `closed`: no-op or typed stale.

**Step 4: Wire loop owner cleanup**

When a turn is interrupted or a parked producer exits, loop must call registrar `CloseGate` for every gate it opened. Replace turn-local `clearGates()` as the authoritative cleanup. Keep local cleanup as a defensive local channel drain only.

**Step 5: Run tests and verify they pass**

Run:

```bash
go test -race ./pkg/session -run 'TestGateClose|TestGateOwner'
go test -race ./pkg/loop -run 'Test.*Gate.*Close|Test.*Interrupt'
```

Expected: PASS.

**Step 6: Commit**

```bash
git add pkg/session pkg/loop
git commit -m "feat: close session-owned gates on owner exit"
```

## Task 9: Restore Gates from Records

**Files:**
- Modify: `pkg/session/restore.go`
- Modify: `pkg/session/restore_test.go`
- Modify: `pkg/session/restore_roundtrip_test.go`
- Modify: `pkg/sessionstore/replay.go`
- Modify: `pkg/sessionstore/replay_test.go`

**Step 1: Write failing restore tests**

Cover:

- `GatePrepared` without `GateOpened` is not listed/answerable after restore.
- `GateOpened` without matching private prepared payload restores as `restore_unavailable`.
- `GatePrepared` + `GateOpened` without `GateResolved` rebuilds a candidate open gate.
- `GateResolved` removes candidate open gate.
- Non-restorable current tool gates close/skip as `restore_unavailable`.

**Step 2: Run tests and verify they fail**

Run: `go test -race ./pkg/session -run 'TestRestore.*Gate'`

Expected: FAIL because restore only folds public events today.

**Step 3: Use record replay for restore**

Restore must read record replay, not event replay only, so it can see private prepared records. Fold:

```text
GatePreparedRecord               -> prepared[GateID] = gate + private payload
EventRecord(GateOpened)          -> opened[GateID] = gate
EventRecord(GateResolved)        -> delete opened/prepared or mark closed
```

Candidate open gates are only `GateOpened` with matching prepared payload and no `GateResolved`.

**Step 4: Add resolver restore hook**

Add a narrow interface for restorable resolvers:

```go
type GateRestorer interface {
	RestoreGate(ctx context.Context, g gate.Gate, payload gate.Payload) (gate.Route, bool, error)
}
```

Current loop permission and `AskUser` gates return `Restorable=false`, so they are skipped/closed.

**Step 5: Run tests and verify they pass**

Run:

```bash
go test -race ./pkg/session -run 'TestRestore.*Gate'
go test -race ./pkg/sessionstore -run 'Test.*Private.*Event|Test.*Replay'
```

Expected: PASS.

**Step 6: Commit**

```bash
git add pkg/session pkg/sessionstore
git commit -m "feat: restore gates from prepared and opened records"
```

## Task 10: Add Response Policy Enforcement

**Files:**
- Modify: `pkg/gate/policy.go`
- Modify: `pkg/session/gates.go`
- Modify: `pkg/session/gates_test.go`

**Step 1: Write failing policy tests**

Cover:

- Tool permission default is 5 minutes then `deny`.
- `PolicyRespond` submits through `RespondGate` and loses to earlier human response.
- `PolicyWait` with no timeout leaves gate open.
- `PolicyModelDecide` rejected for critical gates, permission gates, and missing responder.
- `PolicySuspendSession` rejected for non-restorable gates until session suspend lifecycle exists.
- Timeout and open-gate caps reject before `GatePreparedRecord`.

**Step 2: Run tests and verify they fail**

Run: `go test -race ./pkg/session -run 'TestGatePolicy|TestGateTimeout|TestGateCap'`

Expected: FAIL because no timers/policies are wired.

**Step 3: Implement P1/P3-safe policy subset**

Implement:

- Default policy selection at prepare time.
- Timer registration for `PolicyRespond`.
- Timer callback sends `gate.GateResponse{Source: gate.ResponseFromPolicy}` through `RespondGate`.
- `PolicyWait` no-op.
- Explicit typed rejection for `PolicyModelDecide` until a responder is wired.
- Explicit typed rejection for `PolicySuspendSession` until session lifecycle exists and gate is restorable.

**Step 4: Run tests and verify they pass**

Run: `go test -race ./pkg/session -run 'TestGatePolicy|TestGateTimeout|TestGateCap'`

Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/gate pkg/session
git commit -m "feat: enforce gate response policies"
```

## Task 11: Add Permission Decision Audit

**Files:**
- Modify: `pkg/event/tool.go`
- Modify: `pkg/event/marshal.go`
- Modify: `pkg/event/validate.go`
- Modify: `pkg/tools/check.go`
- Modify: `pkg/tools/permission.go`
- Modify: `pkg/loop/runner.go`
- Modify: `pkg/event/marshal_test.go`
- Modify: `pkg/tools/check_test.go`
- Modify: `pkg/loop/runner_test.go`

**Step 1: Write failing audit tests**

Cover:

- Auto-deny emits exactly one `PermissionDecided{Effect: deny}`.
- Auto-approve emits exactly one `PermissionDecided{Effect: approve}` unless read-fast-path coalescing is explicitly enabled.
- Gated ask emits `GateOpened` and later `GateResolved`, not `PermissionDecided`.
- `.env` hard deny is recorded as deny with reason `hard_deny`.

**Step 2: Run tests and verify they fail**

Run:

```bash
go test -race ./pkg/event ./pkg/tools ./pkg/loop -run 'Test.*Permission.*Decision|Test.*HardDeny|Test.*AutoApprove'
```

Expected: FAIL because `PermissionDecided` does not exist.

**Step 3: Implement event and emission**

Add `event.PermissionDecided` with:

- `Effect`
- `Reason`
- redacted subject/audit
- no raw grant tokens

Emit it in checker paths that do not open a gate. Do not emit it for `Ask`; that path is covered by `GateOpened` + `GateResolved`.

**Step 4: Run tests and verify they pass**

Run:

```bash
go test -race ./pkg/event ./pkg/tools ./pkg/loop -run 'Test.*Permission.*Decision|Test.*HardDeny|Test.*AutoApprove'
```

Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/event pkg/tools pkg/loop
git commit -m "feat: audit non-gated permission decisions"
```

## Task 12: Compatibility Cleanup and Full Verification

**Files:**
- Modify: `pkg/session/session.go`
- Modify: `pkg/api/api.go`
- Modify: `pkg/api/supervisor.go`
- Modify: `docs/plans/2026-06-18-gates-package-design.md` only if implementation discovers a spec correction.

**Step 1: Remove obsolete API/session branches**

Remove or deprecate:

- API supervisor gate lookup.
- Tool-execution-id path wording in comments.
- Direct `Approve` / `Deny` / `ProvideUserInput` use from HTTP.

Keep compatibility wrappers only where internal callers or tests still need them, and mark them as wrappers over `RespondGate` or legacy-only.

**Step 2: Run targeted package tests**

Run:

```bash
go test -race ./pkg/gate ./pkg/event ./pkg/journal ./pkg/sessionstore ./pkg/session ./pkg/loop ./pkg/api
```

Expected: PASS.

**Step 3: Run full race suite**

Run:

```bash
go test -race ./...
```

Expected: PASS.

**Step 4: Run formatting and security checks**

Run:

```bash
make fmt-check
make secure
```

Expected: PASS. If `make secure` requires tools not installed locally, record the missing tool and run the strongest available subset.

**Step 5: Commit**

```bash
git add pkg/gate pkg/event pkg/journal pkg/sessionstore pkg/session pkg/loop pkg/api docs/plans/2026-06-18-gates-package-design.md
git commit -m "feat: implement session-authoritative gates"
```

## Execution Notes

- Use a dedicated worktree before implementation. The current docs workspace may be dirty.
- Keep `pkg/api` dependency-inverted: it may import `pkg/gate` and `pkg/event`, but not concrete `pkg/session` or `pkg/loop`.
- Do not add external dependencies.
- Prefer strict append seams for gate prepare/open/resolve. Do not use `hub.PublishEvent` directly where the caller must observe append failure.
- For public history/SSE, expose `GateOpened` and `GateResolved`; do not expose private prepared records.
- Treat `202 Accepted` as durable session acceptance only. Do not imply runner consumption until an explicit resolver ack exists.
