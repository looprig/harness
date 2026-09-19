package sessionwire

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

func TestReplyProjectionCasesMatchSealedReplyUnion(t *testing.T) {
	t.Parallel()
	want := methodReceiverTypesInPackage(t, filepath.Join("..", "event"), "isReply")
	got := make([]string, 0, len(replyProjectionCases(uuid.UUID{})))
	for _, test := range replyProjectionCases(uuid.UUID{}) {
		if _, ok := test.value.(event.Reply); !ok {
			t.Fatalf("fixture %T does not implement ReplyTo", test.value)
		}
		got = append(got, reflect.TypeOf(test.value).Name())
	}
	sort.Strings(got)
	if len(want) == 0 || len(got) == 0 {
		t.Fatalf("vacuous reply sets: Harness=%d adapter=%d", len(want), len(got))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("reply projection fixtures drifted from sealed Reply union\nHarness:\n%s\nadapter:\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
}

func TestProjectionPolicyMatchesHarnessSealedEventUnion(t *testing.T) {
	t.Parallel()
	want := methodReceiverTypesInPackage(t, filepath.Join("..", "event"), "isEvent")
	got := eventTypesInSwitch(t, "events.go", "classify")
	if len(want) == 0 || len(got) == 0 {
		t.Fatalf("vacuous event sets: Harness=%d adapter=%d", len(want), len(got))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("projection policy drifted from production isEvent union\nHarness:\n%s\nadapter:\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
}

func TestPackageDependencyBoundary(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"bytes": true, "encoding/json": true, "errors": true, "fmt": true, "reflect": true, "time": true,
		"github.com/looprig/core/content": true, "github.com/looprig/core/sessionwire/v1": true,
		// core/uuid is already in this package's closure through pkg/event; it is named
		// directly only so ReadScope.RuntimeSessionID can carry the rig id typed.
		"github.com/looprig/core/uuid":         true,
		"github.com/looprig/harness/pkg/event": true,
	}
	productionFiles := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		productionFiles++
		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote %s import: %v", entry.Name(), err)
			}
			if !allowed[path] {
				t.Errorf("%s imports forbidden dependency %q", entry.Name(), path)
			}
		}
	}
	if productionFiles == 0 {
		t.Fatal("dependency test is vacuous: zero production files")
	}
}

func TestCatalogStateProjectionMatchesHarnessClosedStateUnion(t *testing.T) {
	t.Parallel()

	want := constStringValuesWithPrefix(t, filepath.Join("..", "sessionstore", "catalog.go"), "State")
	got := []string{
		string(CatalogStateRunning),
		string(CatalogStateWaitingOnGate),
		string(CatalogStateIdle),
		string(CatalogStateFailed),
		string(CatalogStateInterrupted),
		string(CatalogStateStopped),
	}
	sort.Strings(got)
	if len(want) == 0 || len(got) == 0 {
		t.Fatalf("vacuous catalog state sets: Harness=%d adapter=%d", len(want), len(got))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("catalog state projection drifted from Harness closed state union\nHarness:\n%s\nadapter:\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
}

func constStringValuesWithPrefix(t *testing.T, path, prefix string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var values []string
	ast.Inspect(file, func(node ast.Node) bool {
		decl, ok := node.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			return true
		}
		for _, specNode := range decl.Specs {
			spec, ok := specNode.(*ast.ValueSpec)
			if !ok || len(spec.Names) != 1 || len(spec.Values) != 1 || !strings.HasPrefix(spec.Names[0].Name, prefix) {
				continue
			}
			literal, ok := spec.Values[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				continue
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatalf("unquote %s in %s: %v", spec.Names[0].Name, path, err)
			}
			values = append(values, value)
		}
		return true
	})
	sort.Strings(values)
	return values
}

func eventTypesInSwitch(t *testing.T, path, function string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var names []string
	ast.Inspect(file, func(node ast.Node) bool {
		decl, ok := node.(*ast.FuncDecl)
		if !ok || decl.Name.Name != function {
			return true
		}
		ast.Inspect(decl.Body, func(node ast.Node) bool {
			typeSwitch, ok := node.(*ast.TypeSwitchStmt)
			if !ok {
				return true
			}
			for _, clauseNode := range typeSwitch.Body.List {
				clause := clauseNode.(*ast.CaseClause)
				for _, expression := range clause.List {
					names = append(names, switchTypeName(t, expression, path))
				}
			}
			return false
		})
		return false
	})
	sort.Strings(names)
	return names
}

func switchTypeName(t *testing.T, expression ast.Expr, file string) string {
	t.Helper()
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		prefix, ok := value.X.(*ast.Ident)
		if !ok || prefix.Name != "event" {
			t.Fatalf("unsupported type-switch selector %T in %s", value.X, file)
		}
		return value.Sel.Name
	case *ast.StarExpr:
		return "*" + switchTypeName(t, value.X, file)
	default:
		t.Fatalf("unsupported type-switch expression %T in %s", expression, file)
		return ""
	}
}

func methodReceiverTypesInPackage(t *testing.T, dir, method string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, entry.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Name.Name != method || function.Recv == nil || len(function.Recv.List) != 1 {
				continue
			}
			names = append(names, methodReceiverTypeName(t, function.Recv.List[0].Type, method, entry.Name()))
		}
	}
	sort.Strings(names)
	for index := 1; index < len(names); index++ {
		if names[index] == names[index-1] {
			t.Fatalf("duplicate %s receiver %s", method, names[index])
		}
	}
	return names
}

func methodReceiverTypeName(t *testing.T, expression ast.Expr, method, file string) string {
	t.Helper()
	switch receiver := expression.(type) {
	case *ast.Ident:
		return receiver.Name
	case *ast.StarExpr:
		name, ok := receiver.X.(*ast.Ident)
		if !ok {
			t.Fatalf("unsupported pointer %s receiver %T in %s", method, receiver.X, file)
		}
		return "*" + name.Name
	default:
		t.Fatalf("unsupported %s receiver %T in %s", method, expression, file)
		return ""
	}
}
