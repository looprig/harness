# Declarative Restore Failure Policy Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add functional Rig options that let compositions name allowed restore drift while preserving fail-closed defaults and exact-first runtime reconstruction, then migrate and release Harness and Carbon in dependency order.

**Architecture:** `pkg/rig` will compile sealed `Allow…Drift` functional options into one immutable value implementing both `session.RestoreDecider` and `session.RuntimeRestoreResolver`. Manifest warnings are matched by category/field; opaque runtime-identity warnings proceed only when a typed runtime allowance exists, then the existing per-loop mismatch kind makes the final decision. Carbon will replace its private policy implementations with the declarative Rig configuration.

**Tech Stack:** Go 1.26, Looprig Harness session/rig/event/runtime APIs, table-driven tests, Git worktrees, Go module semantic-version releases.

---

### Task 1: Specify the public functional-option API

**Files:**
- Create: `pkg/rig/restore_failure_policy_test.go`
- Modify: `pkg/rig/contracts_test.go` or the repository's public-contract allowlist test

**Step 1: Write the failing API tests**

Add compile-time and behavioral construction tests using:

```go
policy := compileRestoreFailurePolicy(
    AllowExternalCapabilityDrift(),
    AllowModelDrift(),
    AllowEffortDrift(),
)
```

Cover every approved option name, duplicate idempotence, and an explicitly empty policy.

**Step 2: Run the focused test to verify RED**

Run:

```bash
GOWORK=off go test ./pkg/rig -run 'TestRestoreFailurePolicyOptions|TestPublic' -count=1
```

Expected: build failure because `RestoreFailureOption` and the `Allow…Drift` functions do not exist.

**Step 3: Add the sealed option types and immutable policy state**

Create `pkg/rig/restore_failure_policy.go` with:

```go
type RestoreFailureOption interface {
    applyRestoreFailure(*restoreFailurePolicy)
}

type restoreFailureOptionFunc func(*restoreFailurePolicy)

func (f restoreFailureOptionFunc) applyRestoreFailure(policy *restoreFailurePolicy) {
    f(policy)
}
```

Use sets keyed by `event.DriftCategory`, category/field pairs, and the exported runtime mismatch constants. Provide the approved options:

```text
AllowModelDrift
AllowExternalCapabilityDrift
AllowConfinementDrift
AllowPermissionDrift
AllowNativePermissionDrift
AllowPermissionPostureDrift
AllowPermissionReviewDrift
AllowWorkspaceDrift
AllowTrustDrift
AllowAgentKindDrift
AllowAgentNameDrift
AllowAdapterDrift
AllowRuntimeSkillsDrift
AllowHookPolicyDrift
AllowRuntimeProfileDrift
AllowRuntimeCatalogDrift
AllowCredentialDrift
AllowEffortDrift
```

`AllowModelDrift` covers `RestoreRuntimeTargetMismatch`; profile, credential, and effort options cover their corresponding typed runtime recovery paths. Do not add no-op options for information-only prompt, tool, topology, or application-field changes.

**Step 4: Run focused tests to verify GREEN**

Run the Task 1 command and expect PASS.

**Step 5: Commit**

```bash
git add pkg/rig/restore_failure_policy.go pkg/rig/restore_failure_policy_test.go pkg/rig/contracts_test.go
git commit -m "feat: define declarative restore failure options"
```

### Task 2: Compile allowances into manifest decisions

**Files:**
- Modify: `pkg/rig/restore_failure_policy.go`
- Modify: `pkg/rig/restore_failure_policy_test.go`

**Step 1: Write failing decider tests**

Exercise the compiled policy through `DecideRestore`:

- empty policy rejects every warning and accepts information-only drift;
- one allowance accepts its matching warning;
- a mixed allowed/unlisted assessment rejects;
- `AllowPermissionDrift` accepts all permission fields;
- posture and review allowances accept only their narrower fields;
- runtime catalog/profile fields remain independent;
- any runtime target/credential/effort allowance admits the opaque
  `runtime/identity_rev` warning so the typed per-loop check can decide it.

**Step 2: Verify RED**

```bash
GOWORK=off go test ./pkg/rig -run TestRestoreFailurePolicyDecider -count=1
```

Expected: failures because the compiled policy does not implement `session.RestoreDecider`.

**Step 3: Implement the minimal decider**

Implement:

```go
func (p restoreFailurePolicy) DecideRestore(
    _ context.Context,
    assessment event.DriftAssessment,
) (session.RestoreDecision, error)
```

Accept information changes. Reject on the first unmatched warning. Accept a fully matched assessment with `DecisionSourcePolicy` and a bounded generic adoption message.

**Step 4: Verify GREEN**

Run the focused test and then:

```bash
GOWORK=off go test ./pkg/rig ./pkg/session ./pkg/event -count=1
```

Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/rig/restore_failure_policy.go pkg/rig/restore_failure_policy_test.go
git commit -m "feat: decide restore drift from rig allowances"
```

### Task 3: Compile runtime failure allowances

**Files:**
- Modify: `pkg/rig/restore_failure_policy.go`
- Modify: `pkg/rig/restore_failure_policy_test.go`
- Modify if required: `internal/sessionruntime/restore_constructor.go`
- Modify if required: `internal/sessionruntime/restore_runtime_test.go`

**Step 1: Write failing runtime tests**

Build real runtime catalogs and verify:

- exact durable selection remains unchanged and does not invoke fallback;
- model allowance resolves the current same-harness default after a target mismatch;
- effort allowance resolves after an effort mismatch but not a target mismatch;
- credential allowance resolves after a credential mismatch only;
- profile allowance resolves the current default when the old profile is unavailable;
- runtime-catalog allowance alone does not authorize any typed fallback;
- a missing harness cannot be resolved;
- a returned runtime cannot cross the durable agent or harness identity.

**Step 2: Verify RED**

```bash
GOWORK=off go test ./pkg/rig ./internal/sessionruntime -run 'TestRestoreFailurePolicyRuntime|TestRestoreRuntimeFailurePolicy' -count=1
```

Expected: failures because the policy does not implement runtime resolution.

**Step 3: Implement the minimal resolver**

Implement `ResolveRuntimeRestore` on the compiled policy. Reject disallowed mismatch kinds. For allowed kinds, call the current catalog for the same agent and harness with empty model/effort selection, returning its current default. Leave Harness's existing post-resolution validation intact.

If the current request shape cannot distinguish profile unavailability from an absent harness, attempt same-harness resolution only for `AllowRuntimeProfileDrift`; catalog failure remains the actionable unavailable-harness result.

**Step 4: Verify GREEN**

Run focused tests and expect PASS.

**Step 5: Commit**

```bash
git add pkg/rig/restore_failure_policy.go pkg/rig/restore_failure_policy_test.go internal/sessionruntime/restore_constructor.go internal/sessionruntime/restore_runtime_test.go
git commit -m "feat: resolve allowed runtime drift"
```

### Task 4: Add `WithRestoreFailurePolicy` and enforce exclusivity

**Files:**
- Modify: `pkg/rig/options.go`
- Modify: `pkg/rig/errors.go`
- Modify: `pkg/rig/restore_failure_policy_test.go`
- Modify: `internal/sessionruntime/restore_decider_plumbing_test.go`

**Step 1: Write failing Rig-definition tests**

Verify that:

```go
rig.WithRestoreFailurePolicy(
    rig.AllowExternalCapabilityDrift(),
    rig.AllowRuntimeCatalogDrift(),
)
```

installs the compiled value as both restore collaborators. Verify empty policy is valid/fail-closed, nil functional options fail definition, and every ordering of these combinations returns a typed duplicate/conflict error:

- failure policy + `WithRestoreDecider`;
- failure policy + `WithRuntimeRestoreResolver`;
- failure policy + `WithAllowConfigMismatch`.

**Step 2: Verify RED**

```bash
GOWORK=off go test ./pkg/rig -run 'TestWithRestoreFailurePolicy|TestRestorePolicyConflict' -count=1
```

Expected: build failure because the Rig option does not exist.

**Step 3: Implement option plumbing**

Add `WithRestoreFailurePolicy(options ...RestoreFailureOption) Option`, a dedicated definition error kind, and symmetric conflict checks in all four restore-policy entry points. Compile a fresh immutable policy and install it through both lifecycle collaborators.

**Step 4: Verify GREEN**

Run focused tests and `GOWORK=off go test ./pkg/rig ./internal/sessionruntime -count=1`.

**Step 5: Commit**

```bash
git add pkg/rig/options.go pkg/rig/errors.go pkg/rig/restore_failure_policy_test.go internal/sessionruntime/restore_decider_plumbing_test.go
git commit -m "feat: configure restore failure policy on rigs"
```

### Task 5: Migrate Carbon to declarative configuration

**Files:**
- Modify: `/Users/ipotter/code/looprig/.worktrees/ephemeral-restore-carbon/internal/app/persistence.go`
- Delete: `/Users/ipotter/code/looprig/.worktrees/ephemeral-restore-carbon/internal/app/restore_policy.go`
- Modify: `/Users/ipotter/code/looprig/.worktrees/ephemeral-restore-carbon/internal/app/restore_policy_test.go`
- Modify as needed: Carbon restore integration tests

**Step 1: Write failing Carbon policy-composition tests**

Assert Carbon's Rig uses allowances equivalent to its current behavior:

```go
rig.WithRestoreFailurePolicy(
    rig.AllowExternalCapabilityDrift(),
    rig.AllowRuntimeSkillsDrift(),
    rig.AllowConfinementDrift(),
    rig.AllowNativePermissionDrift(),
    rig.AllowPermissionPostureDrift(),
    rig.AllowRuntimeProfileDrift(),
    rig.AllowRuntimeCatalogDrift(),
    rig.AllowModelDrift(),
    rig.AllowEffortDrift(),
)
```

Keep permission-review, workspace/trust, agent identity, adapter, hook policy, and credential drift critical. Keep explicit `AllowConfigMismatch` as the legacy caller override path.

**Step 2: Verify RED**

```bash
GOWORK=/private/tmp/ephemeral-restore-work/go.work go test ./internal/app -run 'TestCarbonRestore|TestACPCompositionRemoval|TestACPCompositionMissingLuna' -count=1
```

Expected: failure until Carbon constructs the new Rig option.

**Step 3: Replace private policy implementations**

Use `WithRestoreFailurePolicy` in the normal Carbon assembly path and remove the private decider/resolver. Preserve the legacy explicit mismatch override without combining mutually exclusive policy options.

**Step 4: Verify GREEN**

Run focused tests, then Carbon's full paired-workspace suite:

```bash
GOWORK=/private/tmp/ephemeral-restore-work/go.work go test ./... -count=1
```

Expected: PASS.

**Step 5: Commit in Carbon only**

```bash
git add internal/app
git commit -m "refactor: declare restore failure policy on rig"
```

### Task 6: Verify and release Harness

**Files:**
- Modify only if required by tidy: `go.mod`, `go.sum`

**Step 1: Run formatting and repository checks**

```bash
gofmt -w <changed-go-files>
GOWORK=off go mod tidy
GOWORK=off go vet ./...
GOWORK=off go test ./... -count=1
```

Expected: no uncommitted tidy changes unless justified; vet and tests PASS.

**Step 2: Review the branch diff and public contract**

```bash
git diff --check main...HEAD
git status --short
git log --oneline main..HEAD
```

Preserve unrelated dirty files in the main Harness checkout.

**Step 3: Merge to local Harness `main`**

Fast-forward `feat/ephemeral-restore-config` into Harness `main` without staging or modifying the unrelated OpenTelemetry plan files. Re-run the full standalone checks on the merged commit.

**Step 4: Publish the compatible API release**

Because this adds public compatible API, release the next minor version after confirming remote state (expected `v0.27.0`):

```bash
git push origin main
git tag -a v0.27.0 -m "harness v0.27.0"
git push origin v0.27.0
git ls-remote origin refs/heads/main refs/tags/v0.27.0 refs/tags/v0.27.0^{}
```

### Task 7: Audit Harness dependents and release Carbon

**Files:**
- Modify: Carbon `go.mod`, `go.sum`
- Modify only when an audit proves impact: dependent repository `go.mod`, `go.sum`, and source/tests
- Inspect: root `go.work`, `repositories.mk`

**Step 1: Enumerate actual Harness consumers**

Search every component `go.mod` for `github.com/looprig/harness`. Compare the result to the workspace dependency graph. Do not update unaffected modules merely because a new additive Harness version exists.

**Step 2: Verify direct dependents standalone**

For each direct consumer, run its native checks and:

```bash
GOWORK=off go test ./... -count=1
```

An additive Harness API should leave consumers pinned to the prior release unaffected. Record failures and update only repositories that consume the new API or otherwise demonstrably require the release.

**Step 3: Pin Carbon to the published Harness release**

From Carbon with `GOWORK=off`:

```bash
go get github.com/looprig/harness@v0.27.0
go mod tidy
go vet ./...
go test ./... -count=1
```

Expected: standalone Carbon PASS with no local `replace` directive.

**Step 4: Merge and publish Carbon**

Fast-forward Carbon's feature branch into local `main`, re-run standalone checks, push `main`, then create and push the next compatible minor release (expected `v0.22.0`). Verify remote branch and tag refs.

**Step 5: Synchronize workspace metadata**

Check root `go.work`, `repositories.mk`, and dependency documentation. Update only files that actually encode the published versions; never stage component repositories in the outer workspace repository.

**Step 6: Install the released Carbon binaries**

With published pins and `GOWORK=off`, install `cmd/carbon` and `cmd/carbon-collab-mcp` into `/Users/ipotter/.looprig/bin`, then verify a fresh login shell resolves both executables without ACP environment overrides.
