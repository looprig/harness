package buildtest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStaticcheckRunnerRejectsEmptyPackageList(t *testing.T) {
	result := runStaticcheckRunner(t, "empty", false)
	if result.err == nil {
		t.Fatal("make staticcheck succeeded with an empty package list")
	}
	if !strings.Contains(result.output, "staticcheck: go list ./... returned no packages") {
		t.Fatalf("output = %q, want empty-package diagnostic", result.output)
	}
	if _, err := os.Stat(result.logPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staticcheck invocation log exists or stat failed: %v", err)
	}
}

func TestStaticcheckRunnerPropagatesGoListFailure(t *testing.T) {
	result := runStaticcheckRunner(t, "error", false)
	var exitErr *exec.ExitError
	if !errors.As(result.err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("make staticcheck error = %v, want exit status 23", result.err)
	}
	if !strings.Contains(result.output, "staticcheck: go list ./... failed") {
		t.Fatalf("output = %q, want go-list failure diagnostic", result.output)
	}
	if _, err := os.Stat(result.logPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staticcheck invocation log exists or stat failed: %v", err)
	}
}

func TestStaticcheckTargetPassesExplicitPackages(t *testing.T) {
	result := runStaticcheckRunner(t, "packages", true)
	if result.err != nil {
		t.Fatalf("make staticcheck failed: %v\n%s", result.err, result.output)
	}
	got, err := os.ReadFile(result.logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "tool\nstaticcheck\nexample.test/alpha\nexample.test/beta/sub\n"
	if string(got) != want {
		t.Fatalf("staticcheck argv = %q, want %q", got, want)
	}
}

type staticcheckResult struct {
	err     error
	output  string
	logPath string
}

func runStaticcheckRunner(t *testing.T, mode string, throughMake bool) staticcheckResult {
	t.Helper()

	repoRoot := harnessRepoRoot(t)
	tempDir := t.TempDir()
	fakeGo := filepath.Join(tempDir, "fake-go")
	logPath := filepath.Join(tempDir, "staticcheck.log")
	const fakeGoScript = `#!/bin/sh
if [ "$1" = "list" ]; then
	case "$FAKE_GO_MODE" in
	empty) exit 0 ;;
	error) echo "synthetic go list failure" >&2; exit 23 ;;
	packages) printf '%s\n' 'example.test/alpha' 'example.test/beta/sub'; exit 0 ;;
	esac
fi
if [ "$1" = "tool" ] && [ "$2" = "staticcheck" ]; then
	printf '%s\n' "$@" >"$FAKE_GO_LOG"
	exit 0
fi
echo "unexpected fake go invocation: $*" >&2
exit 97
`
	if err := os.WriteFile(fakeGo, []byte(fakeGoScript), 0o700); err != nil {
		t.Fatal(err)
	}

	var cmd *exec.Cmd
	if throughMake {
		cmd = exec.Command("make", "--no-print-directory", "staticcheck", "GO="+fakeGo)
	} else {
		cmd = exec.Command(filepath.Join(repoRoot, "scripts", "run-staticcheck.sh"))
	}
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GO="+fakeGo, "FAKE_GO_MODE="+mode, "FAKE_GO_LOG="+logPath)
	output, err := cmd.CombinedOutput()
	return staticcheckResult{err: err, output: string(output), logPath: logPath}
}

func harnessRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
