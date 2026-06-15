package tools

import (
	"context"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/tool"
)

// glob.go implements the Glob tool: a workspace-contained, denied-path-excluding
// filename search using the shared `**`-aware matchGlob over WalkDir-discovered
// entries (design §4b). It is AutoApprove and Auditable (pattern/root only).

// maxGlobResults caps the number of paths Glob returns so a broad pattern cannot
// flood the model context. A larger match set is truncated with a notice.
const maxGlobResults = 500

// globToolName is the EXACT tool name classifyTool keys on for the read class.
const globToolName = toolGlob

// globSchema is the JSON Schema for Glob's args. Field names (pattern/root) are
// the boundary-extraction contract shared with check.go.
const globSchema = `{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Glob pattern matched against workspace-relative paths. '**' matches across directories; '*'/'?'/'[...]' match within a single path segment."},
    "root": {"type": "string", "description": "Workspace-relative directory to search under (optional; defaults to the workspace root)."}
  },
  "required": ["pattern"]
}`

const globDesc = "List workspace files whose path matches a glob pattern. '**' spans directories; other wildcards stay within one segment. Results are confined to the workspace, exclude denied (secret) paths, and are capped."

// globArgs is the typed decode of Glob's untrusted argsJSON.
type globArgs struct {
	Pattern string `json:"pattern"`
	Root    string `json:"root"`
}

// Glob searches workspace filenames against a glob pattern. It depends only on
// the workspace root and the narrow loop.ReadGuard (least privilege).
type Glob struct {
	root  string
	guard loop.ReadGuard
}

// NewGlob constructs a Glob bound to the workspace root and read guard.
func NewGlob(root string, guard loop.ReadGuard) *Glob {
	return &Glob{root: root, guard: guard}
}

// Info returns Glob's self-description. Name MUST equal "Glob".
func (g *Glob) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   globToolName,
		Desc:   globDesc,
		Schema: json.RawMessage(globSchema),
	}, nil
}

// AuditSummary returns a redacted one-line summary: the pattern (and root if
// present) only — never any matched path contents.
func (g *Glob) AuditSummary(argsJSON string) string {
	var a globArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || a.Pattern == "" {
		return "Glob (unparsable args)"
	}
	if a.Root != "" {
		return "Glob " + a.Pattern + " in " + a.Root
	}
	return "Glob " + a.Pattern
}

// InvokableRun walks the (contained) search root, matches each workspace-relative
// path against the pattern, EXCLUDES any path DeniedRead reports, caps results,
// and returns a newline-separated list. Every failure is a tool-result string.
func (g *Glob) InvokableRun(_ context.Context, argsJSON string) (*tool.ToolResult, error) {
	var a globArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return tool.TextResult("error: invalid arguments: not a JSON object"), nil
	}
	if a.Pattern == "" {
		return tool.TextResult("error: a non-empty 'pattern' is required"), nil
	}

	searchRel := a.Root
	if searchRel == "" {
		searchRel = defaultSearchDir
	}
	// Contain the search root (rejects an escape; resolves symlinks). The returned
	// abs path is the WalkDir start; the workspace-relative anchor for matching is
	// computed against the resolved workspace root.
	searchAbs, err := containedPath(g.root, searchRel)
	if err != nil {
		return tool.TextResult("error: search root is outside the workspace: " + searchRel), nil
	}
	resolvedRoot, err := resolveAbsRoot(g.root)
	if err != nil {
		return tool.TextResult("error: workspace root could not be resolved"), nil
	}

	matches, truncated := g.walk(searchAbs, resolvedRoot, a.Pattern)
	return tool.TextResult(renderGlobResults(matches, truncated)), nil
}

// walk traverses searchAbs, returning the workspace-relative (slash) paths whose
// relPath matches pattern and that DeniedRead does NOT exclude, sorted and capped
// at maxGlobResults. truncated reports whether the cap was hit. A WalkDir error on
// a single entry is skipped (best-effort listing), never fatal.
func (g *Glob) walk(searchAbs, resolvedRoot, pattern string) (matches []string, truncated bool) {
	_ = filepath.WalkDir(searchAbs, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable entry (permissions, races): skip it, keep walking.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		// Authoritative denied-path exclusion: never leak a secret's name. abs is
		// the symlink-unresolved WalkDir path; resolve it the same way containedPath
		// would so DeniedRead's absolute-path contract holds.
		denyAbs := abs
		if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
			denyAbs = resolved
		}
		if g.guard.DeniedRead(denyAbs) {
			return nil
		}
		rel, rerr := filepath.Rel(resolvedRoot, denyAbs)
		if rerr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if !matchGlob(pattern, relSlash) {
			return nil
		}
		matches = append(matches, relSlash)
		if len(matches) > maxGlobResults {
			truncated = true
			return errStopWalk
		}
		return nil
	})
	if len(matches) > maxGlobResults {
		matches = matches[:maxGlobResults]
		truncated = true
	}
	sort.Strings(matches)
	return matches, truncated
}

// errStopWalk is the sentinel that short-circuits WalkDir once the result cap is
// reached. It is a leaf control-flow sentinel (never surfaced to a caller), so a
// bare errors.New is permitted per CLAUDE.md.
var errStopWalk = stopWalkError{}

// stopWalkError is the typed sentinel returned to abort WalkDir at the cap.
type stopWalkError struct{}

func (stopWalkError) Error() string { return "glob: result cap reached" }

// renderGlobResults formats the sorted match list, appending a truncation notice
// when the cap was hit. An empty list reports "no matches".
func renderGlobResults(matches []string, truncated bool) string {
	if len(matches) == 0 {
		return "no matches"
	}
	out := strings.Join(matches, "\n")
	if truncated {
		out += "\n[truncated: more than " + strconv.Itoa(maxGlobResults) + " matches; refine the pattern]"
	}
	return out
}

// compile-time assertions.
var (
	_ tool.InvokableTool = (*Glob)(nil)
	_ tool.Auditable     = (*Glob)(nil)
)
