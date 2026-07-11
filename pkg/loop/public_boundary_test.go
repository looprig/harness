package loop_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPublicLoopPackageHasNoActorConstructionSurface(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	packages, err := parser.ParseDir(token.NewFileSet(), dir, nil, 0)
	if err != nil {
		t.Fatalf("parse pkg/loop: %v", err)
	}
	forbidden := map[string]bool{
		"New": true, "NewRestored": true, "Loop": true, "Config": true, "ToolSet": true,
	}
	for _, file := range packages["loop"].Files {
		for _, decl := range file.Decls {
			switch declaration := decl.(type) {
			case *ast.FuncDecl:
				if declaration.Recv == nil && forbidden[declaration.Name.Name] {
					t.Errorf("pkg/loop still exports actor constructor %s", declaration.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					if named, ok := spec.(*ast.TypeSpec); ok && forbidden[named.Name.Name] {
						t.Errorf("pkg/loop still exports actor type %s", named.Name.Name)
					}
				}
			}
		}
	}
}

func TestLoopRuntimeDependencyPointsTowardPublicContracts(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	publicDir := filepath.Dir(file)
	root := filepath.Dir(filepath.Dir(publicDir))
	runtimeDir := filepath.Join(root, "internal", "loopruntime")

	parseImports := func(dir string) map[string]bool {
		t.Helper()
		packages, err := parser.ParseDir(token.NewFileSet(), dir, func(info os.FileInfo) bool {
			return filepath.Ext(info.Name()) == ".go" && !strings.HasSuffix(info.Name(), "_test.go")
		}, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", dir, err)
		}
		imports := map[string]bool{}
		for _, pkg := range packages {
			for _, source := range pkg.Files {
				for _, imp := range source.Imports {
					imports[imp.Path.Value] = true
				}
			}
		}
		return imports
	}
	publicImports := parseImports(publicDir)
	if publicImports["\"github.com/looprig/harness/internal/loopruntime\""] {
		t.Fatal("pkg/loop imports internal/loopruntime")
	}
	runtimeImports := parseImports(runtimeDir)
	if !runtimeImports["\"github.com/looprig/harness/pkg/loop\""] {
		t.Fatal("internal/loopruntime does not import pkg/loop contracts")
	}
}
