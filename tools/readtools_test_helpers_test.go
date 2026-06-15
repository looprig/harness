package tools

import (
	"path/filepath"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop"
)

// fakeReadGuard is a configurable test double for loop.ReadGuard. denied holds
// the set of ABSOLUTE paths DeniedRead reports true for; maxBytes is returned by
// MaxReadBytes. It exercises the read tools' two checks (denied-path filtering
// and the read cap) without depending on the concrete PermissionChecker.
type fakeReadGuard struct {
	denied   map[string]bool
	maxBytes int64
}

// newFakeReadGuard builds a fakeReadGuard with the given byte cap and the given
// absolute paths marked denied.
func newFakeReadGuard(maxBytes int64, deniedAbs ...string) *fakeReadGuard {
	d := make(map[string]bool, len(deniedAbs))
	for _, p := range deniedAbs {
		d[p] = true
	}
	return &fakeReadGuard{denied: d, maxBytes: maxBytes}
}

func (g *fakeReadGuard) DeniedRead(absPath string) bool { return g.denied[absPath] }
func (g *fakeReadGuard) MaxReadBytes() int64            { return g.maxBytes }

// compile-time assertion that the fake satisfies the narrow read guard.
var _ loop.ReadGuard = (*fakeReadGuard)(nil)

// resolvedJoin returns the symlink-resolved absolute path of rel under root —
// the exact form containedPath produces and DeniedRead's contract expects (on
// macOS t.TempDir() lives under a /var -> /private/var symlink, so the raw
// filepath.Join would not match the resolved path the tool passes to DeniedRead).
func resolvedJoin(t *testing.T, root, rel string) string {
	t.Helper()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", root, err)
	}
	abs, err := filepath.Abs(filepath.Join(resolvedRoot, rel))
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	return abs
}
