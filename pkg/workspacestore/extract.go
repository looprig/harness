package workspacestore

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// dirCreatePerm is the mode new directories are created with before their
// archived mode is restored: owner-writable/searchable so children can be
// written into them regardless of the (possibly restrictive) archived mode,
// which a final Chmod then applies.
const dirCreatePerm fs.FileMode = 0o755

// maxEntryNameLen bounds the byte length of an archive entry name. It exceeds
// every real filesystem's path limit (Linux PATH_MAX is 4096, darwin 1024), so
// it never rejects a name produced by walking a genuine tree, while it fails a
// hostile name closed before any syscall — the cheapest layer to reject a
// pathologically deep path whose per-component directory creation would
// otherwise amplify one entry into unbounded filesystem work.
const maxEntryNameLen = 4096

// limits bounds a single extraction against decompression bombs: at most
// maxEntries entries read from the tar stream and at most maxBytes cumulative
// bytes written to disk. Both are enforced from observed counts, never from a
// header's self-declared size.
type limits struct {
	maxEntries int64
	maxBytes   int64
}

// extractArchive restores the gzip-compressed tar read from r into dest — the
// trust boundary where an untrusted archive becomes files on disk. The caller
// guarantees dest exists and is empty. Every entry name is validated (no
// absolute paths, no "..", no NUL) and every write is confined by an *os.Root
// sandbox that refuses to traverse or escape via symlinks, so a hostile entry
// name or a planted symlink can never redirect a write outside dest. Entry and
// byte limits guard decompression bombs; device, fifo, and hardlink entries are
// rejected. On any failure the partial output under dest is removed best-effort
// and the raw typed error is returned (Materialize wraps it in *MaterializeError).
func extractArchive(ctx context.Context, r io.Reader, dest string, lim limits) error {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	if err := extractEntries(ctx, root, r, lim); err != nil {
		removeContents(dest)
		return err
	}
	return nil
}

// extractEntries reads the gzip tar stream and materializes each entry through
// root, enforcing the entry-count and cumulative-byte limits as it goes. It
// stops and returns the first error — a hostile entry, a tripped limit, a
// cancelled context, or an I/O fault — leaving cleanup to the caller.
func extractEntries(ctx context.Context, root *os.Root, r io.Reader, lim limits) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	var seen, written int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		seen++
		if seen > lim.maxEntries {
			return &ArchiveLimitError{Limit: ArchiveLimitEntries, Cap: lim.maxEntries, Observed: seen}
		}
		n, err := extractEntry(root, hdr, tr, lim, written)
		if err != nil {
			return err
		}
		written += n
	}
}

// extractEntry validates one entry's name, confirms no ancestor component is a
// symlink, and dispatches on the entry type. It returns the number of content
// bytes written (nonzero only for regular files) so the caller can maintain the
// cumulative-byte total.
func extractEntry(root *os.Root, hdr *tar.Header, tr io.Reader, lim limits, written int64) (int64, error) {
	if err := validateEntryName(hdr.Name); err != nil {
		return 0, err
	}
	name := filepath.FromSlash(path.Clean(hdr.Name))
	if err := rejectSymlinkAncestor(root, hdr.Name, name); err != nil {
		return 0, err
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		return 0, extractDir(root, name, hdr)
	case tar.TypeReg:
		// tar.Reader normalizes the deprecated old-style regular-file flag
		// (TypeRegA) to TypeReg on read, so this one case covers every regular
		// file; any other flag falls through to the fail-closed default.
		return extractRegular(root, name, hdr, tr, lim, written)
	case tar.TypeSymlink:
		return 0, extractSymlink(root, name, hdr)
	default:
		return 0, &ArchiveEntryError{Name: hdr.Name, Reason: "unsupported entry type: " + entryTypeName(hdr.Typeflag)}
	}
}

// validateEntryName enforces the entry-name grammar every archive entry must
// satisfy: non-empty, no NUL byte, not absolute, no ".." segment, and not a name
// that cleans to the destination root itself. A violation yields *ArchiveEntryError
// with the offending name and the rule broken. This is the function the fuzz
// target exercises directly; the os.Root sandbox is the independent second layer.
func validateEntryName(name string) error {
	if name == "" {
		return &ArchiveEntryError{Name: name, Reason: "empty entry name"}
	}
	if len(name) > maxEntryNameLen {
		return &ArchiveEntryError{Name: name[:64] + "...(truncated)", Reason: "entry name exceeds " + strconv.Itoa(maxEntryNameLen) + " bytes"}
	}
	if strings.IndexByte(name, 0) >= 0 {
		return &ArchiveEntryError{Name: name, Reason: "entry name contains a NUL byte"}
	}
	if path.IsAbs(name) {
		return &ArchiveEntryError{Name: name, Reason: "absolute entry name"}
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return &ArchiveEntryError{Name: name, Reason: `entry name contains a ".." segment`}
		}
	}
	cleaned := path.Clean(name)
	if cleaned == "." {
		return &ArchiveEntryError{Name: name, Reason: "entry name resolves to the destination root"}
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned) {
		return &ArchiveEntryError{Name: name, Reason: "entry name escapes the destination"}
	}
	return nil
}

// rejectSymlinkAncestor verifies this entry's immediate parent directory is not a
// symlink, so a symlink planted by an earlier entry can never be traversed to
// redirect this entry's write. It Lstats the parent through the root sandbox,
// which does not follow the final component (revealing a symlink parent as such)
// and refuses to follow any escaping symlink deeper in the path (surfacing that
// as a plain sandbox error) — os.Root is the independent backstop for the deeper
// case. A not-yet-created parent is fine: MkdirAll will make it a real directory.
// origName is the raw entry name used in any resulting *ArchiveEntryError.
func rejectSymlinkAncestor(root *os.Root, origName, name string) error {
	dir := filepath.Dir(name)
	if dir == "." || dir == string(filepath.Separator) {
		return nil
	}
	info, err := root.Lstat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // parent does not exist yet; nothing to traverse
		}
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return &ArchiveEntryError{Name: origName, Reason: "the parent path component is a symlink"}
	}
	return nil
}

// extractDir creates the directory named name (with any missing ancestors) and
// restores its archived permission bits. Directories are first created
// world-nothing-beyond dirCreatePerm so children remain writable, then chmod'd
// to the exact archived mode.
func extractDir(root *os.Root, name string, hdr *tar.Header) error {
	if err := root.MkdirAll(name, dirCreatePerm); err != nil {
		return err
	}
	return root.Chmod(name, hdr.FileInfo().Mode().Perm())
}

// extractSymlink creates name as a symlink to the archived target. The target is
// written verbatim (never resolved): a hostile absolute or escaping target is
// stored as-is but is inert, because every subsequent write is name-validated and
// refuses to traverse a symlink component (see rejectSymlinkAncestor and os.Root).
func extractSymlink(root *os.Root, name string, hdr *tar.Header) error {
	if err := ensureParent(root, name); err != nil {
		return err
	}
	return root.Symlink(hdr.Linkname, name)
}

// extractRegular writes a regular file's contents, capping the copy at the
// remaining byte budget so a bomb cannot inflate past lim.maxBytes. It returns
// the number of bytes written even on error so the caller's cumulative total
// stays accurate for the *ArchiveLimitError it may raise. The file mode is
// restricted to permission bits (setuid/setgid/sticky are never restored from an
// untrusted archive) and applied after the write to defeat the umask.
func extractRegular(root *os.Root, name string, hdr *tar.Header, tr io.Reader, lim limits, written int64) (int64, error) {
	if err := ensureParent(root, name); err != nil {
		return 0, err
	}
	perm := hdr.FileInfo().Mode().Perm()
	// name is validated (no absolute, no "..") and confined to the os.Root
	// sandbox, which refuses any symlink traversal or escape; O_EXCL fails closed
	// on a duplicate/overwrite entry.
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) // #nosec G304 -- validated name inside os.Root sandbox
	if err != nil {
		return 0, err
	}
	n, exceeded, copyErr := copyCapped(f, tr, lim.maxBytes-written)
	closeErr := f.Close()
	if copyErr != nil {
		return n, copyErr
	}
	if exceeded {
		return n, &ArchiveLimitError{Limit: ArchiveLimitBytes, Cap: lim.maxBytes, Observed: written + n}
	}
	if closeErr != nil {
		return n, closeErr
	}
	return n, root.Chmod(name, perm)
}

// copyCapped copies from src to dst, reading at most remaining+1 bytes. It
// reports the bytes copied and whether the source held more than the budget
// allowed (n > remaining): reading the one extra byte is what proves the overflow
// without trusting any declared size. A negative remaining is treated as zero.
func copyCapped(dst io.Writer, src io.Reader, remaining int64) (int64, bool, error) {
	if remaining < 0 {
		remaining = 0
	}
	lr := &io.LimitedReader{R: src, N: remaining + 1}
	n, err := io.Copy(dst, lr)
	if err != nil {
		return n, false, err
	}
	return n, n > remaining, nil
}

// ensureParent creates the parent directory chain for name if name is nested.
// Directories are created with dirCreatePerm; an explicit directory entry (if
// present in the archive) later restores the exact archived mode.
func ensureParent(root *os.Root, name string) error {
	dir := filepath.Dir(name)
	if dir == "." || dir == string(filepath.Separator) {
		return nil
	}
	return root.MkdirAll(dir, dirCreatePerm)
}

// removeContents best-effort deletes every top-level entry under dest, emptying
// it without removing dest itself (which the caller created). os.RemoveAll does
// not follow symlinks, so a symlink planted during a partial extraction is
// unlinked, never traversed. Errors are ignored: cleanup is best-effort and the
// original extraction error is the meaningful one.
func removeContents(dest string) {
	entries, err := os.ReadDir(dest)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dest, e.Name()))
	}
}

// entryTypeName names a rejected tar entry type for an *ArchiveEntryError reason.
// All returned values are static or a numeric flag, so the reason stays log-safe.
func entryTypeName(flag byte) string {
	switch flag {
	case tar.TypeLink:
		return "hard link"
	case tar.TypeChar:
		return "character device"
	case tar.TypeBlock:
		return "block device"
	case tar.TypeFifo:
		return "fifo"
	default:
		return "typeflag " + strconv.Itoa(int(flag))
	}
}
