package hustleruntime

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// focusedFinalizerAdapterFiles is deliberately empty until the first production
// consumer adapter lands. Registering one is an architecture change: its package
// must expose only the focused product capability, and its own contract test must
// prove that the adapter neither receives nor stores Session/Shutdown capability.
var focusedFinalizerAdapterFiles = map[string]struct{}{}

func TestProductionRunAndFinalizeConsumersAreFocusedAdapters(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		root string
	}{
		{name: "module production consumers", root: filepath.Join("..", "..")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			calls, err := productionRunAndFinalizeCalls(tt.root)
			if err != nil {
				t.Fatalf("scan production consumers: %v", err)
			}
			for _, call := range calls {
				if _, allowed := focusedFinalizerAdapterFiles[call.file]; !allowed {
					t.Errorf("%s calls RunAndFinalize outside a registered focused adapter", call.location())
				}
			}
		})
	}
}

func TestRunAndFinalizeCallDetection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  string
		want int
	}{
		{name: "production call", src: `package adapter; func run(c C) { c.RunAndFinalize() }`, want: 1},
		{name: "method declaration is not a call", src: `package adapter; type C struct{}; func (C) RunAndFinalize() {}`, want: 0},
		{name: "method value is not a call", src: `package adapter; func keep(c C) { _ = c.RunAndFinalize }`, want: 0},
		{name: "unrelated call", src: `package adapter; func stop(c C) { c.Close() }`, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fileSet := token.NewFileSet()
			file, err := parser.ParseFile(fileSet, "fixture.go", tt.src, 0)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			if got := len(runAndFinalizeCallLines(fileSet, file)); got != tt.want {
				t.Fatalf("RunAndFinalize calls = %d, want %d", got, tt.want)
			}
		})
	}
}

type productionFinalizerCall struct {
	file string
	line int
}

func (c productionFinalizerCall) location() string { return fmt.Sprintf("%s:%d", c.file, c.line) }

func productionRunAndFinalizeCalls(root string) ([]productionFinalizerCall, error) {
	var calls []productionFinalizerCall
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if productionScanExcludedDirectory(relative) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		for _, line := range runAndFinalizeCallLines(fileSet, file) {
			calls = append(calls, productionFinalizerCall{file: filepath.ToSlash(relative), line: line})
		}
		return nil
	})
	return calls, err
}

func productionScanExcludedDirectory(relative string) bool {
	if relative == "." {
		return false
	}
	base := filepath.Base(relative)
	if base == "vendor" || base == ".git" || base == ".worktrees" {
		return true
	}
	return filepath.ToSlash(relative) == "internal/hustleruntime"
}

func runAndFinalizeCallLines(fileSet *token.FileSet, file *ast.File) []int {
	var lines []int
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok && selector.Sel.Name == "RunAndFinalize" {
			lines = append(lines, fileSet.Position(selector.Sel.Pos()).Line)
		}
		return true
	})
	return lines
}
