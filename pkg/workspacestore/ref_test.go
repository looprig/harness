package workspacestore

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

// validHex is a real, well-formed lowercase sha256 digest (of the empty input):
// exactly 64 hex characters. Tests build valid Refs by prefixing "v1:sha256:".
const validHex = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// storageName mirrors the storage name grammar (segments matching
// [a-z0-9][a-z0-9_.-]* joined by single '/') so blobKey can be asserted valid
// without importing storage (A1 is pure types; the storage dependency lands in
// a later task).
var storageName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*(/[a-z0-9][a-z0-9_.-]*)*$`)

func TestParseRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "valid canonical digest", input: "v1:sha256:" + validHex},
		{name: "valid all-a digest", input: "v1:sha256:" + strings.Repeat("a", 64)},
		{name: "valid all-digit digest", input: "v1:sha256:" + strings.Repeat("0", 64)},
		{name: "valid mixed digits and letters", input: "v1:sha256:" + strings.Repeat("ab12", 16)},

		{name: "empty string", input: "", wantErr: true},
		{name: "unknown version v2", input: "v2:sha256:" + validHex, wantErr: true},
		{name: "unknown version v0", input: "v0:sha256:" + validHex, wantErr: true},
		{name: "bad algorithm md5", input: "v1:md5:" + validHex, wantErr: true},
		{name: "bad algorithm sha512", input: "v1:sha512:" + validHex, wantErr: true},
		{name: "digest too short 63 hex", input: "v1:sha256:" + strings.Repeat("a", 63), wantErr: true},
		{name: "digest too long 65 hex", input: "v1:sha256:" + strings.Repeat("a", 65), wantErr: true},
		{name: "empty digest", input: "v1:sha256:", wantErr: true},
		{name: "uppercase hex", input: "v1:sha256:" + strings.ToUpper(validHex), wantErr: true},
		{name: "one uppercase hex char", input: "v1:sha256:A" + validHex[1:], wantErr: true},
		{name: "non-hex letter g", input: "v1:sha256:" + strings.Repeat("g", 64), wantErr: true},
		{name: "non-hex letter z at end", input: "v1:sha256:" + validHex[:63] + "z", wantErr: true},
		{name: "missing colons entirely", input: "v1sha256" + validHex, wantErr: true},
		{name: "prefix only no colon before digest", input: "v1:sha256" + validHex, wantErr: true},
		{name: "leading whitespace", input: " v1:sha256:" + validHex, wantErr: true},
		{name: "trailing whitespace", input: "v1:sha256:" + validHex + " ", wantErr: true},
		{name: "bare digest no prefix", input: validHex, wantErr: true},
		{name: "extra colon suffix", input: "v1:sha256:" + validHex + ":", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseRef(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseRef(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}

			if tt.wantErr {
				var ire *InvalidRefError
				if !errors.As(err, &ire) {
					t.Fatalf("ParseRef(%q) error = %v, want *InvalidRefError", tt.input, err)
				}
				if ire.Reason == "" {
					t.Errorf("ParseRef(%q) *InvalidRefError.Reason is empty; want a stated rule", tt.input)
				}
				if ire.Value != tt.input {
					t.Errorf("ParseRef(%q) *InvalidRefError.Value = %q, want the rejected input", tt.input, ire.Value)
				}
				if got != "" {
					t.Errorf("ParseRef(%q) returned Ref %q on error; want empty", tt.input, got)
				}
				return
			}

			if string(got) != tt.input {
				t.Errorf("ParseRef(%q) = %q, want the input round-tripped", tt.input, got)
			}
		})
	}
}

func TestRefBlobKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		hex  string
	}{
		{name: "canonical digest", hex: validHex},
		{name: "all a", hex: strings.Repeat("a", 64)},
		{name: "all zero", hex: strings.Repeat("0", 64)},
		{name: "mixed", hex: strings.Repeat("ab12", 16)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ref, err := ParseRef("v1:sha256:" + tt.hex)
			if err != nil {
				t.Fatalf("ParseRef of a valid ref failed: %v", err)
			}

			want := "workspaces/" + tt.hex
			if got := ref.blobKey(); got != want {
				t.Errorf("blobKey() = %q, want %q", got, want)
			}
			if got := ref.hex(); got != tt.hex {
				t.Errorf("hex() = %q, want %q", got, tt.hex)
			}
			if !storageName.MatchString(ref.blobKey()) {
				t.Errorf("blobKey() = %q is not a valid storage name", ref.blobKey())
			}
		})
	}
}

// errSentinelCause is a leaf error used to prove the wrapping error types expose
// their cause via errors.Is / errors.Unwrap.
var errSentinelCause = errors.New("workspacestore_test: sentinel cause")

func TestWrappingErrorsUnwrap(t *testing.T) {
	t.Parallel()

	ref := Ref("v1:sha256:" + validHex)

	tests := []struct {
		name string
		err  error
	}{
		{name: "SnapshotError", err: &SnapshotError{Root: "/work/root", Cause: errSentinelCause}},
		{name: "MaterializeError", err: &MaterializeError{Ref: ref, Dest: "/work/dest", Cause: errSentinelCause}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if !errors.Is(tt.err, errSentinelCause) {
				t.Errorf("%s: errors.Is(err, errSentinelCause) = false; want true (Unwrap must reach Cause)", tt.name)
			}
			if got := errors.Unwrap(tt.err); got != errSentinelCause {
				t.Errorf("%s: errors.Unwrap() = %v, want the sentinel cause", tt.name, got)
			}
		})
	}
}

func TestNonWrappingErrorsDoNotUnwrap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{name: "InvalidRefError", err: &InvalidRefError{Value: "bad", Reason: "bad"}},
		{name: "DestNotEmptyError", err: &DestNotEmptyError{Dest: "/d", Want: Ref("v1:sha256:" + validHex), GotDigest: validHex}},
		{name: "ArchiveEntryError", err: &ArchiveEntryError{Name: "../escape", Reason: "traversal"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, ok := tt.err.(interface{ Unwrap() error }); ok {
				t.Errorf("%s unexpectedly implements Unwrap; it carries no cause", tt.name)
			}
		})
	}
}

func TestErrorMessages(t *testing.T) {
	t.Parallel()

	ref := Ref("v1:sha256:" + validHex)

	tests := []struct {
		name     string
		err      error
		contains []string
	}{
		{
			name:     "InvalidRefError names value and reason",
			err:      &InvalidRefError{Value: "v9:sha256:xyz", Reason: "unknown version"},
			contains: []string{"v9:sha256:xyz", "unknown version"},
		},
		{
			name:     "DestNotEmptyError names dest and ref",
			err:      &DestNotEmptyError{Dest: "/work/dest", Want: ref, GotDigest: strings.Repeat("f", 64)},
			contains: []string{"/work/dest", string(ref)},
		},
		{
			name:     "SnapshotError names root and cause",
			err:      &SnapshotError{Root: "/work/root", Cause: errSentinelCause},
			contains: []string{"/work/root", errSentinelCause.Error()},
		},
		{
			name:     "MaterializeError names ref, dest and cause",
			err:      &MaterializeError{Ref: ref, Dest: "/work/dest", Cause: errSentinelCause},
			contains: []string{string(ref), "/work/dest", errSentinelCause.Error()},
		},
		{
			name:     "ArchiveEntryError names entry and reason",
			err:      &ArchiveEntryError{Name: "../../etc/passwd", Reason: "path traversal"},
			contains: []string{"../../etc/passwd", "path traversal"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg := tt.err.Error()
			if msg == "" {
				t.Fatalf("%s: Error() is empty", tt.name)
			}
			if !strings.HasPrefix(msg, "workspacestore: ") {
				t.Errorf("%s: Error() = %q, want a \"workspacestore: \" prefix", tt.name, msg)
			}
			for _, want := range tt.contains {
				if !strings.Contains(msg, want) {
					t.Errorf("%s: Error() = %q, want it to contain %q", tt.name, msg, want)
				}
			}
			if strings.ContainsAny(msg, "\n\r") {
				t.Errorf("%s: Error() = %q contains a newline; messages must be single-line log-safe", tt.name, msg)
			}
		})
	}
}
