package rig

import (
	"bufio"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

const foreignloopImportPath = "github.com/looprig/harness/pkg/foreignloop"

func isForbiddenForeignloopProductionImport(sourcePath, importPath string) bool {
	sourcePath = filepath.ToSlash(sourcePath)
	if strings.HasSuffix(sourcePath, "_test.go") || strings.HasPrefix(sourcePath, "pkg/foreignloop/") {
		return false
	}
	return importPath == foreignloopImportPath || strings.HasPrefix(importPath, foreignloopImportPath+"/")
}

func validateForeignloopCoverage(tracked []string, manifest io.Reader, root fs.FS) error {
	sources, err := foreignloopManifestSources(manifest)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(sources))
	var problems []string
	for _, source := range sources {
		if seen[source] {
			problems = append(problems, "duplicate manifest source: "+source)
			continue
		}
		seen[source] = true
		info, err := fs.Stat(root, source)
		if err != nil {
			if os.IsNotExist(err) {
				problems = append(problems, "manifest source does not exist: "+source)
				continue
			}
			return fmt.Errorf("stat manifest source %s: %w", source, err)
		}
		if !info.Mode().IsRegular() {
			problems = append(problems, "manifest source is not a regular file: "+source)
		}
	}
	for _, source := range tracked {
		info, err := fs.Stat(root, source)
		if err != nil {
			return fmt.Errorf("stat tracked source %s: %w", source, err)
		}
		if info.Mode().IsRegular() && !seen[source] {
			problems = append(problems, "missing manifest entry: "+source)
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("foreignloop coverage manifest:\n%s", strings.Join(problems, "\n"))
	}
	return nil
}

func foreignloopManifestSources(r io.Reader) ([]string, error) {
	scanner := bufio.NewScanner(r)
	foundHeader := false
	var sources []string
	for scanner.Scan() {
		columns := strings.Split(scanner.Text(), "|")
		if len(columns) < 3 {
			continue
		}
		first := strings.TrimSpace(columns[1])
		if first == "Source file" {
			foundHeader = true
			continue
		}
		if !foundHeader {
			continue
		}
		first = strings.Trim(strings.TrimSpace(first), "`")
		if strings.HasPrefix(first, "pkg/foreignloop/") {
			sources = append(sources, first)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan foreignloop coverage manifest: %w", err)
	}
	if !foundHeader {
		return nil, fmt.Errorf("foreignloop coverage manifest: Source file table not found")
	}
	return sources, nil
}

func trackedForeignloopFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "--", "pkg/foreignloop")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list tracked pkg/foreignloop files: %w", err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

func TestForeignloopCoverageValidator(t *testing.T) {
	files := fstest.MapFS{
		"pkg/foreignloop/first.go":  {},
		"pkg/foreignloop/second.go": {},
	}
	tests := []struct {
		name     string
		tracked  []string
		manifest string
		want     string
	}{
		{
			name:    "missing tracked source",
			tracked: []string{"pkg/foreignloop/first.go", "pkg/foreignloop/second.go"},
			manifest: "| Source file | Final owner | Migration note |\n" +
				"|---|---|---|\n" +
				"| `pkg/foreignloop/first.go` | `backend` | Move. |\n",
			want: "missing manifest entry: pkg/foreignloop/second.go",
		},
		{
			name:    "duplicate source",
			tracked: []string{"pkg/foreignloop/first.go"},
			manifest: "| Source file | Final owner | Migration note |\n" +
				"|---|---|---|\n" +
				"| `pkg/foreignloop/first.go` | `backend` | Move. |\n" +
				"| `pkg/foreignloop/first.go` | `backend` | Move again. |\n",
			want: "duplicate manifest source: pkg/foreignloop/first.go",
		},
		{
			name:    "source does not exist",
			tracked: []string{"pkg/foreignloop/first.go"},
			manifest: "| Source file | Final owner | Migration note |\n" +
				"|---|---|---|\n" +
				"| `pkg/foreignloop/first.go` | `backend` | Move. |\n" +
				"| `pkg/foreignloop/ghost.go` | `backend` | Move. |\n",
			want: "manifest source does not exist: pkg/foreignloop/ghost.go",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateForeignloopCoverage(tt.tracked, strings.NewReader(tt.manifest), files)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateForeignloopCoverage() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestForeignloopCoverageManifestCompleteness(t *testing.T) {
	root := optionalDependenciesModuleRoot(t)
	tracked, err := trackedForeignloopFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.Open(filepath.Join(root, "docs", "plans", "2026-07-17-foreignloop-extraction-coverage.md"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manifest.Close() })
	if err := validateForeignloopCoverage(tracked, manifest, os.DirFS(root)); err != nil {
		t.Fatal(err)
	}
}

func TestForeignloopProductionImportClassifier(t *testing.T) {
	tests := []struct {
		name       string
		sourcePath string
		importPath string
		want       bool
	}{
		{
			name:       "production consumer outside concrete package is rejected",
			sourcePath: "internal/sessionruntime/session.go",
			importPath: foreignloopImportPath,
			want:       true,
		},
		{
			name:       "production consumer of concrete subpackage is rejected",
			sourcePath: "pkg/rig/options.go",
			importPath: foreignloopImportPath + "/codex",
			want:       true,
		},
		{
			name:       "tests remain allowed during migration overlap",
			sourcePath: "internal/sessionruntime/foreign_e2e_test.go",
			importPath: foreignloopImportPath,
		},
		{
			name:       "concrete package production remains allowed",
			sourcePath: "pkg/foreignloop/codex/codex.go",
			importPath: foreignloopImportPath,
		},
		{
			name:       "unrelated production import remains allowed",
			sourcePath: "pkg/rig/options.go",
			importPath: "github.com/looprig/harness/pkg/foreign",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isForbiddenForeignloopProductionImport(tt.sourcePath, tt.importPath); got != tt.want {
				t.Fatalf("isForbiddenForeignloopProductionImport(%q, %q) = %v, want %v", tt.sourcePath, tt.importPath, got, tt.want)
			}
		})
	}
}

func TestForeignloopProductionImportBoundary(t *testing.T) {
	root := optionalDependenciesModuleRoot(t)
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
			if isForbiddenForeignloopProductionImport(rel, importPath) {
				t.Errorf("%s imports concrete foreign loop package %q", filepath.ToSlash(rel), importPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func optionalDependenciesModuleRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate dependency boundary test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
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
