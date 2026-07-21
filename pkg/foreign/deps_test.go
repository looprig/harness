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
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	harnessModulePath          = "github.com/looprig/harness"
	extractedForeignloopModule = "github.com/looprig/foreignloops"
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

func foreignImportViolation(importPath string, standardLibrary map[string]struct{}) string {
	switch {
	case isForbiddenForeignloopDependency(importPath):
		return "imports extracted foreignloop module"
	case importPath == harnessInternalPrefix || strings.HasPrefix(importPath, harnessInternalPrefix+"/"):
		return "imports Harness internal package"
	case importPath == oldForeignloopPackage || strings.HasPrefix(importPath, oldForeignloopPackage+"/"):
		return "imports old concrete foreignloop package"
	}
	if _, ok := standardLibrary[importPath]; ok {
		return ""
	}
	switch {
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

func standardLibraryPackages(root, goBinary string) (map[string]struct{}, error) {
	cmd := exec.Command(goBinary, "list", "-f={{.ImportPath}}", "std")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		message := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			message = strings.TrimSpace(string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("go list standard library packages: %w\n%s", err, message)
	}
	packages := make(map[string]struct{})
	for _, line := range strings.Split(string(out), "\n") {
		if importPath := strings.TrimSpace(line); importPath != "" {
			packages[importPath] = struct{}{}
		}
	}
	if len(packages) == 0 {
		return nil, fmt.Errorf("go list standard library packages: empty result")
	}
	return packages, nil
}

func foreignPackageImportViolations(dir string, standardLibrary map[string]struct{}) ([]string, error) {
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
			if reason := foreignImportViolation(importPath, standardLibrary); reason != "" {
				violations = append(violations, fmt.Sprintf("%s imports %q: %s", entry.Name(), importPath, reason))
			}
		}
	}
	sort.Strings(violations)
	return violations, nil
}

func findHarnessModuleRoot(start string) (string, error) {
	start, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve Harness module search path %q: %w", start, err)
	}
	for dir := start; ; dir = filepath.Dir(dir) {
		goMod := filepath.Join(dir, "go.mod")
		contents, err := os.ReadFile(goMod)
		switch {
		case err == nil && goModModulePath(contents) == harnessModulePath:
			return dir, nil
		case err != nil && !os.IsNotExist(err):
			return "", fmt.Errorf("read %s: %w", goMod, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return "", fmt.Errorf("find module %q from %s: no matching go.mod", harnessModulePath, start)
}

func goModModulePath(contents []byte) string {
	scanner := bufio.NewScanner(strings.NewReader(string(contents)))
	for scanner.Scan() {
		line, _, _ := strings.Cut(scanner.Text(), "//")
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "module" {
			continue
		}
		if unquoted, err := strconv.Unquote(fields[1]); err == nil {
			return unquoted
		}
		return fields[1]
	}
	return ""
}

func harnessSourceImportViolations(root string) ([]string, error) {
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path == root {
				return nil
			}
			if entry.Name() == "vendor" || strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			_, err := os.Stat(filepath.Join(path, "go.mod"))
			switch {
			case err == nil:
				return filepath.SkipDir
			case !os.IsNotExist(err):
				return err
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if isForbiddenForeignloopDependency(importPath) {
				violations = append(violations, fmt.Sprintf("%s imports %q", filepath.ToSlash(rel), importPath))
			}
		}
		return nil
	})
	sort.Strings(violations)
	return violations, err
}

func foreignTestModuleRoot(t *testing.T) string {
	t.Helper()
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get pkg/foreign test working directory: %v", err)
	}
	root, err := findHarnessModuleRoot(workingDir)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestForbiddenForeignloopDependencyClassifierAndParser(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "github.com/looprig/foreignloops", want: true},
		{path: "github.com/looprig/foreignloops/driver/codex", want: true},
		{path: "github.com/looprig/foreignloopsish"},
		{path: "github.com/looprig/foreignloop"},
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
		"github.com/looprig/foreignloops",
		"github.com/looprig/foreignloops/driver/claude",
		"github.com/looprig/harness/pkg/foreignloop",
	}, "\n"))
	got, err := forbiddenForeignloopDependencies(input)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"github.com/looprig/foreignloops",
		"github.com/looprig/foreignloops/driver/claude",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("forbiddenForeignloopDependencies() = %v, want %v", got, want)
	}
}

func TestForeignPackageImportClassifier(t *testing.T) {
	standardLibrary := map[string]struct{}{
		"context":  {},
		"net/http": {},
	}
	tests := []struct {
		name       string
		importPath string
		want       string
	}{
		{name: "standard library root", importPath: "context"},
		{name: "standard library child", importPath: "net/http"},
		{
			name:       "dotless non-standard package",
			importPath: "corp/private",
			want:       "is outside pkg/foreign import allowlist",
		},
		{name: "core public package", importPath: "github.com/looprig/core/uuid"},
		{name: "Harness public package", importPath: "github.com/looprig/harness/pkg/event"},
		{
			name:       "extracted foreignloop module",
			importPath: "github.com/looprig/foreignloops/driver/codex",
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
			if got := foreignImportViolation(tt.importPath, standardLibrary); got != tt.want {
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

func TestHarnessSourceImportBoundary(t *testing.T) {
	violations, err := harnessSourceImportViolations(foreignTestModuleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range violations {
		t.Errorf("Harness source imports forbidden module package: %s", violation)
	}
}

func TestForeignPackageImportBoundary(t *testing.T) {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain is unavailable")
	}
	root := foreignTestModuleRoot(t)
	standardLibrary, err := standardLibraryPackages(root, goBinary)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "pkg", "foreign")
	violations, err := foreignPackageImportViolations(dir, standardLibrary)
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

	got, err := foreignPackageImportViolations(dir, nil)
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

func TestHarnessModuleRootDiscovery(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/looprig/harness\n\ngo 1.26.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "pkg", "foreign")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := findHarnessModuleRoot(nested)
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("findHarnessModuleRoot() = %q, want %q", got, root)
	}
}

func TestHarnessModuleRootRejectsWrongModule(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/example/other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "pkg", "foreign")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := findHarnessModuleRoot(nested)
	if err == nil || !strings.Contains(err.Error(), "github.com/looprig/harness") {
		t.Fatalf("findHarnessModuleRoot() error = %v, want missing Harness module error", err)
	}
}

func TestHarnessSourceImportBoundaryIncludesBuildTaggedFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"pkg/tagged/foreign_plan9_test.go": "//go:build plan9\n\npackage tagged\nimport _ \"github.com/looprig/foreignloops/driver/codex\"\n",
		"vendor/ignored/ignored.go":        "package ignored\nimport _ \"github.com/looprig/foreignloops\"\n",
		".worktrees/ignored/leak.go":       "package ignored\nimport _ \"github.com/looprig/foreignloops\"\n",
		"nested/go.mod":                    "module github.com/example/nested\n",
		"nested/ignored/leak.go":           "package ignored\nimport _ \"github.com/looprig/foreignloops\"\n",
	}
	for rel, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := harnessSourceImportViolations(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`pkg/tagged/foreign_plan9_test.go imports "github.com/looprig/foreignloops/driver/codex"`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("harnessSourceImportViolations() = %v, want %v", got, want)
	}
}
