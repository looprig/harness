package tui

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/inventivepotter/urvi/internal/content"
)

// maxAttachmentBytes caps the size of any single @path attachment (5 MiB).
const maxAttachmentBytes int64 = 5 << 20

// deniedSegments are path components that, if present, deny the whole path.
var deniedSegments = map[string]struct{}{
	".ssh": {}, ".aws": {}, ".gcloud": {}, ".gnupg": {}, ".kube": {},
}

// deniedBasenames are exact lower-cased basenames that are always denied.
var deniedBasenames = map[string]struct{}{
	".env": {}, ".npmrc": {}, ".netrc": {}, ".pypirc": {}, ".dockercfg": {},
	"id_rsa": {}, "id_dsa": {}, "id_ecdsa": {}, "id_ed25519": {},
}

// deniedExts are lower-cased extensions (with dot) that are always denied.
var deniedExts = map[string]struct{}{
	".env": {}, ".pem": {}, ".key": {}, ".p12": {},
	".pfx": {}, ".jks": {}, ".keystore": {},
}

// imageExts map a lower-cased extension to its image media type.
var imageExts = map[string]content.MediaType{
	".png":  content.MediaTypeImagePNG,
	".jpg":  content.MediaTypeImageJPEG,
	".jpeg": content.MediaTypeImageJPEG,
	".gif":  content.MediaTypeImageGIF,
	".webp": content.MediaTypeImageWebP,
}

// plaintextExts are lower-cased extensions read as UTF-8 text. .svg is here
// (XML), deliberately NOT an image, because providers reject image/svg+xml.
var plaintextExts = map[string]struct{}{
	".txt": {}, ".md": {}, ".go": {}, ".py": {}, ".js": {}, ".ts": {},
	".json": {}, ".yaml": {}, ".yml": {}, ".toml": {}, ".sh": {}, ".csv": {},
	".html": {}, ".xml": {}, ".rs": {}, ".java": {}, ".c": {}, ".cpp": {},
	".h": {}, ".svg": {},
}

// buildBlocks parses input into content blocks: a leading prompt TextBlock
// (when non-empty) followed by one block per @path attachment, in order.
// allowImages gates image attachments; when false an image @path is rejected
// at the boundary rather than sent to a text-only model.
//
// Classification (denylist + extension/modality) happens strictly BEFORE any
// filesystem syscall, so a denied, unsupported, or text-only-model attachment
// is rejected without ever opening a file.
func buildBlocks(input string, allowImages bool) ([]content.Block, error) {
	prompt, attachments := splitInput(input)

	if len(attachments) == 0 && prompt == "" {
		return nil, &EmptyInputError{}
	}

	blocks := make([]content.Block, 0, len(attachments)+1)
	if prompt != "" {
		blocks = append(blocks, &content.TextBlock{Text: prompt})
	}
	for _, path := range attachments {
		block, err := buildAttachment(path, allowImages)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

// splitInput tokenizes on whitespace, separating @path attachments from the
// remaining words, which rejoin single-spaced into the prompt text.
func splitInput(input string) (prompt string, attachments []string) {
	var words []string
	for _, tok := range strings.Fields(input) {
		if len(tok) > 1 && tok[0] == '@' {
			attachments = append(attachments, tok[1:])
			continue
		}
		words = append(words, tok)
	}
	return strings.Join(words, " "), attachments
}

// buildAttachment classifies path, then (only for an accepted classification)
// opens, stats, and reads the file into a content block.
func buildAttachment(path string, allowImages bool) (content.Block, error) {
	clean := filepath.Clean(path)
	if reason, denied := denyReason(clean); denied {
		return nil, &DeniedAttachmentError{Path: clean, Reason: reason}
	}

	ext := strings.ToLower(filepath.Ext(clean))
	mediaType, isImage := imageExts[ext]
	switch {
	case isImage:
		if !allowImages {
			return nil, &ImageUnsupportedError{Ext: ext}
		}
		data, err := readAttachment(clean)
		if err != nil {
			return nil, err
		}
		return &content.ImageBlock{MediaType: mediaType, Source: content.ImageSource{Data: data}}, nil
	case isPlaintextExt(ext):
		data, err := readAttachment(clean)
		if err != nil {
			return nil, err
		}
		return &content.TextBlock{Text: "[" + filepath.Base(clean) + "]\n" + string(data)}, nil
	default:
		return nil, &UnsupportedAttachmentError{Ext: ext}
	}
}

func isPlaintextExt(ext string) bool {
	_, ok := plaintextExts[ext]
	return ok
}

// denyReason reports whether clean matches the secret/credential denylist,
// inspecting lower-cased path segments, basename, and extension. It performs
// no I/O.
func denyReason(clean string) (string, bool) {
	for _, seg := range strings.Split(clean, string(os.PathSeparator)) {
		if _, ok := deniedSegments[strings.ToLower(seg)]; ok {
			return "denied path segment " + strings.ToLower(seg), true
		}
	}

	base := strings.ToLower(filepath.Base(clean))
	if _, ok := deniedBasenames[base]; ok {
		return "denied basename " + base, true
	}
	if strings.HasPrefix(base, ".env.") {
		return "denied basename pattern .env.*", true
	}

	ext := strings.ToLower(filepath.Ext(clean))
	if _, ok := deniedExts[ext]; ok {
		return "denied extension " + ext, true
	}
	return "", false
}

// readAttachment opens clean with O_NOFOLLOW (rejecting a symlinked final
// component), confirms it is a regular file via fd stat, enforces the size cap
// at stat time and again after a bounded read, and returns the bytes.
func readAttachment(clean string) ([]byte, error) {
	// #nosec G304 -- user-selected local path, validated by denylist + classify + O_NOFOLLOW + fd stat
	f, err := os.OpenFile(clean, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &AttachmentNotFoundError{Path: clean, Cause: err}
		}
		if errors.Is(err, syscall.ELOOP) {
			return nil, &DeniedAttachmentError{Path: clean, Reason: "symlinked path component (O_NOFOLLOW)"}
		}
		return nil, &DeniedAttachmentError{Path: clean, Reason: "cannot open (symlink or permission)"}
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, &AttachmentReadError{Path: clean, Cause: err}
	}
	if !fi.Mode().IsRegular() {
		return nil, &DeniedAttachmentError{Path: clean, Reason: "not a regular file"}
	}
	if fi.Size() > maxAttachmentBytes {
		return nil, &AttachmentTooLargeError{Path: clean, Size: fi.Size(), Max: maxAttachmentBytes}
	}

	data, err := io.ReadAll(io.LimitReader(f, maxAttachmentBytes+1))
	if err != nil {
		return nil, &AttachmentReadError{Path: clean, Cause: err}
	}
	if int64(len(data)) > maxAttachmentBytes {
		return nil, &AttachmentTooLargeError{Path: clean, Size: int64(len(data)), Max: maxAttachmentBytes}
	}
	return data, nil
}
