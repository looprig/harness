package hustleruntime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestDependencyBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		forbidden []string
	}{
		{name: "runtime owns no concrete high-level capabilities", forbidden: []string{
			"github.com/looprig/harness/pkg/hub",
			"github.com/looprig/harness/pkg/session",
			"github.com/looprig/harness/pkg/rig",
			"github.com/looprig/harness/pkg/tool",
			"github.com/looprig/harness/internal/sessionruntime",
		}},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			entries, err := os.ReadDir(".")
			if err != nil {
				t.Fatal(err)
			}
			fileSet := token.NewFileSet()
			for _, entry := range entries {
				filename := entry.Name()
				if entry.IsDir() || !strings.HasSuffix(filename, ".go") || strings.HasSuffix(filename, "_test.go") {
					continue
				}
				file, parseErr := parser.ParseFile(fileSet, filename, nil, parser.ImportsOnly)
				if parseErr != nil {
					t.Errorf("parse %s: %v", filename, parseErr)
					continue
				}
				ast.Inspect(file, func(node ast.Node) bool {
					importSpec, ok := node.(*ast.ImportSpec)
					if !ok {
						return true
					}
					path, unquoteErr := strconv.Unquote(importSpec.Path.Value)
					if unquoteErr != nil {
						t.Errorf("%s import path: %v", filename, unquoteErr)
						return false
					}
					for _, forbidden := range testCase.forbidden {
						if path == forbidden || strings.HasPrefix(path, forbidden+"/") {
							t.Errorf("%s imports forbidden dependency %q", filename, path)
						}
					}
					return false
				})
			}
		})
	}
}
