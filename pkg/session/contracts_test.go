package session_test

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

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/session"
)

var forbiddenSessionSurface = map[string]bool{
	"New": true, "Restore": true, "Compile": true, "Runner": true, "Option": true, "CompileOption": true,
}

func forbiddenSessionNames(file *ast.File) []string {
	var names []string
	for _, decl := range file.Decls {
		switch node := decl.(type) {
		case *ast.FuncDecl:
			if node.Recv == nil && forbiddenSessionSurface[node.Name.Name] {
				names = append(names, node.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range node.Specs {
				switch named := spec.(type) {
				case *ast.TypeSpec:
					if forbiddenSessionSurface[named.Name.Name] {
						names = append(names, named.Name.Name)
					}
				case *ast.ValueSpec:
					for _, name := range named.Names {
						if forbiddenSessionSurface[name.Name] {
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

func forbiddenSessionDeclarations(filename, source string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), filename, source, 0)
	if err != nil {
		return nil, err
	}
	return forbiddenSessionNames(file), nil
}

func TestPublicSessionContractsAreInterfaces(t *testing.T) {
	t.Parallel()
	var _ interface{ SessionID() uuid.UUID } = session.Session(nil)
	var _ session.Session = (session.SessionController)(nil)
}

func TestRestoreNoPrimerLoopWireValue(t *testing.T) {
	t.Parallel()
	if got, want := string(session.RestoreNoPrimerLoop), "no_primer_loop"; got != want {
		t.Fatalf("RestoreNoPrimerLoop = %q, want %q", got, want)
	}
}

func TestSessionBoundaryGuardRejectsExportedValueAliases(t *testing.T) {
	source := `package session
var New = func() {}
var Restore = New
var Compile = New
var Runner any
`
	got, err := forbiddenSessionDeclarations("fixture.go", source)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Compile", "New", "Restore", "Runner"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("forbidden declarations = %v, want %v", got, want)
	}
}

func TestPublicSessionContainsOnlyContractsAndErrors(t *testing.T) {
	t.Parallel()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	packages, err := parser.ParseDir(token.NewFileSet(), dir, func(info os.FileInfo) bool {
		return strings.HasSuffix(info.Name(), ".go") && !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse pkg/session: %v", err)
	}
	for _, file := range packages["session"].Files {
		for _, decl := range file.Decls {
			switch node := decl.(type) {
			case *ast.FuncDecl:
				if node.Recv == nil && ast.IsExported(node.Name.Name) {
					t.Errorf("pkg/session exports package function %s; construction and helpers belong to rig/internal runtime", node.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range node.Specs {
					switch named := spec.(type) {
					case *ast.TypeSpec:
						if !ast.IsExported(named.Name.Name) {
							continue
						}
						name := named.Name.Name
						if name != "Session" && name != "SessionController" && !strings.HasSuffix(name, "Error") && !strings.HasSuffix(name, "ErrorKind") {
							t.Errorf("pkg/session exports non-contract, non-error type %s", name)
						}
					case *ast.ValueSpec:
						for _, name := range named.Names {
							if forbiddenSessionSurface[name.Name] {
								t.Errorf("pkg/session exports forbidden lifecycle value %s", name.Name)
							}
						}
					}
				}
			}
		}
	}
}

func TestOldLifecycleSurfaceIsAbsent(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "contracts_test.go" || len(entry.Name()) < 3 || entry.Name()[len(entry.Name())-3:] != ".go" {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range forbiddenSessionNames(file) {
			t.Errorf("old public lifecycle declaration %s remains in %s", name, entry.Name())
		}
	}
}
