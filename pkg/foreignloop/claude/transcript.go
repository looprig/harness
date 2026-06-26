package claude

import (
	"path/filepath"
	"regexp"
	"strings"
)

// PathError is the typed, fail-closed failure of transcript-path derivation: a sid
// that is not a plain UUID, or a derived path that escapes the projects root.
type PathError struct{ Reason string }

func (e *PathError) Error() string { return "claude: transcript path: " + e.Reason }

// sidPattern matches a plain UUID. A sid that is not a plain UUID is rejected before
// it is ever joined into a filesystem path, so it can carry no '/', '\', or '..'.
var sidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const (
	claudeDir   = ".claude"
	projectsDir = "projects"
	transcript  = ".jsonl"
)

// transcriptPath derives the deterministic on-disk transcript path Claude Code uses
// for (cwd, sid). It fails closed (returns *PathError) on a non-UUID sid or a derived
// path that escapes the projects root.
//
// ASSUMPTION: Claude Code names its project dir by filepath.Clean(cwd) with every '/'
// replaced by '-' (so /Users/x/y -> -Users-x-y). The exact encoding (e.g. how dots or
// other characters are handled) is pinned by the E4 integration test against the real
// binary; if it diverges the transcript soft-degrades rather than corrupts.
func transcriptPath(home, cwd, sid string) (string, error) {
	if !sidPattern.MatchString(sid) {
		return "", &PathError{Reason: "sid is not a plain UUID"}
	}
	encoded := strings.ReplaceAll(filepath.Clean(cwd), string(filepath.Separator), "-")
	root := filepath.Clean(filepath.Join(home, claudeDir, projectsDir))
	full := filepath.Clean(filepath.Join(root, encoded, sid+transcript))
	if err := within(root, full); err != nil {
		return "", err
	}
	return full, nil
}

// within verifies full stays inside root: filepath.Rel must succeed and must not
// escape upward with "..". Defense in depth behind the sid regex + cwd Clean.
func within(root, full string) error {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return &PathError{Reason: "path is not relative to projects root"}
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return &PathError{Reason: "path escapes projects root"}
	}
	return nil
}
