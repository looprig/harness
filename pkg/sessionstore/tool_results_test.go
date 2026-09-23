package sessionstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	durablestore "github.com/looprig/sessionstore"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

func publishToolResult(t *testing.T, objects loop.ToolResultObjects, session uuid.UUID, body []byte) coresessionwire.ObjectMetadata {
	t.Helper()
	metadata, err := objects.PublishToolResultObject(context.Background(), session, bytes.NewReader(body), uint64(len(body)), sha256.Sum256(body))
	if err != nil {
		t.Fatalf("PublishToolResultObject: %v", err)
	}
	return metadata
}

func readToolResult(objects loop.ToolResultObjects, session uuid.UUID, ref coresessionwire.ObjectReference) ([]byte, error) {
	_, stream, err := objects.OpenToolResultObject(context.Background(), session, ref)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(stream)
	closeErr := stream.Close()
	return data, errors.Join(readErr, closeErr)
}

// TestToolResultObjectsIssueResolvableReferences is I2.2 defect 1 at the store:
// the reference the store issues is SessionStore grammar, and a Store reopened
// over the same backend — a successor process — reads the full bytes back.
func TestToolResultObjectsIssueResolvableReferences(t *testing.T) {
	t.Parallel()
	backend := memstore.New()
	first, err := Open(backend, WithTenant("tenant-a"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	session := newTestUUID(t)
	body := bytes.Repeat([]byte("retained tool output\n"), 4096)
	metadata := publishToolResult(t, first.ToolResultObjects(), session, body)
	if !strings.HasPrefix(metadata.Reference.ObjectID, "v1:tool-result:") {
		t.Fatalf("reference %q is not a store-issued tool-result reference", metadata.Reference.ObjectID)
	}
	if metadata.SizeBytes != uint64(len(body)) {
		t.Fatalf("SizeBytes = %d, want %d", metadata.SizeBytes, len(body))
	}

	successor, err := Open(backend, WithTenant("tenant-a"))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := readToolResult(successor.ToolResultObjects(), session, metadata.Reference)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("read back %d bytes, want the %d published", len(got), len(body))
	}
}

// TestToolResultObjectsRefuseAnotherScope pins isolation: a reference resolves
// only under the session it was published for, and a store for another tenant
// cannot resolve it at all.
func TestToolResultObjectsRefuseAnotherScope(t *testing.T) {
	t.Parallel()
	backend := memstore.New()
	store, err := Open(backend, WithTenant("tenant-a"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	session, other := newTestUUID(t), newTestUUID(t)
	metadata := publishToolResult(t, store.ToolResultObjects(), session, []byte("secret output"))

	if _, err := readToolResult(store.ToolResultObjects(), other, metadata.Reference); err == nil {
		t.Fatal("another session resolved this session's tool result")
	}
	if _, err := Open(backend, WithTenant("tenant-b")); err == nil {
		t.Fatal("a second tenant opened the first tenant's legacy single-tenant backend")
	}
	foreign, err := Open(memstore.New(), WithTenant("tenant-b"))
	if err != nil {
		t.Fatalf("Open foreign: %v", err)
	}
	if _, err := readToolResult(foreign.ToolResultObjects(), session, metadata.Reference); err == nil {
		t.Fatal("another tenant's store resolved the reference")
	}
}

// TestToolResultObjectsClassifyCorruptionAsIntegrity pins that a reader can tell
// substituted bytes from an unavailable store.
func TestToolResultObjectsClassifyCorruptionAsIntegrity(t *testing.T) {
	t.Parallel()
	backend := memstore.New()
	store, err := Open(backend)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	session := newTestUUID(t)
	body := []byte("the original bytes")
	metadata := publishToolResult(t, store.ToolResultObjects(), session, body)
	keys, err := backend.Blobs.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	corrupted := 0
	for _, key := range keys {
		if !strings.Contains(key, "tool-result") {
			continue
		}
		if err := backend.Blobs.Delete(context.Background(), key); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if err := backend.Blobs.Put(context.Background(), key, bytes.NewReader([]byte("the SUBSTITUTED!!!"))); err != nil {
			t.Fatalf("Put: %v", err)
		}
		corrupted++
	}
	if corrupted != 1 {
		t.Fatalf("corrupted %d blobs, want exactly the one tool result", corrupted)
	}
	_, err = readToolResult(store.ToolResultObjects(), session, metadata.Reference)
	if !errors.Is(err, loop.ErrToolResultObjectIntegrity) {
		t.Fatalf("read of substituted bytes = %v, want loop.ErrToolResultObjectIntegrity", err)
	}
}

func TestClassifyToolResultObjectErr(t *testing.T) {
	t.Parallel()
	for _, code := range []durablestore.ObjectErrorCode{durablestore.ObjectErrorSize, durablestore.ObjectErrorDigest, durablestore.ObjectErrorIntegrity} {
		err := classifyToolResultObjectErr(&durablestore.ObjectError{Code: code})
		var objectErr *durablestore.ObjectError
		if !errors.Is(err, loop.ErrToolResultObjectIntegrity) || !errors.As(err, &objectErr) {
			t.Errorf("code %q: %v does not classify as integrity while keeping its cause", code, err)
		}
	}
	for _, err := range []error{&durablestore.ObjectError{Code: durablestore.ObjectErrorBackend}, &storage.BlobNotFoundError{Key: "k"}} {
		if errors.Is(classifyToolResultObjectErr(err), loop.ErrToolResultObjectIntegrity) {
			t.Errorf("%v classified as integrity", err)
		}
	}
	if classifyToolResultObjectErr(nil) != nil {
		t.Error("nil classified as an error")
	}
}

func stepDoneWithCapture(t *testing.T, session uuid.UUID, ref *coresessionwire.ObjectReference) event.StepDone {
	t.Helper()
	size := uint64(4096)
	return event.StepDone{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: session, LoopID: newTestUUID(t), TurnID: newTestUUID(t), StepID: newTestUUID(t)},
			EventID:     newTestUUID(t),
			CreatedAt:   time.Unix(1, 0).UTC(),
		},
		Messages: content.AgenticMessages{
			&content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: "calling"}}}},
			&content.ToolResultMessage{Message: content.Message{Role: content.RoleTool, Blocks: []content.Block{&content.TextBlock{Text: "preview"}}}, ToolUseID: "tu-1"},
		},
		Captures: []event.ToolResultCapture{{
			ToolExecutionID: newTestUUID(t), ToolUseID: "tu-1", Reference: ref,
			CapturedBytes: size, OriginalBytes: &size, Encoding: event.ToolResultEncodingUTF8,
		}},
	}
}

func appendEventsT(t *testing.T, st *Store, session uuid.UUID, events ...event.Event) {
	t.Helper()
	lease, _ := leaseFor(1, session)
	j, err := st.OpenJournal(context.Background(), session, lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	for _, ev := range events {
		if _, err := j.Append(context.Background(), journal.NewEventRecord(ev)); err != nil {
			t.Fatalf("Append(%T): %v", ev, err)
		}
	}
}

func fillerEvent(t *testing.T, session uuid.UUID) event.Event {
	t.Helper()
	return event.WorkspaceCheckpointed{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: session}, EventID: newTestUUID(t)},
		Ref:    "v1:sha256:" + strings.Repeat("e", 64), Consistency: event.SnapshotQuiescent, Trigger: event.SnapshotTriggerManual,
	}
}

// TestLookupToolResultCaptureRequiresCommittedEvidence is D1: an object is
// evidenced only by a committed StepDone naming it, never by the metadata row
// an unreferenced orphan also has.
func TestLookupToolResultCaptureRequiresCommittedEvidence(t *testing.T) {
	t.Parallel()
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	session := newTestUUID(t)
	referenced := publishToolResult(t, store.ToolResultObjects(), session, []byte("referenced output"))
	orphan := publishToolResult(t, store.ToolResultObjects(), session, []byte("orphaned output"))
	done := stepDoneWithCapture(t, session, &referenced.Reference)
	// The evidence sits below more than one scan window, so the backward scan
	// has to step past the newest window to find it.
	events := []event.Event{done}
	for range toolResultCaptureScanWindow + 3 {
		events = append(events, fillerEvent(t, session))
	}
	appendEventsT(t, store, session, events...)

	capture, found, err := store.LookupToolResultCapture(context.Background(), session, referenced.Reference)
	if err != nil || !found {
		t.Fatalf("lookup of a referenced object = (%v, %v), want found", found, err)
	}
	if capture.ToolExecutionID != done.Captures[0].ToolExecutionID {
		t.Fatalf("lookup returned capture %v, want %v", capture.ToolExecutionID, done.Captures[0].ToolExecutionID)
	}
	if _, found, err := store.LookupToolResultCapture(context.Background(), session, orphan.Reference); err != nil || found {
		t.Fatalf("lookup of an unreferenced orphan = (%v, %v), want not found", found, err)
	}
	if _, found, err := store.LookupToolResultCapture(context.Background(), newTestUUID(t), referenced.Reference); err != nil || found {
		t.Fatalf("lookup under another session = (%v, %v), want not found", found, err)
	}
	// A cold Store — a restarted Factory — finds the same evidence.
	cold := &Store{backend: store.backend, durable: store.durable, opts: store.opts}
	if _, found, err := cold.LookupToolResultCapture(context.Background(), session, referenced.Reference); err != nil || !found {
		t.Fatalf("cold lookup = (%v, %v), want found", found, err)
	}
}

// TestLookupToolResultCaptureCachesOnlyFoundEvidence pins that the positive
// cache answers without a scan: after a hit, the ledger is not read again.
func TestLookupToolResultCaptureCachesOnlyFoundEvidence(t *testing.T) {
	t.Parallel()
	backend := memstore.New()
	counting := &tipCountingLedger{Ledger: backend.Ledger}
	composite := *backend
	composite.Ledger = counting
	store, err := Open(&composite)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	session := newTestUUID(t)
	metadata := publishToolResult(t, store.ToolResultObjects(), session, []byte("output"))
	appendEventsT(t, store, session, stepDoneWithCapture(t, session, &metadata.Reference))
	counting.tips = 0
	for i := range 3 {
		if _, found, err := store.LookupToolResultCapture(context.Background(), session, metadata.Reference); err != nil || !found {
			t.Fatalf("lookup %d = (%v, %v)", i, found, err)
		}
	}
	if got := counting.tips; got != 1 {
		t.Fatalf("ledger tips read = %d, want 1: a cached hit must not rescan", got)
	}
}

// TestLookupToolResultCaptureBudgetIsNotAMiss pins that an exhausted scan budget
// is a typed error, never "not found": the evidence may lie below the window.
func TestLookupToolResultCaptureBudgetIsNotAMiss(t *testing.T) {
	t.Parallel()
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	session := newTestUUID(t)
	// A tip far above the budget is simulated by a ledger that reports it.
	store.backend.Ledger = &fixedTipLedger{Ledger: store.backend.Ledger, tip: ToolResultCaptureScanBudget + 10*toolResultCaptureScanWindow}
	ref := coresessionwire.ObjectReference{ObjectID: "v1:tool-result:absent"}
	_, found, err := store.LookupToolResultCapture(context.Background(), session, ref)
	var budget *ToolResultCaptureScanBudgetError
	if found || !errors.As(err, &budget) {
		t.Fatalf("lookup = (%v, %v), want *ToolResultCaptureScanBudgetError", found, err)
	}
}

type tipCountingLedger struct {
	storage.Ledger
	tips int
}

func (l *tipCountingLedger) Tip(ctx context.Context, name string) (uint64, error) {
	l.tips++
	return l.Ledger.Tip(ctx, name)
}

type fixedTipLedger struct {
	storage.Ledger
	tip uint64
}

func (l *fixedTipLedger) Tip(context.Context, string) (uint64, error) { return l.tip, nil }

// TestLegacyCaptureReferencesStillReplay is the compatibility half of the
// release: a journal written by harness <= v0.39 carries harness-minted
// "v1:sha256:<hex>" references. The StepDone codec is unchanged, so such a
// journal still replays and its capture is still found as evidence; the
// reference simply does not resolve, and a read fails rather than panicking or
// serving anything.
func TestLegacyCaptureReferencesStillReplay(t *testing.T) {
	t.Parallel()
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	session := newTestUUID(t)
	legacy := coresessionwire.ObjectReference{ObjectID: "v1:sha256:" + strings.Repeat("ab", 32)}
	appendEventsT(t, store, session, stepDoneWithCapture(t, session, &legacy))
	capture, found, err := store.LookupToolResultCapture(context.Background(), session, legacy)
	if err != nil || !found || capture.Reference.ObjectID != legacy.ObjectID {
		t.Fatalf("legacy capture lookup = (%+v, %v, %v), want found", capture, found, err)
	}
	if _, err := readToolResult(store.ToolResultObjects(), session, legacy); err == nil {
		t.Fatal("a harness-minted legacy reference resolved")
	}
}
