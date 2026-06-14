package tui

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
)

// writeFile creates a file under dir with the given relative name and contents,
// creating intermediate directories as needed. It returns the absolute path.
func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", p, err)
	}
	return p
}

func TestBuildBlocksTextOnly(t *testing.T) {
	t.Parallel()
	got, err := buildBlocks("hello world", true)
	if err != nil {
		t.Fatalf("buildBlocks() error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(blocks) = %d, want 1", len(got))
	}
	tb, ok := got[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("block[0] = %T, want *content.TextBlock", got[0])
	}
	if tb.Text != "hello world" {
		t.Errorf("Text = %q, want %q", tb.Text, "hello world")
	}
}

func TestBuildBlocksEmptyInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
	}{
		{name: "empty string", input: ""},
		{name: "whitespace only", input: "   "},
		{name: "tabs and newlines", input: "\t \n  "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := buildBlocks(tt.input, true)
			var target *EmptyInputError
			if !errors.As(err, &target) {
				t.Fatalf("buildBlocks(%q) error = %v, want *EmptyInputError", tt.input, err)
			}
		})
	}
}

func TestBuildBlocksImageAllowed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a}
	p := writeFile(t, dir, "a.png", want)

	got, err := buildBlocks("@"+p, true)
	if err != nil {
		t.Fatalf("buildBlocks() error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(blocks) = %d, want 1", len(got))
	}
	ib, ok := got[0].(*content.ImageBlock)
	if !ok {
		t.Fatalf("block[0] = %T, want *content.ImageBlock", got[0])
	}
	if ib.MediaType != content.MediaTypeImagePNG {
		t.Errorf("MediaType = %q, want %q", ib.MediaType, content.MediaTypeImagePNG)
	}
	if !bytes.Equal(ib.Source.Data, want) {
		t.Errorf("Data = %v, want %v", ib.Source.Data, want)
	}
}

func TestBuildBlocksImageMediaTypeByExt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ext  string
		want content.MediaType
	}{
		{name: "png", ext: ".png", want: content.MediaTypeImagePNG},
		{name: "jpg", ext: ".jpg", want: content.MediaTypeImageJPEG},
		{name: "jpeg", ext: ".jpeg", want: content.MediaTypeImageJPEG},
		{name: "gif", ext: ".gif", want: content.MediaTypeImageGIF},
		{name: "webp", ext: ".webp", want: content.MediaTypeImageWebP},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			p := writeFile(t, dir, "img"+tt.ext, []byte("data"))
			got, err := buildBlocks("@"+p, true)
			if err != nil {
				t.Fatalf("buildBlocks() error = %v", err)
			}
			ib, ok := got[0].(*content.ImageBlock)
			if !ok {
				t.Fatalf("block[0] = %T, want *content.ImageBlock", got[0])
			}
			if ib.MediaType != tt.want {
				t.Errorf("MediaType = %q, want %q", ib.MediaType, tt.want)
			}
		})
	}
}

func TestBuildBlocksImageDisallowedReturnsBeforeOpen(t *testing.T) {
	t.Parallel()
	// Path deliberately does not exist: proves classify-before-open. If the
	// implementation opened the file, it would surface AttachmentNotFoundError.
	_, err := buildBlocks("@/does/not/exist/a.png", false)
	var target *ImageUnsupportedError
	if !errors.As(err, &target) {
		t.Fatalf("error = %v, want *ImageUnsupportedError", err)
	}
	if target.Ext != ".png" {
		t.Errorf("Ext = %q, want %q", target.Ext, ".png")
	}
}

func TestBuildBlocksPlaintext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := writeFile(t, dir, "a.txt", []byte("abc"))
	got, err := buildBlocks("@"+p, true)
	if err != nil {
		t.Fatalf("buildBlocks() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(blocks) = %d, want 1", len(got))
	}
	tb, ok := got[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("block[0] = %T, want *content.TextBlock", got[0])
	}
	if tb.Text != "[a.txt]\nabc" {
		t.Errorf("Text = %q, want %q", tb.Text, "[a.txt]\nabc")
	}
}

func TestBuildBlocksSVGIsPlaintext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// allowImages=false to prove .svg is NOT treated as an image (would be
	// rejected as ImageUnsupportedError if it were).
	p := writeFile(t, dir, "a.svg", []byte("<svg/>"))
	got, err := buildBlocks("@"+p, false)
	if err != nil {
		t.Fatalf("buildBlocks() error = %v, want nil (svg is plaintext)", err)
	}
	tb, ok := got[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("block[0] = %T, want *content.TextBlock", got[0])
	}
	if tb.Text != "[a.svg]\n<svg/>" {
		t.Errorf("Text = %q, want %q", tb.Text, "[a.svg]\n<svg/>")
	}
}

func TestBuildBlocksUnknownExtReturnsBeforeOpen(t *testing.T) {
	t.Parallel()
	// Path need not exist: classify rejects before any open.
	_, err := buildBlocks("@/does/not/exist/a.xyz", true)
	var target *UnsupportedAttachmentError
	if !errors.As(err, &target) {
		t.Fatalf("error = %v, want *UnsupportedAttachmentError", err)
	}
	if target.Ext != ".xyz" {
		t.Errorf("Ext = %q, want %q", target.Ext, ".xyz")
	}
}

func TestBuildBlocksMissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "nope.txt") // accepted ext, but file absent
	_, err := buildBlocks("@"+p, true)
	var target *AttachmentNotFoundError
	if !errors.As(err, &target) {
		t.Fatalf("error = %v, want *AttachmentNotFoundError", err)
	}
}

func TestBuildBlocksDirectoryNotRegular(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	d := filepath.Join(dir, "d.txt") // accepted ext, but it's a directory
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	_, err := buildBlocks("@"+d, true)
	var target *DeniedAttachmentError
	if !errors.As(err, &target) {
		t.Fatalf("error = %v, want *DeniedAttachmentError", err)
	}
	if !strings.Contains(target.Reason, "regular") {
		t.Errorf("Reason = %q, want it to mention 'regular'", target.Reason)
	}
}

func TestBuildBlocksSymlinkRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := writeFile(t, dir, "target.txt", []byte("secret"))
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	_, err := buildBlocks("@"+link, true)
	var denied *DeniedAttachmentError
	if !errors.As(err, &denied) {
		t.Fatalf("error = %v, want *DeniedAttachmentError (O_NOFOLLOW)", err)
	}
}

func TestBuildBlocksDenied(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		token string // appended after "@"
	}{
		{name: "denied basename .env", token: ".env"},
		{name: "denied basename .env.local pattern", token: ".env.local"},
		{name: "denied basename .npmrc", token: ".npmrc"},
		{name: "denied basename id_rsa", token: "id_rsa"},
		{name: "denied basename id_ed25519", token: "id_ed25519"},
		{name: "denied segment .ssh", token: ".ssh/known.txt"},
		{name: "denied segment .aws", token: ".aws/credentials.txt"},
		{name: "denied segment .kube", token: ".kube/config.txt"},
		{name: "denied ext .pem", token: "k.pem"},
		{name: "denied ext .key", token: "k.key"},
		{name: "denied ext .p12", token: "k.p12"},
		{name: "denied ext uppercase .PEM", token: "K.PEM"},
		{name: "denied segment uppercase .SSH", token: ".SSH/x.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Use a non-existent absolute path: denylist must fire before any open.
			p := filepath.Join("/does/not/exist", tt.token)
			_, err := buildBlocks("@"+p, true)
			var target *DeniedAttachmentError
			if !errors.As(err, &target) {
				t.Fatalf("buildBlocks(@%s) error = %v, want *DeniedAttachmentError", p, err)
			}
		})
	}
}

func TestBuildBlocksTooLarge(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const maxBytes = 5 << 20
	big := bytes.Repeat([]byte("a"), maxBytes+1)
	p := writeFile(t, dir, "big.txt", big)
	_, err := buildBlocks("@"+p, true)
	var target *AttachmentTooLargeError
	if !errors.As(err, &target) {
		t.Fatalf("error = %v, want *AttachmentTooLargeError", err)
	}
	if target.Max != maxBytes {
		t.Errorf("Max = %d, want %d", target.Max, int64(maxBytes))
	}
}

func TestBuildBlocksPromptThenAttachmentOrdering(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := writeFile(t, dir, "a.txt", []byte("x"))
	// "see @a.txt now" — prompt words "see" and "now" rejoin single-spaced.
	got, err := buildBlocks("see @"+p+" now", true)
	if err != nil {
		t.Fatalf("buildBlocks() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(blocks) = %d, want 2", len(got))
	}
	lead, ok := got[0].(*content.TextBlock)
	if !ok || lead.Text != "see now" {
		t.Fatalf("block[0] = %#v, want *TextBlock{Text:%q}", got[0], "see now")
	}
	att, ok := got[1].(*content.TextBlock)
	if !ok || att.Text != "[a.txt]\nx" {
		t.Fatalf("block[1] = %#v, want *TextBlock{Text:%q}", got[1], "[a.txt]\nx")
	}
}

func TestBuildBlocksMultipleMixed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	txt := writeFile(t, dir, "note.txt", []byte("hi"))
	imgData := []byte{1, 2, 3}
	img := writeFile(t, dir, "pic.png", imgData)

	got, err := buildBlocks("look @"+txt+" and @"+img, true)
	if err != nil {
		t.Fatalf("buildBlocks() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(blocks) = %d, want 3", len(got))
	}
	lead, ok := got[0].(*content.TextBlock)
	if !ok || lead.Text != "look and" {
		t.Fatalf("block[0] = %#v, want *TextBlock{Text:%q}", got[0], "look and")
	}
	noteB, ok := got[1].(*content.TextBlock)
	if !ok || noteB.Text != "[note.txt]\nhi" {
		t.Fatalf("block[1] = %#v, want note TextBlock", got[1])
	}
	picB, ok := got[2].(*content.ImageBlock)
	if !ok {
		t.Fatalf("block[2] = %T, want *content.ImageBlock", got[2])
	}
	if picB.MediaType != content.MediaTypeImagePNG || !bytes.Equal(picB.Source.Data, imgData) {
		t.Errorf("image block = %#v, want png with data %v", picB, imgData)
	}
}

func TestBuildBlocksAttachmentOnlyNoLeadingText(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := writeFile(t, dir, "a.txt", []byte("x"))
	got, err := buildBlocks("@"+p, true)
	if err != nil {
		t.Fatalf("buildBlocks() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(blocks) = %d, want 1 (no leading empty text block)", len(got))
	}
}
