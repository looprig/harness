# pkg/gate

`pkg/gate` is the harness's **generic access-decision layer**. It defines the
durable domain envelope for human- and policy-resolved gates and the generic
three-state access evaluator that decides one typed prepared request per tool
call.

It is deliberately **generic**. `pkg/gate`:

- does **not** parse tool arguments — tools prepare typed requests
  (`tool.CallPreparer` in `pkg/tool`) before evaluation ever starts;
- does **not** define sandbox profiles and does **not** import a sandbox or any
  other enforcement package — the access, rule, and grant seams are structural,
  built-in-typed interfaces an enforcing consumer satisfies without importing
  harness;
- does **not** implement a permission-file format — durable rule matching and
  persistence are consumer-provided (`RuleMatcher`, `RuleWriter`).

## What is gate?

A `gate.Evaluator` decides one prepared `tool.Request` per tool call:

- **`Deny`** — the call is not executed; the model sees a paired
  permission-denied tool result.
- **`Gated`** — the call needs approval. The whole unmet set is resolved by
  **one combined prompt** with exactly three actions: `Approve` (once),
  `Approve always for this workspace` (persists the displayed candidates
  atomically before any grant is minted), `Deny` (no error; nothing minted).
- **`Allow`** — the call runs; no grant token is needed from this layer.

Construction explicitly selects the interaction mode:

- `NewInteractiveEvaluator(bindings, matcher, approver, writer, issuer)` —
  requires both an `Approver` and a durable `RuleWriter`, so all three
  approval actions are honest.
- `NewHeadlessEvaluator(bindings, matcher, issuer)` — accepts neither,
  never prompts, and resolves an unmet gated requirement as a typed
  approval-required denial (`EvaluationApprovalRequired`).

The package also defines the durable **gate envelope** (`Gate`, `Payload`,
`GateResponse`, `GateRoute`, `ID`, `Answer`, `CloseReason`) used by every
kind of host-facing gate — permission, ask-user, form, open-URL — and
the response routing the session uses to deliver a human's reply back to
the loop that opened one.

## Boundary of responsibilities

| Concern | Owner |
| --- | --- |
| Argument decoding, normalization (commands, URLs, paths), canonical resource identity, per-call artifacts | The tool, via `tool.CallPreparer.PrepareCall` (`pkg/tool`) |
| Three-state decision (`Deny`/`Gated`/`Allow`), deny-before-allow ordering, one combined approval, response transport, redacted audit | `pkg/gate` (this package) |
| Access profiles, OS confinement, grant-token minting and enforcement | The enforcing consumer (e.g. a sandbox module), behind the structural `AccessSource` and `GrantIssuer` seams |
| Durable rule storage and matching (whatever file or store format) | The consumer, behind `RuleMatcher` / `RuleWriter` |

Invalid tool input fails during preparation and never reaches the evaluator;
`tool.ValidateRequest` re-checks every prepared-request invariant at the start
of evaluation and at both durable codec boundaries.

## Typed prepared requests

A tool call is evaluated as one `tool.Request`: the tool name, a bounded
display summary, optional execution-binding fields (`ExecutionID`, `Command`,
`WorkingDirectory`, `ExpiresAtUnixMilli` — required exactly when any
requirement requests a grant), and a set of `tool.Requirement` values. Each
requirement carries:

- `Kind` — routed to exactly one `AccessSource` via `AccessBindings`;
- `Scope` — used only for access routing;
- `Match` — used only for stored-rule matching;
- `Description` — used only for bounded display and audit;
- an optional `GrantClass`/`GrantTarget` pair requesting one post-decision
  execution-bound grant;
- `Candidates` — the exact reusable allow rules displayed to the user and
  offered for durable persistence.

The access ABI is versioned (`CurrentAccessVersion`, currently 1). Sources
return the raw `uint8` states `AccessDeny`/`AccessGated`/`AccessAllow`;
unknown kinds, unknown values, source errors, and version mismatches all fail
closed as typed `AccessError` values.

## Evaluator lifecycle

Construction explicitly selects the interaction mode:

- `NewInteractiveEvaluator(bindings, matcher, approver, writer, issuer)` —
  requires both an `Approver` and a durable `RuleWriter`, so all three
  approval actions are honest.
- `NewHeadlessEvaluator(bindings, matcher, issuer)` — accepts neither, never
  prompts, and resolves an unmet gated requirement as a typed
  approval-required denial (`EvaluationApprovalRequired`).

`Authorize(ctx, request)` is the single entry: it runs `Evaluate`, opens at
most one combined approval (interactive construction only, and only when gated
requirements remain unmet), applies the chosen action via `Resolve`, and mints
fresh execution-bound grants for the approved call.

`Evaluate` applies the generic order:

1. **Configured access first.** Every requirement is routed to its sole bound
   source. Any `Deny` short-circuits: the evaluation returns the combined
   denied set and nothing further is consulted. `Allow` needs no grant token
   from this layer; `Gated` continues.
2. **Every stored deny before any allow.** Each gated requirement is checked
   against `RuleMatcher.MatchesDeny`; any match denies the call.
3. **Stored allows.** A gated requirement matched by `MatchesAllow` is met; the
   rest form **one combined unmet set** together with every displayed reusable
   candidate.

`Resolve` applies exactly one of the three approval actions:

- `Approve` — approve once; nothing is persisted.
- `Approve always for this workspace` — atomically persists the entire
  displayed candidate batch in one `RuleWriter.WriteRules` call *before* any
  grant is minted; a persistence failure blocks execution.
- `Deny` — an unapproved `Resolution` with no error; nothing is minted.

Every dependency failure (rule match, approver, writer, issuer) is a typed,
fail-closed `EvaluationError`. An unapproved `Resolution` with a nil error is a
policy or user denial, not a fault.

## One combined prompt

Multiple gated requirements never produce serial prompts. The whole unmet set
travels in one `ApprovalPrompt{Request, Unmet, Candidates}`, resolved by the
consumer's `Approver` to exactly one `ApprovalAction`. `ApprovalControls()`
returns the exact, complete control set — there is no session scope,
user-global scope, persistent-deny action, or second capability prompt. A
partial saved approval yields one prompt containing only the still-unmet
requirements.

Inside a running loop, `loop.GateApprover()` is the `Approver` a consumer
passes to interactive construction: it resolves each combined prompt through
the live loop's per-call approval capability (installed on ctx by the runner)
and fails closed outside a live loop call.

## Response routing

An interactive approval travels as a durable permission gate:

- The runner opens a `Gate` of `KindPermission` whose private
  `PermissionPayload` carries the displayed `tool.Request`. The payload is
  validated at **both** codec boundaries (`tool.ValidateRequest` on marshal,
  the strict `DecodeRequest` on unmarshal), so a malformed or token-bearing
  record can neither be journaled nor restored.
- The session routes the human's reply by `Route`
  (`GateID`/`LoopID`/`ToolExecutionID`): an approve action becomes
  `command.ApproveToolCall` carrying the exact `gate.ApprovalAction`, a deny
  becomes `command.DenyToolCall`. `ParseApprovalAction` is the single
  validation source shared by the strict wire decoder
  (`DecodeApprovalAction`) and the session route; anything but the three exact
  actions fails closed.
- The runner maps the routed command back to the action and hands it to
  `Resolve`.

Other gate kinds (`KindAskUser`, `KindForm`, `KindOpenURL`) share the same
envelope, payload codec, and response routing; see the type docs in
`payload.go`, `form.go`, and `prompt.go`.

## Audit behavior

Durable audit records are **descriptions only, never tokens**:

- `PermissionAudit` stores the bounded display descriptions of the approved
  requirements and — only for a workspace approval, which persists them — of
  the displayed reusable candidates. Never grant tokens, token material, or
  raw tool arguments.
- `Resolution.Grants` is excluded from JSON (`json:"-"`): minted tokens travel
  only through the prepared execution contract (`tool.PreparedCall`), never a
  prompt, display, journal, or audit payload.
- `RuleCandidate` contains no grant or token material; its
  `GrantClass`/`GrantTarget` describe only the structural enforcement contract
  a future match must preserve.

## Example

This example is compiled and run as a doc test (`example_test.go`); keep the
two in sync.

```go
// staticAllow is a minimal AccessSource that allows every routed scope.
type staticAllow struct{}

func (staticAllow) AccessVersion() uint16 { return gate.CurrentAccessVersion }
func (staticAllow) AccessFor(kind, scope string) (uint8, error) {
	return gate.AccessAllow, nil
}

func Example() {
	evaluator, err := gate.NewHeadlessEvaluator(
		[]gate.AccessBinding{{Kind: "fs.read", Source: staticAllow{}}},
		nil, // no stored rules
		nil, // no grant issuer: no requirement below requests a grant
	)
	if err != nil {
		fmt.Println(err)
		return
	}
	resolution, err := evaluator.Authorize(context.Background(), tool.Request{
		ToolName: "Read",
		Requirements: []tool.Requirement{{
			Kind:        "fs.read",
			Match:       "Read(/repo/README.md)",
			Description: "Read /repo/README.md",
		}},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(resolution.Approved)
	// Output: true
}
```

## Sibling packages

- [`pkg/tool`](../tool/README.md) — `tool.Request`, `tool.Requirement`,
  `tool.RuleCandidate`, `tool.ValidateRequest`, and the `CallPreparer`
  boundary that produces the typed request the evaluator consumes.
- [`pkg/command`](../command/README.md) — `command.ApproveToolCall` /
  `command.DenyToolCall`, the routed wire forms of an `ApprovalAction`;
  `ParseApprovalAction` is the single validation source shared by the
  strict decoder here and the session route.
- [`pkg/event`](../event/README.md) — `event.PermissionRequested`
  carries the gate id the session uses to route a reply.
- [`pkg/loop`](../loop/README.md) — `loop.AccessGate` is the runner's view
  of an evaluator; `loop.GateApprover` is the `Approver` a live loop
  passes to interactive construction.
- `github.com/looprig/sandbox` — satisfies `AccessSource` /
  `GrantIssuer` with OS confinement. Harness never imports it.
