package tools

import (
	"context"
	"testing"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// posture_test.go exercises the posture-driven auto-approve stage (Stage 6.5) and
// its guarantee interlock (SPEC §10.2/§10.3). The safety property under test: an
// auto-approve posture only ever upgrades a call to AutoApprove when the held
// runner ACTUALLY enforces the required guarantees (fail-closed on no runner / no
// GuaranteeBits / a missing bit), never overrides a hard-deny, and never
// auto-approves a grant-carrying call.

// Arbitrary guarantee bits for the tests. Harness is mode-agnostic: the exact bit
// positions are the consumer's concern; the checker only computes bits&req==req.
const (
	gA uint64 = 1 << 0
	gB uint64 = 1 << 1
	gC uint64 = 1 << 2
)

// fakeGuaranteeRunner is a tool.CommandRunner that ALSO exposes GuaranteeBits and
// Level — the structural capabilities the interlock probes for.
type fakeGuaranteeRunner struct {
	bits  uint64
	level uint8
}

func (f *fakeGuaranteeRunner) RunCommand(context.Context, string, string) ([]byte, int, error) {
	return nil, 0, nil
}
func (f *fakeGuaranteeRunner) GuaranteeBits() uint64 { return f.bits }
func (f *fakeGuaranteeRunner) Level() uint8          { return f.level }

// fakeBitsOnlyRunner exposes GuaranteeBits but NOT Level (proves a missing Level
// probe fails closed when a RequiredLevel floor is set).
type fakeBitsOnlyRunner struct{ bits uint64 }

func (f *fakeBitsOnlyRunner) RunCommand(context.Context, string, string) ([]byte, int, error) {
	return nil, 0, nil
}
func (f *fakeBitsOnlyRunner) GuaranteeBits() uint64 { return f.bits }

// fakeCeiling is a CeilingSource whose ordinal the test mutates between Checks.
type fakeCeiling struct{ cur uint8 }

func (f *fakeCeiling) Current() uint8 { return f.cur }

var (
	_ tool.CommandRunner = (*fakeGuaranteeRunner)(nil)
	_ tool.CommandRunner = (*fakeBitsOnlyRunner)(nil)
	_ CeilingSource      = (*fakeCeiling)(nil)
)

// newPostureChecker builds a hermetic checker (temp home so Stage 5 never reads
// the real ~/.looprig) with the given policy and options.
func newPostureChecker(t *testing.T, policy PermissionPolicy, opts ...Option) *PermissionChecker {
	t.Helper()
	home := t.TempDir()
	all := append([]Option{WithHomeDir(func() (string, error) { return home, nil })}, opts...)
	c, err := NewPermissionChecker(policy, all...)
	if err != nil {
		t.Fatalf("NewPermissionChecker: %v", err)
	}
	return c
}

// TestPostureAutoApproveBashInterlock: Bash auto-approves under an AutoApproveBash
// posture ONLY when the runner's guarantee bits satisfy RequiredGuarantees.
func TestPostureAutoApproveBashInterlock(t *testing.T) {
	t.Parallel()
	required := gA | gB | gC
	tests := []struct {
		name string
		bits uint64
		want loop.Effect
	}{
		{name: "all required bits set -> approve", bits: gA | gB | gC, want: loop.EffectAutoApprove},
		{name: "superset of required -> approve", bits: gA | gB | gC | (1 << 5), want: loop.EffectAutoApprove},
		{name: "missing one required bit -> ask", bits: gA | gB, want: loop.EffectAsk},
		{name: "zero bits -> ask", bits: 0, want: loop.EffectAsk},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runner := &fakeGuaranteeRunner{bits: tt.bits}
			posture := Posture{AutoApproveBash: true, RequiredGuarantees: required, GrantCarryingAlwaysAsk: true}
			c := newPostureChecker(t, PermissionPolicy{}, WithPosture(posture, runner))
			got := c.Check(context.Background(), plainTool{name: toolBash}, toolBash, `{"command":"echo hi"}`)
			if got != tt.want {
				t.Errorf("Check() = %v, want %v (bits=%b required=%b)", got, tt.want, tt.bits, required)
			}
		})
	}
}

// TestPostureBashFailClosed: a nil runner OR a runner without GuaranteeBits, under
// a trusted-like posture that requires guarantees, degrades to Ask (fail-closed).
func TestPostureBashFailClosed(t *testing.T) {
	t.Parallel()
	posture := Posture{AutoApproveBash: true, RequiredGuarantees: gA | gB, GrantCarryingAlwaysAsk: true}
	tests := []struct {
		name   string
		runner tool.CommandRunner
	}{
		{name: "nil runner", runner: nil},
		{name: "runner without GuaranteeBits", runner: &fakeCommandRunner{}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newPostureChecker(t, PermissionPolicy{}, WithPosture(posture, tt.runner))
			got := c.Check(context.Background(), plainTool{name: toolBash}, toolBash, `{"command":"echo hi"}`)
			if got != loop.EffectAsk {
				t.Errorf("Check() = %v, want EffectAsk (fail-closed: no enforceable guarantees)", got)
			}
		})
	}
}

// TestPostureAutoApproveEdits: file-edit/write tools auto-approve under
// AutoApproveEdits (no interlock — edits are guarded by write-containment +
// ReadGuard, not the runner); a read tool is never edit-approved; without the flag
// a write tool falls to Ask.
func TestPostureAutoApproveEdits(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	tests := []struct {
		name     string
		edits    bool
		toolName string
		args     string
		want     loop.Effect
	}{
		{name: "WriteFile approves", edits: true, toolName: toolWriteFile, args: `{"path":"a.txt"}`, want: loop.EffectAutoApprove},
		{name: "EditFile approves", edits: true, toolName: toolEditFile, args: `{"path":"a.txt"}`, want: loop.EffectAutoApprove},
		{name: "ReadFile not edit-approved", edits: true, toolName: toolReadFile, args: `{"path":"a.txt"}`, want: loop.EffectAsk},
		{name: "flag off -> ask", edits: false, toolName: toolWriteFile, args: `{"path":"a.txt"}`, want: loop.EffectAsk},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			posture := Posture{AutoApproveEdits: tt.edits}
			c := newPostureChecker(t, PermissionPolicy{WorkspaceRoot: ws}, WithPosture(posture, &fakeGuaranteeRunner{}))
			got := c.Check(context.Background(), plainTool{name: tt.toolName}, tt.toolName, tt.args)
			if got != tt.want {
				t.Errorf("Check() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPostureGrantCarryingAlwaysAsk: a grant-carrying call (non-empty top-level
// "grants" array) is never auto-approved by posture, even when the interlock would
// otherwise pass; an empty/absent grants field does not block auto-approve.
func TestPostureGrantCarryingAlwaysAsk(t *testing.T) {
	t.Parallel()
	// RequiredGuarantees 0 => interlock trivially passes, isolating the grant gate.
	posture := Posture{AutoApproveBash: true, RequiredGuarantees: 0, GrantCarryingAlwaysAsk: true}
	tests := []struct {
		name string
		args string
		want loop.Effect
	}{
		{name: "grant-carrying -> ask", args: `{"command":"ls","grants":["tok"]}`, want: loop.EffectAsk},
		{name: "no grants -> approve", args: `{"command":"ls"}`, want: loop.EffectAutoApprove},
		{name: "empty grants -> approve", args: `{"command":"ls","grants":[]}`, want: loop.EffectAutoApprove},
		{name: "null grants -> approve", args: `{"command":"ls","grants":null}`, want: loop.EffectAutoApprove},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newPostureChecker(t, PermissionPolicy{}, WithPosture(posture, &fakeGuaranteeRunner{}))
			got := c.Check(context.Background(), plainTool{name: toolBash}, toolBash, tt.args)
			if got != tt.want {
				t.Errorf("Check(%s) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestWithCeilingPostures: the posture is selected per-Check from table[ordinal];
// lowering the ceiling immediately downgrades the next Check; an out-of-range
// ordinal fails closed to table[0] (the most restrictive).
func TestWithCeilingPostures(t *testing.T) {
	t.Parallel()
	restrictive := Posture{} // auto-approves nothing.
	trusted := Posture{AutoApproveBash: true, RequiredGuarantees: gA, GrantCarryingAlwaysAsk: true}
	table := []Posture{restrictive, trusted}

	ceiling := &fakeCeiling{cur: 1}
	runner := &fakeGuaranteeRunner{bits: gA}
	c := newPostureChecker(t, PermissionPolicy{}, WithCeilingPostures(ceiling, table, runner))
	bash := plainTool{name: toolBash}
	args := `{"command":"echo hi"}`

	// Ordinal 1 (trusted) with a satisfied interlock -> auto-approve.
	if got := c.Check(context.Background(), bash, toolBash, args); got != loop.EffectAutoApprove {
		t.Fatalf("ceiling=1 Check() = %v, want EffectAutoApprove", got)
	}
	// Lower the ceiling to 0 (restrictive) -> the very next Check downgrades to Ask.
	ceiling.cur = 0
	if got := c.Check(context.Background(), bash, toolBash, args); got != loop.EffectAsk {
		t.Fatalf("ceiling=0 Check() = %v, want EffectAsk (clamped by lowered ceiling)", got)
	}
	// Out-of-range ordinal -> fail closed to table[0] (restrictive), not table[max].
	ceiling.cur = 99
	if got := c.Check(context.Background(), bash, toolBash, args); got != loop.EffectAsk {
		t.Fatalf("ceiling=99 Check() = %v, want EffectAsk (out-of-range fails closed to table[0])", got)
	}
}

// TestPostureNeverOverridesHardDeny: no posture auto-approve can override a
// non-bypassable Stage 1/2 deny (containment or hard-deny prefix).
func TestPostureNeverOverridesHardDeny(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	bashPosture := Posture{AutoApproveBash: true, RequiredGuarantees: 0}
	editPosture := Posture{AutoApproveEdits: true}
	tests := []struct {
		name     string
		policy   PermissionPolicy
		posture  Posture
		toolName string
		args     string
	}{
		{
			name:     "denied bash prefix beats auto-approve-bash",
			policy:   PermissionPolicy{HardDeny: HardDenyRules{DeniedBashPrefixes: []string{"sudo"}}},
			posture:  bashPosture,
			toolName: toolBash,
			args:     `{"command":"sudo rm -rf x"}`,
		},
		{
			name:     "write escape beats auto-approve-edits",
			policy:   PermissionPolicy{WorkspaceRoot: ws},
			posture:  editPosture,
			toolName: toolWriteFile,
			args:     `{"path":"../escape.txt"}`,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newPostureChecker(t, tt.policy, WithPosture(tt.posture, &fakeGuaranteeRunner{}))
			got := c.Check(context.Background(), plainTool{name: tt.toolName}, tt.toolName, tt.args)
			if got != loop.EffectDeny {
				t.Errorf("Check() = %v, want EffectDeny (posture must never override a hard-deny)", got)
			}
		})
	}
}

// TestPostureTrivialBash: the "trivial auto, rest ask" slot (write mode) approves
// only trivial commands AND only when the interlock passes.
func TestPostureTrivialBash(t *testing.T) {
	t.Parallel()
	isTrivial := func(cmd string) bool { return cmd == "ls" }
	required := gA
	tests := []struct {
		name string
		bits uint64
		cmd  string
		want loop.Effect
	}{
		{name: "trivial + interlock ok -> approve", bits: gA, cmd: "ls", want: loop.EffectAutoApprove},
		{name: "non-trivial + interlock ok -> ask", bits: gA, cmd: "rm -rf .", want: loop.EffectAsk},
		{name: "trivial but interlock fails -> ask", bits: 0, cmd: "ls", want: loop.EffectAsk},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			posture := Posture{AutoApproveBash: false, TrivialBash: isTrivial, RequiredGuarantees: required}
			c := newPostureChecker(t, PermissionPolicy{}, WithPosture(posture, &fakeGuaranteeRunner{bits: tt.bits}))
			args := `{"command":` + strconvQuote(tt.cmd) + `}`
			got := c.Check(context.Background(), plainTool{name: toolBash}, toolBash, args)
			if got != tt.want {
				t.Errorf("Check(%q) = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}
}

// TestPostureRequiredLevelFloor: the optional Level() secondary floor is
// fail-closed — a runner below the floor, or one without a Level() probe, does not
// auto-approve even when the guarantee bits are satisfied.
func TestPostureRequiredLevelFloor(t *testing.T) {
	t.Parallel()
	posture := Posture{AutoApproveBash: true, RequiredGuarantees: gA, RequiredLevel: 2, GrantCarryingAlwaysAsk: true}
	tests := []struct {
		name   string
		runner tool.CommandRunner
		want   loop.Effect
	}{
		{name: "level meets floor -> approve", runner: &fakeGuaranteeRunner{bits: gA, level: 2}, want: loop.EffectAutoApprove},
		{name: "level above floor -> approve", runner: &fakeGuaranteeRunner{bits: gA, level: 3}, want: loop.EffectAutoApprove},
		{name: "level below floor -> ask", runner: &fakeGuaranteeRunner{bits: gA, level: 1}, want: loop.EffectAsk},
		{name: "no Level probe -> ask", runner: &fakeBitsOnlyRunner{bits: gA}, want: loop.EffectAsk},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newPostureChecker(t, PermissionPolicy{}, WithPosture(posture, tt.runner))
			got := c.Check(context.Background(), plainTool{name: toolBash}, toolBash, `{"command":"echo hi"}`)
			if got != tt.want {
				t.Errorf("Check() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPostureNilUnchanged: a checker that configures no auto-approval behaves
// exactly as today — Bash falls through to Ask. Both a nil posture (no option) and
// an all-flags-off posture must match the no-posture baseline.
func TestPostureNilUnchanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts []Option
	}{
		{name: "no posture configured", opts: nil},
		{
			name: "posture configured but all auto-approve flags off",
			opts: []Option{WithPosture(Posture{}, &fakeGuaranteeRunner{bits: gA | gB | gC})},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newPostureChecker(t, PermissionPolicy{}, tt.opts...)
			got := c.Check(context.Background(), plainTool{name: toolBash}, toolBash, `{"command":"echo hi"}`)
			if got != loop.EffectAsk {
				t.Errorf("Check() = %v, want EffectAsk (no auto-approve => today's default)", got)
			}
		})
	}
}

// posturesEqual compares two Postures field-by-field. Posture holds a func field
// (TrivialBash) so it is not comparable with ==; TrivialBash is compared by
// nil-ness only (sufficient for selector tests, none of which set it).
func posturesEqual(a, b Posture) bool {
	return a.AutoApproveEdits == b.AutoApproveEdits &&
		a.AutoApproveBash == b.AutoApproveBash &&
		a.RequiredGuarantees == b.RequiredGuarantees &&
		a.RequiredLevel == b.RequiredLevel &&
		a.GrantCarryingAlwaysAsk == b.GrantCarryingAlwaysAsk &&
		(a.TrivialBash == nil) == (b.TrivialBash == nil)
}

// TestCeilingPosturesSelect exercises the dynamic selector directly (not through
// Check): empty table, nil source, in-range, and out-of-range ordinals — all
// fail-closed to the most restrictive entry (table[0], or the zero Posture).
func TestCeilingPosturesSelect(t *testing.T) {
	t.Parallel()
	restrictive := Posture{}
	mid := Posture{AutoApproveEdits: true}
	trusted := Posture{AutoApproveBash: true, RequiredGuarantees: gA}
	table := []Posture{restrictive, mid, trusted}

	tests := []struct {
		name  string
		src   CeilingSource
		table []Posture
		want  Posture
	}{
		{name: "empty table -> zero posture", src: &fakeCeiling{cur: 0}, table: nil, want: Posture{}},
		{name: "nil source -> table[0]", src: nil, table: table, want: restrictive},
		{name: "in-range ordinal 1 -> table[1]", src: &fakeCeiling{cur: 1}, table: table, want: mid},
		{name: "in-range ordinal 2 -> table[2]", src: &fakeCeiling{cur: 2}, table: table, want: trusted},
		{name: "out-of-range ordinal -> table[0]", src: &fakeCeiling{cur: 99}, table: table, want: restrictive},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ceilingPostures{src: tt.src, table: tt.table}.current()
			if !posturesEqual(got, tt.want) {
				t.Errorf("current() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestDenyBeatsPosture pins the Stage-6.5 ordering: even with a PASSING interlock
// and AutoApproveBash, a Stage-3 EffectChecker veto (deny/ask) and a Stage-6
// session-policy deny still win. The control row proves the same setup WOULD
// auto-approve absent a veto/deny — so the deny cases are genuinely overriding it.
// (Stage-5 persisted deny shares reduceApprovalRecords with Stage 6, which is
// representative.) Guards against a refactor that moves stagePosture earlier.
func TestDenyBeatsPosture(t *testing.T) {
	t.Parallel()
	required := gA | gB
	posture := Posture{AutoApproveBash: true, RequiredGuarantees: required, GrantCarryingAlwaysAsk: true}
	runner := &fakeGuaranteeRunner{bits: required} // interlock PASSES.

	tests := []struct {
		name   string
		tool   tool.InvokableTool
		policy PermissionPolicy
		want   loop.Effect
	}{
		{
			name:   "control: no veto/deny -> posture auto-approves",
			tool:   plainTool{name: toolBash},
			policy: PermissionPolicy{},
			want:   loop.EffectAutoApprove,
		},
		{
			name:   "stage-3 EffectChecker deny beats posture",
			tool:   staticEffect{name: toolBash, effect: loop.EffectDeny, handled: true},
			policy: PermissionPolicy{},
			want:   loop.EffectDeny,
		},
		{
			name:   "stage-3 EffectChecker ask beats posture",
			tool:   staticEffect{name: toolBash, effect: loop.EffectAsk, handled: true},
			policy: PermissionPolicy{},
			want:   loop.EffectAsk,
		},
		{
			name:   "stage-6 session-policy deny beats posture",
			tool:   plainTool{name: toolBash},
			policy: PermissionPolicy{Policies: []loop.ToolPolicy{{Tool: toolBash, Effect: loop.EffectDeny}}},
			want:   loop.EffectDeny,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newPostureChecker(t, tt.policy, WithPosture(posture, runner))
			got := c.Check(context.Background(), tt.tool, toolBash, `{"command":"echo hi"}`)
			if got != tt.want {
				t.Errorf("Check() = %v, want %v (a deny/veto must beat posture auto-approve)", got, tt.want)
			}
		})
	}
}
