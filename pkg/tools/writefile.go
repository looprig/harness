package tools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/looprig/harness/pkg/tool"
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

// WriteFile writes a workspace-contained file atomically under the loop's
// optimistic-concurrency policy. It depends only on the workspace root (least
// privilege — the hard-deny gate is the runner's concern) and the loop's shared
// observation map: overwriting an EXISTING file requires a complete prior read of
// this path whose hash still equals the file's current on-disk hash; a genuinely
// ABSENT path may be created without any prior read via an atomic no-replace
// publication.
type WriteFile struct {
	root string
	obs  *fileObservations
}

// NewWriteFile constructs a WriteFile bound to the workspace root and the loop's
// shared observation map (supplied by Files, one per loop binding).
func NewWriteFile(root string, obs *fileObservations) *WriteFile {
	return &WriteFile{root: root, obs: obs}
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
	// symlink pointing OUT) is rejected here; echo only the requested path. The
	// resolved path is the CANONICAL observation key (lexical/symlinked-dir aliases
	// of the same real file collapse to one entry).
	abs, err := containedPath(w.root, a.Path)
	if err != nil {
		return tool.TextResult("error: path is outside the workspace: " + a.Path), nil
	}

	// Stage 2: commit under the path's optimistic-concurrency critical section. The
	// on-disk write targets the LEXICAL joined path (NOT the symlink-resolved form),
	// mirroring ReadFile/EditFile: an atomic Rename/Link on this lexical name never
	// follows a final-component symlink.
	if err := w.commit(canonicalObservationKey(abs), joinedUnderRoot(w.root, a.Path), a.Path, []byte(a.Content)); err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	return tool.TextResult("wrote " + a.Path + " (" + strconv.Itoa(len(a.Content)) + " bytes)"), nil
}

// commit performs the optimistic-concurrency write for one path while holding that
// path's critical section: it cheaply classifies the target (absent / regular /
// irregular), then either creates a genuinely absent file via an atomic no-replace
// publication, or overwrites an existing regular file — but ONLY if this loop's
// complete observation still equals the file's current on-disk hash. The classify,
// the (deferred) hash read, and the publish all happen under one lock hold, so a
// concurrent same-loop change between check and write cannot slip through. On a
// stale/conflict/irregular refusal the entry is invalidated and a typed error is
// returned; on success the new content's hash is recorded. lexical is the on-disk
// target; display is the model-supplied path used only in error messages.
func (w *WriteFile) commit(key canonicalObservationKey, lexical, display string, data []byte) error {
	st := w.obs.state(key)
	st.mu.Lock()
	defer st.mu.Unlock()

	switch classifyWriteTarget(lexical) {
	case writeTargetAbsent:
		// Create a genuinely new file. No prior read is required. The no-replace
		// publication fails without clobbering if another writer wins the race
		// between this absence check and the link.
		if err := atomicCreateFile(lexical, data); err != nil {
			st.clearLocked()
			if errors.Is(err, errCreateConflict) {
				return &FileCreateConflictError{Path: display}
			}
			return err
		}
		st.setPresentLocked(sha256.Sum256(data))
		return nil

	case writeTargetIrregular:
		// A final-component symlink or other non-regular node: it cannot be observed
		// (a ReadFile refuses it O_NOFOLLOW), so overwriting is refused with a
		// distinct error that does NOT tell the model to "read again".
		st.clearLocked()
		return &IrregularFileError{Path: display}
	}

	// writeTargetRegular. Without a complete observation the overwrite is doomed
	// regardless of the bytes, so refuse BEFORE hashing — avoiding an O(file-size)
	// read of a file we may not touch. Only when an observation exists do we hash
	// the current contents to complete the compare-and-swap.
	if !st.observed || !st.obs.present {
		st.clearLocked()
		return &StaleFileError{Path: display}
	}
	curHash, present, err := hashFileOnDisk(lexical)
	if err != nil {
		// The target became irregular/unreadable since the classify (a race): fail
		// secure with the distinct irregular error.
		st.clearLocked()
		return &IrregularFileError{Path: display}
	}
	if !present || st.obs.hash != curHash {
		// Vanished, or its content changed since our read: an optimistic-concurrency
		// conflict — read again.
		st.clearLocked()
		return &StaleFileError{Path: display}
	}
	if err := atomicWriteFile(lexical, data); err != nil {
		return err
	}
	st.setPresentLocked(sha256.Sum256(data))
	return nil
}

// writeTargetKind classifies a write/edit target's final component without reading
// its contents.
type writeTargetKind int

const (
	writeTargetAbsent    writeTargetKind = iota // no node at the path
	writeTargetRegular                          // a plain regular file
	writeTargetIrregular                        // a final-component symlink or other non-regular node
)

// classifyWriteTarget cheaply classifies the LEXICAL target via a single os.Lstat
// (no content read, and — being Lstat — a final-component symlink is detected, never
// followed). A stat error other than not-exist is treated as irregular so the caller
// fails secure (deny). This lets commit reject a doomed unobserved overwrite, or an
// irregular target, WITHOUT the O(file-size) hash read.
func classifyWriteTarget(lexical string) writeTargetKind {
	fi, err := os.Lstat(lexical)
	if err != nil {
		if os.IsNotExist(err) {
			return writeTargetAbsent
		}
		return writeTargetIrregular
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return writeTargetIrregular
	}
	return writeTargetRegular
}

// atomicWriteFile publishes data to an EXISTING (or to-be-replaced) target: it
// stages a sibling temp file and os.Rename's it over target. target is the LEXICAL
// joined path (the caller has proved its symlink-resolved form is contained);
// rename to a target that is a final-component symlink REPLACES the symlink rather
// than following it. The temp file is removed on any post-stage failure. All
// failures are typed writeFileError (non-secret reason, never contents).
func atomicWriteFile(target string, data []byte) error {
	tmp, err := stageTempFile(filepath.Dir(target), data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return &writeFileError{reason: "could not rename temp file into place", cause: err}
	}
	return nil
}

// errCreateConflict is the leaf cause atomicCreateFile returns when the no-replace
// link fails because the destination already exists. It carries no context;
// WriteFile.commit wraps it into a *FileCreateConflictError with the display path.
var errCreateConflict = errors.New("create destination already exists")

// atomicCreateFile publishes data to a currently-ABSENT target via an atomic
// no-replace publication: it stages a sibling temp file, then os.Link's it into the
// destination. os.Link fails with EEXIST if target already exists — the conflict
// signal (surfaced as errCreateConflict) — so a create never clobbers an existing
// file or follows/replaces a final-component symlink. On success the temp link is
// removed, leaving target as the sole link to the written inode. target is the
// LEXICAL joined path (the caller proved its symlink-resolved form is contained).
func atomicCreateFile(target string, data []byte) error {
	tmp, err := stageTempFile(filepath.Dir(target), data)
	if err != nil {
		return err
	}
	if err := os.Link(tmp, target); err != nil {
		_ = os.Remove(tmp)
		if errors.Is(err, os.ErrExist) {
			return errCreateConflict
		}
		return &writeFileError{reason: "could not link new file into place", cause: err}
	}
	// The publication succeeded via the link; drop the redundant temp name.
	_ = os.Remove(tmp)
	return nil
}

// stageTempFile creates dir (and ancestors), writes data to a uniquely-named temp
// file in the SAME directory (O_CREATE|O_EXCL|O_WRONLY|O_NOFOLLOW @0600), fsyncs and
// closes it, and returns the temp path for the caller to publish (rename over, or
// link into, the final destination). Same-directory placement guarantees an atomic
// rename AND a same-filesystem link. All failures are typed writeFileError.
func stageTempFile(dir string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, newDirPerm); err != nil {
		return "", &writeFileError{reason: "could not create parent directories", cause: err}
	}
	tmp, err := uniqueWriteTempPath(dir)
	if err != nil {
		return "", err
	}
	// #nosec G304 -- tmp = target's containment-proven parent dir + a crypto/rand
	// suffix. O_EXCL|O_NOFOLLOW refuse to clobber an existing name or to follow a
	// pre-planted symlink AT THE TEMP NAME (cheap defence-in-depth). This does NOT
	// close the broader parent-dir resolve→open TOCTOU window, which §3c
	// (write-side threat model) explicitly accepts as out of scope for this local
	// single-user tool. target's resolved form was proven contained by the caller.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, newFilePerm)
	if err != nil {
		return "", &writeFileError{reason: "could not create temp file", cause: err}
	}
	if err := writeSyncClose(f, data); err != nil {
		_ = os.Remove(tmp)
		return "", &writeFileError{reason: "could not write temp file", cause: err}
	}
	return tmp, nil
}

// hashFileOnDisk computes the SHA-256 of target's COMPLETE current raw bytes for
// the optimistic-concurrency compare. It opens O_RDONLY|O_NOFOLLOW (a final-
// component symlink fails with ELOOP) and streams the file through the hash (O(1)
// memory, any size). present is false with a nil error ONLY for a definitive
// not-found; any other open/stat/read failure (symlink, non-regular, unreadable)
// returns a non-nil error so the caller fails secure (treats the state as
// unverifiable and refuses the mutation). The hash is never exposed to the model.
func hashFileOnDisk(target string) (hash [sha256.Size]byte, present bool, err error) {
	// #nosec G304 -- target is the containment-proven lexical joined path;
	// O_NOFOLLOW rejects a final-component symlink and the fd stat confirms a
	// regular file before any bytes are read.
	f, oerr := os.OpenFile(target, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if oerr != nil {
		if os.IsNotExist(oerr) {
			return hash, false, nil
		}
		return hash, false, &writeFileError{reason: "could not open the file to verify freshness", cause: oerr}
	}
	defer func() { _ = f.Close() }()

	fi, serr := f.Stat()
	if serr != nil {
		return hash, false, &writeFileError{reason: "could not stat the file to verify freshness", cause: serr}
	}
	if !fi.Mode().IsRegular() {
		return hash, false, &writeFileError{reason: "not a regular file"}
	}

	h := sha256.New()
	if _, cerr := io.Copy(h, f); cerr != nil {
		return hash, false, &writeFileError{reason: "could not read the file to verify freshness", cause: cerr}
	}
	h.Sum(hash[:0])
	return hash, true, nil
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
