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
const harnessModulePath = "github.com/looprig/harness"

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
		columns, tableRow := markdownTableCells(scanner.Text())
		if !foundHeader {
			if tableRow && len(columns) == 3 && columns[0] == "Source file" && columns[1] == "Final owner" && columns[2] == "Migration note" {
				foundHeader = true
			}
			continue
		}
		if !tableRow || len(columns) != 3 {
			break
		}
		if markdownTableSeparator(columns) {
			continue
		}
		source := trimMarkdownCode(columns[0])
		if !strings.HasPrefix(source, "pkg/foreignloop/") {
			continue
		}
		owner := trimMarkdownCode(columns[1])
		if !validForeignloopManifestOwner(owner) {
			return nil, fmt.Errorf("invalid manifest owner for %s: %q", source, owner)
		}
		sources = append(sources, source)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan foreignloop coverage manifest: %w", err)
	}
	if !foundHeader {
		return nil, fmt.Errorf("foreignloop coverage manifest: Source file table not found")
	}
	return sources, nil
}

func markdownTableCells(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil, false
	}
	parts := strings.Split(line, "|")
	cells := make([]string, 0, len(parts)-2)
	for _, part := range parts[1 : len(parts)-1] {
		cells = append(cells, strings.TrimSpace(part))
	}
	return cells, true
}

func markdownTableSeparator(cells []string) bool {
	for _, cell := range cells {
		if !strings.Contains(cell, "-") || strings.Trim(cell, "-: ") != "" {
			return false
		}
	}
	return true
}

func trimMarkdownCode(cell string) string {
	cell = strings.TrimSpace(cell)
	if len(cell) >= 2 && strings.Count(cell, "`") == 2 && cell[0] == '`' && cell[len(cell)-1] == '`' {
		return cell[1 : len(cell)-1]
	}
	return cell
}

func validForeignloopManifestOwner(owner string) bool {
	switch owner {
	case "driver", "driver/claude", "driver/codex", "backend", "harness/pkg/foreign", "tests", "delete":
		return true
	default:
		return false
	}
}

func trackedForeignloopFiles(root string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect Git metadata: %w", err)
		}
		return foreignloopFilesFromTree(root)
	}
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

func foreignloopFilesFromTree(root string) ([]string, error) {
	base := filepath.Join(root, "pkg", "foreignloop")
	var files []string
	err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("enumerate pkg/foreignloop files: %w", err)
	}
	sort.Strings(files)
	return files, nil
}

func TestTrackedForeignloopFilesWithoutGit(t *testing.T) {
	root := t.TempDir()
	want := []string{
		"pkg/foreignloop/driver.go",
		"pkg/foreignloop/testdata/fixture.jsonl",
	}
	for _, rel := range append(want, "pkg/elsewhere/ignored.go") {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := trackedForeignloopFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("trackedForeignloopFiles() = %v, want %v", got, want)
	}
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
		{
			name:    "empty owner",
			tracked: []string{"pkg/foreignloop/first.go"},
			manifest: "| Source file | Final owner | Migration note |\n" +
				"|---|---|---|\n" +
				"| `pkg/foreignloop/first.go` | | Move. |\n",
			want: "invalid manifest owner for pkg/foreignloop/first.go: \"\"",
		},
		{
			name:    "compound owner",
			tracked: []string{"pkg/foreignloop/first.go"},
			manifest: "| Source file | Final owner | Migration note |\n" +
				"|---|---|---|\n" +
				"| `pkg/foreignloop/first.go` | `driver` + `backend` | Move. |\n",
			want: "invalid manifest owner for pkg/foreignloop/first.go: \"`driver` + `backend`\"",
		},
		{
			name:    "unknown owner",
			tracked: []string{"pkg/foreignloop/first.go"},
			manifest: "| Source file | Final owner | Migration note |\n" +
				"|---|---|---|\n" +
				"| `pkg/foreignloop/first.go` | `storage` | Move. |\n",
			want: "invalid manifest owner for pkg/foreignloop/first.go: \"storage\"",
		},
		{
			name:    "later table is not consumed",
			tracked: []string{"pkg/foreignloop/first.go"},
			manifest: "| Source file | Final owner | Migration note |\n" +
				"|---|---|---|\n" +
				"| `pkg/foreignloop/first.go` | `backend` | Move. |\n" +
				"\n" +
				"## Later table\n" +
				"\n" +
				"| Source file | Final owner | Migration note |\n" +
				"|---|---|---|\n" +
				"| `pkg/foreignloop/ghost.go` | `storage` | Ignore. |\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateForeignloopCoverage(tt.tracked, strings.NewReader(tt.manifest), files)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("validateForeignloopCoverage() unexpected error: %v", err)
				}
				return
			}
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
