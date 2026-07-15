package tool_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenModule is the sandbox module path the harness must NEVER import. The
// structural-coupling invariant (SPEC §10, "coupled only via stdlib-typed
// interfaces") requires the harness to depend on the sandbox EXCLUSIVELY through
// stdlib-typed seams (loop.ReadGuard, tool.ArgvRunner, …) that it defines itself.
// A concrete import of the sandbox module would invert that dependency and let
// sandbox types leak into the harness — the one coupling this guard forbids.
const forbiddenModule = "github.com/looprig/sandbox"

// harnessModulePath is this module's own path — used with `go list -m` to resolve
// the module root. Kept as a named constant so the resolution target is explicit.
const harnessModulePath = "github.com/looprig/harness"

const toolPackagePath = harnessModulePath + "/pkg/tool"

// TestNoSandboxImport is the dependency-direction guard: it shells `go list -deps
// ./...` over the whole harness module and FAILS if any package in the transitive
// dependency closure is the sandbox module or one of its subpackages. It is a real
// failing test, not a skip: the only skip is when the `go` toolchain is absent
// (recorded, so the reason is visible in the run). The detection logic itself is
// pinned separately in TestSandboxViolations so the "fails on a sandbox import"
// behaviour is proven without needing the (uncommitted) sandbox module present.
//
// Hermetic: it locates the module root via `go list -m` (independent of the process
// CWD) and runs the closure query there, so the result does not depend on where
// `go test` was invoked. Vendoring keeps the closure query offline.
func TestNoSandboxImport(t *testing.T) {
	t.Parallel()

	goBin, err := exec.LookPath("go")
	if err != nil {
		// The guard needs the toolchain to enumerate the dependency closure. This is
		// the ONLY sanctioned skip; record why so a skipped run is never mistaken for
		// a passing one.
		t.Skipf("go toolchain not found on PATH (%v); cannot enumerate the dependency closure", err)
	}

	moduleRoot := harnessModuleRoot(t, goBin)

	// -deps lists the full transitive import closure of every package under ./...;
	// each output line is one import path.
	cmd := exec.Command(goBin, "list", "-deps", "./...")
	cmd.Dir = moduleRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./... in %q failed: %v\n%s", moduleRoot, err, stderrOf(err))
	}

	if violations := sandboxViolations(string(out)); len(violations) > 0 {
		t.Errorf("harness imports forbidden sandbox package(s) %v: the structural-coupling "+
			"invariant requires coupling to the sandbox ONLY via stdlib-typed interfaces "+
			"(e.g. loop.ReadGuard, tool.ArgvRunner); the harness must never import %s",
			violations, forbiddenModule)
	}
}

// sandboxViolations returns every package path in `go list -deps` output that is
// the forbidden sandbox module or one of its subpackages. It matches on the MODULE
// BOUNDARY (exact path, or the path followed by "/") rather than a raw substring, so
// a hypothetical unrelated module whose name merely begins with the same string
// (e.g. ".../sandboxutil") is not a false positive, while every real sandbox package
// is caught. It is a pure function so the guard's fail-on-violation behaviour is
// unit-testable without the sandbox module being present.
func sandboxViolations(listOutput string) []string {
	var violations []string
	for _, pkg := range strings.Split(listOutput, "\n") {
		pkg = strings.TrimSpace(pkg)
		if pkg == "" {
			continue
		}
		if pkg == forbiddenModule || strings.HasPrefix(pkg, forbiddenModule+"/") {
			violations = append(violations, pkg)
		}
	}
	return violations
}

// TestSandboxViolations pins the detection logic that makes TestNoSandboxImport a
// REAL failing test on a sandbox import: fed synthetic `go list` output, it must
// flag the sandbox module and its subpackages and must NOT flag stdlib, the harness
// itself, or a lookalike module that only shares the name prefix.
func TestSandboxViolations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		lines []string
		want  []string
	}{
		{
			name:  "clean closure has no violations",
			lines: []string{"context", "github.com/looprig/harness/pkg/loop", "github.com/looprig/core/content"},
			want:  nil,
		},
		{
			name:  "sandbox module root is flagged",
			lines: []string{"context", forbiddenModule, "os"},
			want:  []string{forbiddenModule},
		},
		{
			name:  "sandbox subpackage is flagged",
			lines: []string{"github.com/looprig/harness/pkg/tool", forbiddenModule + "/policy"},
			want:  []string{forbiddenModule + "/policy"},
		},
		{
			name:  "multiple sandbox packages all flagged",
			lines: []string{forbiddenModule, forbiddenModule + "/policy", "strings"},
			want:  []string{forbiddenModule, forbiddenModule + "/policy"},
		},
		{
			name:  "lookalike module sharing the name prefix is not flagged",
			lines: []string{forbiddenModule + "util", forbiddenModule + "es/foo"},
			want:  nil,
		},
		{
			name:  "blank lines are ignored",
			lines: []string{"", "  ", "os"},
			want:  nil,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sandboxViolations(strings.Join(tt.lines, "\n"))
			if len(got) != len(tt.want) {
				t.Fatalf("sandboxViolations = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("sandboxViolations[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestToolPackageDependencies keeps Definition and Bindings in the low-level
// contract package: pkg/tool must not grow imports on either the loop runtime or
// the concrete tools package.
func TestToolPackageDependencies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		forbidden string
	}{
		{name: "does not import loop runtime", forbidden: harnessModulePath + "/pkg/loop"},
		{name: "does not import concrete tools", forbidden: harnessModulePath + "/pkg/tools"},
		{name: "does not import optional tools module", forbidden: "github.com/looprig/tools"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			goBin, err := exec.LookPath("go")
			if err != nil {
				t.Skipf("go toolchain not found on PATH (%v); cannot inspect imports", err)
			}
			cmd := exec.Command(goBin, "list", "-f", "{{join .Imports \"\\n\"}}", toolPackagePath)
			cmd.Dir = harnessModuleRoot(t, goBin)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("go list imports for %s failed: %v\n%s", toolPackagePath, err, stderrOf(err))
			}
			for _, imported := range strings.Fields(string(out)) {
				if imported == tt.forbidden {
					t.Fatalf("%s imports forbidden package %s", toolPackagePath, tt.forbidden)
				}
			}
		})
	}
}

// harnessModuleRoot returns the harness module's root directory, resolved via the
// go toolchain so the guard is independent of the test process's CWD.
func harnessModuleRoot(t *testing.T, goBin string) string {
	t.Helper()
	// This test file lives in the harness module, so `go list -m {{.Dir}}` for the
	// harness module path yields its own root dir.
	cmd := exec.Command(goBin, "list", "-m", "-f", "{{.Dir}}", harnessModulePath)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -m %s failed: %v\n%s", harnessModulePath, err, stderrOf(err))
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Fatalf("go list -m %s returned an empty module dir", harnessModulePath)
	}
	return filepath.Clean(dir)
}

// stderrOf extracts an *exec.ExitError's captured stderr, if any, for a clearer
// failure message.
func stderrOf(err error) string {
	if ee, ok := err.(*exec.ExitError); ok {
		return string(ee.Stderr)
	}
	return ""
}
