package sessionruntime

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/harness/internal/loopruntime"
)

type stubToolResultObjects struct{}

func (stubToolResultObjects) PutToolResultObject(context.Context, string, []byte) error { return nil }

func (stubToolResultObjects) StatToolResultObject(context.Context, string) (loopruntime.ToolResultObjectStat, error) {
	return loopruntime.ToolResultObjectStat{}, nil
}

// TestWithToolResultCaptureWiresTheStore covers both halves of the option's
// contract: a store is installed, and a nil store leaves the unconfigured default
// rather than installing one the loop would nil-deref on.
func TestWithToolResultCaptureWiresTheStore(t *testing.T) {
	t.Parallel()
	var session Session
	WithToolResultCapture(nil)(&session)
	if session.toolResultObjects != nil {
		t.Fatal("a nil store was installed")
	}
	store := stubToolResultObjects{}
	WithToolResultCapture(store)(&session)
	if session.toolResultObjects != loopruntime.ToolResultObjectStore(store) {
		t.Fatalf("toolResultObjects = %#v, want the wired store", session.toolResultObjects)
	}
	WithToolResultCapture(nil)(&session)
	if session.toolResultObjects != loopruntime.ToolResultObjectStore(store) {
		t.Fatal("a later nil store cleared an already-wired store")
	}
}

// TestEveryLoopConstructionPassesTheCaptureStore holds the option to the layer
// that has the reader. WithToolResultCapture storing a field proves nothing on
// its own: the store only matters if every loop this session builds receives it,
// and this package builds loops at more than one site. The file set is derived by
// walking the package directory, so a site added later is covered without editing
// this test.
func TestEveryLoopConstructionPassesTheCaptureStore(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	fset := token.NewFileSet()
	sites := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			literal, ok := n.(*ast.CompositeLit)
			if !ok || !namesRuntimeDependencies(literal.Type) {
				return true
			}
			sites++
			for _, element := range literal.Elts {
				kv, isKV := element.(*ast.KeyValueExpr)
				if !isKV {
					continue
				}
				if key, isIdent := kv.Key.(*ast.Ident); isIdent && key.Name == "ToolResultObjects" {
					return true
				}
			}
			t.Errorf("%v: loopruntime.RuntimeDependencies is built without ToolResultObjects, so a loop from this site retains nothing",
				fset.Position(literal.Pos()))
			return true
		})
	}
	if sites == 0 {
		t.Fatal("no loopruntime.RuntimeDependencies construction found; the guard is vacuous")
	}
}

func namesRuntimeDependencies(expr ast.Expr) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "loopruntime" && selector.Sel.Name == "RuntimeDependencies"
}

// TestWithLifecycleToolResultCaptureForwardsToEverySession pins the forwarding
// hop, which is where a lifecycle option most easily becomes a no-op: the option
// must reach a Session's field through baseOpts, not merely be recorded on the
// Lifecycle.
func TestWithLifecycleToolResultCaptureForwardsToEverySession(t *testing.T) {
	t.Parallel()
	var lifecycle Lifecycle
	WithLifecycleToolResultCapture(nil)(&lifecycle)
	if len(lifecycle.baseOpts) != 0 {
		t.Fatalf("a nil store contributed %d base options, want 0", len(lifecycle.baseOpts))
	}
	store := stubToolResultObjects{}
	WithLifecycleToolResultCapture(store)(&lifecycle)
	if len(lifecycle.baseOpts) != 1 {
		t.Fatalf("base options = %d, want 1", len(lifecycle.baseOpts))
	}
	var session Session
	for _, option := range lifecycle.baseOpts {
		option(&session)
	}
	if session.toolResultObjects != loopruntime.ToolResultObjectStore(store) {
		t.Fatalf("session toolResultObjects = %#v, want the wired store", session.toolResultObjects)
	}
}
