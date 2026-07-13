package rig_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/looprig/harness"

func legacySourceViolations(filename string, source any) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), filename, source, 0)
	if err != nil {
		return nil, err
	}
	aliases := make(map[string]string)
	violations := make(map[string]bool)
	for _, imp := range file.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, err
		}
		kind := ""
		switch importPath {
		case modulePath + "/pkg/loop":
			kind = "loop"
		case modulePath + "/pkg/session":
			kind = "session"
		case modulePath + "/pkg/serve":
			kind = "serve"
		default:
			for _, segment := range strings.Split(importPath, "/") {
				if segment == "storekit" {
					kind = "storekit"
					violations["import "+importPath] = true
					break
				}
			}
		}
		if kind == "" {
			continue
		}
		alias := importPath[strings.LastIndex(importPath, "/")+1:]
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		if alias == "." {
			violations["dot import "+importPath] = true
			continue
		}
		if alias != "_" {
			aliases[alias] = kind
		}
	}

	selectorNames := make(map[*ast.Ident]bool)
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		selectorNames[selector.Sel] = true
		prefix, ok := selector.X.(*ast.Ident)
		if !ok || prefix.Obj != nil {
			return true
		}
		kind := aliases[prefix.Name]
		name := selector.Sel.Name
		switch {
		case kind == "loop" && (name == "Config" || name == "New" || name == "NewRestored" || name == "ToolSet"):
			violations["loop."+name] = true
		case kind == "session" && (name == "Runner" || name == "New" || name == "Restore" || name == "Compile" || strings.HasPrefix(name, "WithCompile")):
			violations["session."+name] = true
		case kind == "serve" && name == "Runner":
			violations["serve.Runner"] = true
		case kind == "storekit":
			violations["storekit."+name] = true
		}
		return true
	})
	ast.Inspect(file, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if !ok || selectorNames[identifier] {
			return true
		}
		if identifier.Name == "WithWorkspaceStore" || identifier.Name == "WithConfigFingerprintFields" ||
			identifier.Name == "WithForeignBuilder" || strings.HasPrefix(identifier.Name, "WithCompile") {
			violations[identifier.Name] = true
		}
		return true
	})

	out := make([]string, 0, len(violations))
	for violation := range violations {
		out = append(out, violation)
	}
	sort.Strings(out)
	return out, nil
}

func internalSessionRuntimeImportViolations(root string) ([]string, error) {
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".worktrees", "vendor", "docs", "examples", "testdata":
				if path != root {
					return filepath.SkipDir
				}
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
			if importPath != modulePath+"/internal/sessionruntime" {
				continue
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if !strings.HasPrefix(rel, "pkg/rig/") {
				violations = append(violations, rel)
			}
		}
		return nil
	})
	sort.Strings(violations)
	return violations, err
}

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
	violations, err := internalSessionRuntimeImportViolations(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range violations {
		t.Errorf("%s imports internal/sessionruntime; only pkg/rig may compose it", violation)
	}
}

func TestInternalSessionRuntimeImportGuardCoversWholeModule(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"internal/leak/leak.go", "cmd/leak/main.go", "pkg/rig/allowed.go", "testdata/ignored.go", ".worktrees/ignored/leak.go"} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package leak\nimport _ \""+modulePath+"/internal/sessionruntime\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	violations, err := internalSessionRuntimeImportViolations(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cmd/leak/main.go", "internal/leak/leak.go"}
	if strings.Join(violations, "\n") != strings.Join(want, "\n") {
		t.Fatalf("violations = %v, want %v", violations, want)
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
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".worktrees", "vendor", "docs", "examples", "testdata":
				if path != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		violations, err := legacySourceViolations(path, nil)
		if err != nil {
			return err
		}
		for _, violation := range violations {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s uses removed lifecycle surface %s", filepath.ToSlash(rel), violation)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLegacySourceGuardIgnoresCommentsStringsAndUnrelatedConfig(t *testing.T) {
	source := `package fixture
// loop.Config, session.Runner, WithWorkspaceStore
var message = "loop.New Config.Client Config.Model serve.Runner"
type Config struct { Client, Model any }
var local Config
var _ = local.Client
`
	got, err := legacySourceViolations("fixture.go", source)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("violations = %v, want none", got)
	}
}

func TestLegacySourceGuardResolvesAliasesAndRealUses(t *testing.T) {
	source := `package fixture
import (
    l "github.com/looprig/harness/pkg/loop"
    sess "github.com/looprig/harness/pkg/session"
    api "github.com/looprig/harness/pkg/serve"
    sk "github.com/looprig/storekit"
)
var _ l.Config
var _ sess.Runner
var _ api.Runner
var _ = sk.Open
func f() { WithWorkspaceStore(); WithCompileSession(); sess.WithCompileRestore() }
`
	got, err := legacySourceViolations("fixture.go", source)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"WithCompileSession", "WithWorkspaceStore", "import github.com/looprig/storekit",
		"loop.Config", "serve.Runner", "session.Runner", "session.WithCompileRestore", "storekit.Open",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("violations = %v, want %v", got, want)
	}
}

func TestLegacySourceGuardHonorsLexicalImportShadowing(t *testing.T) {
	source := `package fixture
import l "github.com/looprig/harness/pkg/loop"
var _ l.Definition
func f() {
    l := struct{ Config int }{}
    _ = l.Config
}
`
	got, err := legacySourceViolations("fixture.go", source)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("shadowed alias violations = %v, want none", got)
	}

	unshadowed := `package fixture
import l "github.com/looprig/harness/pkg/loop"
var _ l.Config
`
	got, err = legacySourceViolations("fixture.go", unshadowed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "\n") != "loop.Config" {
		t.Fatalf("unshadowed alias violations = %v, want [loop.Config]", got)
	}
}
