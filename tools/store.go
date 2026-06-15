package tools

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// store.go resolves the OUT-OF-REPO policy-store file paths (design §3c). The
// store deliberately lives under the user's home, NEVER in the repo, so a cloned
// or hostile repo cannot ship an approvals.json that silently auto-approves a
// tool call. The workspace-scoped file is keyed by a sha256 of the RESOLVED
// workspace root so two checkouts of different roots never share approvals.
//
// Only the reading path (workspaceHash + the two file-path resolvers) lives here
// for now; Grant's atomic-write + filesystem hardening (Task 3.6) extends this
// file.

const (
	// urviDirName is the per-user urvi store directory under the home dir.
	urviDirName = ".urvi"
	// workspacesDirName holds one subdirectory per workspace (named by hash).
	workspacesDirName = "workspaces"
	// userApprovalsName is the user-global approvals file (~/.urvi/approvals.json).
	userApprovalsName = "approvals.json"
	// workspaceApprovalsName is the per-workspace approvals file
	// (~/.urvi/workspaces/<hash>/approvals.json).
	workspaceApprovalsName = "approvals.json"
)

// workspaceHash returns the lowercase hex sha256 of the EvalSymlinks-resolved
// workspace root. Resolving symlinks first makes the hash stable across symlink
// aliases of the same directory and matches the containment root resolution, so
// the workspace file is found regardless of which alias the workspace root was
// supplied as. A root that cannot be resolved yields the error so the caller can
// fail secure (treat the workspace store as absent).
func workspaceHash(workspaceRoot string) (string, error) {
	resolved, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return "", &PolicyPathError{Root: workspaceRoot, Reason: "workspace root could not be resolved", Err: err}
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", &PolicyPathError{Root: workspaceRoot, Reason: "workspace root could not be made absolute", Err: err}
	}
	sum := sha256.Sum256([]byte(resolved))
	return hex.EncodeToString(sum[:]), nil
}

// userApprovalsPath returns the path to the user-global approvals file given a
// resolved home directory: <home>/.urvi/approvals.json.
func userApprovalsPath(home string) string {
	return filepath.Join(home, urviDirName, userApprovalsName)
}

// workspaceApprovalsPath returns the path to the workspace-scoped approvals file:
// <home>/.urvi/workspaces/<hash>/approvals.json.
func workspaceApprovalsPath(home, hash string) string {
	return filepath.Join(home, urviDirName, workspacesDirName, hash, workspaceApprovalsName)
}

// PolicyPathError is the typed failure for resolving a policy-store path (e.g. an
// unresolvable workspace root). It is fail-secure: the caller treats a non-nil
// PolicyPathError as "this store is absent", contributing no approvals.
type PolicyPathError struct {
	Root   string // the workspace root being hashed (when applicable)
	Reason string // non-secret, human-readable reason
	Err    error  // underlying cause, may be nil
}

func (e *PolicyPathError) Error() string {
	if e.Err != nil {
		return "tools: policy path error: " + e.Reason + " (root=" + e.Root + "): " + e.Err.Error()
	}
	return "tools: policy path error: " + e.Reason + " (root=" + e.Root + ")"
}

func (e *PolicyPathError) Unwrap() error { return e.Err }

// Filesystem-hardening permission constants (design §3c). The policy store is
// security-sensitive, so directories are owner-only and files are owner
// read/write only — a group/world bit on either is a hardening violation.
const (
	// storeDirPerm is the mode for ~/.urvi and ~/.urvi/workspaces/<hash> (owner rwx).
	storeDirPerm os.FileMode = 0o700
	// storeFilePerm is the mode for the approvals.json file (owner rw).
	storeFilePerm os.FileMode = 0o600
	// groupWorldWritable is the perm-bit mask that flags a file as writable by the
	// group or other ("020" group-write | "002" other-write). The loader rejects
	// any approvals file with either bit set (a non-owner could tamper with it).
	groupWorldWritable os.FileMode = 0o022
)

// assertNoSymlinkComponent walks every path component from base (inclusive,
// exclusive of base's own ancestry) down to full and rejects if ANY component is
// a symlink. It is the §3c "don't follow a symlinked ~/.urvi or workspaces/<hash>"
// rule, shared by the write path (Grant) and the read path (the loader). base
// must be an ancestor of full and is assumed trusted (the resolved home dir); it
// is NOT itself re-checked here (the home dir is the trust anchor). A component
// that does not yet exist is fine (Lstat ErrNotExist is not a violation — Grant
// will create it 0700); any OTHER Lstat error, or a symlink, is a violation.
//
// It uses os.Lstat (which does NOT follow the final component) at each level so a
// symlinked directory is detected rather than traversed.
func assertNoSymlinkComponent(base, full string) error {
	rel, err := filepath.Rel(base, full)
	if err != nil {
		return &PolicyStoreError{Path: full, Reason: "policy path is not relative to home", Err: err}
	}
	// A "." or a ".."-escaping rel means full is not genuinely under base — refuse.
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return &PolicyStoreError{Path: full, Reason: "policy path is not under the home dir"}
	}
	cur := base
	for _, seg := range strings.Split(rel, string(os.PathSeparator)) {
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				// This component (and therefore everything below it) does not exist
				// yet; there is nothing to follow. Stop walking — no violation.
				return nil
			}
			return &PolicyStoreError{Path: cur, Reason: "policy path component could not be stat-ed", Err: err}
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return &PolicyStoreError{Path: cur, Reason: "policy path component is a symlink (refusing to follow)"}
		}
	}
	return nil
}

// mkdirStoreDir creates dir (and any missing parents up to home) at 0700 and
// verifies the final mode is exactly owner-only — defending against a permissive
// umask or a pre-existing too-open directory. base (the resolved home) is assumed
// to exist; only the store sub-tree is created/verified.
func mkdirStoreDir(dir string) error {
	if err := os.MkdirAll(dir, storeDirPerm); err != nil {
		return &PolicyStoreError{Path: dir, Reason: "could not create policy store directory", Err: err}
	}
	// MkdirAll honours the umask, which may have stripped group/other bits already,
	// but an EXISTING dir keeps its old (possibly looser) mode; force 0700.
	if err := os.Chmod(dir, storeDirPerm); err != nil {
		return &PolicyStoreError{Path: dir, Reason: "could not set policy store directory mode", Err: err}
	}
	return nil
}

// writeApprovalsFileAtomically serializes af and writes it to finalPath via a
// temp-file-in-the-same-dir + Rename, with the §3c hardening:
//   - the temp file is opened O_CREATE|O_EXCL|O_WRONLY|O_NOFOLLOW at 0600, so a
//     pre-planted symlink/temp cannot be followed or clobbered;
//   - the bytes are fsync'd before close so the rename publishes durable content;
//   - on ANY failure after creation the temp file is removed (no litter, no
//     half-written file left readable);
//   - os.Rename is atomic within a directory, so a concurrent reader sees either
//     the old file or the new one, never a partial write.
//
// dir (finalPath's directory) must already exist and be hardened by the caller.
func writeApprovalsFileAtomically(dir, finalPath string, af ApprovalsFile) error {
	data, err := marshalApprovals(af)
	if err != nil {
		return err
	}

	// A unique temp name in the SAME dir (so Rename stays on one filesystem).
	tmp, err := uniqueTempPath(dir)
	if err != nil {
		return err
	}

	// #nosec G304 -- tmp = dir (trusted home + fixed store names + a sha256 hash) +
	// a crypto/rand suffix; NOT user input. O_EXCL|O_NOFOLLOW additionally refuse to
	// follow or clobber a pre-planted symlink/file (the §3c write hardening).
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, storeFilePerm)
	if err != nil {
		return &PolicyStoreError{Path: tmp, Reason: "could not create temp approvals file", Err: err}
	}
	// From here on, any failure must remove the temp file.
	if err := writeSyncClose(f, data); err != nil {
		_ = os.Remove(tmp)
		return &PolicyStoreError{Path: tmp, Reason: "could not write temp approvals file", Err: err}
	}
	if err := os.Rename(tmp, finalPath); err != nil {
		_ = os.Remove(tmp)
		return &PolicyStoreError{Path: finalPath, Reason: "could not rename temp approvals file into place", Err: err}
	}
	return nil
}

// writeSyncClose writes data to f, fsyncs, and closes it. It returns the first
// error encountered; Close is always attempted so the fd is not leaked.
func writeSyncClose(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// uniqueTempPath returns a never-before-used temp file path in dir using a
// crypto/rand suffix (collision-resistant; O_EXCL still guards the create). It
// does NOT create the file — the caller opens it O_EXCL|O_NOFOLLOW.
func uniqueTempPath(dir string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", &PolicyStoreError{Path: dir, Reason: "could not generate temp file name", Err: err}
	}
	return filepath.Join(dir, ".approvals-"+hex.EncodeToString(b[:])+".tmp"), nil
}

// marshalApprovals serializes an ApprovalsFile to indented JSON (human-editable
// store). A marshal error (e.g. an out-of-range Effect via Effect.MarshalJSON) is
// returned typed so Grant fails secure without writing.
func marshalApprovals(af ApprovalsFile) ([]byte, error) {
	data, err := json.MarshalIndent(af, "", "  ")
	if err != nil {
		return nil, &PolicyStoreError{Path: "", Reason: "could not marshal approvals", Err: err}
	}
	return data, nil
}

// PolicyStoreError is the typed failure for a policy-store WRITE or a hardening
// violation (a symlinked path component, an unresolvable home dir during Grant, a
// directory-creation/temp-write/rename failure). It is fail-secure: Grant returns
// it WITHOUT having persisted anything to a wrong place. Path names the offending
// path (never file contents).
type PolicyStoreError struct {
	Path   string // the offending store path (never contents)
	Reason string // non-secret, human-readable reason
	Err    error  // underlying cause, may be nil
}

func (e *PolicyStoreError) Error() string {
	if e.Err != nil {
		return "tools: policy store error: " + e.Reason + " (path=" + e.Path + "): " + e.Err.Error()
	}
	return "tools: policy store error: " + e.Reason + " (path=" + e.Path + ")"
}

func (e *PolicyStoreError) Unwrap() error { return e.Err }

// UnsupportedScopeError is the typed failure Grant returns for a scope it will
// not persist: ScopeOnce (the runner never passes it — it persists nothing by
// definition) or any out-of-range ApprovalScope value. It is fail-secure: Grant
// returns it WITHOUT writing a file or adding a session policy.
type UnsupportedScopeError struct {
	Scope uint8 // the offending ApprovalScope value
}

func (e *UnsupportedScopeError) Error() string {
	return "tools: Grant does not persist this approval scope: " + strconv.Itoa(int(e.Scope))
}
