package sessionruntime

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hub"
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

// TestSessionShutdownReportsAFailedSpillRelease is the reader for the release
// error reaching Shutdown's failure list rather than being dropped. Every other
// teardown phase reports through that list; a discarded RemoveAll would leave the
// session's complete tool output on disk with no signal at all, which on a pooled
// host is a disk-fill vector nobody can see.
func TestSessionShutdownReportsAFailedSpillRelease(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	session, err := newTestSession(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("hi")}}),
		WithToolResultCapture(stubToolResultObjects{}, base))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	root := filepath.Join(base, session.sessionID.String())
	if _, err := os.Lstat(root); err != nil {
		t.Fatalf("lstat spill root after construction: %v", err)
	}
	// Removing an entry needs write permission on its PARENT, so a read-and-execute
	// base makes RemoveAll fail while leaving the root itself perfectly readable.
	if err := os.Chmod(base, 0o500); err != nil {
		t.Fatalf("chmod base: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(base, 0o700) })

	shutdownErr := session.Shutdown(context.Background())
	if _, statErr := os.Lstat(root); statErr != nil {
		t.Skipf("this environment removed the root from a read-only parent (running as root?): %v", statErr)
	}
	if shutdownErr == nil {
		t.Fatal("Shutdown reported success while the spill root it could not remove is still on disk")
	}
	var spillErr *loopruntime.ToolResultSpillError
	if !errors.As(shutdownErr, &spillErr) {
		t.Fatalf("Shutdown error = %v, want it to carry the spill release failure", shutdownErr)
	}
	if spillErr.Reason != "remove spill root" {
		t.Fatalf("spill failure reason = %q, want %q", spillErr.Reason, "remove spill root")
	}
}

// TestSessionSpillEstablishmentAndReleaseAreOrderedUnderConcurrency is the
// Session-layer test the two single-goroutine tests above cannot stand in for.
// Loop construction calls toolResultSpillDirectory WITHOUT loopsMu held
// (session.go's newLoopWithAdmission unlocks before building the runtime), while
// teardown calls releaseToolResultSpills under a different path, so the two
// really can run concurrently on a live session: a Session.NewLoop or a delegate
// spawn that passes the closing gate before teardown latches it lands here.
//
// Two properties, and the second is the one that leaks. The pair must be free of
// data races — a sync.Once orders only the goroutines that CALL Do, so a bare read
// of the field would be unordered against the establishing write. And whichever
// side wins, the session's spill base must be EMPTY afterwards: a construction
// that reaches the accessor after teardown must create nothing, because nothing is
// left to remove it.
func TestSessionSpillEstablishmentAndReleaseAreOrderedUnderConcurrency(t *testing.T) {
	t.Parallel()
	for trial := 0; trial < 64; trial++ {
		base := t.TempDir()
		id, err := uuid.New()
		if err != nil {
			t.Fatalf("uuid.New: %v", err)
		}
		session := &Session{sessionID: id, toolResultSpillBase: base}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			session.toolResultSpillDirectory()
		}()
		go func() {
			defer wg.Done()
			<-start
			session.releaseToolResultSpills()
		}()
		close(start)
		wg.Wait()
		entries, readErr := os.ReadDir(base)
		if readErr != nil {
			t.Fatalf("trial %d: read base: %v", trial, readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("trial %d: spill base holds %d entries after a raced teardown; a root was created with nothing left to remove it", trial, len(entries))
		}
	}
}

// TestSessionSpillDirectoryAfterReleaseCreatesNothing pins the same property
// deterministically, in the order that leaks: the release runs FIRST, and the
// later loop construction must get a directory that establishes nothing on disk.
// The concurrent probe above can only sample this ordering; this one guarantees it.
func TestSessionSpillDirectoryAfterReleaseCreatesNothing(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	session := &Session{sessionID: id, toolResultSpillBase: base}
	session.releaseToolResultSpills()
	dir := session.toolResultSpillDirectory()
	if dir == nil {
		t.Fatal("a wired spill base produced no directory after release; retention would silently fall back to memory")
	}
	entries, readErr := os.ReadDir(base)
	if readErr != nil {
		t.Fatalf("read base: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("spill base holds %d entries after a post-teardown construction, want none", len(entries))
	}
	if _, err := os.Lstat(filepath.Join(base, id.String())); !os.IsNotExist(err) {
		t.Fatalf("lstat session root = %v, want not-exist", err)
	}
}

// TestAbortConstructionReleasesTheToolResultSpillRoot is the reader for the
// SECOND release call site, and for that alone. A session that fails during
// construction never reaches teardown, so abortConstruction is the only thing that
// can remove a spill root a loop built before the failure — and a construction
// abort is exactly when a half-built session is most likely to have one.
//
// It is deliberately NOT a reader for the sync.Once claim: it is single-goroutine,
// so it passes with the Once claim removed. The two readers for that are
// TestSessionSpillEstablishmentAndReleaseAreOrderedUnderConcurrency and
// TestSessionSpillDirectoryAfterReleaseCreatesNothing.
func TestAbortConstructionReleasesTheToolResultSpillRoot(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	session := &Session{
		sessionID:           id,
		sessionCtx:          ctx,
		sessionCancel:       cancel,
		hub:                 hub.New(id),
		checkpointAdmission: newCheckpointAdmissionGate(),
		loops:               map[uuid.UUID]*loopHandle{},
		gates:               map[gate.ID]gateEntry{},
		gateTimers:          map[gate.ID]*time.Timer{},
		toolResultSpillBase: base,
	}
	// A loop built before the failure is what puts a root on disk.
	if session.toolResultSpillDirectory() == nil {
		t.Fatal("no spill directory was established")
	}
	root := filepath.Join(base, id.String())
	if _, err := os.Lstat(root); err != nil {
		t.Fatalf("lstat spill root before abort: %v", err)
	}
	session.abortConstruction(errors.New("construction failed"))
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("lstat spill root after abortConstruction = %v, want not-exist", err)
	}
}
