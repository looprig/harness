package session_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/session"
)

// publicSessionContracts are the contract interfaces pkg/session is allowed to
// export. Everything else it exports must be an error type: the package is
// contracts plus errors, and construction belongs to rig.
//
// GateHost is separate from the two session views on purpose — it is the
// integration host's end of a gate, not an operator's view of a session — and it
// is listed here rather than folded into SessionController for the reasons on the
// type itself.
var publicSessionContracts = map[string]bool{
	"Session": true, "SessionController": true, "GateHost": true,
}

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
						if !publicSessionContracts[name] && !strings.HasSuffix(name, "Error") && !strings.HasSuffix(name, "ErrorKind") {
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

func TestSessionContractsDoNotExposeGenericHustleExecution(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		contract   reflect.Type
		methodName string
	}{
		{name: "data plane has no generic runner", contract: reflect.TypeOf((*session.Session)(nil)).Elem(), methodName: "RunHustle"},
		{name: "controller has no generic runner", contract: reflect.TypeOf((*session.SessionController)(nil)).Elem(), methodName: "RunHustle"},
		{name: "data plane has no generic invoke", contract: reflect.TypeOf((*session.Session)(nil)).Elem(), methodName: "InvokeHustle"},
		{name: "controller has no generic invoke", contract: reflect.TypeOf((*session.SessionController)(nil)).Elem(), methodName: "InvokeHustle"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, exists := tt.contract.MethodByName(tt.methodName); exists {
				t.Fatalf("%s exposes forbidden generic hustle method %s", tt.contract.Name(), tt.methodName)
			}
		})
	}
}

func TestSessionContractsExposeOnlyFocusedCompaction(t *testing.T) {
	t.Parallel()
	dataPlane := reflect.TypeOf((*session.Session)(nil)).Elem()
	controller := reflect.TypeOf((*session.SessionController)(nil)).Elem()
	tests := []struct {
		name       string
		contract   reflect.Type
		methodName string
		want       bool
	}{
		{name: "session exposes active convenience", contract: dataPlane, methodName: "Compact", want: true},
		{name: "session exposes exact target", contract: dataPlane, methodName: "CompactToLoop", want: true},
		{name: "controller inherits active convenience", contract: controller, methodName: "Compact", want: true},
		{name: "controller inherits exact target", contract: controller, methodName: "CompactToLoop", want: true},
		{name: "session has no arbitrary rewrite", contract: dataPlane, methodName: "RewriteContext"},
		{name: "controller has no arbitrary rewrite", contract: controller, methodName: "RewriteContext"},
		{name: "session has no supplied summary", contract: dataPlane, methodName: "CompactWithSummary"},
		{name: "controller has no generic compaction runner", contract: controller, methodName: "RunCompaction"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, exists := tt.contract.MethodByName(tt.methodName)
			if exists != tt.want {
				t.Fatalf("%s method %s exists=%v, want %v", tt.contract.Name(), tt.methodName, exists, tt.want)
			}
		})
	}
}
