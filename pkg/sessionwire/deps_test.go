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
	want := eventTypesInSwitch(t, filepath.Join("..", "event", "validate.go"), "classify")
	got := eventTypesInSwitch(t, "events.go", "classify")
	if len(want) == 0 || len(got) == 0 {
		t.Fatalf("vacuous event switches: Harness=%d adapter=%d", len(want), len(got))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("projection policy drifted from sealed event union\nHarness:\n%s\nadapter:\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
}

func TestPackageDependencyBoundary(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"bytes": true, "encoding/json": true, "errors": true, "fmt": true, "reflect": true,
		"github.com/looprig/core/content": true, "github.com/looprig/core/sessionwire/v1": true,
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
					switch value := expression.(type) {
					case *ast.Ident:
						names = append(names, value.Name)
					case *ast.SelectorExpr:
						if prefix, ok := value.X.(*ast.Ident); ok && prefix.Name == "event" {
							names = append(names, value.Sel.Name)
						}
					}
				}
			}
			return false
		})
		return false
	})
	sort.Strings(names)
	return names
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
			receiver, ok := function.Recv.List[0].Type.(*ast.Ident)
			if !ok {
				t.Fatalf("unsupported %s receiver in %s", method, entry.Name())
			}
			names = append(names, receiver.Name)
		}
	}
	sort.Strings(names)
	return names
}
