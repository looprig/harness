package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tool"
)

// grep.go implements the Grep tool: a workspace-contained content search that
// prefers ripgrep (arg-list exec — never a shell string — so a "-"-leading
// pattern/path can never become a flag) and falls back to a stdlib WalkDir+regexp
// scan. Denied-path enforcement is TWO-LAYER (design §4b note): a best-effort
// noise/secret `rg --glob '!…'` skip for performance, BACKSTOPPED by an
// authoritative DeniedRead filter on every emitted result path (the rg path) and
// directly during traversal (the WalkDir path). AutoApprove; Auditable.

// grepToolName is the EXACT tool name classifyTool keys on for the read class.
const grepToolName = toolGrep

// maxGrepMatches caps the emitted match lines so a broad pattern cannot flood the
// model context. A larger result set is truncated with a notice.
const maxGrepMatches = 200

// maxGrepLineBytes caps a single scanned/emitted line so a pathological file
// cannot exhaust memory or produce an enormous match line.
const maxGrepLineBytes = 64 * 1024

// rgBinary is the ripgrep executable name resolved on PATH (a binary, not a Go
// dependency).
const rgBinary = "rg"

// grepTimeout bounds a single Grep invocation (the rg subprocess exec AND the
// in-process fallback walk). The CLAUDE.md "Context" rule forbids unbounded
// external/blocking I/O: a pathological tree or a wedged rg process must not hang
// the agent. 30s is generous for an interactive code search over a workspace yet
// firmly bounded; on expiry the tool returns "error: grep timed out".
const grepTimeout = 30 * time.Second

// grepNoiseDirs are directory names skipped during the WalkDir fallback and
// translated to `rg --glob '!<dir>'` exclusions (best-effort) — large, generated,
// or VCS-internal trees that pollute a code search.
var grepNoiseDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	".hg":          true,
	".svn":         true,
	"dist":         true,
	"build":        true,
	".idea":        true,
	".vscode":      true,
}

// grepSchema is the JSON Schema for Grep's args. Field names (pattern/path/…) are
// the boundary-extraction contract shared with check.go (which parses "pattern"
// and "path").
const grepSchema = `{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Regular expression to search for (RE2 syntax in the fallback)."},
    "path": {"type": "string", "description": "Workspace-relative file or directory to search (optional; defaults to the workspace root)."},
    "recursive": {"type": "boolean", "description": "Search subdirectories (default true for a directory)."},
    "ignore_case": {"type": "boolean", "description": "Case-insensitive match."},
    "context_lines": {"type": "integer", "minimum": 0, "description": "Lines of context around each match."},
    "include_all": {"type": "boolean", "description": "Also search files normally skipped as noise (best-effort)."}
  },
  "required": ["pattern"]
}`

const grepDesc = "Search workspace file contents for a regular expression. Confined to the workspace, skips denied (secret) paths and noise directories, and is capped. Uses ripgrep when available, otherwise a built-in scanner."

// grepArgs is the typed decode of Grep's untrusted argsJSON.
type grepArgs struct {
	Pattern      string `json:"pattern"`
	Path         string `json:"path"`
	Recursive    *bool  `json:"recursive"`
	IgnoreCase   bool   `json:"ignore_case"`
	ContextLines int    `json:"context_lines"`
	IncludeAll   bool   `json:"include_all"`
}

// grepOptions is the validated, internal view of a Grep call (decoupled from the
// JSON shape) shared by both backends.
type grepOptions struct {
	recursive    bool
	ignoreCase   bool
	contextLines int
	includeAll   bool
}

// Grep searches workspace file contents. It depends only on the workspace root
// and the narrow loop.ReadGuard. useRg is resolved at construction (ripgrep on
// PATH) and is overridable in tests to force the deterministic fallback.
type Grep struct {
	root  string
	guard loop.ReadGuard
	useRg bool
}

// NewGrep constructs a Grep bound to the workspace root and read guard, preferring
// ripgrep when it is found on PATH.
func NewGrep(root string, guard loop.ReadGuard) *Grep {
	_, err := exec.LookPath(rgBinary)
	return newGrepWithBackend(root, guard, err == nil)
}

// newGrepWithBackend constructs a Grep with an explicit backend choice (test seam).
func newGrepWithBackend(root string, guard loop.ReadGuard, useRg bool) *Grep {
	return &Grep{root: root, guard: guard, useRg: useRg}
}

// Info returns Grep's self-description. Name MUST equal "Grep".
func (g *Grep) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   grepToolName,
		Desc:   grepDesc,
		Schema: json.RawMessage(grepSchema),
	}, nil
}

// AuditSummary returns a redacted one-line summary: the pattern (and path if
// present) only — never any matched file content.
func (g *Grep) AuditSummary(argsJSON string) string {
	var a grepArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || a.Pattern == "" {
		return "Grep (unparsable args)"
	}
	if a.Path != "" {
		return "Grep " + a.Pattern + " in " + a.Path
	}
	return "Grep " + a.Pattern
}

// InvokableRun validates args, contains the search path, compiles the pattern,
// runs the chosen backend, and renders capped, denied-filtered matches. Every
// failure is a tool-result string.
func (g *Grep) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	var a grepArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return tool.TextResult("error: invalid arguments: not a JSON object"), nil
	}
	if a.Pattern == "" {
		return tool.TextResult("error: a non-empty 'pattern' is required"), nil
	}
	if a.ContextLines < 0 {
		return tool.TextResult("error: context_lines must be >= 0"), nil
	}

	searchRel := a.Path
	if searchRel == "" {
		searchRel = defaultSearchDir
	}
	searchAbs, err := containedPath(g.root, searchRel)
	if err != nil {
		return tool.TextResult("error: search path is outside the workspace: " + searchRel), nil
	}
	resolvedRoot, err := resolveAbsRoot(g.root)
	if err != nil {
		return tool.TextResult("error: workspace root could not be resolved"), nil
	}

	// Compile the pattern up front: an invalid regex is a clean tool-result error
	// (and the fallback needs the compiled form anyway).
	re, err := compileGrepRegexp(a.Pattern, a.IgnoreCase)
	if err != nil {
		return tool.TextResult("error: invalid pattern: " + err.Error()), nil
	}

	opts := grepOptions{
		recursive:    a.Recursive == nil || *a.Recursive, // default true.
		ignoreCase:   a.IgnoreCase,
		contextLines: a.ContextLines,
		includeAll:   a.IncludeAll,
	}

	// Bound the search (rg exec or fallback walk) so neither can block past
	// grepTimeout (CLAUDE.md "Context": no unbounded external/blocking I/O). A
	// caller deadline that is already tighter is honoured (WithTimeout never
	// extends it).
	ctx, cancel := context.WithTimeout(ctx, grepTimeout)
	defer cancel()

	var matches []string
	var truncated, expired bool
	if g.useRg {
		matches, truncated, expired = g.runRg(ctx, a.Pattern, searchAbs, resolvedRoot, opts)
	} else {
		matches, truncated, expired = g.runFallback(ctx, searchAbs, resolvedRoot, re, opts)
	}
	if expired {
		return tool.TextResult("error: grep timed out"), nil
	}
	return tool.TextResult(renderGrepResults(matches, truncated)), nil
}

// compileGrepRegexp compiles the pattern, prefixing "(?i)" for a case-insensitive
// search. A bad pattern returns the regexp error (rendered as a tool-result).
func compileGrepRegexp(pattern string, ignoreCase bool) (*regexp.Regexp, error) {
	expr := pattern
	if ignoreCase {
		expr = "(?i)" + expr
	}
	return regexp.Compile(expr)
}

// buildRgArgs builds the ripgrep argument VECTOR (never a shell string). The
// pattern follows --regexp and the path follows the "--" terminator, so a
// "-"-leading pattern or path can NEVER be parsed as a flag (flag-injection
// defense). denyGlobs are translated to `--glob '!<g>'` exclusions (best-effort).
func buildRgArgs(pattern, path string, opts grepOptions, denyGlobs []string) []string {
	args := []string{
		"--line-number",
		"--with-filename",
		"--no-heading",
		"--color", "never",
	}
	if opts.ignoreCase {
		args = append(args, "--ignore-case")
	}
	if opts.contextLines > 0 {
		args = append(args, "--context", strconv.Itoa(opts.contextLines))
	}
	if !opts.recursive {
		args = append(args, "--max-depth", "1")
	}
	if !opts.includeAll {
		for _, g := range denyGlobs {
			args = append(args, "--glob", "!"+g)
		}
	}
	// Pattern AFTER --regexp; path AFTER -- : both are values, never flags.
	args = append(args, "--regexp", pattern, "--", path)
	return args
}

// rgDenyGlobs returns the best-effort `rg --glob` exclusion patterns: the noise
// dirs plus the read-deny globs reachable through the policy. The narrow
// loop.ReadGuard exposes only DeniedRead/MaxReadBytes (no glob list), so the
// authoritative DeniedRead filter (applied to every emitted path) is the real
// boundary; this list is a perf optimization. When the guard also implements the
// optional deniedGlobLister seam (the concrete PermissionChecker may), its globs
// are added so rg never even opens them.
func (g *Grep) rgDenyGlobs() []string {
	globs := make([]string, 0, len(grepNoiseDirs))
	for dir := range grepNoiseDirs {
		globs = append(globs, dir)
	}
	if lister, ok := g.guard.(deniedGlobLister); ok {
		globs = append(globs, lister.DeniedReadGlobs()...)
	}
	sort.Strings(globs)
	return globs
}

// deniedGlobLister is an OPTIONAL widening the rg-skip perf layer may use to learn
// the denied-read globs for `--glob '!…'` translation. It is NOT required for
// correctness: the authoritative DeniedRead filter backstops every result. The
// narrow loop.ReadGuard does not include it (least privilege); a concrete guard
// may implement it.
type deniedGlobLister interface {
	DeniedReadGlobs() []string
}

// runRg executes ripgrep with the injection-safe arg vector and AUTHORITATIVELY
// filters every result path through the shared deny-filter before emitting
// (two-layer: the --glob skip is best-effort; that filter is the boundary). rg's
// exit status 1 (no matches) is not an error. The exec is bounded by ctx
// (exec.CommandContext kills rg on deadline): expired=true reports a ctx
// cancellation/timeout so the caller renders the timeout tool-result.
func (g *Grep) runRg(ctx context.Context, pattern, searchAbs, resolvedRoot string, opts grepOptions) (matches []string, truncated, expired bool) {
	args := buildRgArgs(pattern, searchAbs, opts, g.rgDenyGlobs())
	// #nosec G204 -- fixed binary "rg"; pattern/path are passed as VALUES after
	// --regexp / -- so they can never be interpreted as flags (no shell, arg list).
	cmd := exec.CommandContext(ctx, rgBinary, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		// A ctx timeout/cancel killed rg: surface it as a bounded-I/O timeout rather
		// than a silent empty result.
		if ctx.Err() != nil {
			return nil, false, true
		}
		// rg exits 1 when there are simply no matches — not an execution failure.
		if code, ok := asExitCode(err); !ok || code != 1 {
			// A genuine failure (binary vanished, killed): fall back to nothing.
			return nil, false, false
		}
	}
	matches, truncated = g.collectRgLines(&out, resolvedRoot)
	return matches, truncated, false
}

// collectRgLines parses rg's "path:line:text" output, rewrites the absolute file
// path to a workspace-relative one via the shared authoritative deny-filter
// (excluding any denied/non-relativisable path), and caps the result.
func (g *Grep) collectRgLines(out *bytes.Buffer, resolvedRoot string) (matches []string, truncated bool) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64*1024), maxGrepLineBytes)
	for sc.Scan() {
		line := sc.Text()
		absFile, rest, ok := splitRgLine(line)
		if !ok {
			continue
		}
		// Authoritative denied filter (shared helper): a denied path is never emitted.
		relSlash, denied := denyFilteredRel(g.guard, resolvedRoot, absFile)
		if denied {
			continue
		}
		matches = append(matches, relSlash+rest)
		if len(matches) >= maxGrepMatches {
			truncated = true
			break
		}
	}
	return matches, truncated
}

// splitRgLine splits an rg "--with-filename --line-number" line into the file
// path and the trailing ":line:text" (or ":line-text" for context). rg separates
// the filename from the rest with the first ":". A Windows drive letter is not a
// concern here (workspace paths). ok=false for an unparseable line.
func splitRgLine(line string) (file, rest string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i <= 0 {
		return "", "", false
	}
	return line[:i], line[i:], true
}

// asExitCode reports the process exit code of err when err is (or wraps) an
// *exec.ExitError. ok=false for any non-exit error (including nil). It uses
// errors.As for wrapped-error robustness, consistent with the rest of the package.
func asExitCode(err error) (code int, ok bool) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), true
	}
	return 0, false
}

// runFallback walks searchAbs with the stdlib scanner, applying the shared
// authoritative deny-filter DIRECTLY during traversal (so a denied file is never
// opened) and skipping noise dirs, matching each line against re, and capping the
// result. The walk is cheaply cancellable: if ctx is done it aborts and reports
// expired=true so the caller renders the timeout tool-result instead of a partial
// scan.
func (g *Grep) runFallback(ctx context.Context, searchAbs, resolvedRoot string, re *regexp.Regexp, opts grepOptions) (matches []string, truncated, expired bool) {
	walkErr := filepath.WalkDir(searchAbs, func(abs string, d fs.DirEntry, err error) error {
		// Cheap cancellability: abort before touching this entry if ctx is done so a
		// huge tree cannot block past cancellation.
		if ctx.Err() != nil {
			return errCtxCancelled
		}
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if abs != searchAbs && !opts.includeAll && grepNoiseDirs[d.Name()] {
				return fs.SkipDir
			}
			if !opts.recursive && abs != searchAbs {
				return fs.SkipDir
			}
			return nil
		}
		relSlash, denied := denyFilteredRel(g.guard, resolvedRoot, abs)
		if denied {
			return nil // never open a denied (or non-relativisable) file.
		}
		fileMatches := g.grepFile(abs, relSlash, re)
		for _, m := range fileMatches {
			matches = append(matches, m)
			if len(matches) >= maxGrepMatches {
				truncated = true
				return errStopWalk
			}
		}
		return nil
	})
	// A ctx cancellation aborts to errCtxCancelled -> report the timeout. errStopWalk
	// already set truncated=true before aborting; a nil walkErr means the tree was
	// fully scanned. Other walk errors are swallowed per-entry above.
	if errors.Is(walkErr, errCtxCancelled) {
		return nil, false, true
	}
	return matches, truncated, false
}

// grepFile opens relSlash's file with O_NOFOLLOW (a symlinked final component is
// not followed), confirms a regular file via fd stat, and returns "rel:line:text"
// for each matching line, capped to maxGrepLineBytes per line. A binary-looking or
// unreadable file yields no matches (best-effort).
func (g *Grep) grepFile(abs, relSlash string, re *regexp.Regexp) []string {
	// #nosec G304 -- abs is under the contained, walked search root; O_NOFOLLOW +
	// fd stat below reject a symlinked final component / non-regular file.
	f, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxGrepLineBytes)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		text := sc.Text()
		if !re.MatchString(text) {
			continue
		}
		out = append(out, relSlash+":"+strconv.Itoa(lineNo)+":"+text)
	}
	return out
}

// renderGrepResults formats the match lines, appending a truncation notice when
// the cap was hit. An empty list reports "no matches".
func renderGrepResults(matches []string, truncated bool) string {
	if len(matches) == 0 {
		return "no matches"
	}
	out := strings.Join(matches, "\n")
	if truncated {
		out += "\n[truncated: more than " + strconv.Itoa(maxGrepMatches) + " matches; refine the pattern]"
	}
	return out
}

// compile-time assertions.
var (
	_ tool.InvokableTool = (*Grep)(nil)
	_ tool.Auditable     = (*Grep)(nil)
)
