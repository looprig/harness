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
var focusedFinalizerAdapterFiles = map[string]struct{}{
	"internal/sessionruntime/compaction_adapter.go": {},
}

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
			uses, err := productionRunAndFinalizeUses(tt.root)
			if err != nil {
				t.Fatalf("scan production consumers: %v", err)
			}
			for _, use := range uses {
				if _, allowed := focusedFinalizerAdapterFiles[use.file]; !allowed {
					t.Errorf("%s selects RunAndFinalize outside a registered focused adapter", use.location())
				}
			}
		})
	}
}

func TestRunAndFinalizeSelectorDetection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  string
		want int
	}{
		{name: "direct call", src: `package adapter; func run(c C) { c.RunAndFinalize() }`, want: 1},
		{name: "method declaration is not a selector", src: `package adapter; type C struct{}; func (C) RunAndFinalize() {}`, want: 0},
		{name: "method value", src: `package adapter; func keep(c C) { _ = c.RunAndFinalize }`, want: 1},
		{name: "pass through argument", src: `package adapter; func pass(c C) { consume(c.RunAndFinalize) }`, want: 1},
		{name: "returned method value", src: `package adapter; func pass(c C) F { return c.RunAndFinalize }`, want: 1},
		{name: "unrelated selector", src: `package adapter; func stop(c C) { c.Close() }`, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fileSet := token.NewFileSet()
			file, err := parser.ParseFile(fileSet, "fixture.go", tt.src, 0)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			if got := len(runAndFinalizeSelectorLines(fileSet, file)); got != tt.want {
				t.Fatalf("RunAndFinalize selector uses = %d, want %d", got, tt.want)
			}
		})
	}
}

type productionFinalizerUse struct {
	file string
	line int
}

func (u productionFinalizerUse) location() string { return fmt.Sprintf("%s:%d", u.file, u.line) }

func productionRunAndFinalizeUses(root string) ([]productionFinalizerUse, error) {
	var uses []productionFinalizerUse
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
		for _, line := range runAndFinalizeSelectorLines(fileSet, file) {
			uses = append(uses, productionFinalizerUse{file: filepath.ToSlash(relative), line: line})
		}
		return nil
	})
	return uses, err
}

func productionScanExcludedDirectory(relative string) bool {
	if relative == "." {
		return false
	}
	base := filepath.Base(relative)
	return base == "vendor" || base == ".git" || base == ".worktrees"
}

func runAndFinalizeSelectorLines(fileSet *token.FileSet, file *ast.File) []int {
	var lines []int
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if selector.Sel.Name == "RunAndFinalize" {
			lines = append(lines, fileSet.Position(selector.Sel.Pos()).Line)
		}
		return true
	})
	return lines
}
