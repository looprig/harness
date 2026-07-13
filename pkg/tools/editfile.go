package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/looprig/harness/pkg/tool"
)

// editfile.go implements the EditFile tool: an exact-string-replace editor over a
// workspace-contained file (design §4b). It proves containment (containedPath,
// symlink-resolved) then reads and writes the LEXICAL joined path — the read is
// O_RDONLY|O_NOFOLLOW so a final-component symlink is REJECTED (consistent with
// ReadFile), and the atomic write targets the same lexical name so it REPLACES a
// final-component symlink rather than following it. It replaces `old` with
// `replacement` under strict occurrence rules, writes back atomically (the shared
// atomicWriteFile temp+Rename), and returns a diff preview. Like WriteFile it
// defaults to Ask (PermissionPrompter → tool.FileWriteRequest), is Auditable (no
// content), and is a WriteTarget.
//
// Occurrence rules (the §4b contract):
//   - 0 matches of `old`          → tool-result error ("not found")
//   - ≥2 matches && !replace_all  → tool-result error ("ambiguous: N matches…")
//   - exactly 1, or replace_all   → perform the replacement (all if replace_all)

// editFileToolName is the EXACT tool name classifyTool keys on for the write
// class — it MUST equal "EditFile" (check.go's toolEditFile).
const editFileToolName = toolEditFile

// maxEditFileBytes caps the file EditFile will read so a pathological target
// cannot exhaust memory. It matches the 1 MiB ceiling used elsewhere in the
// package for human-edited/source files.
const maxEditFileBytes int64 = 1 << 20

// diffPreviewContextLines is how many unchanged lines of context the diff preview
// shows on each side of a changed region (keeps the preview compact but readable).
const diffPreviewContextLines = 2

// editFileSchema is the JSON Schema for EditFile's argument object. The field
// names (path/old/new/replace_all) are the boundary-extraction contract: check.go
// parses "path"; the runner/tool parse the rest.
const editFileSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Workspace-relative path of the file to edit."},
    "old": {"type": "string", "description": "The exact substring to find. Must match exactly once unless replace_all is true."},
    "new": {"type": "string", "description": "The replacement substring."},
    "replace_all": {"type": "boolean", "description": "Replace every occurrence of 'old' instead of requiring a single unique match."}
  },
  "required": ["path", "old", "new"]
}`

const editFileDesc = "Edit a UTF-8 text file in the workspace by replacing an exact substring. By default 'old' must occur exactly once (a unique edit); set replace_all to replace every occurrence. Returns a diff preview. Edits are confined to the workspace and never follow a final-component symlink. Requires approval before each edit."

// editFileArgs is the typed decode of EditFile's untrusted argsJSON.
type editFileArgs struct {
	Path       string `json:"path"`
	Old        string `json:"old"`
	New        string `json:"new"`
	ReplaceAll bool   `json:"replace_all"`
}

// EditFile edits a workspace-contained file by exact-string replacement under the
// loop's optimistic-concurrency policy. It depends only on the workspace root
// (least privilege), the loop's shared observation map, and an OPTIONAL session
// workspace coordinator: an edit requires a complete prior read of this path whose
// hash still equals the file's current on-disk hash. When a coordinator is bound the
// commit runs under a SHARED session-mutation + canonical-PATH permit (serializing
// same-real-file edits across loops, excluded by a Bash/checkpoint permit).
type EditFile struct {
	root  string
	obs   *fileObservations
	coord tool.WorkspaceCoordinator
}

// NewEditFile constructs an EditFile bound to the workspace root and the loop's
// shared observation map (supplied by Files, one per loop binding). A
// WithMutationCoordinator option binds the session workspace coordinator; without it
// the tool runs coordinator-free (the standalone/bare path).
func NewEditFile(root string, obs *fileObservations, opts ...FileMutatorOption) *EditFile {
	cfg := resolveFileMutatorConfig(opts)
	return &EditFile{root: root, obs: obs, coord: cfg.coord}
}

// Info returns EditFile's self-description. Name MUST equal "EditFile".
func (e *EditFile) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   editFileToolName,
		Desc:   editFileDesc,
		Schema: json.RawMessage(editFileSchema),
	}, nil
}

// AuditSummary returns a redacted, content-free one-line summary: the path only
// (never the old/new substrings, which can carry secrets). An unparseable args
// document yields a generic summary.
func (e *EditFile) AuditSummary(argsJSON string) string {
	var a editFileArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || a.Path == "" {
		return "EditFile (unparsable args)"
	}
	return "EditFile " + a.Path
}

// BuildRequest derives the approval prompt (the resolved path only — never the
// substrings). An unparseable args document or an escaping path is a typed error.
func (e *EditFile) BuildRequest(argsJSON string, _ tool.PreparedArtifact) (tool.PermissionRequest, error) {
	abs, err := e.resolveEditPath(argsJSON)
	if err != nil {
		return nil, err
	}
	return tool.FileWriteRequest{Path: abs}, nil
}

// WriteTarget returns the resolved edit path as the serialization key (an edit is
// a write). ok is true for a well-formed call; a non-nil err (bad args/escape)
// tells the runner to treat the call as invalid.
func (e *EditFile) WriteTarget(argsJSON string) (string, bool, error) {
	abs, err := e.resolveEditPath(argsJSON)
	if err != nil {
		return "", false, err
	}
	return abs, true, nil
}

// resolveEditPath is the shared parse-and-contain step for BuildRequest and
// WriteTarget: decode the args, require a non-empty path, and contain it.
func (e *EditFile) resolveEditPath(argsJSON string) (string, error) {
	var a editFileArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", &writeFileError{reason: "invalid arguments: not a JSON object", cause: err}
	}
	if a.Path == "" {
		return "", &writeFileError{reason: "a non-empty 'path' is required"}
	}
	abs, err := containedPath(e.root, a.Path)
	if err != nil {
		return "", &writeFileError{reason: "path is outside the workspace", cause: err}
	}
	return abs, nil
}

// InvokableRun applies the edit and returns a diff preview, or a tool-result
// error string for every failure mode (bad args, escape, not-found file, empty
// 'old', 0 matches, ambiguous matches, read/write failure). Never a Go error,
// never echoing the full file body.
func (e *EditFile) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	var a editFileArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return tool.TextResult("error: invalid arguments: not a JSON object"), nil
	}
	if a.Path == "" {
		return tool.TextResult("error: a non-empty 'path' is required"), nil
	}
	if a.Old == "" {
		return tool.TextResult("error: 'old' must be a non-empty substring to find"), nil
	}

	// Stage 1: containment (symlink-aware). Proves the symlink-RESOLVED target is
	// inside the workspace; an escape (including an in-workspace symlink pointing
	// OUT) is rejected here. The resolved path is the CANONICAL observation key.
	abs, err := containedPath(e.root, a.Path)
	if err != nil {
		return tool.TextResult("error: path is outside the workspace: " + a.Path), nil
	}

	// Stage 2: take the SHARED session-mutation + canonical-PATH permit (and verify
	// lease health) BEFORE the commit critical section — the OUTER lock over commit's
	// per-path st.mu (consistent ordering). A ctx-canceled acquire or an unhealthy
	// lease returns WITHOUT editing.
	key := canonicalObservationKey(abs)
	permit, err := acquirePathMutation(ctx, e.coord, key)
	if err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	defer permit.Release()

	// Stage 3: commit under the path's optimistic-concurrency critical section. The
	// read/write below operate on the LEXICAL joined path (NOT the symlink-resolved
	// form), mirroring ReadFile: the O_NOFOLLOW read rejects a final-component
	// symlink rather than following it, and the atomic write targets the same
	// lexical name so it REPLACES a final-component symlink rather than following it.
	preview, err := e.commit(key, joinedUnderRoot(e.root, a.Path), a.Path, a.Old, a.New, a.ReplaceAll)
	if err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	return tool.TextResult(preview), nil
}

// commit performs the optimistic-concurrency edit for one path while holding that
// path's critical section: it reads the current content ONCE (that same read yields
// both the bytes to edit and the hash to compare), refuses with StaleFileError if
// this loop lacks a complete observation whose hash equals the current on-disk hash,
// applies the occurrence-rule replacement (an anchor miss is a DISTINCT
// editAnchorError, not a freshness conflict), publishes atomically, and records the
// new content's hash. lexical is the on-disk target; display is the model-supplied
// path used in messages and the diff header.
func (e *EditFile) commit(key canonicalObservationKey, lexical, display, old, replacement string, replaceAll bool) (string, error) {
	st := e.obs.state(key)
	st.mu.Lock()
	defer st.mu.Unlock()

	// Cheap classify first (a single Lstat, no content read). An absent target is an
	// honest "file not found" (there is nothing to edit, and "read again" would
	// dead-end); a symlink/non-regular target is refused with the DISTINCT
	// IrregularFileError (re-reading it via ReadFile also fails O_NOFOLLOW, so
	// "read again" would dead-end there too).
	switch classifyWriteTarget(lexical) {
	case writeTargetAbsent:
		return "", &writeFileError{reason: "file not found"}
	case writeTargetIrregular:
		st.clearLocked()
		return "", &IrregularFileError{Path: display}
	}

	// writeTargetRegular. Read the file ONCE (that same read yields both the bytes to
	// edit and the hash to compare); the read is bounded by maxEditFileBytes, and a
	// read failure (too-large, or a race to symlink/absent since the classify) is
	// returned as-is — it is not an optimistic-concurrency conflict, so it must not
	// masquerade as a StaleFileError telling the model to "read again".
	original, rerr := e.readForEdit(lexical)
	if rerr != nil {
		return "", rerr
	}

	// Freshness: the edit is authorized only if this loop completely observed the
	// file and its recorded hash still equals the current on-disk content.
	if curHash := sha256.Sum256([]byte(original)); !st.observed || !st.obs.present || st.obs.hash != curHash {
		st.clearLocked()
		return "", &StaleFileError{Path: display}
	}

	updated, errMsg := applyReplacement(original, old, replacement, replaceAll)
	if errMsg != "" {
		return "", &editAnchorError{message: errMsg}
	}
	if err := atomicWriteFile(lexical, []byte(updated)); err != nil {
		return "", err
	}
	st.setPresentLocked(sha256.Sum256([]byte(updated)))
	return diffPreview(display, original, updated), nil
}

// readForEdit opens path O_RDONLY|O_NOFOLLOW (a final-component symlink fails to
// open with ELOOP), confirms a regular file via the fd stat, and reads up to
// maxEditFileBytes. path is the LEXICAL joined path (joinedUnderRoot); the caller
// has already proven the symlink-resolved form is contained. Errors are typed
// writeFileError (non-secret reason, never contents).
func (e *EditFile) readForEdit(path string) (string, error) {
	// #nosec G304 -- path is joinedUnderRoot(root, input): the workspace root +
	// the lexically-cleaned, contained input (containedPath already proved the
	// symlink-resolved target is inside the workspace). O_NOFOLLOW rejects a
	// FINAL-COMPONENT symlink (consistent with ReadFile); it does NOT by itself
	// close the broader parent-dir resolve→open TOCTOU window, which §3c
	// (write-side threat model) explicitly accepts as out of scope for this local
	// single-user tool.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return "", &writeFileError{reason: "file not found", cause: err}
		}
		if isSymlinkLoop(err) {
			return "", &writeFileError{reason: "refusing to follow a symlinked path", cause: err}
		}
		return "", &writeFileError{reason: "could not open the file", cause: err}
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return "", &writeFileError{reason: "could not stat the file", cause: err}
	}
	if !fi.Mode().IsRegular() {
		return "", &writeFileError{reason: "not a regular file"}
	}

	data, err := io.ReadAll(io.LimitReader(f, maxEditFileBytes+1))
	if err != nil {
		return "", &writeFileError{reason: "could not read the file", cause: err}
	}
	if int64(len(data)) > maxEditFileBytes {
		return "", &writeFileError{reason: "file is too large to edit (exceeds the " + strconv.FormatInt(maxEditFileBytes, 10) + "-byte cap)"}
	}
	return string(data), nil
}

// applyReplacement enforces the occurrence rules and returns the updated content.
// `replacement` is the new substring (the param is named replacement, not `new`,
// to avoid shadowing the builtin). On a rule violation it returns ("", errMsg) —
// a non-secret message naming the match count, never the file body. On success it
// returns (updated, "").
func applyReplacement(original, old, replacement string, replaceAll bool) (string, string) {
	n := strings.Count(original, old)
	switch {
	case n == 0:
		return "", "'old' substring not found in the file"
	case n >= 2 && !replaceAll:
		return "", "ambiguous: 'old' matches " + strconv.Itoa(n) + " times; set replace_all to replace every occurrence"
	case replaceAll:
		return strings.ReplaceAll(original, old, replacement), ""
	default: // exactly 1 match
		return strings.Replace(original, old, replacement, 1), ""
	}
}

// isSymlinkLoop reports whether err is an ELOOP (O_NOFOLLOW hit a symlink).
func isSymlinkLoop(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}

// compile-time assertions: EditFile is an InvokableTool, a PermissionPrompter
// (Ask), Auditable, and a WriteTarget.
var (
	_ tool.InvokableTool      = (*EditFile)(nil)
	_ tool.PermissionPrompter = (*EditFile)(nil)
	_ tool.Auditable          = (*EditFile)(nil)
	_ tool.WriteTarget        = (*EditFile)(nil)
)
