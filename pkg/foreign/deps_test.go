package foreign_test

import (
	"bufio"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	extractedForeignloopModule = "github.com/looprig/foreignloop"
	harnessInternalPrefix      = "github.com/looprig/harness/internal"
	oldForeignloopPackage      = "github.com/looprig/harness/pkg/foreignloop"
)

func isForbiddenForeignloopDependency(importPath string) bool {
	return importPath == extractedForeignloopModule || strings.HasPrefix(importPath, extractedForeignloopModule+"/")
}

func forbiddenForeignloopDependencies(r io.Reader) ([]string, error) {
	var forbidden []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		importPath := strings.TrimSpace(scanner.Text())
		if isForbiddenForeignloopDependency(importPath) {
			forbidden = append(forbidden, importPath)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return forbidden, nil
}

func foreignImportViolation(importPath string) string {
	switch {
	case isForbiddenForeignloopDependency(importPath):
		return "imports extracted foreignloop module"
	case importPath == harnessInternalPrefix || strings.HasPrefix(importPath, harnessInternalPrefix+"/"):
		return "imports Harness internal package"
	case importPath == oldForeignloopPackage || strings.HasPrefix(importPath, oldForeignloopPackage+"/"):
		return "imports old concrete foreignloop package"
	case !strings.Contains(strings.Split(importPath, "/")[0], "."):
		return ""
	case importPath == "github.com/looprig/core" || strings.HasPrefix(importPath, "github.com/looprig/core/"):
		return ""
	case strings.HasPrefix(importPath, "github.com/looprig/harness/pkg/"):
		return ""
	default:
		return "is outside pkg/foreign import allowlist"
	}
}

func harnessDependencyViolations(root, goBinary string) ([]string, error) {
	cmd := exec.Command(goBinary, "list", "-deps", "-test", "-f={{.ImportPath}}", "./...")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("go list Harness dependency closure: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	violations, err := forbiddenForeignloopDependencies(strings.NewReader(string(out)))
	if err != nil {
		return nil, fmt.Errorf("parse Harness dependency closure: %w", err)
	}
	return violations, nil
}

func foreignPackageImportViolations(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read pkg/foreign: %w", err)
	}
	var violations []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return nil, fmt.Errorf("parse %s imports: %w", entry.Name(), err)
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return nil, fmt.Errorf("parse %s import path %s: %w", entry.Name(), spec.Path.Value, err)
			}
			if reason := foreignImportViolation(importPath); reason != "" {
				violations = append(violations, fmt.Sprintf("%s imports %q: %s", entry.Name(), importPath, reason))
			}
		}
	}
	sort.Strings(violations)
	return violations, nil
}

func foreignTestModuleRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate pkg/foreign dependency test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func TestForbiddenForeignloopDependencyClassifierAndParser(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "github.com/looprig/foreignloop", want: true},
		{path: "github.com/looprig/foreignloop/codex", want: true},
		{path: "github.com/looprig/foreignloopish"},
		{path: "github.com/looprig/harness/pkg/foreignloop"},
		{path: "github.com/looprig/core/uuid"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isForbiddenForeignloopDependency(tt.path); got != tt.want {
				t.Fatalf("isForbiddenForeignloopDependency(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}

	input := strings.NewReader(strings.Join([]string{
		"context",
		"github.com/looprig/core/uuid",
		"github.com/looprig/foreignloop",
		"github.com/looprig/foreignloop/claude",
		"github.com/looprig/harness/pkg/foreignloop",
	}, "\n"))
	got, err := forbiddenForeignloopDependencies(input)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"github.com/looprig/foreignloop",
		"github.com/looprig/foreignloop/claude",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("forbiddenForeignloopDependencies() = %v, want %v", got, want)
	}
}

func TestForeignPackageImportClassifier(t *testing.T) {
	tests := []struct {
		name       string
		importPath string
		want       string
	}{
		{name: "standard library", importPath: "context"},
		{name: "core public package", importPath: "github.com/looprig/core/uuid"},
		{name: "Harness public package", importPath: "github.com/looprig/harness/pkg/event"},
		{
			name:       "extracted foreignloop module",
			importPath: "github.com/looprig/foreignloop/codex",
			want:       "imports extracted foreignloop module",
		},
		{
			name:       "Harness internal package",
			importPath: "github.com/looprig/harness/internal/sessionruntime",
			want:       "imports Harness internal package",
		},
		{
			name:       "old concrete foreignloop package",
			importPath: "github.com/looprig/harness/pkg/foreignloop/claude",
			want:       "imports old concrete foreignloop package",
		},
		{
			name:       "other external package",
			importPath: "github.com/looprig/inference",
			want:       "is outside pkg/foreign import allowlist",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := foreignImportViolation(tt.importPath); got != tt.want {
				t.Fatalf("foreignImportViolation(%q) = %q, want %q", tt.importPath, got, tt.want)
			}
		})
	}
}

func TestHarnessDependencyDirection(t *testing.T) {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain is unavailable")
	}
	violations, err := harnessDependencyViolations(foreignTestModuleRoot(t), goBinary)
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range violations {
		t.Errorf("Harness dependency closure includes forbidden module package %q", violation)
	}
}

func TestForeignPackageImportBoundary(t *testing.T) {
	dir := filepath.Join(foreignTestModuleRoot(t), "pkg", "foreign")
	violations, err := foreignPackageImportViolations(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range violations {
		t.Error(violation)
	}
}

func TestForeignPackageImportBoundaryIncludesProductionAndTestFiles(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"foreign.go":      "package foreign\nimport _ \"github.com/looprig/inference\"\n",
		"foreign_test.go": "package foreign_test\nimport _ \"github.com/looprig/harness/internal/sessionruntime\"\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := foreignPackageImportViolations(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"foreign.go imports \"github.com/looprig/inference\": is outside pkg/foreign import allowlist",
		"foreign_test.go imports \"github.com/looprig/harness/internal/sessionruntime\": imports Harness internal package",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("foreignPackageImportViolations() = %v, want %v", got, want)
	}
}
