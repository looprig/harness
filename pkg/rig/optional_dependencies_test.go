package rig

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const foreignloopImportPath = "github.com/looprig/harness/pkg/foreignloop"
const extractedForeignloopImportPath = "github.com/looprig/foreignloop"
const harnessModulePath = "github.com/looprig/harness"

func isForbiddenForeignloopImport(importPath string) bool {
	return importPath == foreignloopImportPath || strings.HasPrefix(importPath, foreignloopImportPath+"/") ||
		importPath == extractedForeignloopImportPath || strings.HasPrefix(importPath, extractedForeignloopImportPath+"/")
}

func TestForeignloopImportClassifier(t *testing.T) {
	tests := []struct {
		name       string
		importPath string
		want       bool
	}{
		{
			name:       "old concrete package is rejected",
			importPath: foreignloopImportPath,
			want:       true,
		},
		{
			name:       "old concrete subpackage is rejected",
			importPath: foreignloopImportPath + "/codex",
			want:       true,
		},
		{
			name:       "extracted module is rejected",
			importPath: extractedForeignloopImportPath,
			want:       true,
		},
		{
			name:       "extracted subpackage is rejected",
			importPath: extractedForeignloopImportPath + "/driver/claude",
			want:       true,
		},
		{
			name:       "similarly named module is allowed",
			importPath: extractedForeignloopImportPath + "ish",
		},
		{
			name:       "unrelated import remains allowed",
			importPath: "github.com/looprig/harness/pkg/foreign",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isForbiddenForeignloopImport(tt.importPath); got != tt.want {
				t.Fatalf("isForbiddenForeignloopImport(%q) = %v, want %v", tt.importPath, got, tt.want)
			}
		})
	}
}

func TestForeignloopImportBoundaryIncludesTestsBuildTagsAndNestedFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"prod.go":                         "package fixture\nimport _ \"github.com/looprig/harness/pkg/foreignloop\"\n",
		"nested/consumer_test.go":         "package nested\nimport _ \"github.com/looprig/harness/pkg/foreignloop/claude\"\n",
		"tagged/consumer_plan9_test.go":   "//go:build plan9\n\npackage tagged\nimport _ \"github.com/looprig/foreignloop/backend\"\n",
		"vendor/ignored/consumer_test.go": "package ignored\nimport _ \"github.com/looprig/foreignloop\"\n",
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

	got, err := foreignloopImportViolations(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`nested/consumer_test.go imports "github.com/looprig/harness/pkg/foreignloop/claude"`,
		`prod.go imports "github.com/looprig/harness/pkg/foreignloop"`,
		`tagged/consumer_plan9_test.go imports "github.com/looprig/foreignloop/backend"`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("foreignloopImportViolations() = %v, want %v", got, want)
	}
}

func foreignloopImportViolations(root string) ([]string, error) {
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == "vendor" || strings.HasPrefix(entry.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if isForbiddenForeignloopImport(importPath) {
				violations = append(violations, fmt.Sprintf("%s imports %q", filepath.ToSlash(rel), importPath))
			}
		}
		return nil
	})
	sort.Strings(violations)
	return violations, err
}

func TestForeignloopImportBoundary(t *testing.T) {
	root := optionalDependenciesModuleRoot(t)
	violations, err := foreignloopImportViolations(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range violations {
		t.Error(violation)
	}
}

func optionalDependenciesModuleRoot(t *testing.T) string {
	t.Helper()
	cwd, cwdErr := os.Getwd()
	if cwdErr == nil {
		root, err := findHarnessModuleRoot(cwd)
		if err == nil {
			return root
		}
		cwdErr = err
	}

	_, filename, _, ok := runtime.Caller(0)
	if ok && filepath.IsAbs(filename) {
		root, err := findHarnessModuleRoot(filepath.Dir(filename))
		if err == nil {
			return root
		}
	}
	t.Fatalf("locate %s module root from working directory %q: %v", harnessModulePath, cwd, cwdErr)
	return ""
}

func findHarnessModuleRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve module-root start %q: %w", start, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("stat module-root start %q: %w", dir, err)
	}
	if !info.IsDir() {
		dir = filepath.Dir(dir)
	}
	for {
		goMod := filepath.Join(dir, "go.mod")
		data, err := os.ReadFile(goMod)
		if err == nil {
			if goModModulePath(data) == harnessModulePath {
				return dir, nil
			}
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("read %s: %w", goMod, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("module root declaring %s not found from %s", harnessModulePath, start)
}

func goModModulePath(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "module" {
			continue
		}
		modulePath := fields[1]
		if unquoted, err := strconv.Unquote(modulePath); err == nil {
			modulePath = unquoted
		}
		return modulePath
	}
	return ""
}

func TestHarnessModuleRootDiscovery(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/looprig/harness\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nestedModule := filepath.Join(root, "nested")
	if err := os.MkdirAll(filepath.Join(nestedModule, "pkg", "leaf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedModule, "go.mod"), []byte("module example.com/wrong\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := findHarnessModuleRoot(filepath.Join(nestedModule, "pkg", "leaf"))
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("findHarnessModuleRoot() = %q, want %q", got, root)
	}
}

func TestHarnessModuleRootRejectsWrongModule(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/wrong\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := findHarnessModuleRoot(root)
	if err == nil || !strings.Contains(err.Error(), "github.com/looprig/harness") {
		t.Fatalf("findHarnessModuleRoot() error = %v, want missing Harness module error", err)
	}
}

func TestHarnessDoesNotImportOptionalToolOrConfinementModules(t *testing.T) {
	root := optionalDependenciesModuleRoot(t)
	forbidden := []string{
		"github.com/looprig/tools",
		"github.com/looprig/sandbox",
		"github.com/looprig/confinement",
	}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == "vendor" || strings.HasPrefix(entry.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			for _, prefix := range forbidden {
				if strings.HasPrefix(importPath, prefix) {
					t.Errorf("%s imports optional dependency %q", path, importPath)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
