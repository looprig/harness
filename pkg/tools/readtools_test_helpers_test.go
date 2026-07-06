package tools

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/loop"
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

// patternReadGuard is a loop.ReadGuard double that decides DeniedRead with a
// PREDICATE over the absolute path rather than an enumerated set. It models the
// §10.5 read-adaptation seam faithfully: the swe sandbox adapter derives
// DeniedRead from the sandbox Policy's read RULES (globs like "**/.env*"), not a
// fixed path list, so pinning the seam with a rule-shaped guard proves the native
// read tools honour a policy-derived deny, not just an exact path.
type patternReadGuard struct {
	deny     func(absPath string) bool
	maxBytes int64
}

func (g *patternReadGuard) DeniedRead(absPath string) bool { return g.deny(absPath) }
func (g *patternReadGuard) MaxReadBytes() int64            { return g.maxBytes }

// compile-time assertion that the pattern guard satisfies the narrow read guard.
var _ loop.ReadGuard = (*patternReadGuard)(nil)

// denyDotEnv models the §5.3 secret deny-read "**/.env*": it returns true for any
// absolute path whose FINAL component begins with ".env" (.env, .env.local,
// .env.production, …) at ANY directory depth — the same set the doublestar glob
// "**/.env*" selects, expressed with stdlib only. Per the DeniedRead canonical-path
// contract this match is purely lexical; the caller feeds an absolute, cleaned,
// symlink-resolved path (which the native read tools do).
func denyDotEnv(absPath string) bool {
	return strings.HasPrefix(filepath.Base(absPath), ".env")
}

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
