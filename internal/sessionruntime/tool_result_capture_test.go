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

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
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
	WithToolResultCapture(nil, "/spills")(&session)
	if session.toolResultObjects != nil {
		t.Fatal("a nil store was installed")
	}
	if session.toolResultSpillBase != "" {
		t.Fatal("a nil store still installed a spill base")
	}
	store := stubToolResultObjects{}
	WithToolResultCapture(store, "/spills")(&session)
	if session.toolResultObjects != loopruntime.ToolResultObjectStore(store) {
		t.Fatalf("toolResultObjects = %#v, want the wired store", session.toolResultObjects)
	}
	if session.toolResultSpillBase != "/spills" {
		t.Fatalf("toolResultSpillBase = %q, want the wired base", session.toolResultSpillBase)
	}
	WithToolResultCapture(nil, "/other")(&session)
	if session.toolResultObjects != loopruntime.ToolResultObjectStore(store) {
		t.Fatal("a later nil store cleared an already-wired store")
	}
	if session.toolResultSpillBase != "/spills" {
		t.Fatalf("toolResultSpillBase = %q, want the first wired base", session.toolResultSpillBase)
	}
}

// TestToolResultSpillDirectoryIsSessionScopedAndReleased pins the lazy accessor
// on all three of its outcomes: no base wired at all, a base that works (rooted
// at THIS session's id, and the same instance on a second call), and a base that
// cannot be established — which must yield an unavailable directory rather than
// nil, so a session wired for durable retention reports the failure instead of
// silently retaining nothing.
func TestToolResultSpillDirectoryIsSessionScopedAndReleased(t *testing.T) {
	t.Parallel()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}

	unwired := Session{sessionID: id}
	if unwired.toolResultSpillDirectory() != nil {
		t.Fatal("a session with no spill base produced a spill directory")
	}

	base := t.TempDir()
	wired := Session{sessionID: id, toolResultSpillBase: base}
	dir := wired.toolResultSpillDirectory()
	if dir == nil {
		t.Fatal("a wired spill base produced no directory")
	}
	if dir != wired.toolResultSpillDirectory() {
		t.Fatal("the spill directory was re-established on a second call")
	}
	root := filepath.Join(base, id.String())
	if _, err := os.Lstat(root); err != nil {
		t.Fatalf("lstat session spill root: %v", err)
	}
	wired.releaseToolResultSpills()
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("lstat after release = %v, want not-exist", err)
	}

	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}
	broken := Session{sessionID: id, toolResultSpillBase: blocked}
	if broken.toolResultSpillDirectory() == nil {
		t.Fatal("an unusable spill base produced a nil directory, which would silently disable retention")
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
			wired := 0
			for _, element := range literal.Elts {
				kv, isKV := element.(*ast.KeyValueExpr)
				if !isKV {
					continue
				}
				key, isIdent := kv.Key.(*ast.Ident)
				if !isIdent {
					continue
				}
				switch key.Name {
				case "ToolResultObjects":
					wired |= 1
				case "ToolResultSpills":
					wired |= 2
				}
			}
			if wired != 3 {
				t.Errorf("%v: loopruntime.RuntimeDependencies is built without ToolResultObjects and/or ToolResultSpills (mask %d), so a loop from this site retains nothing or spills nowhere",
					fset.Position(literal.Pos()), wired)
			}
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
	WithLifecycleToolResultCapture(nil, "/spills")(&lifecycle)
	if len(lifecycle.baseOpts) != 0 {
		t.Fatalf("a nil store contributed %d base options, want 0", len(lifecycle.baseOpts))
	}
	store := stubToolResultObjects{}
	WithLifecycleToolResultCapture(store, "/spills")(&lifecycle)
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
	if session.toolResultSpillBase != "/spills" {
		t.Fatalf("session toolResultSpillBase = %q, want the forwarded base", session.toolResultSpillBase)
	}
}

// TestSessionShutdownReleasesTheToolResultSpillRoot is the layer that has the
// reader for the shutdown cleanup: the accessor test above calls
// releaseToolResultSpills directly, which proves the method works and nothing
// about whether a real session ever calls it. Here a session is constructed with
// a spill base wired, its loop construction establishes the root, and Shutdown is
// what has to remove it.
func TestSessionShutdownReleasesTheToolResultSpillRoot(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	session, err := newTestSession(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("hi")}}),
		WithToolResultCapture(stubToolResultObjects{}, base))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	root := filepath.Join(base, session.sessionID.String())
	if _, err := os.Lstat(root); err != nil {
		t.Fatalf("lstat spill root after construction = %v, want the root a loop construction established", err)
	}
	if err := session.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("lstat spill root after Shutdown = %v, want not-exist", err)
	}
}
