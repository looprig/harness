package rig_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/looprig/harness"

func harnessRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

func TestOnlyRigImportsInternalSessionRuntime(t *testing.T) {
	root := harnessRoot(t)
	err := filepath.WalkDir(filepath.Join(root, "pkg"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if importPath == modulePath+"/internal/sessionruntime" {
				rel, _ := filepath.Rel(root, path)
				if !strings.HasPrefix(filepath.ToSlash(rel), "pkg/rig/") {
					t.Errorf("%s imports internal/sessionruntime; only pkg/rig may compose it", rel)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFinalBoundaryPackagesAreDocumented(t *testing.T) {
	root := harnessRoot(t)
	for _, rel := range []string{"pkg/loop", "pkg/session", "pkg/rig", "internal/loopruntime", "internal/sessionruntime"} {
		packages, err := parser.ParseDir(token.NewFileSet(), filepath.Join(root, filepath.FromSlash(rel)), func(info os.FileInfo) bool {
			return strings.HasSuffix(info.Name(), ".go") && !strings.HasSuffix(info.Name(), "_test.go")
		}, parser.PackageClauseOnly|parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, pkg := range packages {
			documented := false
			for _, file := range pkg.Files {
				if file.Doc != nil && strings.TrimSpace(file.Doc.Text()) != "" {
					documented = true
					break
				}
			}
			if !documented {
				t.Errorf("%s has no package comment", rel)
			}
		}
	}
}

func TestInternalSessionRuntimeHasNoSingleLoopCompatibilityConstructors(t *testing.T) {
	dir := filepath.Join(harnessRoot(t), "internal", "sessionruntime")
	packages, err := parser.ParseDir(token.NewFileSet(), dir, func(info os.FileInfo) bool {
		return strings.HasSuffix(info.Name(), ".go") && !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse internal/sessionruntime: %v", err)
	}
	for _, file := range packages["sessionruntime"].Files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && (fn.Name.Name == "NewLifecycle" || fn.Name.Name == "Restore") {
				t.Errorf("internal/sessionruntime retains single-loop compatibility constructor %s", fn.Name.Name)
			}
		}
	}
}

func TestNoLegacyLifecycleNamesInActiveSource(t *testing.T) {
	root := harnessRoot(t)
	forbiddenIdentifiers := map[string]bool{
		"WithWorkspace" + "Store":          true,
		"With" + "Compile":                 true,
		"WithConfig" + "FingerprintFields": true,
		"WithForeign" + "Builder":          true,
	}
	forbiddenSelectors := map[string]bool{
		"session." + "Runner": true,
		"serve." + "Runner":   true,
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == ".git" || name == "vendor" || name == "docs" || name == "examples" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if strings.Contains(importPath, "store"+"kit") {
					t.Errorf("%s imports a removed storage package path %q", path, importPath)
			}
		}
		file, err = parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.Ident:
				if forbiddenIdentifiers[n.Name] {
					t.Errorf("%s uses legacy identifier %s", path, n.Name)
				}
			case *ast.SelectorExpr:
				if pkg, ok := n.X.(*ast.Ident); ok && forbiddenSelectors[pkg.Name+"."+n.Sel.Name] {
					t.Errorf("%s uses legacy selector %s.%s", path, pkg.Name, n.Sel.Name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
