# SWE Swarm — P2 Implementation Plan (Embedded Skill tool + loader)

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a `Skill` tool that loads **embedded, trusted** `SKILL.md` behavior on demand,
scoped per-agent, with a progressive-disclosure catalog in the system prompt.

**Architecture:** Skills are `//go:embed`-baked `SKILL.md` files under `swarms/swe/skills/`. A
`SkillLoader` resolves `(agent, name)` against a static per-agent allow-map, reads the embedded
body (size-capped), and parses a tiny stdlib frontmatter. The `Skill` tool is auto-approve (a
trusted, side-effect-free read). Depends on P1 (`AgentRegistry`, the agent packages).

**Tech Stack:** Go 1.26 stdlib (`embed`, no YAML dep). Design:
`docs/plans/2026-06-21-swe-swarm-roles-design.md` §7.

**Conventions:** as in `2026-06-21-swe-swarm-roles-impl.md` (TDD, `-race`, `make secure`,
no co-author trailer, green-keeping).

---

## Phase 1 — Skill content + frontmatter parser

### Task 1.1: tiny stdlib frontmatter parser + `maxSkillBytes`

**Files:** Create `tools/skill_frontmatter.go`; Test: `tools/skill_frontmatter_test.go` + `tools/skill_frontmatter_fuzz_test.go`.

- `const maxSkillBytes = 64 * 1024`.
- `parseSkill(raw []byte) (meta SkillMeta, body string, err error)` — split the leading `---`
  fences, read flat `key: value` lines for `name`/`description`; **malformed or duplicate
  frontmatter → `*MalformedSkillError`** (never partial). Over `maxSkillBytes` → error.

**Step 1 — failing tests (table):** happy path; missing fences; duplicate key; unterminated
fence; oversize; empty body. **Step 2 — red. Step 3 — implement (stdlib only). Step 4 — green.**
**Step 5 — Fuzz:** `FuzzSkillFrontmatter` (never panics; bounded). Run `go test -fuzz=FuzzSkillFrontmatter ./tools -fuzztime=30s`. **Commit:** `feat(tools): stdlib SKILL.md frontmatter parser + maxSkillBytes`.

### Task 1.2: typed skill errors

**Files:** `tools/errors.go`; Test: errors test. `UnknownSkillError`, `MalformedSkillError`
(`errors.As`-recoverable). Commit `feat(tools): typed skill errors`.

### Task 1.3: embed the skill tree

**Files:** Create `swarms/swe/skills/` with at least one real `SKILL.md` (and a
`//go:embed skills/*/SKILL.md` var in `swarms/swe/skills.go`); Test: asserts the embed FS lists
the file. Commit `feat(swarms/swe): embed skills/ tree`.

---

## Phase 2 — SkillLoader (embedded, per-agent scoped)

### Task 2.1: `SkillLoader` interface + embedded impl

**Files:** Create `tools/skill_loader.go`; Test: `tools/skill_loader_test.go`.

- Interface: `type SkillLoader interface { Load(ctx context.Context, agent identity.AgentName, name string) (string, error) }`.
- Concrete `embeddedSkillLoader` owns the `embed.FS` + a **static per-agent allow-map**
  (`map[AgentName]map[string]struct{}`). `name` only **selects** from the closed set — never
  interpolated into a path.

**Step 1 — failing tests:** embedded load succeeds; unknown/unauthorized name → `UnknownSkillError`
(fail-secure); body over `maxSkillBytes` / malformed → typed error. **Step 2-4.** **Commit:**
`feat(tools): embedded per-agent SkillLoader`.

---

## Phase 3 — `Skill` tool + catalog + wiring

### Task 3.1: `tools.Skill` tool (auto-approve, Auditable)

**Files:** Create `tools/skill.go`; Test: `tools/skill_test.go`.

- Param `{name}`; resolves against *this agent's* set via the injected `SkillLoader`; returns the
  body as the tool result; fail-secure tool-result string on unknown/unauthorized.
- `Auditable` (audit logs the **skill name only**). AutoApprove (HardApprove list — same class as
  ReadFile): add `"Skill"` to each skilled agent's `autoApprovedTools`.

**Step 1 — failing tests:** valid name → body; unknown → error string; audit summary == name.
**Step 2-4. Commit:** `feat(tools): Skill tool (auto-approve, Auditable)`.

### Task 3.2: per-agent allowed-skills on the `Agent` boundary + catalog injection

**Files:** Modify `swarms/swe/registry.go` (`Agent` gains `Skills []string`); modify each
`agents/<agent>` to declare its skills + add `Skill` to `BuildTools` **only when** `len(Skills)>0`;
modify the system-prompt assembly to inject `<available_skills>` (name+description from the loader)
when the agent has ≥1 skill (else no catalog, no `Skill` tool wired). Test:
`swarms/swe/registry_test.go` + an agent test.

**Step 1 — failing tests:** an agent with skills gets the `Skill` tool + a non-empty
`<available_skills>` block; an agent with zero skills gets neither. **Step 2-4. Commit:**
`feat(swarms/swe): per-agent allowed-skills + <available_skills> catalog injection`.

### Task 3.3: wire `Skill` into the chosen agents

**Files:** the relevant `agents/<agent>` + `swarms/swe` wiring; Test: `buildToolSet` allowlist
tests updated to expect `Skill` for skilled agents (and its auto-approve). Decide which agents get
skills (recommend: all five start with an empty set; enable per real skill). **Commit:**
`feat(swarm): wire Skill tool into skilled agents`.

**P2 gate:** `go test -race ./... && go test -fuzz=FuzzSkillFrontmatter ./tools -fuzztime=30s` +
`make secure` green; an agent loads an embedded skill end-to-end.

**Out of scope (P2b):** workspace `.skills/`, the `Preparer` seam, `SkillLoadRequest`, `os.Root`,
`.skills/**` HardDeny, `AllowsRuntimeSkills` behavior, fingerprint additions.
