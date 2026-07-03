# Open Gate / Headless Permission Mode — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Give looprig a headless/non-interactive permission posture — a consumer-declared auto-approve allowlist plus a fail-secure `NonInteractiveGate` decorator so an unattended turn never parks — and a reusable, agent-agnostic HTTP runner (`pkg/api`) that serves any session over HTTP.

**Architecture:** The native loop already depends only on the `loop.PermissionGate` interface, so the headless posture is added at the composition root with **no loop/runner change**. Phase 1 adds a fallible `NewPermissionChecker(policy, opts...)` (construction-time home resolution, fail-fast), an `Unattended` checker option (disables persisted approvals + suppresses `EffectChecker` auto-approve), the `NonInteractiveGate` decorator (`EffectAsk→EffectDeny`), the `Intent` selector, and a `NativePermissionPolicyRev` config-fingerprint field. Phase 2 adds `pkg/api`: a request-carrying `Factory`, a narrow `pkg/api.Agent` interface, SSE streaming, a per-session supervisor maintaining a pending-gate registry, and the HTTP endpoints (plain/unauthenticated v1, loopback-default).

**Tech Stack:** Go (stdlib only — no new deps), `net/http`, `crypto/sha256`, table-driven `-race` tests, `httptest`. Design source of truth: `docs/plans/2026-07-01-open-gate-posture-design.md`.

**Build/test in this worktree:** a parent `go.work` shadows the module, so **prefix every go command with `GOWORK=off`**, e.g. `GOWORK=off go test -race ./pkg/tools/...`.

**Commit convention:** conventional commits; end every commit message body with:
```
Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
```

---

# Phase 1 — Headless posture (`pkg/tools` + fingerprint plumbing)

## Task 1: `HomeUnresolvableError` typed error

**Files:**
- Modify: `pkg/tools/permission.go` (add the error type near the other types)
- Test: `pkg/tools/permission_home_test.go` (create)

**Step 1: Write the failing test**

```go
package tools

import (
	"errors"
	"testing"
)

func TestHomeUnresolvableError(t *testing.T) {
	t.Parallel()
	err := error(&HomeUnresolvableError{Cause: errors.New("no $HOME")})
	var target *HomeUnresolvableError
	if !errors.As(err, &target) {
		t.Fatalf("errors.As failed for *HomeUnresolvableError")
	}
	if target.Cause == nil {
		t.Errorf("Cause not preserved")
	}
	if got := err.Error(); got == "" {
		t.Errorf("Error() is empty")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `GOWORK=off go test ./pkg/tools/ -run TestHomeUnresolvableError -v`
Expected: FAIL — `undefined: HomeUnresolvableError`.

**Step 3: Write minimal implementation** (add to `pkg/tools/permission.go`)

```go
// HomeUnresolvableError is returned by NewPermissionChecker when the user's home
// directory cannot be resolved WHILE the policy configures a home-relative ("~/…")
// hard-deny or read-deny pattern. Such a checker cannot enforce those secret-deny
// rules (a "~/.ssh/**" glob has nothing to expand against), so construction fails
// LOUDLY rather than silently running fail-open (CLAUDE.md: fail loudly on missing/
// unresolvable required config). It is fail-secure and typed per CLAUDE.md.
type HomeUnresolvableError struct {
	// Cause is the underlying os.UserHomeDir (or injected seam) error.
	Cause error
}

func (e *HomeUnresolvableError) Error() string {
	return "tools: home directory unresolvable but a ~/ hard-deny pattern is configured: " + e.Cause.Error()
}

func (e *HomeUnresolvableError) Unwrap() error { return e.Cause }
```

**Step 4: Run test to verify it passes**

Run: `GOWORK=off go test -race ./pkg/tools/ -run TestHomeUnresolvableError -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/tools/permission.go pkg/tools/permission_home_test.go
git commit -m "$(cat <<'EOF'
feat(tools): add typed HomeUnresolvableError

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Fallible `NewPermissionChecker(policy, opts...)` + `WithHomeDir` + construction-time home resolution

This is a **breaking change** (return `*PermissionChecker` → `(*PermissionChecker, error)`; `SetHomeDir` removed in favor of `WithHomeDir`). There are **no non-test looprig callers** of `NewPermissionChecker`, so the migration is test-only within looprig; the swe consumer migrates on its next looprig upgrade (Phase 3).

**Files:**
- Modify: `pkg/tools/permission.go` (constructor signature, `Option`, `WithHomeDir`, `home` field, remove `SetHomeDir`)
- Test: `pkg/tools/permission_home_test.go`

**Step 1: Write the failing tests**

```go
func TestNewPermissionChecker_HomeUnresolvable(t *testing.T) {
	t.Parallel()
	boom := func() (string, error) { return "", errors.New("no home") }
	tests := []struct {
		name    string
		deny    HardDenyRules
		wantErr bool
	}{
		{
			name:    "read-deny ~/ pattern + unresolvable home -> construction error",
			deny:    HardDenyRules{DeniedReadPaths: []string{"~/.ssh/**"}},
			wantErr: true,
		},
		{
			name:    "write-deny ~/ pattern + unresolvable home -> construction error",
			deny:    HardDenyRules{DeniedWritePaths: []string{"~/.looprig/**"}},
			wantErr: true,
		},
		{
			name:    "no ~/ pattern + unresolvable home -> ok",
			deny:    HardDenyRules{DeniedReadPaths: []string{"**/.env"}},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, err := NewPermissionChecker(PermissionPolicy{HardDeny: tt.deny}, WithHomeDir(boom))
			var hue *HomeUnresolvableError
			if tt.wantErr {
				if !errors.As(err, &hue) {
					t.Fatalf("want *HomeUnresolvableError, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c == nil {
				t.Fatalf("nil checker on success")
			}
		})
	}
}

func TestNewPermissionChecker_HomeResolvedOnce(t *testing.T) {
	t.Parallel()
	c, err := NewPermissionChecker(
		PermissionPolicy{HardDeny: DefaultHardDeny()},
		WithHomeDir(func() (string, error) { return "/home/tester", nil }),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.home != "/home/tester" {
		t.Errorf("home = %q, want /home/tester", c.home)
	}
}
```

**Step 2: Run to verify it fails**

Run: `GOWORK=off go test ./pkg/tools/ -run TestNewPermissionChecker_Home -v`
Expected: FAIL — `NewPermissionChecker` returns one value / `WithHomeDir` undefined / no `home` field.

**Step 3: Write the implementation** (edit `pkg/tools/permission.go`)

Replace the `homeDir homeDirFunc` field on `PermissionChecker` with a resolved `home string`, delete `SetHomeDir`, and rewrite the constructor:

```go
// Option configures a PermissionChecker at construction (functional-option idiom).
type Option func(*checkerConfig)

// checkerConfig holds construction-time knobs applied by Options before the
// PermissionChecker is built. homeFn is the home-dir seam; unattended flips the
// two headless suppressions (Task 4).
type checkerConfig struct {
	homeFn      homeDirFunc
	unattended  bool
}

// WithHomeDir overrides the home-dir resolution seam at CONSTRUCTION (default
// os.UserHomeDir). It REPLACES the former post-construction SetHomeDir: home is
// resolved once, in the constructor, so an unresolvable home fails fast (a
// post-construction setter could not) and tests can force the failure.
func WithHomeDir(fn homeDirFunc) Option { return func(c *checkerConfig) { c.homeFn = fn } }

// NewPermissionChecker builds a PermissionChecker for the given policy. It resolves
// the home dir ONCE via the (optionally injected) seam; if resolution fails while
// any "~/…" hard-deny OR read-deny pattern is configured it returns a typed
// *HomeUnresolvableError (fail-fast — the checker could not enforce those secret
// globs). With no "~/…" pattern, an unresolvable home is fine (home stays "").
func NewPermissionChecker(policy PermissionPolicy, opts ...Option) (*PermissionChecker, error) {
	cfg := checkerConfig{homeFn: os.UserHomeDir}
	for _, o := range opts {
		o(&cfg)
	}
	home, herr := cfg.homeFn()
	if herr != nil {
		home = ""
		if policyHasHomePattern(policy) {
			return nil, &HomeUnresolvableError{Cause: herr}
		}
	}
	return &PermissionChecker{
		policy:                 policy,
		home:                   home,
		unattended:             cfg.unattended,
		wsCache:                hashcache.New(parseApprovalsFile),
		userCache:              hashcache.New(parseApprovalsFile),
	}, nil
}

// policyHasHomePattern reports whether any read- or write-deny glob is
// home-relative ("~/…"), i.e. requires a resolved home to enforce.
func policyHasHomePattern(policy PermissionPolicy) bool {
	for _, p := range policy.HardDeny.DeniedReadPaths {
		if strings.HasPrefix(p, "~/") {
			return true
		}
	}
	for _, p := range policy.HardDeny.DeniedWritePaths {
		if strings.HasPrefix(p, "~/") {
			return true
		}
	}
	return false
}
```

Update the `PermissionChecker` struct: replace `homeDir homeDirFunc` with `home string` and add `unattended bool` (used in Task 4). Add `"strings"` to imports. Delete `SetHomeDir`. (The `homeDirFunc` type stays — `WithHomeDir` and the constructor use it.)

**Step 4: Migrate the affected code** in the same commit:
- `DeniedRead` (Task 3 rewrites it) and any `checkPathBoundary`/`matchHardDenyAbs` call site currently using `resolveHomeOrEmpty(c.homeDir)` must use `c.home`. Grep: `GOWORK=off go vet ./pkg/tools/` and `grep -rn 'c.homeDir\|SetHomeDir\|resolveHomeOrEmpty' pkg/tools/` — fix every hit (Task 3 covers `DeniedRead`; do the Check-path site here or in Task 3, whichever the compiler points to).
- Test call sites: `grep -rln 'NewPermissionChecker(' pkg/tools/*_test.go` — update each to the two-value return (`c, err := NewPermissionChecker(policy)`; `if err != nil { t.Fatal(err) }`) and replace any `.SetHomeDir(fn)` with the `WithHomeDir(fn)` option.

**Step 5: Run**

Run: `GOWORK=off go build ./pkg/tools/ && GOWORK=off go test -race ./pkg/tools/ -run TestNewPermissionChecker_Home -v`
Expected: builds; PASS.

**Step 6: Full-package regression**

Run: `GOWORK=off go test -race ./pkg/tools/`
Expected: PASS (all migrated call sites compile + pass).

**Step 7: Commit**

```bash
git add pkg/tools/
git commit -m "$(cat <<'EOF'
feat(tools)!: make NewPermissionChecker fallible with construction-time home resolution

BREAKING CHANGE: NewPermissionChecker now returns (*PermissionChecker, error) and
SetHomeDir is replaced by the WithHomeDir option. Home is resolved once at
construction; an unresolvable home while a ~/ hard-deny pattern is configured
returns *HomeUnresolvableError (fail-fast) instead of silently failing open.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: `DeniedRead` (+ Check path) use the construction-resolved home; defensive fail-closed

**Files:**
- Modify: `pkg/tools/permission.go` (`DeniedRead`; delete/retire `resolveHomeOrEmpty`)
- Modify: `pkg/tools/check.go` (any hard-deny match that resolved home per-call)
- Test: `pkg/tools/permission_home_test.go`

**Step 1: Write the failing test**

```go
func TestDeniedRead_HomeResolvedAtConstruction(t *testing.T) {
	t.Parallel()
	c, err := NewPermissionChecker(
		PermissionPolicy{HardDeny: HardDenyRules{DeniedReadPaths: []string{"~/.ssh/**"}}},
		WithHomeDir(func() (string, error) { return "/home/tester", nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !c.DeniedRead("/home/tester/.ssh/id_rsa") {
		t.Errorf("~/.ssh/** must deny /home/tester/.ssh/id_rsa")
	}
	if c.DeniedRead("/home/tester/project/main.go") {
		t.Errorf("non-secret path must not be denied")
	}
}

func TestDeniedRead_DefensiveFailClosed_EmptyHome(t *testing.T) {
	t.Parallel()
	// Constructed with a NON-home policy so construction succeeds with home="",
	// then a ~/ read pattern is present -> defensive deny (backstop; should not
	// occur post-construction, but must fail closed if it ever does).
	c, err := NewPermissionChecker(
		PermissionPolicy{HardDeny: HardDenyRules{DeniedReadPaths: []string{"**/.env"}}},
		WithHomeDir(func() (string, error) { return "", errors.New("no home") }),
	)
	if err != nil {
		t.Fatal(err)
	}
	c.policy.HardDeny.DeniedReadPaths = append(c.policy.HardDeny.DeniedReadPaths, "~/.ssh/**")
	if !c.DeniedRead("/anything/.ssh/id_rsa") {
		t.Errorf("empty home + ~/ pattern must fail CLOSED (deny)")
	}
}
```

**Step 2: Run to verify it fails**

Run: `GOWORK=off go test ./pkg/tools/ -run TestDeniedRead -v`
Expected: FAIL (still using `resolveHomeOrEmpty` / fail-open on empty home).

**Step 3: Implementation** — rewrite `DeniedRead`:

```go
func (c *PermissionChecker) DeniedRead(absPath string) bool {
	c.mu.Lock()
	denied := c.policy.HardDeny.DeniedReadPaths
	home := c.home
	c.mu.Unlock()

	for _, pat := range denied {
		if strings.HasPrefix(pat, "~/") && home == "" {
			// Defensive backstop: a ~/ pattern with no resolved home cannot be
			// matched, so fail CLOSED (deny) rather than no-match (fail-open).
			// Construction (Task 2) normally prevents this state.
			return true
		}
		if matchHardDenyAbs(pat, absPath, home) {
			return true
		}
	}
	return false
}
```

Delete `resolveHomeOrEmpty` (now unused). In `check.go`, replace any `resolveHomeOrEmpty(c.homeDir)` used for hard-deny matching with `c.home`, and apply the same defensive `~/`-with-empty-home deny in the path-boundary matcher. Run `GOWORK=off go build ./pkg/tools/` and fix every compiler error the removal surfaces.

**Step 4: Run**

Run: `GOWORK=off go test -race ./pkg/tools/ -run TestDeniedRead -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/tools/
git commit -m "$(cat <<'EOF'
fix(tools): DeniedRead uses construction-resolved home, fails closed on empty home

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: `WithUnattended()` — disable persisted approvals + suppress EffectChecker auto-approve

**Files:**
- Modify: `pkg/tools/permission.go` (`WithUnattended`, set `cfg.unattended`)
- Modify: `pkg/tools/check.go` (`Check`: gate Stage 3 auto-approve + skip Stage 5)
- Test: `pkg/tools/check_unattended_test.go` (create)

**Step 1: Write the failing tests**

```go
package tools

import (
	"context"
	"testing"

	"github.com/looprig/harness/pkg/loop"
)

// selfApprovingTool is an InvokableTool that also implements EffectChecker and
// pins EffectAutoApprove for every call. (Use the package's existing test tool
// helpers for Info/Invoke; only CheckEffect matters here.)
type selfApprovingTool struct{ effect loop.Effect }

func (selfApprovingTool) /* ...InvokableTool methods per the package test helpers... */

func (s selfApprovingTool) CheckEffect(string) (loop.Effect, bool) { return s.effect, true }

func TestUnattended_SuppressesEffectCheckerAutoApprove(t *testing.T) {
	t.Parallel()
	tool := selfApprovingTool{effect: loop.EffectAutoApprove}
	// Not on HardApprove.Tools -> under Unattended it must NOT auto-approve;
	// it falls through to Stage 7 -> EffectAsk.
	c, err := NewPermissionChecker(PermissionPolicy{}, WithUnattended())
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Check(context.Background(), tool, "SelfApprove", "{}"); got == loop.EffectAutoApprove {
		t.Errorf("Unattended must not honor EffectChecker auto-approve; got %v", got)
	}
	// A plain checker DOES honor it (proves the suppression is option-scoped).
	plain, _ := NewPermissionChecker(PermissionPolicy{})
	if got := plain.Check(context.Background(), tool, "SelfApprove", "{}"); got != loop.EffectAutoApprove {
		t.Errorf("interactive checker should honor EffectChecker auto-approve; got %v", got)
	}
}

func TestUnattended_HonorsEffectCheckerDeny(t *testing.T) {
	t.Parallel()
	tool := selfApprovingTool{effect: loop.EffectDeny}
	c, _ := NewPermissionChecker(PermissionPolicy{}, WithUnattended())
	if got := c.Check(context.Background(), tool, "SelfDeny", "{}"); got != loop.EffectDeny {
		t.Errorf("EffectChecker deny must still be honored under Unattended; got %v", got)
	}
}
```

(Reuse the package's existing fake-tool helpers from `check_test.go` for the `InvokableTool` methods — read that file for the pattern. A persisted-approvals test is added in Task 8 with the store fixture.)

**Step 2: Run to verify it fails**

Run: `GOWORK=off go test ./pkg/tools/ -run TestUnattended -v`
Expected: FAIL — `WithUnattended` undefined / auto-approve still honored.

**Step 3: Implementation**

In `permission.go`:
```go
// WithUnattended puts the checker in the headless posture: it (1) does NOT honor a
// Stage-3 EffectChecker EffectAutoApprove (the call falls through to the allowlist
// stages, so a tool cannot self-approve ahead of the definer's declared allowlist)
// and (2) skips Stage-5 persisted approvals (a stale ~/.looprig grant can never
// auto-approve a call the definer did not declare). EffectChecker EffectDeny is
// still honored (a safety veto). Pair with NonInteractiveGate (Task 5).
func WithUnattended() Option { return func(c *checkerConfig) { c.unattended = true } }
```

In `check.go` `Check`, change Stage 3 and Stage 5:
```go
	// Stage 3: EffectChecker (an explicit per-call override from the tool).
	// Under the Unattended posture an EffectAutoApprove is NOT honored (it would
	// bypass the declared allowlist); the call falls through. Deny/Ask are honored.
	if eff, handled := stageEffectChecker(t, argsJSON); handled &&
		!(c.unattended && eff == loop.EffectAutoApprove) {
		return eff
	}

	// Stage 4: operator always-allow.
	if c.stageHardApprove(toolName) {
		return loop.EffectAutoApprove
	}

	// Stage 5: persisted approvals — SKIPPED under the Unattended posture so only
	// the definer's declared allowlist (Stage 4/6) can approve.
	if !c.unattended {
		if eff, decided := c.stagePersistedApprovals(ctx, toolName, class, argsJSON); decided {
			return eff
		}
	}
```

**Step 4: Run**

Run: `GOWORK=off go test -race ./pkg/tools/ -run TestUnattended -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/tools/
git commit -m "$(cat <<'EOF'
feat(tools): add WithUnattended checker option (no persisted, no self-approve)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: `NonInteractiveGate` decorator

**Files:**
- Create: `pkg/tools/noninteractive.go`
- Test: `pkg/tools/noninteractive_test.go`

**Step 1: Write the failing test**

```go
package tools

import (
	"context"
	"testing"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

type stubGate struct {
	effect    loop.Effect
	grantErr  error
	grantSeen bool
}

func (s stubGate) Check(context.Context, tool.InvokableTool, string, string) loop.Effect {
	return s.effect
}
func (s *stubGate) Grant(context.Context, string, string, tool.ApprovalScope) error {
	s.grantSeen = true
	return s.grantErr
}

func TestNonInteractiveGate_Check(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   loop.Effect
		want loop.Effect
	}{
		{"ask becomes deny", loop.EffectAsk, loop.EffectDeny},
		{"auto-approve passes through", loop.EffectAutoApprove, loop.EffectAutoApprove},
		{"deny passes through", loop.EffectDeny, loop.EffectDeny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := NonInteractiveGate{Inner: stubGate{effect: tt.in}}
			if got := g.Check(context.Background(), nil, "X", "{}"); got != tt.want {
				t.Errorf("Check(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestNonInteractiveGate_GrantPassThrough(t *testing.T) {
	t.Parallel()
	inner := &stubGate{}
	g := NonInteractiveGate{Inner: inner}
	if err := g.Grant(context.Background(), "X", "{}", tool.ApprovalScope(0)); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if !inner.grantSeen {
		t.Errorf("Grant did not delegate to inner")
	}
}
```

**Step 2: Run to verify it fails**

Run: `GOWORK=off go test ./pkg/tools/ -run TestNonInteractiveGate -v`
Expected: FAIL — `NonInteractiveGate` undefined.

**Step 3: Implementation** (`pkg/tools/noninteractive.go`)

```go
package tools

import (
	"context"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// NonInteractiveGate makes any loop.PermissionGate safe to run with no human
// present: a would-be prompt (EffectAsk) becomes a fail-secure EffectDeny, so an
// unattended turn continues with a denied tool result and NEVER parks.
// EffectAutoApprove and EffectDeny pass through unchanged, so the inner gate's
// declared allowlist and the non-bypassable safety floor are fully honored. Wrap
// only for the Unattended posture (pair with the WithUnattended checker option).
// It is Open/Closed — it wraps, it does not modify, the checker.
type NonInteractiveGate struct {
	// Inner is the wrapped gate (typically a *PermissionChecker built WithUnattended).
	Inner loop.PermissionGate
}

var _ loop.PermissionGate = NonInteractiveGate{}

func (g NonInteractiveGate) Check(ctx context.Context, t tool.InvokableTool, name, argsJSON string) loop.Effect {
	if e := g.Inner.Check(ctx, t, name, argsJSON); e != loop.EffectAsk {
		return e // EffectAutoApprove / EffectDeny pass through
	}
	return loop.EffectDeny // fail-secure: no one to ask
}

func (g NonInteractiveGate) Grant(ctx context.Context, name, argsJSON string, scope tool.ApprovalScope) error {
	return g.Inner.Grant(ctx, name, argsJSON, scope) // pass-through; no gate is ever opened
}
```

**Step 4: Run**

Run: `GOWORK=off go test -race ./pkg/tools/ -run TestNonInteractiveGate -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/tools/noninteractive.go pkg/tools/noninteractive_test.go
git commit -m "$(cat <<'EOF'
feat(tools): add NonInteractiveGate decorator (Ask->Deny fail-secure)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: `Intent` selector + `Wrap` helper

A lightweight composition-root selector (spec §7). Placed in `pkg/tools` (composition roots already import it to build the checker); `loop`/`session` never reference it.

**Files:**
- Create: `pkg/tools/intent.go`
- Test: `pkg/tools/intent_test.go`

**Step 1: Write the failing test**

```go
package tools

import (
	"testing"

	"github.com/looprig/harness/pkg/loop"
)

func TestIntent_Wrap(t *testing.T) {
	t.Parallel()
	inner := stubGate{effect: loop.EffectAsk}
	// Interactive: no wrapping — returns inner unchanged.
	if got := Interactive.Wrap(inner); got != loop.PermissionGate(inner) {
		t.Errorf("Interactive.Wrap must return inner unchanged")
	}
	// Unattended: wraps with NonInteractiveGate.
	if _, ok := Unattended.Wrap(inner).(NonInteractiveGate); !ok {
		t.Errorf("Unattended.Wrap must return a NonInteractiveGate")
	}
}
```

**Step 2: Run to verify it fails**

Run: `GOWORK=off go test ./pkg/tools/ -run TestIntent -v`
Expected: FAIL — `Interactive`/`Unattended`/`Wrap` undefined.

**Step 3: Implementation** (`pkg/tools/intent.go`)

```go
package tools

import "github.com/looprig/harness/pkg/loop"

// Intent is the composition-root selector for how autonomous tool approval is.
// It is NOT session state — neither loop.Config nor Session stores it; the
// composition root reads it to decide the permission wiring and then discards it.
// Zero value is Interactive (fail-secure: a human answers gates by default).
type Intent uint8

const (
	// Interactive: the full permission pipeline; gates fire and a human answers.
	Interactive Intent = iota
	// Unattended: no permission prompt; the declared allowlist approves and the
	// rest deny fail-secure. Build the inner checker WithUnattended() and Wrap it.
	Unattended
)

// Wrap returns the permission gate for this intent: Interactive returns inner
// unchanged; Unattended wraps it in a NonInteractiveGate. The caller is
// responsible for building `inner` WithUnattended() for the Unattended intent.
func (i Intent) Wrap(inner loop.PermissionGate) loop.PermissionGate {
	if i == Unattended {
		return NonInteractiveGate{Inner: inner}
	}
	return inner
}
```

**Step 4: Run**

Run: `GOWORK=off go test -race ./pkg/tools/ -run TestIntent -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/tools/intent.go pkg/tools/intent_test.go
git commit -m "$(cat <<'EOF'
feat(tools): add Intent selector + Wrap helper

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: `NativePermissionPolicyRev` — digest helper + fingerprint field + plumbing

**Files:**
- Create: `pkg/tools/policyrev.go` (canonical digest helper) + test
- Modify: `pkg/event/config_fingerprint.go` (add field + `Equal`)
- Modify: `pkg/session/config_fingerprint.go` (add to `ConfigFingerprintFields` + `fingerprintWith`)
- Tests: `pkg/tools/policyrev_test.go`, `pkg/event/config_fingerprint_test.go`

**Step 1 (digest helper): failing test** (`pkg/tools/policyrev_test.go`)

```go
package tools

import "testing"

func TestPolicyFingerprint_StableAndSensitive(t *testing.T) {
	t.Parallel()
	base := PermissionPolicy{
		HardApprove: HardApproveRules{Tools: []string{"ReadFile", "Glob"}},
		HardDeny:    DefaultHardDeny(),
	}
	m := FingerprintMode{Unattended: true, Wrapped: true}

	a := PolicyFingerprint(base, m)
	// Reordering HardApprove.Tools must NOT change the digest (canonical sort).
	reordered := base
	reordered.HardApprove.Tools = []string{"Glob", "ReadFile"}
	if PolicyFingerprint(reordered, m) != a {
		t.Errorf("digest must be order-insensitive for HardApprove.Tools")
	}
	// Adding a tool MUST change it.
	more := base
	more.HardApprove.Tools = []string{"ReadFile", "Glob", "Bash"}
	if PolicyFingerprint(more, m) == a {
		t.Errorf("digest must change when the allowlist changes")
	}
	// Flipping a mode bit MUST change it.
	if PolicyFingerprint(base, FingerprintMode{Unattended: false, Wrapped: true}) == a {
		t.Errorf("digest must change when the Unattended bit changes")
	}
}
```

**Step 2: Run → fails** (`PolicyFingerprint`/`FingerprintMode` undefined).
Run: `GOWORK=off go test ./pkg/tools/ -run TestPolicyFingerprint -v`

**Step 3: Implementation** (`pkg/tools/policyrev.go`)

```go
package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"

	"github.com/looprig/harness/pkg/loop"
)

// policySchemaVersion is bumped whenever the fingerprint input shape changes, so
// digests computed by different schema versions never compare equal by accident.
const policySchemaVersion = 1

// FingerprintMode carries the headless mode bits that affect the effective
// permission decision but are not on PermissionPolicy: whether the gate is
// NonInteractiveGate-wrapped and whether the checker was built WithUnattended.
type FingerprintMode struct {
	Wrapped    bool
	Unattended bool
}

// PolicyFingerprint returns a canonical, deterministic hex-sha256 digest over the
// effective native permission configuration, for event.ConfigFingerprint.
// NativePermissionPolicyRev (so a durable session cannot silently restore under a
// changed allowlist or posture). Inputs are sorted/canonicalized so semantically
// identical configs digest equally regardless of source ordering.
func PolicyFingerprint(policy PermissionPolicy, mode FingerprintMode) string {
	var b strings.Builder
	b.WriteString("v")
	b.WriteString(strconv.Itoa(policySchemaVersion))
	b.WriteString("\nwrapped=")
	b.WriteString(strconv.FormatBool(mode.Wrapped))
	b.WriteString("\nunattended=")
	b.WriteString(strconv.FormatBool(mode.Unattended))
	writeSorted(&b, "hardApprove", policy.HardApprove.Tools)
	writePolicies(&b, policy.Policies)
	writeSorted(&b, "denyRead", policy.HardDeny.DeniedReadPaths)
	writeSorted(&b, "denyWrite", policy.HardDeny.DeniedWritePaths)
	writeSorted(&b, "denyBash", policy.HardDeny.DeniedBashPrefixes)
	b.WriteString("\nmaxRead=")
	b.WriteString(strconv.FormatInt(policy.HardDeny.MaxReadBytes, 10))
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func writeSorted(b *strings.Builder, label string, in []string) {
	s := slices.Clone(in)
	slices.Sort(s)
	b.WriteString("\n")
	b.WriteString(label)
	b.WriteString("=")
	b.WriteString(strings.Join(s, ","))
}

func writePolicies(b *strings.Builder, in []loop.ToolPolicy) {
	lines := make([]string, 0, len(in))
	for _, p := range in {
		m := slices.Clone(p.Match)
		slices.Sort(m)
		lines = append(lines, p.Tool+"|"+strconv.Itoa(int(p.Effect))+"|"+strings.Join(m, ";"))
	}
	slices.Sort(lines)
	b.WriteString("\npolicies=")
	b.WriteString(strings.Join(lines, ","))
}
```

**Step 4: Run → PASS.** `GOWORK=off go test -race ./pkg/tools/ -run TestPolicyFingerprint -v`

**Step 5 (event field): failing test** (`pkg/event/config_fingerprint_test.go`)

```go
func TestConfigFingerprint_NativePermissionPolicyRev(t *testing.T) {
	t.Parallel()
	a := ConfigFingerprint{NativePermissionPolicyRev: "aaa"}
	b := ConfigFingerprint{NativePermissionPolicyRev: "bbb"}
	if a.Equal(b) {
		t.Errorf("different NativePermissionPolicyRev must not be Equal")
	}
	if !a.Equal(ConfigFingerprint{NativePermissionPolicyRev: "aaa"}) {
		t.Errorf("same NativePermissionPolicyRev must be Equal")
	}
	// Additive/omitzero: an old record (empty) equals a current record that also leaves it empty.
	if !(ConfigFingerprint{}).Equal(ConfigFingerprint{}) {
		t.Errorf("empty fingerprints must be Equal")
	}
}
```

Run → FAIL (`unknown field NativePermissionPolicyRev`).

**Step 6: Implementation** — add to `event.ConfigFingerprint`:
```go
	// NativePermissionPolicyRev is a content digest (hex sha256) of the NATIVE
	// permission configuration (allowlist + hard-deny lists + MaxReadBytes + the
	// headless mode bits), computed by tools.PolicyFingerprint at the composition
	// root and injected. Empty for a foreign session (which uses PermissionPosture)
	// or a caller that does not inject it. A change is a behavior change that must
	// not resume unnoticed.
	NativePermissionPolicyRev string `json:"native_permission_policy_rev,omitzero"`
```
and to `Equal`:
```go
		f.PermissionPosture == other.PermissionPosture &&
		f.NativePermissionPolicyRev == other.NativePermissionPolicyRev
```

Run → PASS: `GOWORK=off go test -race ./pkg/event/ -run TestConfigFingerprint_Native -v`

**Step 7: Plumb through session** — add `NativePermissionPolicyRev string` to `ConfigFingerprintFields` (session) and set it in `fingerprintWith`:
```go
	fpr.NativePermissionPolicyRev = fields.NativePermissionPolicyRev
```
Add a session test asserting a restore mismatch when `NativePermissionPolicyRev` differs (mirror the existing fingerprint-mismatch test in `pkg/session`; read it for the pattern), then:
Run: `GOWORK=off go test -race ./pkg/event/ ./pkg/session/`
Expected: PASS.

**Step 8: Commit**

```bash
git add pkg/tools/policyrev.go pkg/tools/policyrev_test.go pkg/event/ pkg/session/config_fingerprint.go pkg/session/*_test.go
git commit -m "$(cat <<'EOF'
feat(event,tools): fingerprint native permission policy for restore safety

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: Phase-1 integration test matrix (spec §6)

**Files:** `pkg/tools/headless_integration_test.go` (in-package `tools` test).

Add table-driven `-race` tests wiring `NonInteractiveGate{Inner: checker(WithUnattended())}` and asserting the full spec §6 matrix (reuse `check_test.go` fake-tool + approvals-store fixtures — read that file first):
- **Floor still denies** even when the tool is on `HardApprove.Tools`: `~/.ssh/**`, `**/.env`, `**/*.pem`, `~/.looprig/**`, `**/id_rsa`, `**/.skills/**`, workspace-escape path, and Bash `sudo`/`rm -rf /`/`curl | bash`/`dd if=` → `EffectDeny`.
- **Allowlisted** (HardApprove or a Stage-6 `Policies` `EffectAutoApprove` match) → `EffectAutoApprove`.
- **Non-allowlisted** (would be Stage-7 Ask) → `EffectDeny` via the decorator (never parks).
- **Persisted ignored under Unattended**: a workspace/user approvals file with an ALLOW record for a non-allowlisted tool does NOT auto-approve under Unattended (Stage 5 skipped); the SAME record DOES auto-approve on an interactive checker.
- **Deny-write globs**: `**/.git/config`, `**/go.sum`, `**/.looprig/**`, `~/.looprig/**` → `EffectDeny` even when WriteFile/EditFile is allowlisted.
- **ReadGuard preserved**: with the inner checker as `ReadGuard`, `DeniedRead` still denies `~/.ssh/id_rsa`, `**/.env`.

Run: `GOWORK=off go test -race ./pkg/tools/`
Expected: PASS. Commit:

```bash
git add pkg/tools/headless_integration_test.go
git commit -m "$(cat <<'EOF'
test(tools): headless posture integration matrix (floor/allowlist/persisted/deny-write)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: Phase-1 gate — full build + race + lint

Run:
```bash
GOWORK=off go build ./...
GOWORK=off go test -race ./pkg/tools/ ./pkg/event/ ./pkg/session/
GOWORK=off go vet ./pkg/tools/ ./pkg/event/ ./pkg/session/
gofmt -l pkg/tools pkg/event/config_fingerprint.go pkg/session/config_fingerprint.go
```
Expected: builds; all tests PASS under `-race`; vet clean; `gofmt -l` prints nothing. (If `make secure` is wired, run it.) Fix anything, then this phase is done.

---

# Phase 2 — `pkg/api` (reusable, agent-agnostic HTTP runner)

All tasks are `httptest`-driven, stdlib `net/http` only, plain/unauthenticated v1, loopback-default. Read the real `pkg/session` method signatures before coding each handler (`Submit`, `SubscribeEvents`, `Approve`, `Deny`, `ProvideUserInput`, `Interrupt`, `Shutdown`, `PrimaryLoopID`, and the export seam `ExportSource` on the agent) so the plumbing matches; the narrow interface below is what `pkg/api` depends on.

## Task 10: `pkg/api.Agent` interface + `AgentRequest` + `Factory` + `Config`

**Files:** `pkg/api/api.go` (create) + `pkg/api/api_test.go`.

Define (complete code):
```go
package api

import (
	"context"
	"time"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/transcript"
	"github.com/looprig/harness/pkg/uuid"
)

// Agent is the narrow surface the HTTP runner drives (Interface Segregation: a
// subset of tui.Agent minus the TUI-only AcceptsImages/ReplayBacklog). Existing
// agents satisfy it structurally.
type Agent interface {
	Submit(ctx context.Context, blocks []content.Block) (uuid.UUID, error)
	PrimaryLoopID() uuid.UUID
	Subscribe(filter event.EventFilter) (event.Subscription, error)
	Approve(ctx context.Context, loopID, callID uuid.UUID, scope tool.ApprovalScope) error
	Deny(ctx context.Context, loopID, callID uuid.UUID) error
	ProvideAnswer(ctx context.Context, loopID, callID uuid.UUID, answer string) error
	Interrupt(ctx context.Context) (bool, error)
	Close(ctx context.Context) error
	ExportSource(ctx context.Context) (transcript.RecordSource, transcript.SystemPromptResolver, error)
}

// AgentRequest tells the Factory which session to build and whether to create or
// resume it. The runner mints (or, for resume, is given) the SessionID.
type AgentRequest struct {
	SessionID uuid.UUID
	Resume    bool
}

// Factory builds a driven Agent for one session. The consumer owns the agent's
// composition, policy, and PERSISTENCE (session.New WithSessionID for create;
// Restore for resume). pkg/api owns the HTTP surface, the many-session map, SSE,
// and gate routing — never policy or credentials.
type Factory func(ctx context.Context, req AgentRequest) (Agent, error)

// Config configures the HTTP server. Zero value binds to loopback.
type Config struct {
	// Addr is the listen address. Empty => "127.0.0.1:0" (loopback, ephemeral
	// port). Binding to a non-loopback host requires AllowPublic=true.
	Addr string
	// AllowPublic must be set explicitly to bind a non-loopback interface; plain
	// v1 endpoints expose autonomous execution, so public exposure is opt-in.
	AllowPublic bool
	ReadTimeout, WriteTimeout, IdleTimeout time.Duration
	MaxHeaderBytes int
	MaxBodyBytes   int64
}
```

Test: `TestConfig_LoopbackDefault` (empty Addr resolves to a loopback bind), `TestConfig_PublicRequiresOptIn` (a non-loopback Addr without `AllowPublic` is rejected by the address-guard helper). Commit `feat(api): agent interface, factory, config`.

## Task 11: pending-gate **registry** + per-session supervisor

**Files:** `pkg/api/supervisor.go` + test.

The supervisor opens the agent's whole-session `Subscribe` and maintains a registry `map[uuid.UUID]pendingGate` where `pendingGate = {LoopID uuid.UUID; Kind string /* "permission"|"user-input" */; Prompt string}`, keyed by `ToolExecutionID`, populated from `event.PermissionRequested`/`event.UserInputRequested` (LoopID from the event `Header`/`Coordinates`), and dropped on the resolving/terminal event. Mutex-guarded; independent of any client SSE stream.

Test (fake Agent emitting a `UserInputRequested` on its Subscribe channel): assert the registry records `{loopID, kind, prompt}` and that a lookup returns the LoopID even with **no** SSE client attached; assert removal after resolution. Commit `feat(api): per-session supervisor + pending-gate registry`.

## Task 12: `Handler(cfg, f) http.Handler` + `Serve` + routing skeleton

**Files:** `pkg/api/server.go` + test.

`Handler` builds an `*http.ServeMux` wiring all routes (Task 13) and returns it (for consumer-owned middleware wrapping). `Serve(ctx, cfg, f)` constructs a hardened `*http.Server` (explicit `ReadTimeout`/`WriteTimeout`/`IdleTimeout`, `MaxHeaderBytes`, `TLSConfig{MinVersion: tls.VersionTLS12}` when TLS is used), binds per `Config` (loopback-default, `AllowPublic` guard), and serves until ctx is cancelled (graceful `Shutdown`). Bodies are limited via `http.MaxBytesReader`. `TestServe_HealthzOK` via `httptest.NewServer(Handler(cfg, fakeFactory))`. Commit `feat(api): Handler + hardened Serve`.

## Task 13: endpoints (one bite-sized task each, `httptest`-driven)

For each: write the handler + a `httptest` test asserting status + body + the mapped `Agent`/supervisor call; commit per endpoint.

- **`POST /sessions`** (`?resume=<sid>`): mint sid (or parse `resume`), call `Factory(ctx, AgentRequest{SessionID, Resume})`, register the Agent + start its supervisor (Task 11), return `{ "sid": "…" }`. Test: create returns a new sid; `?resume=<sid>` passes `Resume:true`.
- **`POST /sessions/{sid}/input`**: decode `{ "blocks": [...] }` (size-limited), `Submit` → return `{ "input_id": "…" }`. Test: returns the InputID.
- **`GET /sessions/{sid}/events`** (SSE): set `Content-Type: text/event-stream`, `Subscribe`, stream each `event.Event` as one `data:` frame, flush per event; close on ctx/stream end. Test: a submitted turn's events arrive; correlate `Header.Cause.CommandID` to the InputID; drain to a terminal event.
- **`POST /sessions/{sid}/gates/{toolExecutionID}`**: decode `{ "action": "approve|deny|answer", "answer": "…", "scope": … }`; resolve `LoopID` from the supervisor registry; dispatch `Approve`/`Deny`/`ProvideAnswer`. Test: an answer routes with the registry's LoopID (no SSE client attached); unknown toolExecutionID → 404.
- **`GET /sessions/{sid}/gates`**: return the registry's open gates `[{toolExecutionID, kind, prompt, loopID}]`. Test: after a `UserInputRequested`, a reconnecting client lists it and then answers it (reconnect discovery).
- **`POST /sessions/{sid}/interrupt`**: `Interrupt` → `{ "interrupted": bool }`. Test.
- **`GET /sessions/{sid}/export`**: `ExportSource` → `transcript.Reconstruct` + `html.Render` → `text/html`; `errors.As(*journalsource.ExportUnavailableError)` → 4xx. Test: durable → HTML; nop → 4xx.
- **`DELETE /sessions/{sid}`**: `Close` + deregister + stop supervisor → 204. Test.
- **`GET /healthz`**: 200, data-free, the one explicitly-unauthenticated route. Test.

## Task 14: Phase-2 gate

Run `GOWORK=off go test -race ./pkg/api/`, `GOWORK=off go vet ./pkg/api/`, `gofmt -l pkg/api`. All green. Commit any fixups.

---

# Phase 3 — swe consumer (OUT of looprig scope)

Not implemented here (different repo, `github.com/looprig/swe`). On its next looprig upgrade swe must: (1) migrate its `NewPermissionChecker(policy)` call site (`swarms/swe/swarm.go`) to the fallible `NewPermissionChecker(policy, opts...)` (handle the error; use `WithHomeDir`/`WithUnattended` as needed); (2) add a `cmd/swe-serve` that builds each agent with the `Unattended` intent + its declared `PermissionPolicy` allowlist, wraps the gate via `tools.Unattended.Wrap`, injects the `NativePermissionPolicyRev` fingerprint field, and passes an `api.Factory` to `api.Serve`; (3) own credentials, auth (front the `http.Handler`), and deployment.
