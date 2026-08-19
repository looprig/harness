# Declarative Restore Failure Policy Design

## Goal

Let a Rig composition declare which configuration drift is allowed during
restore without implementing `session.RestoreDecider` and
`session.RuntimeRestoreResolver` directly. The policy controls failure only; it
does not change what state is durable or the existing exact-first reconstruction
order.

## API

Harness adds a functional-option surface at the Rig layer:

```go
rig.WithRestoreFailurePolicy(
    rig.AllowExternalCapabilityDrift(),
    rig.AllowModelDrift(),
    rig.AllowEffortDrift(),
)
```

The absence of `WithRestoreFailurePolicy` preserves the existing fail-closed
default. An explicitly empty failure policy is also fail-closed. A composition
names only drift that it permits; every unlisted warning remains fatal.

The initial option set follows warning-level facts and typed runtime failures
Harness understands:

- `AllowModelDrift()`
- `AllowExternalCapabilityDrift()`
- `AllowConfinementDrift()`
- `AllowPermissionDrift()`
- `AllowPermissionPostureDrift()`
- `AllowPermissionReviewDrift()`
- `AllowWorkspaceDrift()`
- `AllowTrustDrift()`
- `AllowAgentKindDrift()`
- `AllowAgentNameDrift()`
- `AllowAdapterDrift()`
- `AllowRuntimeSkillsDrift()`
- `AllowHookPolicyDrift()`
- `AllowRuntimeProfileDrift()`
- `AllowRuntimeCatalogDrift()`
- `AllowCredentialDrift()`
- `AllowEffortDrift()`

Information-only manifest changes such as prompt, tool, topology, and
application-field changes already restore under the default policy, so the API
does not add no-op allowances for them. `AllowModelDrift` remains meaningful
because it also governs a typed per-loop runtime target mismatch.

Broad options cover their narrower forms. For example,
`AllowPermissionDrift()` covers native permission policy, foreign posture, and
permission-review changes; the narrower permission options allow a composition
to keep review policy critical while accepting access-posture changes.

Duplicate allow options are idempotent. `WithRestoreFailurePolicy` is mutually
exclusive with the
advanced `WithRestoreDecider`, `WithRuntimeRestoreResolver`, and legacy
`WithAllowConfigMismatch` paths so option order cannot silently change policy.

## Ownership boundary

The policy names Harness facts, not product integrations. Harness therefore
offers `AllowExternalCapabilityDrift`, not `AllowMCPDrift`: Carbon uses the
external-capability revision for MCP, but another composition may use it for a
different attached capability set. Product application fields remain
information-level under Harness's existing drift classification and need no
allowance.

The manifest intentionally includes both Harness-derived and
composition-supplied facts. Harness derives model, prompt, tools, topology,
permission-review, hook, and runtime identities. The composition supplies
agent kind, runtime-skills state, workspace/trust, adapter/posture, native
permission identity, confinement identity, external capability identity, and
application fields. Composer-supplied facts remain in the manifest because
Harness owns the durable comparison even when it does not compute the value.

## Manifest decision flow

Harness continues to build an `event.DriftAssessment` from the latest adopted
manifest and the current Rig manifest. The declarative policy compiles to a
`session.RestoreDecider`:

1. Information-level changes are accepted as today.
2. Each warning is matched against the configured allowances.
3. Any unmatched warning rejects the restore.
4. If all warnings match, Harness adopts the current manifest and records the
   ordinary bounded `ConfigurationAdopted` audit event.

The existing `RestoreDecider` interface remains the advanced escape hatch for
directional, value-aware, or external policy decisions.

## Runtime decision flow

Runtime behavior remains exact-first:

1. Harness tries to reconstruct the durable agent, harness, profile, model,
   credential mode, and effort exactly.
2. If exact reconstruction succeeds, the failure policy does nothing.
3. If it fails, the compiled runtime policy checks the typed mismatch.
4. An allowed profile, target/model, credential, or effort mismatch may resolve
   the current default for the same agent and harness.
5. Harness validates that the resolution exists in the current catalog and
   cannot cross durable agent or harness identity.
6. If the harness itself is absent, no policy can fabricate it. An inactive
   child follows the existing tombstone behavior; an active child fails with
   the actionable used-harness diagnostic.

`AllowRuntimeCatalogDrift` permits the manifest catalog revision to change; it
does not by itself authorize a target, credential, or effort fallback. Those
require their corresponding allowances. Because `RuntimeIdentityRev` is an
opaque digest, any enabled runtime fallback allowance lets that manifest warning
reach the per-loop typed check, where the actual mismatch kind is enforced.

## Compatibility and errors

- Omitting the new option retains `DefaultPolicyDecider` and exact fail-closed
  runtime restoration.
- `WithAllowConfigMismatch` remains available as a deprecated blanket shim.
- Existing custom deciders and resolvers retain their interfaces and behavior.
- Invalid or conflicting policy configuration fails during `rig.Define` with a
  typed `DefinitionError`.
- Public errors continue to expose bounded categories and harness names, never
  raw model targets, credentials, endpoints, or configuration contents.

## Testing

Harness tests will cover:

- empty policy preserving fail-closed behavior;
- one allowed manifest fact accepting only its matching warning;
- unlisted and mixed warnings rejecting;
- narrow versus broad permission allowances;
- application-field name matching;
- duplicate allowances and invalid application fields;
- conflicts with legacy and advanced policy options;
- exact runtime reconstruction remaining unchanged;
- independently allowed target, effort, credential, and profile fallback;
- disallowed runtime mismatches remaining fatal/tombstoned as appropriate;
- unavailable active harness retaining the actionable diagnostic;
- public contract allowlists and full standalone verification.

Carbon will replace its private decider and resolver with the declarative Rig
configuration. Its policy will allow current ephemeral MCP/external capability,
runtime skills, confinement, access posture/profile, runtime catalog/profile,
model target, and effort drift while retaining its current critical facts.
