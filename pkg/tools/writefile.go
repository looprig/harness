package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/ciram-co/looprig/pkg/tool"
)

// writefile.go implements the WriteFile tool: a workspace-contained atomic file
// writer (design §4b). WriteFile defaults to Ask (it implements
// PermissionPrompter → tool.FileWriteRequest), is Auditable with a content-free
// summary, and is a WriteTarget so the runner serializes same-resolved-path
// writes. The denied-write hard-deny list is enforced by the PermissionChecker
// BEFORE the tool runs (the tool takes only root); the tool still runs
// containment itself for correct path resolution and defense in depth.
//
// Path handling: containedPath proves the symlink-RESOLVED target is inside the
// workspace (an escape — including an in-workspace symlink pointing out — is
// rejected). The atomic write then targets the LEXICAL joined path, so a write to
// a path whose final component is an existing in-workspace symlink REPLACES the
// symlink with the new regular file rather than following it to clobber the
// symlink's target (consistent with ReadFile/EditFile not silently following a
// final-component symlink).
//
// Atomicity: parent dirs are created (MkdirAll on the lexical parent), then the
// content is written to a uniquely-named temp file in the SAME directory opened
// O_CREATE|O_EXCL|O_WRONLY|O_NOFOLLOW (refuses a pre-planted symlinked temp, no
// clobber), fsync'd, closed, and os.Rename'd over the target (atomic within a
// directory; rename replaces a final-component symlink rather than following it).
// The temp file is removed on any failure so no half-written litter is left
// behind. This mirrors store.go's writeApprovalsFileAtomically hardening.
//
// O_NOFOLLOW on the temp open rejects a pre-planted symlink AT THE TEMP NAME; it
// does NOT close the broader parent-dir resolve→open TOCTOU window (a parent dir
// swapped to a symlink between the containment check and the write). §3c
// (write-side threat model) explicitly accepts that residual window as out of
// scope for this local single-user tool acting with the user's own privileges;
// the O_EXCL|O_NOFOLLOW temp is cheap defence-in-depth, not a complete TOCTOU fix.

// writeFileToolName is the EXACT tool name classifyTool keys on for the write
// class — it MUST equal "WriteFile" (check.go's toolWriteFile).
const writeFileToolName = toolWriteFile

// newFilePerm is the mode a freshly-written file ends up with. The temp file is
// created 0o600 (owner-only while being written), and that mode carries through
// the Rename — a written source file is owner read/write, no group/world bits.
const newFilePerm os.FileMode = 0o600

// newDirPerm is the mode for parent directories created by MkdirAll (owner rwx,
// group/other read+execute), the conventional directory mode for a workspace.
const newDirPerm os.FileMode = 0o755

// writeFileSchema is the JSON Schema for WriteFile's argument object. The field
// names (path/content) are the boundary-extraction contract shared with check.go
// (which parses "path").
const writeFileSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Workspace-relative path of the file to write (parent directories are created as needed)."},
    "content": {"type": "string", "description": "The full file contents to write. The file is overwritten atomically."}
  },
  "required": ["path", "content"]
}`

const writeFileDesc = "Write a UTF-8 text file in the workspace, creating parent directories as needed and overwriting any existing file atomically. Writes are confined to the workspace and never follow a final-component symlink. Requires approval before each write."

// writeFileArgs is the typed decode of WriteFile's untrusted argsJSON.
type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// WriteFile writes a workspace-contained file atomically. It depends only on the
// workspace root (least privilege): the hard-deny gate is the runner's concern.
type WriteFile struct {
	root string
}

// NewWriteFile constructs a WriteFile bound to the workspace root.
func NewWriteFile(root string) *WriteFile {
	return &WriteFile{root: root}
}

// Info returns WriteFile's self-description. Name MUST equal "WriteFile".
func (w *WriteFile) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   writeFileToolName,
		Desc:   writeFileDesc,
		Schema: json.RawMessage(writeFileSchema),
	}, nil
}

// AuditSummary returns a redacted, content-free one-line summary: the path and
// byte count only — NEVER the content. An unparseable args document yields a
// generic summary.
func (w *WriteFile) AuditSummary(argsJSON string) string {
	var a writeFileArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || a.Path == "" {
		return "WriteFile (unparsable args)"
	}
	return "WriteFile " + a.Path + " (" + strconv.Itoa(len(a.Content)) + " bytes)"
}

// BuildRequest derives the approval prompt from the (untrusted) args. The prompt
// carries only the resolved write path (never the content). An unparseable args
// document or a path that escapes the workspace is a typed error so the runner
// treats the call as invalid (and never prompts for an out-of-bounds write).
func (w *WriteFile) BuildRequest(argsJSON string, _ tool.PreparedArtifact) (tool.PermissionRequest, error) {
	abs, err := w.resolveWritePath(argsJSON)
	if err != nil {
		return nil, err
	}
	return tool.FileWriteRequest{Path: abs}, nil
}

// WriteTarget returns the resolved write path as the serialization key so the
// runner groups concurrent writes to the same on-disk file. ok is true for every
// well-formed write; a non-nil err (unparseable args or an escape) tells the
// runner to treat the call as invalid rather than execute it ungrouped.
func (w *WriteFile) WriteTarget(argsJSON string) (string, bool, error) {
	abs, err := w.resolveWritePath(argsJSON)
	if err != nil {
		return "", false, err
	}
	return abs, true, nil
}

// resolveWritePath is the shared parse-and-contain step for BuildRequest and
// WriteTarget: decode the args, require a non-empty path, and resolve it through
// containedPath. The returned path is the canonical resolved write target.
func (w *WriteFile) resolveWritePath(argsJSON string) (string, error) {
	var a writeFileArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", &writeFileError{reason: "invalid arguments: not a JSON object", cause: err}
	}
	if a.Path == "" {
		return "", &writeFileError{reason: "a non-empty 'path' is required"}
	}
	abs, err := containedPath(w.root, a.Path)
	if err != nil {
		return "", &writeFileError{reason: "path is outside the workspace", cause: err}
	}
	return abs, nil
}

// InvokableRun writes the file atomically. Every failure mode (bad args, escape,
// mkdir/temp/rename failure) is returned as a tool-result error string — never a
// Go error and never echoing the content.
func (w *WriteFile) InvokableRun(_ context.Context, argsJSON string) (*tool.ToolResult, error) {
	var a writeFileArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return tool.TextResult("error: invalid arguments: not a JSON object"), nil
	}
	if a.Path == "" {
		return tool.TextResult("error: a non-empty 'path' is required"), nil
	}

	// Stage 1: containment (symlink-aware). An escape (including an in-workspace
	// symlink pointing OUT) is rejected here; echo only the requested path. We
	// discard the resolved path: the atomic write below targets the LEXICAL form.
	if _, err := containedPath(w.root, a.Path); err != nil {
		return tool.TextResult("error: path is outside the workspace: " + a.Path), nil
	}

	// Stage 2: write the LEXICALLY-joined path (NOT the symlink-resolved form),
	// mirroring ReadFile/EditFile. The atomic Rename targets this lexical name, so
	// a final-component symlink is REPLACED by the new regular file rather than
	// followed to clobber the symlink's target. Containment above already proved
	// the resolved target is inside the workspace.
	lexical := joinedUnderRoot(w.root, a.Path)
	if err := atomicWriteFile(lexical, []byte(a.Content)); err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	return tool.TextResult("wrote " + a.Path + " (" + strconv.Itoa(len(a.Content)) + " bytes)"), nil
}

// atomicWriteFile creates target's parent directories then writes data to a temp
// file in the SAME directory (O_CREATE|O_EXCL|O_WRONLY|O_NOFOLLOW @0600) and
// os.Rename's it over target. target is the LEXICAL joined path (the caller has
// proved its symlink-resolved form is contained); rename to a target that is a
// final-component symlink REPLACES the symlink rather than following it. The temp
// file is removed on any post-create failure. All failures are typed
// writeFileError (non-secret reason, never contents).
func atomicWriteFile(target string, data []byte) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, newDirPerm); err != nil {
		return &writeFileError{reason: "could not create parent directories", cause: err}
	}

	tmp, err := uniqueWriteTempPath(dir)
	if err != nil {
		return err
	}

	// #nosec G304 -- tmp = target's containment-proven parent dir + a crypto/rand
	// suffix. O_EXCL|O_NOFOLLOW refuse to clobber an existing name or to follow a
	// pre-planted symlink AT THE TEMP NAME (cheap defence-in-depth). This does NOT
	// close the broader parent-dir resolve→open TOCTOU window, which §3c
	// (write-side threat model) explicitly accepts as out of scope for this local
	// single-user tool. target's resolved form was proven contained by the caller.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, newFilePerm)
	if err != nil {
		return &writeFileError{reason: "could not create temp file", cause: err}
	}
	if err := writeSyncClose(f, data); err != nil {
		_ = os.Remove(tmp)
		return &writeFileError{reason: "could not write temp file", cause: err}
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return &writeFileError{reason: "could not rename temp file into place", cause: err}
	}
	return nil
}

// uniqueWriteTempPath returns a never-before-used temp file path in dir using a
// crypto/rand suffix (collision-resistant; O_EXCL still guards the create). It
// does NOT create the file — the caller opens it O_EXCL|O_NOFOLLOW. This mirrors
// store.go's uniqueTempPath but for the write tools' temp files.
func uniqueWriteTempPath(dir string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", &writeFileError{reason: "could not generate temp file name", cause: err}
	}
	return filepath.Join(dir, ".looprig-write-"+hex.EncodeToString(b[:])+".tmp"), nil
}

// writeFileError is the typed failure for a WriteFile/EditFile write attempt. It
// carries a non-secret reason and an optional cause; its message NEVER includes
// file contents.
type writeFileError struct {
	reason string
	cause  error
}

func (e *writeFileError) Error() string { return e.reason }

func (e *writeFileError) Unwrap() error { return e.cause }

// compile-time assertions: WriteFile is an InvokableTool, a PermissionPrompter
// (Ask), Auditable, and a WriteTarget.
var (
	_ tool.InvokableTool      = (*WriteFile)(nil)
	_ tool.PermissionPrompter = (*WriteFile)(nil)
	_ tool.Auditable          = (*WriteFile)(nil)
	_ tool.WriteTarget        = (*WriteFile)(nil)
)
