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

// parseProductionGoFiles parses every production Go file in dir without applying
// the current platform's build constraints. Boundary guards must see declarations
// hidden behind inactive tags; package loading would intentionally omit them.
func parseProductionGoFiles(dir string, mode parser.Mode) ([]*ast.File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make([]*ast.File, 0, len(entries))
	set := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		file, err := parser.ParseFile(set, filepath.Join(dir, name), nil, mode)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, nil
}

// productionGoImports scans a boundary subtree recursively and without applying
// build constraints. A platform-only or nested package cannot hide a reversed
// dependency from the architectural guard.
func productionGoImports(root string) (map[string]bool, error) {
	imports := map[string]bool{}
	set := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "testdata" || entry.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return nil
		}
		file, err := parser.ParseFile(set, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			imports[imp.Path.Value] = true
		}
		return nil
	})
	return imports, err
}

func TestLoopBoundaryFileScanIncludesInactiveBuildTags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inactive.go")
	source := "//go:build boundary_never\n\npackage loop\n\nfunc New() {}\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	files, err := parseProductionGoFiles(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("parsed production files = %d, want 1 inactive-tag file", len(files))
	}
	if got := forbiddenLoopNames(files[0]); len(got) != 1 || got[0] != "New" {
		t.Fatalf("inactive-tag forbidden declarations = %v, want [New]", got)
	}
}

func TestLoopDependencyImportScanIncludesNestedInactiveBuildTags(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "inactive.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	source := "//go:build boundary_never\n\npackage nested\n\nimport _ \"github.com/looprig/harness/internal/loopruntime\"\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	imports, err := productionGoImports(root)
	if err != nil {
		t.Fatal(err)
	}
	if !imports["\"github.com/looprig/harness/internal/loopruntime\""] {
		t.Fatal("nested inactive-tag import was not scanned")
	}
}

func TestPublicLoopPackageHasNoActorConstructionSurface(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	files, err := parseProductionGoFiles(dir, 0)
	if err != nil {
		t.Fatalf("parse pkg/loop: %v", err)
	}
	for _, file := range files {
		if file.Name.Name != "loop" {
			continue
		}
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
	files, err := parseProductionGoFiles(filepath.Dir(file), 0)
	if err != nil {
		t.Fatalf("parse pkg/loop: %v", err)
	}
	exports := map[string]bool{}
	for _, file := range files {
		if file.Name.Name != "loop" {
			continue
		}
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
		imports, err := productionGoImports(dir)
		if err != nil {
			t.Fatalf("parse %s: %v", dir, err)
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
