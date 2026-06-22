package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tool"
)

// glob.go implements the Glob tool: a workspace-contained, denied-path-excluding
// filename search using the shared `**`-aware matchGlob over WalkDir-discovered
// entries (design §4b). It is AutoApprove and Auditable (pattern/root only).

// maxGlobResults caps the number of paths Glob returns so a broad pattern cannot
// flood the model context. A larger match set is truncated with a notice.
const maxGlobResults = 500

// globNoiseDirs are directory names Glob prunes at the directory level (it does
// not descend into them) by DEFAULT, so `**/*` over a workspace never floods the
// model with VCS internals or large generated trees that are never useful to a
// coding agent (a real provider context-window overflow motivated this). `.git`
// is the must-have — its hundreds of objects/refs are pure noise; the rest are
// well-known heavy/generated trees (dependencies, build output, editor metadata).
// Pruning is bypassed when the search ROOT is itself one of these dirs (the user
// explicitly targeted it), mirroring grep's grepNoiseDirs behaviour. The set is
// kept intentionally parallel to grep's grepNoiseDirs (same purpose, two callers);
// duplicating this short, stable list keeps each read tool self-contained.
var globNoiseDirs = map[string]bool{
	".git":         true, // VCS internals: objects/refs/logs — never useful to an agent.
	".hg":          true, // Mercurial internals.
	".svn":         true, // Subversion internals.
	"node_modules": true, // JS dependencies — huge, generated.
	"vendor":       true, // vendored deps (Go and others).
	"dist":         true, // build/distribution output.
	"build":        true, // build output.
	"target":       true, // Rust/Java (cargo/maven) build output.
	".next":        true, // Next.js build cache.
	"__pycache__":  true, // Python bytecode cache.
}

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
func (g *Glob) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
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

	matches, truncated, expired := g.walk(ctx, searchAbs, resolvedRoot, a.Pattern)
	if expired {
		return tool.TextResult("error: glob timed out"), nil
	}
	return tool.TextResult(renderGlobResults(matches, truncated)), nil
}

// walk traverses searchAbs, returning the workspace-relative (slash) paths whose
// relPath matches pattern and that the shared deny-filter does NOT exclude, sorted
// and capped at maxGlobResults. truncated reports whether the cap was hit; expired
// reports that ctx was cancelled/expired mid-walk (the caller renders a timeout
// instead of a partial listing). A WalkDir error on a single entry is skipped
// (best-effort listing), never fatal.
func (g *Glob) walk(ctx context.Context, searchAbs, resolvedRoot, pattern string) (matches []string, truncated, expired bool) {
	walkErr := filepath.WalkDir(searchAbs, func(abs string, d fs.DirEntry, err error) error {
		// Cheap cancellability: abort before touching this entry if ctx is done so a
		// huge tree cannot block past cancellation.
		if ctx.Err() != nil {
			return errCtxCancelled
		}
		if err != nil {
			// Unreadable entry (permissions, races): skip it, keep walking.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// Prune noise/VCS dirs at the directory level (don't descend) so a broad
			// pattern cannot flood the model context. The search root itself is never
			// pruned, so an explicit `root: ".git"` is still honoured.
			if abs != searchAbs && globNoiseDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		// Authoritative denied-path exclusion (shared helper): never leak a secret's
		// name. A denied or non-relativisable path is excluded.
		relSlash, denied := denyFilteredRel(g.guard, resolvedRoot, abs)
		if denied {
			return nil
		}
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
	if errors.Is(walkErr, errCtxCancelled) {
		return nil, false, true
	}
	if len(matches) > maxGlobResults {
		matches = matches[:maxGlobResults]
		truncated = true
	}
	sort.Strings(matches)
	return matches, truncated, false
}

// errStopWalk is the shared sentinel that short-circuits a WalkDir traversal
// (Glob and Grep) once the result cap is reached. It is a leaf control-flow
// sentinel, never surfaced to a caller.
var errStopWalk = stopWalkError{}

// stopWalkError is the typed sentinel returned to abort WalkDir at the cap.
type stopWalkError struct{}

func (stopWalkError) Error() string { return "tools: walk result cap reached" }

// errCtxCancelled is the shared sentinel a WalkDir callback returns to abort a
// traversal when its context is cancelled or its deadline has expired (Glob and
// Grep's fallback), so a huge tree cannot block past cancellation. Like
// errStopWalk it is a leaf control-flow sentinel, never surfaced to a caller.
var errCtxCancelled = ctxCancelledError{}

// ctxCancelledError is the typed sentinel returned to abort WalkDir on context
// cancellation/expiry.
type ctxCancelledError struct{}

func (ctxCancelledError) Error() string { return "tools: walk aborted; context cancelled" }

// denyFilteredRel is the SINGLE source of truth for the security-critical
// deny-filter applied to every path the read tools traverse or emit (Glob's walk,
// Grep's rg-result path, and Grep's fallback walk). It resolves abs the way
// DeniedRead's absolute-path contract expects (best-effort EvalSymlinks; an
// unresolvable path falls back to abs unchanged — fail-secure: the authoritative
// DeniedRead below still runs on it), applies the AUTHORITATIVE DeniedRead filter,
// and returns the workspace-relative slash path for a permitted entry. denied=true
// means the path MUST be excluded — either DeniedRead reported it, OR the
// symlink-resolved path ESCAPES the workspace root (a symlinked entry whose target
// is outside, or any other containment surprise), OR it could not be made relative
// to the resolved root. Centralising this guarantees the deny semantics cannot
// drift between the three call sites.
//
// Containment is enforced on the RESOLVED (EvalSymlinks) path, mirroring
// containedPath: WalkDir does not descend into a symlinked directory, but it still
// VISITS a symlink entry, and an rg result path may itself be a symlink. Resolving
// then rejecting a "../"-escaping rel ensures such an entry is excluded rather than
// emitted with an out-of-workspace "../" path (which would leak the target's
// location, and could match a "**" pattern).
func denyFilteredRel(guard loop.ReadGuard, resolvedRoot, abs string) (relSlash string, denied bool) {
	denyAbs := abs
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		denyAbs = resolved
	}
	if guard.DeniedRead(denyAbs) {
		return "", true
	}
	rel, rerr := filepath.Rel(resolvedRoot, denyAbs)
	if rerr != nil {
		return "", true
	}
	// Reject a resolved path that climbs above the workspace root (e.g. an
	// in-workspace symlink pointing OUT). Such an entry must never be emitted.
	if hasParentEscape(rel) {
		return "", true
	}
	return filepath.ToSlash(rel), false
}

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
