package loop_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

var forbiddenLoopSurface = map[string]bool{
	"New": true, "NewRestored": true, "Loop": true, "Config": true, "ToolSet": true,
}

func forbiddenLoopNames(file *ast.File) []string {
	var names []string
	for _, decl := range file.Decls {
		switch node := decl.(type) {
		case *ast.FuncDecl:
			if node.Recv == nil && forbiddenLoopSurface[node.Name.Name] {
				names = append(names, node.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range node.Specs {
				switch named := spec.(type) {
				case *ast.TypeSpec:
					if forbiddenLoopSurface[named.Name.Name] {
						names = append(names, named.Name.Name)
					}
				case *ast.ValueSpec:
					for _, name := range named.Names {
						if forbiddenLoopSurface[name.Name] {
							names = append(names, name.Name)
						}
					}
				}
			}
		}
	}
	sort.Strings(names)
	return names
}

func forbiddenLoopDeclarations(filename, source string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), filename, source, 0)
	if err != nil {
		return nil, err
	}
	return forbiddenLoopNames(file), nil
}

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
	for _, file := range packages["loop"].Files {
		for _, name := range forbiddenLoopNames(file) {
			t.Errorf("pkg/loop still exports forbidden actor surface %s", name)
		}
	}
}

func TestLoopBoundaryGuardRejectsExportedValueAliases(t *testing.T) {
	source := `package loop
var New = func() {}
var NewRestored = New
var Config, ToolSet any
`
	got, err := forbiddenLoopDeclarations("fixture.go", source)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Config", "New", "NewRestored", "ToolSet"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("forbidden declarations = %v, want %v", got, want)
	}
}

func TestPublicLoopPackageExportsFinalContracts(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	packages, err := parser.ParseDir(token.NewFileSet(), filepath.Dir(file), nil, 0)
	if err != nil {
		t.Fatalf("parse pkg/loop: %v", err)
	}
	exports := map[string]bool{}
	for _, file := range packages["loop"].Files {
		for _, decl := range file.Decls {
			switch declaration := decl.(type) {
			case *ast.FuncDecl:
				if declaration.Recv == nil && ast.IsExported(declaration.Name.Name) {
					exports[declaration.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					if named, ok := spec.(*ast.TypeSpec); ok && ast.IsExported(named.Name.Name) {
						exports[named.Name.Name] = true
					}
				}
			}
		}
	}
	for _, name := range []string{"Define", "Definition", "Mode", "Handle", "Controller"} {
		if !exports[name] {
			t.Errorf("pkg/loop does not export final contract %s", name)
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
