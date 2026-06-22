package tui

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzBuildBlocks exercises tokenize + denylist + ext/modality classification +
// error typing. Every @path token is rewritten so it can only point inside a
// per-run t.TempDir(): buildBlocks can never open a host path. The point is
// parser/classify/error-typing robustness, not arbitrary filesystem reads.
func FuzzBuildBlocks(f *testing.F) {
	f.Add("hi", true)
	f.Add("@a.txt", true)
	f.Add("@", false)
	f.Add("@../x", true)
	f.Add("x\x00y", true)
	f.Add("@a.png @b.svg text", true)
	f.Add("@"+strings.Repeat("a", 8192)+".txt", true)
	f.Add("   ", false)
	f.Add("@.env @.ssh/id_rsa @k.pem", true)

	f.Fuzz(func(t *testing.T, input string, allow bool) {
		tmp := t.TempDir()
		rewritten := rewriteTokensToTemp(input, tmp)

		got, err := buildBlocks(rewritten, allow)
		if err != nil {
			if !isKnownBuildError(err) {
				t.Fatalf("buildBlocks(%q) returned unrecognized error type %T: %v", rewritten, err, err)
			}
			return
		}
		if len(got) == 0 {
			t.Fatalf("buildBlocks(%q) returned nil error but empty blocks", rewritten)
		}
		for i, b := range got {
			if b == nil {
				t.Fatalf("buildBlocks(%q) block[%d] is nil", rewritten, i)
			}
		}
	})
}

// rewriteTokensToTemp confines every @path token to tmp by replacing it with
// @<tmp>/<basename>. A token whose cleaned basename would escape tmp (".", "..",
// empty, or still containing a separator) is dropped entirely.
func rewriteTokensToTemp(input, tmp string) string {
	out := make([]string, 0, len(strings.Fields(input)))
	for _, tok := range strings.Fields(input) {
		if len(tok) > 1 && tok[0] == '@' {
			base := filepath.Base(filepath.Clean(tok[1:]))
			if base == "." || base == ".." || base == "" || strings.ContainsRune(base, filepath.Separator) {
				continue
			}
			out = append(out, "@"+filepath.Join(tmp, base))
			continue
		}
		out = append(out, tok)
	}
	return strings.Join(out, " ")
}

// isKnownBuildError reports whether err is one of buildBlocks' typed errors.
func isKnownBuildError(err error) bool {
	var (
		empty       *EmptyInputError
		unsupported *UnsupportedAttachmentError
		binary      *BinaryAttachmentError
		imageUnsup  *ImageUnsupportedError
		denied      *DeniedAttachmentError
		tooLarge    *AttachmentTooLargeError
		notFound    *AttachmentNotFoundError
		readErr     *AttachmentReadError
	)
	switch {
	case errors.As(err, &empty),
		errors.As(err, &unsupported),
		errors.As(err, &binary),
		errors.As(err, &imageUnsup),
		errors.As(err, &denied),
		errors.As(err, &tooLarge),
		errors.As(err, &notFound),
		errors.As(err, &readErr):
		return true
	default:
		return false
	}
}
