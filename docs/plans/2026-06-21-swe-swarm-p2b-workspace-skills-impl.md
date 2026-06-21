# SWE Swarm — P2b Implementation Plan (Workspace `.skills/` — opt-in, untrusted, gated)

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add an **opt-in, human-enabled** second skill source — `<workspace>/.skills/` —
loaded as **untrusted** content behind an **enforced Ask gate**, with TOCTOU-safe binding.

**Architecture:** A launch flag enables the workspace source. Generic auto-approved file tools
are hard-denied `.skills/**` (no bypass). The `Skill` tool's `EffectChecker` returns **Ask** for a
workspace candidate; a runner **`Preparer`** seam takes a no-follow `os.Root` snapshot + SHA-256
**before** the prompt, surfaced as a `ScopeOnce`-only `SkillLoadRequest`; after approval the
**snapshot** runs. Depends on P1 + P2.

**Tech Stack:** Go 1.26 `os.Root` (safe-root traversal), `crypto/sha256`, the merged persistence
codec. Design: `docs/plans/2026-06-21-swe-swarm-roles-design.md` §7a, §10, §16.4/§16.5.

**Conventions:** as in `2026-06-21-swe-swarm-roles-impl.md`.

---

## Phase 1 — Close the read-tool bypass

### Task 1.1: `.skills/**` HardDeny in the SWE policy

**Files:** Modify the SWE policy assembly (where each agent's `PermissionPolicy` is built, P1
`BuildTools`); add `.skills/**` to both `DeniedReadPaths` and `DeniedWritePaths` (SWE additions on
top of `tools.DefaultHardDeny()`). Test: per-agent policy test.

**Step 1 — failing test:** `ReadFile`/`Glob`/`Grep`/`WriteFile`/`EditFile` on `.skills/**` are
denied for every agent (the only path in is the `Skill` tool). **Step 2-4. Commit:**
`feat(swarm): hard-deny .skills/** for generic file tools`.

---

## Phase 2 — Runner `Preparer` seam

### Task 2.1: `Preparer` interface + `resolved.prepared` + runner wiring

**Files:** Modify `internal/agent/loop/runner.go`; Test: `internal/agent/loop/runner_test.go`.

- `type Preparer interface { Prepare(ctx context.Context, callID uuid.UUID, argsJSON string) (PreparedArtifact, error) }`.
- After `newResolved` (callID minted), if `r.t` implements `Preparer`, call `Prepare` and store on
  `resolved.prepared`. Thread `resolved.prepared` into the **request-building** call site (so
  `BuildRequest` can read the prepared SHA-256) and into **execution** (the tool consumes the same
  artifact, never re-reads).

**Step 1 — failing tests:** a Preparer tool's artifact is computed **once** (assert single call),
visible to request-building, and consumed by execution (permission-time hash == executed bytes);
a non-Preparer tool path is unchanged. **Step 2-4 (`-race`). Commit:**
`feat(loop): runner Preparer seam binds one artifact across permission+execution`.

---

## Phase 3 — Workspace skill loading (TOCTOU-safe)

### Task 3.1: `os.Root` safe snapshot + strict names

**Files:** Modify `tools/skill_loader.go` (add a workspace source); Create
`tools/skill_workspace.go`; Test: `tools/skill_workspace_test.go` + `*_integration_test.go`
(tagged) for the FS surface.

- Strict name: ASCII slug `[a-z0-9][a-z0-9_-]*`, length-capped; reject empty/`.`/`..`/`/`/`\`/control → `SkillContainmentError`.
- Open the workspace root as `os.Root`; open `.skills/<name>/SKILL.md` **through it**
  (descriptor-relative, refuses symlink/`..` escape on every component); **regular-file check**;
  read bounded (`maxSkillBytes`) snapshot; `crypto/sha256` the bytes.
- **Embedded wins collision:** if `name` exists in the embedded allow-map, never consult workspace.

**Step 1 — failing tests + tagged integration:** happy load; **intermediate-dir symlink escape
rejected**; final-symlink rejected; non-regular target rejected; oversize; bad name →
`SkillContainmentError`; embedded name shadows workspace. **Step 2-4. Commit:**
`feat(tools): os.Root workspace skill snapshot (TOCTOU-safe) + strict names`.

### Task 3.2: `SkillLoadRequest` + `Skill` Preparer + `EffectChecker`

**Files:** Modify `internal/tool/permission_request.go` (`SkillLoadRequest`); modify `tools/skill.go`
(implement `Preparer` + `EffectChecker` + `BuildRequest`); Test: `tools/skill_test.go`.

- `SkillLoadRequest{RelPath, Agent, Size, SHA256}` — `permissionRequest()` marker;
  `AllowedScopes() == []ApprovalScope{ScopeOnce}` (never persisted).
- `Skill.CheckEffect(argsJSON)` → **Ask** when the `name` is a workspace candidate (absent from
  the embedded allow-map), else handled=false (embedded → existing auto-approve).
- `Skill.Prepare(ctx, callID, argsJSON)` → the §3.1 snapshot artifact.
- `Skill.BuildRequest(argsJSON)` → a `SkillLoadRequest` from the prepared artifact (via
  `resolved.prepared`); execution returns the **snapshot bytes**.

**Step 1 — failing tests:** workspace name → Ask + `SkillLoadRequest` with the right hash;
`AllowedScopes()=={ScopeOnce}`; embedded name → auto-approve; approval runs the snapshot, not a
re-read. **Step 2-4. Commit:** `feat(tools): SkillLoadRequest + Skill Preparer/EffectChecker (ScopeOnce, gated)`.

### Task 3.3: `AllowsRuntimeSkills` behavior + enablement flag

**Files:** Modify `swarms/swe` (a `RuntimeSkills bool` swarm config / `--runtime-skills` flag in
`cmd/swe`); only agents with `AllowsRuntimeSkills` (set true for `explorer`+`researcher`) get the
workspace source wired. Test: `swarms/swe` + `cmd/swe`.

**Step 1 — failing tests:** flag off → no workspace source for any agent; flag on → only
explorer/researcher reach it; orchestrator/operator/reviewer never do. **Step 2-4. Commit:**
`feat(swarm): opt-in runtime-skills flag; explorer/researcher only`.

---

## Phase 4 — Persistence (fingerprint + codec)

### Task 4.1: `SkillLoadRequest` in the persisted `PermissionRequest` codec

**Files:** Modify `internal/agent/loop/event/marshal.go` (`MarshalPermissionRequest`/unmarshal
sealed union — add the `SkillLoadRequest` case); Test: `internal/agent/loop/event/marshal_test.go`.

**Step 1 — failing test:** a `PermissionRequested{Request: SkillLoadRequest{...}}` round-trips —
**metadata + SHA-256 survive, body absent**. **Step 2-4. Commit:**
`feat(event): persist SkillLoadRequest (metadata+hash, never body)`.

### Task 4.2: fingerprint — runtime-skills mode + canonical workspace-root id

**Files:** Modify `internal/agent/loop/event/config_fingerprint.go` (+ `session/config_fingerprint.go`
`FingerprintFrom`); also populate `AgentKind` with swarm/primary identity. Test:
`config_fingerprint_test.go`.

**Step 1 — failing test:** two sessions differing only in runtime-skills mode (or workspace-root)
have **different** fingerprints; restore across a mismatch hits the reject/confirm path. **Step 2-4.
Commit:** `feat(fingerprint): add runtime-skills mode + canonical workspace-root id`.

**P2b gate:** `go test -race ./...` + tagged FS integration + `make secure` green; read-tool bypass
blocked; TOCTOU/intermediate-dir symlink rejected; workspace load is Ask-gated and `ScopeOnce`.
