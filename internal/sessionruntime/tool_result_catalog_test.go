package sessionruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// memoryToolResultObjects is an in-memory loop.ToolResultObjects that scopes
// every object by session, mints its own references, and can be told to serve
// corrupted bytes.
type memoryToolResultObjects struct {
	mu      sync.Mutex
	objects map[string]memoryToolResultObject
	next    int
	corrupt bool
	opens   int
}

type memoryToolResultObject struct {
	session uuid.UUID
	data    []byte
}

func newMemoryToolResultObjects() *memoryToolResultObjects {
	return &memoryToolResultObjects{objects: map[string]memoryToolResultObject{}}
}

func (m *memoryToolResultObjects) PublishToolResultObject(_ context.Context, session uuid.UUID, body io.Reader, size uint64, sum [32]byte) (sessionwire.ObjectMetadata, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return sessionwire.ObjectMetadata{}, err
	}
	if uint64(len(data)) != size || sha256.Sum256(data) != sum {
		return sessionwire.ObjectMetadata{}, errors.New("memory objects: body mismatch")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	id := "v1:tool-result:" + strings.Repeat("g", m.next) + ":" + hex.EncodeToString(sum[:])
	m.objects[id] = memoryToolResultObject{session: session, data: data}
	return sessionwire.ObjectMetadata{Reference: sessionwire.ObjectReference{ObjectID: id}, SizeBytes: size, Digest: "sha256:" + hex.EncodeToString(sum[:])}, nil
}

func (m *memoryToolResultObjects) OpenToolResultObject(_ context.Context, session uuid.UUID, ref sessionwire.ObjectReference) (sessionwire.ObjectMetadata, io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opens++
	object, ok := m.objects[ref.ObjectID]
	if !ok || object.session != session {
		return sessionwire.ObjectMetadata{}, nil, errors.New("memory objects: no such object in this session")
	}
	sum := sha256.Sum256(object.data)
	data := object.data
	if m.corrupt {
		data = bytes.Repeat([]byte("X"), len(data))
	}
	return sessionwire.ObjectMetadata{Reference: ref, SizeBytes: uint64(len(object.data)), Digest: "sha256:" + hex.EncodeToString(sum[:])},
		io.NopCloser(bytes.NewReader(data)), nil
}

// retainedCapture publishes body through the catalog's own publisher and
// returns the committed StepDone a loop would have recorded for it.
func retainedCapture(t *testing.T, catalog *toolResultCatalog, loopID uuid.UUID, body []byte, encoding event.ToolResultEncoding) event.StepDone {
	t.Helper()
	metadata, err := catalog.publisher().PublishToolResultObject(context.Background(), bytes.NewReader(body), uint64(len(body)), sha256.Sum256(body))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	size := uint64(len(body))
	reference := metadata.Reference
	return event.StepDone{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: catalog.session, LoopID: loopID}},
		Captures: []event.ToolResultCapture{{
			ToolExecutionID: captureTestUUID(t), ToolUseID: "tu", Reference: &reference,
			CapturedBytes: size, OriginalBytes: &size, Encoding: encoding,
		}},
	}
}

func captureTestUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return id
}

func readErrorKind(err error) tool.ToolResultReadErrorKind {
	var readErr *tool.ToolResultReadError
	if errors.As(err, &readErr) {
		return readErr.Kind
	}
	return ""
}

// pageAll reads a capture page by page until the reader reports the end, and
// returns the reassembled bytes.
func pageAll(t *testing.T, reader tool.ToolResultReader, captureID uuid.UUID, maxBytes uint64) []byte {
	t.Helper()
	var assembled []byte
	offset := uint64(0)
	for pages := 0; ; pages++ {
		if pages > 100000 {
			t.Fatal("paging did not terminate")
		}
		page, err := reader.ReadToolResult(context.Background(), tool.ToolResultPageRequest{CaptureID: captureID.String(), Offset: offset, MaxBytes: maxBytes})
		if err != nil {
			t.Fatalf("ReadToolResult(offset %d): %v", offset, err)
		}
		if page.Offset != offset {
			t.Fatalf("page offset = %d, want %d", page.Offset, offset)
		}
		assembled = append(assembled, page.Data...)
		next, more := page.NextOffset()
		if !more {
			return assembled
		}
		offset = next
	}
}

func TestToolResultCatalogIsAbsentWithoutAReadableStore(t *testing.T) {
	t.Parallel()
	var catalog *toolResultCatalog = newToolResultCatalog(nil, captureTestUUID(t))
	if catalog != nil {
		t.Fatal("a catalog was built with no store")
	}
	// Both accessors must return NIL INTERFACES: a typed nil would read as
	// configured to the loop and to tool binding validation.
	if catalog.publisher() != nil {
		t.Fatal("an unwired catalog returned a non-nil publisher interface")
	}
	if catalog.readerFor(captureTestUUID(t)) != nil {
		t.Fatal("an unwired catalog returned a non-nil reader interface")
	}
	catalog.observe(event.StepDone{})
	catalog.fold([]event.Event{event.StepDone{}})
}

// TestToolResultReaderReassemblesTheFullCapture is the read half of the
// capture → store-issued ref → reopen round trip at the session layer.
func TestToolResultReaderReassemblesTheFullCapture(t *testing.T) {
	t.Parallel()
	catalog := newToolResultCatalog(newMemoryToolResultObjects(), captureTestUUID(t))
	loopID := captureTestUUID(t)
	body := bytes.Repeat([]byte("0123456789abcdef"), 20000) // 320000 bytes: several full pages
	done := retainedCapture(t, catalog, loopID, body, event.ToolResultEncodingUTF8)
	catalog.observe(done)
	reader := catalog.readerFor(loopID)
	for _, maxBytes := range []uint64{0, 4093, tool.MaxToolResultPageBytes + 1} {
		if got := pageAll(t, reader, done.Captures[0].ToolExecutionID, maxBytes); !bytes.Equal(got, body) {
			t.Fatalf("max_bytes=%d: reassembled %d bytes, want %d", maxBytes, len(got), len(body))
		}
	}
	page, err := reader.ReadToolResult(context.Background(), tool.ToolResultPageRequest{CaptureID: done.Captures[0].ToolExecutionID.String()})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(page.Data) != tool.MaxToolResultPageBytes {
		t.Fatalf("default page = %d bytes, want the %d-byte ceiling", len(page.Data), tool.MaxToolResultPageBytes)
	}
}

// TestToolResultReaderPagesOnRuneBoundaries pins that a UTF-8 page never
// splits a character, whatever window size the model asks for.
func TestToolResultReaderPagesOnRuneBoundaries(t *testing.T) {
	t.Parallel()
	catalog := newToolResultCatalog(newMemoryToolResultObjects(), captureTestUUID(t))
	loopID := captureTestUUID(t)
	body := []byte(strings.Repeat("aé€𝄞", 500))
	done := retainedCapture(t, catalog, loopID, body, event.ToolResultEncodingUTF8)
	catalog.observe(done)
	reader := catalog.readerFor(loopID)
	id := done.Captures[0].ToolExecutionID
	for _, maxBytes := range []uint64{1, 2, 3, 5, 7, 64} {
		offset := uint64(0)
		var assembled []byte
		for {
			page, err := reader.ReadToolResult(context.Background(), tool.ToolResultPageRequest{CaptureID: id.String(), Offset: offset, MaxBytes: maxBytes})
			if err != nil {
				t.Fatalf("max_bytes=%d offset=%d: %v", maxBytes, offset, err)
			}
			if !utf8.Valid(page.Data) || len(page.Data) == 0 {
				t.Fatalf("max_bytes=%d offset=%d: page %q is empty or splits a rune", maxBytes, offset, page.Data)
			}
			assembled = append(assembled, page.Data...)
			next, more := page.NextOffset()
			if !more {
				break
			}
			offset = next
		}
		if !bytes.Equal(assembled, body) {
			t.Fatalf("max_bytes=%d: reassembly differs", maxBytes)
		}
	}
	// An offset the model made up in the middle of a rune starts at the next
	// rune rather than returning a fragment.
	page, err := reader.ReadToolResult(context.Background(), tool.ToolResultPageRequest{CaptureID: id.String(), Offset: 2, MaxBytes: 8})
	if err != nil {
		t.Fatalf("mid-rune offset: %v", err)
	}
	if !utf8.Valid(page.Data) || page.Offset != 3 {
		t.Fatalf("mid-rune offset page = (offset %d, %q), want a valid page starting at 3", page.Offset, page.Data)
	}
}

// TestToolResultReaderServesBinaryAsBase64 is D6.
func TestToolResultReaderServesBinaryAsBase64(t *testing.T) {
	t.Parallel()
	catalog := newToolResultCatalog(newMemoryToolResultObjects(), captureTestUUID(t))
	loopID := captureTestUUID(t)
	body := make([]byte, 200000)
	for i := range body {
		body[i] = byte(i*7 + 0x80)
	}
	done := retainedCapture(t, catalog, loopID, body, event.ToolResultEncodingBinary)
	catalog.observe(done)
	reader := catalog.readerFor(loopID)
	id := done.Captures[0].ToolExecutionID
	page, err := reader.ReadToolResult(context.Background(), tool.ToolResultPageRequest{CaptureID: id.String()})
	if err != nil {
		t.Fatalf("ReadToolResult: %v", err)
	}
	rendered := page.Render()
	if !strings.Contains(rendered, "encoding=base64") {
		t.Fatalf("binary page footer does not name its encoding: %q", rendered[len(rendered)-200:])
	}
	encoded := rendered[:strings.Index(rendered, "\n[capture ")]
	if len(encoded) > tool.MaxToolResultPageBytes {
		t.Fatalf("base64 page is %d bytes, above the %d-byte page ceiling", len(encoded), tool.MaxToolResultPageBytes)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !bytes.Equal(decoded, body[:len(decoded)]) {
		t.Fatalf("base64 page does not decode to the capture's first bytes: %v", err)
	}
	if got := pageAll(t, reader, id, 0); !bytes.Equal(got, body) {
		t.Fatal("binary paging does not reassemble the capture")
	}
}

// TestToolResultReaderIsScopedToTheCallingLoop is D4, and the cross-session
// half of isolation: another loop's capture, or an id from another session, is
// indistinguishable from an id that names nothing.
func TestToolResultReaderIsScopedToTheCallingLoop(t *testing.T) {
	t.Parallel()
	objects := newMemoryToolResultObjects()
	catalog := newToolResultCatalog(objects, captureTestUUID(t))
	parent, child := captureTestUUID(t), captureTestUUID(t)
	parentDone := retainedCapture(t, catalog, parent, []byte(strings.Repeat("p", 1000)), event.ToolResultEncodingUTF8)
	childDone := retainedCapture(t, catalog, child, []byte(strings.Repeat("c", 1000)), event.ToolResultEncodingUTF8)
	catalog.observe(parentDone)
	catalog.observe(childDone)

	otherSession := newToolResultCatalog(objects, captureTestUUID(t))
	foreignDone := retainedCapture(t, otherSession, parent, []byte(strings.Repeat("f", 1000)), event.ToolResultEncodingUTF8)
	otherSession.observe(foreignDone)

	tests := []struct {
		name   string
		reader tool.ToolResultReader
		id     string
		want   tool.ToolResultReadErrorKind
	}{
		{name: "own capture", reader: catalog.readerFor(parent), id: parentDone.Captures[0].ToolExecutionID.String()},
		{name: "child's capture from the parent", reader: catalog.readerFor(parent), id: childDone.Captures[0].ToolExecutionID.String(), want: tool.ToolResultReadUnknownCapture},
		{name: "parent's capture from the child", reader: catalog.readerFor(child), id: parentDone.Captures[0].ToolExecutionID.String(), want: tool.ToolResultReadUnknownCapture},
		{name: "another session's capture", reader: catalog.readerFor(parent), id: foreignDone.Captures[0].ToolExecutionID.String(), want: tool.ToolResultReadUnknownCapture},
		{name: "unknown id", reader: catalog.readerFor(parent), id: captureTestUUID(t).String(), want: tool.ToolResultReadUnknownCapture},
		{name: "malformed id", reader: catalog.readerFor(parent), id: "../../etc/passwd", want: tool.ToolResultReadUnknownCapture},
		{name: "zero id", reader: catalog.readerFor(parent), id: uuid.UUID{}.String(), want: tool.ToolResultReadUnknownCapture},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.reader.ReadToolResult(context.Background(), tool.ToolResultPageRequest{CaptureID: tt.id})
			if got := readErrorKind(err); got != tt.want {
				t.Fatalf("error = %v (kind %q), want kind %q", err, got, tt.want)
			}
		})
	}
}

func TestToolResultReaderRefusals(t *testing.T) {
	t.Parallel()
	objects := newMemoryToolResultObjects()
	catalog := newToolResultCatalog(objects, captureTestUUID(t))
	loopID := captureTestUUID(t)
	done := retainedCapture(t, catalog, loopID, []byte("0123456789"), event.ToolResultEncodingUTF8)
	inline := captureTestUUID(t)
	size := uint64(5)
	done.Captures = append(done.Captures, event.ToolResultCapture{ToolExecutionID: inline, ToolUseID: "tu2", CapturedBytes: size, OriginalBytes: &size, Encoding: event.ToolResultEncodingUTF8})
	catalog.observe(done)
	reader := catalog.readerFor(loopID)
	retained := done.Captures[0].ToolExecutionID.String()

	if _, err := reader.ReadToolResult(context.Background(), tool.ToolResultPageRequest{CaptureID: inline.String()}); readErrorKind(err) != tool.ToolResultReadNotRetained {
		t.Fatalf("inline capture = %v, want not_retained", err)
	}
	for _, offset := range []uint64{10, 11, 1 << 62} {
		if _, err := reader.ReadToolResult(context.Background(), tool.ToolResultPageRequest{CaptureID: retained, Offset: offset}); readErrorKind(err) != tool.ToolResultReadOffsetOutOfRange {
			t.Fatalf("offset %d = %v, want offset_out_of_range", offset, err)
		}
	}
	page, err := reader.ReadToolResult(context.Background(), tool.ToolResultPageRequest{CaptureID: retained, Offset: 9})
	if err != nil || string(page.Data) != "9" {
		t.Fatalf("last byte page = (%q, %v), want \"9\"", page.Data, err)
	}
	objects.mu.Lock()
	objects.corrupt = true
	objects.mu.Unlock()
	if _, err := reader.ReadToolResult(context.Background(), tool.ToolResultPageRequest{CaptureID: retained}); readErrorKind(err) != tool.ToolResultReadIntegrity {
		t.Fatalf("corrupted object = %v, want integrity", err)
	}
}

// TestToolResultCatalogFoldRebuildsTheIndex is the restore half: a catalog
// built fresh and folded from the replayed events answers exactly as the live
// one did.
func TestToolResultCatalogFoldRebuildsTheIndex(t *testing.T) {
	t.Parallel()
	objects := newMemoryToolResultObjects()
	session := captureTestUUID(t)
	live := newToolResultCatalog(objects, session)
	loopID := captureTestUUID(t)
	body := []byte(strings.Repeat("restored ", 3000))
	done := retainedCapture(t, live, loopID, body, event.ToolResultEncodingUTF8)

	restored := newToolResultCatalog(objects, session)
	if _, err := restored.readerFor(loopID).ReadToolResult(context.Background(), tool.ToolResultPageRequest{CaptureID: done.Captures[0].ToolExecutionID.String()}); readErrorKind(err) != tool.ToolResultReadUnknownCapture {
		t.Fatalf("an unfolded catalog answered: %v", err)
	}
	restored.fold([]event.Event{event.TurnStarted{}, done, event.TurnDone{}})
	if got := pageAll(t, restored.readerFor(loopID), done.Captures[0].ToolExecutionID, 0); !bytes.Equal(got, body) {
		t.Fatal("a folded catalog does not serve the capture")
	}
}

type limitsBound struct {
	loop.BoundDefinition
	base  loop.ToolLimits
	modes []loop.BoundMode
}

func (b limitsBound) ToolLimits() loop.ToolLimits { return b.base }
func (b limitsBound) Modes() []loop.BoundMode     { return b.modes }

// TestFitToolResultReaderKeepsAPageUnderTheModelBudget pins that a page never
// exceeds the smallest preview budget any mode declares.
func TestFitToolResultReaderKeepsAPageUnderTheModelBudget(t *testing.T) {
	t.Parallel()
	catalog := newToolResultCatalog(newMemoryToolResultObjects(), captureTestUUID(t))
	tests := []struct {
		name  string
		bound limitsBound
		want  int64
	}{
		{name: "unbounded keeps the ceiling", bound: limitsBound{}, want: tool.MaxToolResultPageBytes},
		{name: "base budget", bound: limitsBound{base: loop.ToolLimits{ResultBytes: 4096}}, want: 4096 - toolResultPageFooterReserve},
		{name: "smallest mode wins", bound: limitsBound{base: loop.ToolLimits{ResultBytes: 1 << 20}, modes: []loop.BoundMode{{ToolLimits: loop.ToolLimits{ResultBytes: 2048}}}}, want: 2048 - toolResultPageFooterReserve},
		{name: "tiny budget halves", bound: limitsBound{base: loop.ToolLimits{ResultBytes: 256}}, want: 128},
		{name: "large budget keeps the ceiling", bound: limitsBound{base: loop.ToolLimits{ResultBytes: 1 << 20}}, want: tool.MaxToolResultPageBytes},
	}
	for _, tt := range tests {
		reader := catalog.readerFor(captureTestUUID(t))
		fitToolResultReader(reader, tt.bound)
		if got := reader.(*loopToolResultReader).pageLimit.Load(); got != tt.want {
			t.Errorf("%s: page limit = %d, want %d", tt.name, got, tt.want)
		}
	}
	fitToolResultReader(nil, limitsBound{})
}

// TestEveryLoopBindingPassesTheToolResultReader holds the reader to every site
// that binds a loop's tools. The file set is derived by walking the package, so
// a binding site added later is covered without editing this test.
func TestEveryLoopBindingPassesTheToolResultReader(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	fset := token.NewFileSet()
	sites := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			literal, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			selector, ok := literal.Type.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Bindings" {
				return true
			}
			if pkg, ok := selector.X.(*ast.Ident); !ok || pkg.Name != "tool" {
				return true
			}
			sites++
			for _, element := range literal.Elts {
				if kv, ok := element.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "ToolResults" {
						return true
					}
				}
			}
			t.Errorf("%v: tool.Bindings is built without ToolResults, so read_tool_result cannot bind for loops from this site", fset.Position(literal.Pos()))
			return true
		})
	}
	if sites == 0 {
		t.Fatal("no tool.Bindings construction found; the guard is vacuous")
	}
}
