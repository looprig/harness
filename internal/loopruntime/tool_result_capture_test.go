package loopruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	identitydomain "github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
)

// fakeObjectStore is an in-memory ToolResultObjectStore. It is deliberately
// STRICTER than the interface requires on the one property the pipeline relies
// on — an identity is immutable — so a production change that reused an identity
// for different bytes fails here rather than passing silently.
type fakeObjectStore struct {
	mu      sync.Mutex
	objects map[string][]byte

	// putErr, when non-nil, fails every Put.
	putErr error
	// statErr, when non-nil, fails every Stat.
	statErr error
	// statSize/statDigest, when non-nil, override what Stat reports, so a store
	// that accepted the write but stored something else can be exercised
	// without a second failure mode on the Put path.
	statSize   *uint64
	statDigest *string

	// storeDespitePutErr models the ambiguous upload: the Put reports an error
	// but the object is stored anyway, which is what a timed-out write that
	// actually landed looks like to the caller.
	storeDespitePutErr bool

	// failFor, when non-nil, restricts all four arrangements above to the
	// objects whose stored bytes it selects. Without it a failure is global,
	// which makes every other result in the step fail too; with it a step can
	// hold a result that is retained SUCCESSFULLY alongside one that is not.
	failFor func(content []byte) bool

	puts []string
}

// selected reports whether the arranged failure applies to these stored bytes.
func (f *fakeObjectStore) selected(content []byte) bool {
	return f.failFor == nil || f.failFor(content)
}

func newFakeObjectStore() *fakeObjectStore {
	return &fakeObjectStore{objects: map[string][]byte{}}
}

var errFakeObjectIdentityReused = errors.New("fake object store: identity reused with different bytes")

func (f *fakeObjectStore) PutToolResultObject(_ context.Context, objectID string, content []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, objectID)
	if f.putErr != nil && f.selected(content) {
		if f.storeDespitePutErr {
			f.objects[objectID] = append([]byte(nil), content...)
		}
		return f.putErr
	}
	stored := append([]byte(nil), content...)
	if existing, ok := f.objects[objectID]; ok && string(existing) != string(stored) {
		return errFakeObjectIdentityReused
	}
	f.objects[objectID] = stored
	return nil
}

func (f *fakeObjectStore) StatToolResultObject(_ context.Context, objectID string) (ToolResultObjectStat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored, ok := f.objects[objectID]
	if !ok {
		// A Stat before any Put is a defect in the pipeline whatever else is
		// arranged, so it is reported ahead of the arranged failure.
		if f.statErr != nil && f.failFor == nil {
			return ToolResultObjectStat{}, f.statErr
		}
		return ToolResultObjectStat{}, errors.New("fake object store: no such object")
	}
	if !f.selected(stored) {
		sum := sha256.Sum256(stored)
		return ToolResultObjectStat{SizeBytes: uint64(len(stored)), Digest: hex.EncodeToString(sum[:])}, nil
	}
	if f.statErr != nil {
		return ToolResultObjectStat{}, f.statErr
	}
	sum := sha256.Sum256(stored)
	stat := ToolResultObjectStat{SizeBytes: uint64(len(stored)), Digest: hex.EncodeToString(sum[:])}
	if f.statSize != nil {
		stat.SizeBytes = *f.statSize
	}
	if f.statDigest != nil {
		stat.Digest = *f.statDigest
	}
	return stat, nil
}

func (f *fakeObjectStore) get(objectID string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored, ok := f.objects[objectID]
	return append([]byte(nil), stored...), ok
}

func (f *fakeObjectStore) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts)
}

// captureConfig builds the minimal turnConfig retainToolResults reads: the tool
// caps and the object store. Every bound is passed through resolveToolSetCaps so
// a test states the same declared values production would.
func captureConfig(store ToolResultObjectStore, modelBytes, captureCeiling, materializedMax int) turnConfig {
	return turnConfig{
		tools: resolveToolSetCaps(ToolSet{
			MaxToolResultBytes:             modelBytes,
			MaxToolResultCaptureBytes:      captureCeiling,
			MaxMaterializedToolResultBytes: materializedMax,
		}),
		toolResultObjects: store,
	}
}

func newTestResult(t *testing.T, toolUseID string, blocks ...content.Block) result {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return result{ToolExecutionID: id, ToolUseID: toolUseID, Content: blocks}
}

func textResult(t *testing.T, toolUseID, text string) result {
	t.Helper()
	return newTestResult(t, toolUseID, &content.TextBlock{Text: text})
}

// stepDoneFor assembles the StepDone a committed step would carry for these
// messages and captures, so a test can put the pipeline's output through the
// real codec validation rather than only through its own expectations.
func stepDoneFor(t *testing.T, commit toolResultCommit) event.StepDone {
	t.Helper()
	sessionID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	messages := content.AgenticMessages{&content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: "calling"}}}}}
	for _, message := range commit.messages {
		messages = append(messages, message)
	}
	return event.StepDone{
		Header: event.Header{
			Coordinates: identitydomain.Coordinates{SessionID: sessionID, LoopID: sessionID, TurnID: sessionID, StepID: sessionID},
			EventID:     sessionID,
			CreatedAt:   time.Unix(1, 0).UTC(),
		},
		Messages: messages,
		Captures: commit.captures,
	}
}

func requireValidStepDone(t *testing.T, commit toolResultCommit) {
	t.Helper()
	if err := event.ValidateEvent(stepDoneFor(t, commit)); err != nil {
		t.Fatalf("the pipeline produced a StepDone the event codec rejects: %v", err)
	}
}

func committedText(t *testing.T, message *content.ToolResultMessage) string {
	t.Helper()
	if len(message.Blocks) != 1 {
		t.Fatalf("committed tool result blocks = %d, want 1", len(message.Blocks))
	}
	block, ok := message.Blocks[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("committed tool result block = %T, want *content.TextBlock", message.Blocks[0])
	}
	return block.Text
}

// TestToolResultRetentionKeepsAnObjectExactlyWhenThePreviewIsIncomplete
// enumerates result sizes either side of the model budget instead of pinning one:
// the property is "an object exists iff the committed message does not already
// carry the complete bytes", and a fixture that only ever ran one size could be
// satisfied by "always" or by "never".
func TestToolResultRetentionKeepsAnObjectExactlyWhenThePreviewIsIncomplete(t *testing.T) {
	t.Parallel()
	const modelBytes = 512
	for _, size := range []int{0, 1, 100, modelBytes - 1, modelBytes, modelBytes + 1, 4096} {
		text := strings.Repeat("x", size)
		store := newFakeObjectStore()
		cfg := captureConfig(store, modelBytes, 1<<20, 1<<20)
		commit, err := retainToolResults(context.Background(), cfg, []result{textResult(t, "tu-1", text)})
		if err != nil {
			t.Fatalf("size=%d: retainToolResults: %v", size, err)
		}
		if commit.retention != nil {
			t.Fatalf("size=%d: unexpected retention failure %v", size, commit.retention)
		}
		requireValidStepDone(t, commit)
		if len(commit.captures) != 1 {
			t.Fatalf("size=%d: captures = %d, want 1", size, len(commit.captures))
		}
		capture := commit.captures[0]
		wantObject := size > modelBytes
		if (capture.Reference != nil) != wantObject {
			t.Errorf("size=%d: reference present = %v, want %v", size, capture.Reference != nil, wantObject)
		}
		if got := store.putCount(); got != boolToInt(wantObject) {
			t.Errorf("size=%d: object writes = %d, want %d", size, got, boolToInt(wantObject))
		}
		original, exact := capture.OriginalSize()
		if !exact || original != uint64(size) {
			t.Errorf("size=%d: OriginalSize() = (%d, %v), want (%d, true)", size, original, exact, size)
		}
		if capture.Truncated {
			t.Errorf("size=%d: capture reports truncation under a 1 MiB ceiling", size)
		}
		if capture.Encoding != event.ToolResultEncodingUTF8 {
			t.Errorf("size=%d: encoding = %q, want utf-8", size, capture.Encoding)
		}
		text2 := committedText(t, commit.messages[0])
		if len(text2) > modelBytes {
			t.Errorf("size=%d: committed model text = %d bytes, want <= %d", size, len(text2), modelBytes)
		}
		if wantObject {
			stored, ok := store.get(capture.Reference.ObjectID)
			if !ok || string(stored) != text {
				t.Errorf("size=%d: retained object = %q, want the complete result", size, stored)
			}
		}
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestToolResultRetentionTruncatesAtTheCaptureCeiling enumerates ceilings and
// sizes: what is pinned is that the retained object is the ceiling-length prefix
// and that the recorded original count is the producer's full length, for every
// pair, not for one chosen pair where the two numbers happen to agree.
func TestToolResultRetentionTruncatesAtTheCaptureCeiling(t *testing.T) {
	t.Parallel()
	for _, ceiling := range []int{256, 300, 512} {
		for _, size := range []int{255, 256, 257, 400, 1000} {
			text := strings.Repeat("y", size)
			store := newFakeObjectStore()
			// 128 is below every size enumerated here, so each case is past the
			// model budget and therefore reaches the object path; what varies
			// across the loop is only whether the CAPTURE ceiling also binds.
			cfg := captureConfig(store, 128, ceiling, 1<<20)
			commit, err := retainToolResults(context.Background(), cfg, []result{textResult(t, "tu-1", text)})
			if err != nil {
				t.Fatalf("ceiling=%d size=%d: %v", ceiling, size, err)
			}
			if commit.retention != nil {
				t.Fatalf("ceiling=%d size=%d: unexpected retention failure %v", ceiling, size, commit.retention)
			}
			requireValidStepDone(t, commit)
			capture := commit.captures[0]
			wantCaptured := min(size, ceiling)
			if capture.CapturedBytes != uint64(wantCaptured) {
				t.Errorf("ceiling=%d size=%d: CapturedBytes = %d, want %d", ceiling, size, capture.CapturedBytes, wantCaptured)
			}
			original, exact := capture.OriginalSize()
			if !exact || original != uint64(size) {
				t.Errorf("ceiling=%d size=%d: OriginalSize() = (%d, %v), want (%d, true)", ceiling, size, original, exact, size)
			}
			if got, want := capture.Truncated, size > ceiling; got != want {
				t.Errorf("ceiling=%d size=%d: Truncated = %v, want %v", ceiling, size, got, want)
			}
			if capture.Truncated && capture.TruncationReason != event.ToolResultTruncatedCaptureCeiling {
				t.Errorf("ceiling=%d size=%d: TruncationReason = %q, want capture_ceiling", ceiling, size, capture.TruncationReason)
			}
			if !capture.Truncated && capture.TruncationReason != "" {
				t.Errorf("ceiling=%d size=%d: TruncationReason = %q on a complete capture", ceiling, size, capture.TruncationReason)
			}
			if capture.Reference == nil {
				t.Fatalf("ceiling=%d size=%d: no object reference", ceiling, size)
			}
			stored, ok := store.get(capture.Reference.ObjectID)
			if !ok || string(stored) != text[:wantCaptured] {
				t.Errorf("ceiling=%d size=%d: retained object = %q, want the %d-byte prefix", ceiling, size, stored, wantCaptured)
			}
		}
	}
}

// TestToolResultRetentionIsBoundedByTheMaterializedMaximum pins that the declared
// materialized hard maximum is enforced and not merely declared, and that it
// binds independently of the definition's capture ceiling.
func TestToolResultRetentionIsBoundedByTheMaterializedMaximum(t *testing.T) {
	t.Parallel()
	const size = 2048
	text := strings.Repeat("z", size)
	for _, materialized := range []int{300, 700, 4096} {
		store := newFakeObjectStore()
		cfg := captureConfig(store, 256, 1024, materialized)
		commit, err := retainToolResults(context.Background(), cfg, []result{textResult(t, "tu-1", text)})
		if err != nil {
			t.Fatalf("materialized=%d: %v", materialized, err)
		}
		capture := commit.captures[0]
		want := uint64(min(1024, materialized))
		if capture.CapturedBytes != want {
			t.Errorf("materialized=%d: CapturedBytes = %d, want %d", materialized, capture.CapturedBytes, want)
		}
	}
}

// TestToolResultRetentionKeepsRawBytesTheModelNeverSees covers the two shapes
// where the committed preview is lossy for a reason other than length: bytes that
// are not valid UTF-8 (which shaping replaces with U+FFFD) and a non-text block
// (which flattening replaces with a placeholder). Both must reach the object
// intact, and both must therefore produce a reference even though they are small.
func TestToolResultRetentionKeepsRawBytesTheModelNeverSees(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		blocks []content.Block
		want   func(raw []byte) error
	}{
		{
			name:   "invalid utf-8 survives in the object",
			blocks: []content.Block{&content.TextBlock{Text: "head\xff\xfetail"}},
			want: func(raw []byte) error {
				if string(raw) != "head\xff\xfetail" {
					return fmt.Errorf("object = %q, want the original bytes", raw)
				}
				return nil
			},
		},
		{
			name:   "a non-text block is retained as JSON, not as a placeholder",
			blocks: []content.Block{&content.ImageBlock{MediaType: content.MediaTypeImagePNG, Source: content.ImageSource{URL: "https://example.test/a.png"}}},
			want: func(raw []byte) error {
				if strings.Contains(string(raw), "[unsupported ") {
					return fmt.Errorf("object = %q, want the structured payload rather than the model placeholder", raw)
				}
				if !json.Valid(raw) {
					return fmt.Errorf("object = %q, want valid JSON for a structured block", raw)
				}
				return nil
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newFakeObjectStore()
			cfg := captureConfig(store, 4096, 1<<20, 1<<20)
			commit, err := retainToolResults(context.Background(), cfg, []result{newTestResult(t, "tu-1", tt.blocks...)})
			if err != nil {
				t.Fatalf("retainToolResults: %v", err)
			}
			requireValidStepDone(t, commit)
			capture := commit.captures[0]
			if capture.Reference == nil {
				t.Fatal("no object reference: the committed preview is lossy, so the bytes are unreachable")
			}
			stored, ok := store.get(capture.Reference.ObjectID)
			if !ok {
				t.Fatalf("object %q missing from the store", capture.Reference.ObjectID)
			}
			if err := tt.want(stored); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestToolResultRetentionEncodingFollowsTheRetainedBytes pins that a capture whose
// object is not valid UTF-8 says so, which is the only signal a reader has that
// it must not decode the object as text.
func TestToolResultRetentionEncodingFollowsTheRetainedBytes(t *testing.T) {
	t.Parallel()
	store := newFakeObjectStore()
	cfg := captureConfig(store, 4096, 1<<20, 1<<20)
	commit, err := retainToolResults(context.Background(), cfg, []result{textResult(t, "tu-1", "head\xff\xfetail")})
	if err != nil {
		t.Fatalf("retainToolResults: %v", err)
	}
	if got := commit.captures[0].Encoding; got != event.ToolResultEncodingBinary {
		t.Fatalf("encoding = %q, want binary", got)
	}
}

// TestToolResultRetentionParallelResultsKeepTheirIdentity pins that a step holding
// several results records one capture per result, each carrying ITS OWN
// execution and provider ids and ITS OWN object. The three payloads are distinct
// so a capture paired with the wrong result fails on content, not only on ids.
func TestToolResultRetentionParallelResultsKeepTheirIdentity(t *testing.T) {
	t.Parallel()
	store := newFakeObjectStore()
	cfg := captureConfig(store, 256, 1<<20, 1<<20)
	results := []result{
		textResult(t, "tu-a", strings.Repeat("a", 700)),
		textResult(t, "tu-b", strings.Repeat("b", 800)),
		textResult(t, "tu-c", strings.Repeat("c", 900)),
	}
	commit, err := retainToolResults(context.Background(), cfg, results)
	if err != nil {
		t.Fatalf("retainToolResults: %v", err)
	}
	requireValidStepDone(t, commit)
	if len(commit.captures) != len(results) {
		t.Fatalf("captures = %d, want %d", len(commit.captures), len(results))
	}
	for i, capture := range commit.captures {
		if capture.ToolUseID != results[i].ToolUseID {
			t.Errorf("capture %d ToolUseID = %q, want %q", i, capture.ToolUseID, results[i].ToolUseID)
		}
		if capture.ToolExecutionID != results[i].ToolExecutionID {
			t.Errorf("capture %d ToolExecutionID = %v, want %v", i, capture.ToolExecutionID, results[i].ToolExecutionID)
		}
		if capture.Reference == nil {
			t.Fatalf("capture %d has no reference", i)
		}
		stored, ok := store.get(capture.Reference.ObjectID)
		if !ok {
			t.Fatalf("capture %d object %q missing", i, capture.Reference.ObjectID)
		}
		want := flattenToText(results[i].Content)
		if string(stored) != want {
			t.Errorf("capture %d object = %d bytes of %q, want the result for %s", i, len(stored), string(stored[:1]), results[i].ToolUseID)
		}
	}
}

// cancelingObjectStore cancels the turn from inside the object write, which is
// how a real Interrupt lands during retention: the store call returns because
// its context died, not because storage failed.
type cancelingObjectStore struct {
	cancel context.CancelFunc
	puts   int
}

func (c *cancelingObjectStore) PutToolResultObject(ctx context.Context, _ string, _ []byte) error {
	c.puts++
	c.cancel()
	return ctx.Err()
}

func (c *cancelingObjectStore) StatToolResultObject(context.Context, string) (ToolResultObjectStat, error) {
	return ToolResultObjectStat{}, errors.New("cancelingObjectStore: Stat must not be reached")
}

// TestToolResultRetentionCancellationDiscardsTheWholeStep pins that a cancelled
// retention is NOT reported as a retention failure: it returns the context error
// and no messages and no captures at all. The distinction matters because a
// retention failure commits a durable notice and ends the turn as failed, while
// a cancellation commits nothing.
func TestToolResultRetentionCancellationDiscardsTheWholeStep(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &cancelingObjectStore{cancel: cancel}
	cfg := captureConfig(store, 128, 1<<20, 1<<20)
	commit, err := retainToolResults(ctx, cfg, []result{textResult(t, "tu-1", strings.Repeat("q", 4096))})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if commit.messages != nil || commit.captures != nil || commit.retention != nil {
		t.Fatalf("cancelled retention returned %+v, want the zero commit", commit)
	}
	var retention *ToolResultRetentionError
	if errors.As(err, &retention) {
		t.Fatal("a cancellation was reported as a retention failure")
	}
}

// TestToolResultRetentionFailureStages walks the four failure points. Each
// case is arranged so exactly ONE check can fire: the size case leaves the digest
// agreeing, and the digest case leaves the size agreeing, so neither can be
// killed by the other's check.
//
// Every case runs with the failing result both FIRST and SECOND. Ordering is
// load-bearing for the all-or-nothing assertion: with the failure first, the
// capture list is empty when it happens, so "captures is empty" would hold even
// for an implementation that returned whatever it had accumulated.
//
// Every case also runs with the surviving result both BELOW and ABOVE the model
// budget, because only the second size makes the marker assertion mean anything.
// A survivor below the budget carries no marker in a successful step either, so
// "the survivor carries no marker" holds for an implementation that never
// rebuilt the messages at all; a survivor above it WOULD carry one, so the
// assertion then reads the rebuild. The large survivor is retained successfully
// alongside the failing one, which is what fakeObjectStore.failFor exists for.
func TestToolResultRetentionFailureStages(t *testing.T) {
	t.Parallel()
	wrongSize := uint64(1)
	wrongDigest := strings.Repeat("0", 64)
	putBoom := errors.New("put boom")
	statBoom := errors.New("stat boom")
	tests := []struct {
		name      string
		arrange   func(*fakeObjectStore)
		wantStage ToolResultRetentionStage
		wantCause error
	}{
		{"put fails", func(f *fakeObjectStore) { f.putErr = putBoom }, ToolResultRetentionStagePut, putBoom},
		{"stat fails", func(f *fakeObjectStore) { f.statErr = statBoom }, ToolResultRetentionStageStat, statBoom},
		{"stored size disagrees", func(f *fakeObjectStore) { f.statSize = &wrongSize }, ToolResultRetentionStageSize, nil},
		{"stored content disagrees", func(f *fakeObjectStore) { f.statDigest = &wrongDigest }, ToolResultRetentionStageDigest, nil},
	}
	const failingText = "wwwwwwww"
	for _, tt := range tests {
		for _, failingFirst := range []bool{true, false} {
			for _, largeSurvivor := range []bool{false, true} {
				tt, failingFirst, largeSurvivor := tt, failingFirst, largeSurvivor
				name := tt.name + "/failing result last"
				if failingFirst {
					name = tt.name + "/failing result first"
				}
				if largeSurvivor {
					name += "/survivor above the model budget"
				} else {
					name += "/survivor below the model budget"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					store := newFakeObjectStore()
					tt.arrange(store)
					// Only the failing result's bytes are failed, so a survivor above
					// the model budget is retained SUCCESSFULLY and reaches the
					// discarded-messages path carrying a marker.
					store.failFor = func(content []byte) bool { return bytes.Contains(content, []byte(failingText)) }
					cfg := captureConfig(store, 128, 1<<20, 1<<20)
					// "short" is below the model budget, so it needs no object and its
					// capture is recorded without touching the store — which is what
					// makes it a survivor that can only differ by ordering.
					failing := textResult(t, "tu-big", strings.Repeat(failingText, 512))
					survivorText := "short"
					if largeSurvivor {
						survivorText = strings.Repeat("s", 4096)
					}
					survivor := textResult(t, "tu-small", survivorText)
					results := []result{survivor, failing}
					failingIndex, survivorIndex := 1, 0
					if failingFirst {
						results = []result{failing, survivor}
						failingIndex, survivorIndex = 0, 1
					}
					commit, err := retainToolResults(context.Background(), cfg, results)
					if err != nil {
						t.Fatalf("retainToolResults returned a cancellation error %v", err)
					}
					if commit.retention == nil {
						t.Fatal("retention failure was not reported")
					}
					if commit.retention.Stage != tt.wantStage {
						t.Fatalf("stage = %q, want %q", commit.retention.Stage, tt.wantStage)
					}
					if commit.retention.ToolUseID != failing.ToolUseID || commit.retention.ToolExecutionID != failing.ToolExecutionID {
						t.Fatalf("retention names %v/%q, want the failing result %v/%q",
							commit.retention.ToolExecutionID, commit.retention.ToolUseID, failing.ToolExecutionID, failing.ToolUseID)
					}
					if tt.wantCause != nil && !errors.Is(commit.retention, tt.wantCause) {
						t.Fatalf("cause = %v, want %v", commit.retention.Cause, tt.wantCause)
					}
					if len(commit.captures) != 0 {
						t.Fatalf("captures = %d, want none: a partial list cannot be told from a truncated one", len(commit.captures))
					}
					requireValidStepDone(t, commit)
					if len(commit.messages) != 2 {
						t.Fatalf("messages = %d, want 2", len(commit.messages))
					}
					notice := committedText(t, commit.messages[failingIndex])
					if notice != toolResultRetentionNoticeText {
						t.Fatalf("failing result committed %q, want the retention notice", notice)
					}
					if !commit.messages[failingIndex].IsError {
						t.Fatal("the retention notice is not flagged as an error, so the model reads it as output")
					}
					// The surviving result keeps exactly the text an unretained result
					// would carry, and must NOT claim a retention that this step did
					// not record. The expectation is the plain shaped message, so a
					// large survivor's marker shows up as a difference here as well as
					// in the prefix check below.
					wantSurvivor := committedText(t, toolResultMessage(survivor, cfg.tools.MaxToolResultBytes))
					if got := committedText(t, commit.messages[survivorIndex]); got != wantSurvivor {
						t.Fatalf("surviving result committed %q, want %q", got, wantSurvivor)
					}
					if strings.Contains(committedText(t, commit.messages[survivorIndex]), toolResultRetainedMarkerPrefix) {
						t.Fatal("a surviving result carries a retention marker although no capture was recorded")
					}
				})
			}
		}
	}
}

// TestToolResultRetentionWithoutAStoreIsUnchanged pins the compatibility default:
// with no object store wired, every committed message is byte-identical to what
// toolResultMessage alone produces and nothing is recorded.
func TestToolResultRetentionWithoutAStoreIsUnchanged(t *testing.T) {
	t.Parallel()
	cfg := captureConfig(nil, 128, 1<<20, 1<<20)
	results := []result{textResult(t, "tu-1", strings.Repeat("m", 4096)), textResult(t, "tu-2", "short")}
	commit, err := retainToolResults(context.Background(), cfg, results)
	if err != nil {
		t.Fatalf("retainToolResults: %v", err)
	}
	if commit.captures != nil || commit.retention != nil {
		t.Fatalf("commit = %+v, want messages only", commit)
	}
	for i, r := range results {
		want := committedText(t, toolResultMessage(r, cfg.tools.MaxToolResultBytes))
		if got := committedText(t, commit.messages[i]); got != want {
			t.Errorf("message %d = %q, want %q", i, got, want)
		}
	}
}

// TestRunTurnToolResultRetentionFailureEndsTheTurnBeforeAnotherInference is the whole-turn
// half of the retention-failure contract. The unit test above proves what the
// pipeline returns; this proves the turn ACTS on it — the step commits with the
// model-visible notice, the terminal carries the typed cause, and no second
// inference is issued with a preview whose elided bytes no longer exist.
func TestRunTurnToolResultRetentionFailureEndsTheTurnBeforeAnotherInference(t *testing.T) {
	t.Parallel()
	const maxBytes = 128
	big := strings.Repeat("g", 4096)
	rawTool := &rawGraphTool{name: "RawGraph", raw: []content.Block{&content.TextBlock{Text: big}}}
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{toolUseChunk(0, "id-raw", "RawGraph", `{}`)},
		{textChunk("done")},
	}}
	ts := agenticToolSet([]tool.InvokableTool{rawTool}, 25, 100)
	ts.MaxToolResultBytes = maxBytes
	store := newFakeObjectStore()
	store.putErr = errors.New("object store unavailable")
	cfg, st, rec := newTurnFixture(nil, nil, ts, client, noGateReg())
	cfg.toolResultObjects = store

	terminal := runTurn(context.Background(), cfg, st)
	failed, ok := terminal.(event.TurnFailed)
	if !ok {
		t.Fatalf("terminal = %T, want TurnFailed", terminal)
	}
	var retention *ToolResultRetentionError
	if !errors.As(failed.Err, &retention) {
		t.Fatalf("terminal error = %v, want *ToolResultRetentionError", failed.Err)
	}
	if retention.Stage != ToolResultRetentionStagePut {
		t.Fatalf("stage = %q, want put", retention.Stage)
	}

	sds := stepDones(rec.events())
	if len(sds) != 1 {
		t.Fatalf("StepDone count = %d, want 1: the failing step commits and the turn stops", len(sds))
	}
	if len(sds[0].Captures) != 0 {
		t.Fatalf("Captures = %d, want none", len(sds[0].Captures))
	}
	trm, ok := sds[0].Messages[1].(*content.ToolResultMessage)
	if !ok {
		t.Fatalf("StepDone.Messages[1] = %T, want *content.ToolResultMessage", sds[0].Messages[1])
	}
	if got := committedText(t, trm); got != toolResultRetentionNoticeText {
		t.Fatalf("committed tool result = %q, want the retention notice", got)
	}
	if !trm.IsError {
		t.Fatal("the committed retention notice is not flagged as an error")
	}
	if got := len(client.requests()); got != 1 {
		t.Fatalf("inference requests = %d, want 1: the turn must not run another step", got)
	}
}

// TestToolResultCaptureResolvesFromTheCommittedEventAlone pins the property a Host
// or process restart depends on: everything needed to fetch the complete result
// is IN the committed StepDone. The turn is run inside a closure that returns
// only the committed events, so the loop's config, state and recorder are
// lexically out of scope for every assertion below — the fetch cannot
// accidentally reach for something a restarted process would not have.
func TestToolResultCaptureResolvesFromTheCommittedEventAlone(t *testing.T) {
	t.Parallel()
	const maxBytes = 128
	body := strings.Repeat("h", 4096)
	store := newFakeObjectStore()

	committed := func() []event.StepDone {
		rawTool := &rawGraphTool{name: "RawGraph", raw: []content.Block{&content.TextBlock{Text: body}}}
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-raw", "RawGraph", `{}`)},
			{textChunk("done")},
		}}
		ts := agenticToolSet([]tool.InvokableTool{rawTool}, 25, 100)
		ts.MaxToolResultBytes = maxBytes
		cfg, st, rec := newTurnFixture(nil, nil, ts, client, noGateReg())
		cfg.toolResultObjects = store
		if _, ok := runTurn(context.Background(), cfg, st).(event.TurnDone); !ok {
			t.Fatal("turn did not complete")
		}
		return stepDones(rec.events())
	}()

	if len(committed) != 2 || len(committed[0].Captures) != 1 {
		t.Fatalf("committed StepDones = %d with %d captures, want 2 and 1", len(committed), len(committed[0].Captures))
	}
	capture := committed[0].Captures[0]
	if capture.Reference == nil {
		t.Fatal("committed capture carries no reference")
	}
	stored, ok := store.get(capture.Reference.ObjectID)
	if !ok {
		t.Fatalf("object %q is not in the store", capture.Reference.ObjectID)
	}
	if string(stored) != body {
		t.Fatalf("retrieved %d bytes, want the complete %d-byte result", len(stored), len(body))
	}
	original, exact := capture.OriginalSize()
	if !exact || original != uint64(len(body)) {
		t.Fatalf("committed OriginalSize() = (%d, %v), want (%d, true)", original, exact, len(body))
	}
}

// TestToolResultRetentionTruncatedCaptureAlwaysKeepsAnObject covers the one shape
// where the "the preview already carries everything" shortcut would otherwise
// fire on a TRUNCATED capture: when the model budget is too small for shaping's
// own truncation notice, shapeToolResultText falls back to a raw prefix, and with
// a capture ceiling of the same size that prefix is byte-identical to the
// retained bytes. Recording no object there would commit a capture that claims
// truncation while naming nothing to retrieve, which the event codec rejects.
func TestToolResultRetentionTruncatedCaptureAlwaysKeepsAnObject(t *testing.T) {
	t.Parallel()
	const bound = 20
	text := strings.Repeat("p", 1000)
	store := newFakeObjectStore()
	cfg := captureConfig(store, bound, bound, 1<<20)
	commit, err := retainToolResults(context.Background(), cfg, []result{textResult(t, "tu-1", text)})
	if err != nil {
		t.Fatalf("retainToolResults: %v", err)
	}
	capture := commit.captures[0]
	if !capture.Truncated {
		t.Fatalf("fixture is vacuous: capture is not truncated (captured %d of %d)", capture.CapturedBytes, len(text))
	}
	// The shortcut compares the PLAIN preview (before any retention marker) with
	// the retained bytes, so that is what has to coincide for this case to bite.
	if got := shapeToolResultText(text, bound); got != text[:bound] {
		t.Fatalf("fixture is vacuous: plain preview %q is not the raw prefix this case needs", got)
	}
	if capture.Reference == nil {
		t.Fatal("a truncated capture recorded no object; the elided bytes are unreachable")
	}
	requireValidStepDone(t, commit)
}

// --- session-workspace streaming capture (H5.3) ---

// streamingObjectStore is a fakeObjectStore that ALSO implements the optional
// ToolResultObjectStreamStore. It records the concrete reader type it was handed
// so a test can tell a stream off the local spill from a stream over a
// materialized copy — the difference the "never buffers the capture in Host
// memory" requirement is actually about.
type streamingObjectStore struct {
	*fakeObjectStore

	mu          sync.Mutex
	streamed    int
	readerTypes []string
	declared    []uint64
	streamErr   error
}

func newStreamingObjectStore() *streamingObjectStore {
	return &streamingObjectStore{fakeObjectStore: newFakeObjectStore()}
}

func (s *streamingObjectStore) PutToolResultObjectStream(_ context.Context, objectID string, content io.Reader, size uint64) error {
	s.mu.Lock()
	s.streamed++
	s.readerTypes = append(s.readerTypes, fmt.Sprintf("%T", content))
	s.declared = append(s.declared, size)
	streamErr := s.streamErr
	s.mu.Unlock()
	if streamErr != nil {
		return streamErr
	}
	buf, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	if uint64(len(buf)) != size {
		return fmt.Errorf("stream delivered %d bytes, declared %d", len(buf), size)
	}
	// Stored directly rather than through PutToolResultObject so putCount stays
	// a count of MATERIALIZED puts alone: a delegating fake would make "the
	// streaming path was preferred" untestable.
	s.fakeObjectStore.mu.Lock()
	defer s.fakeObjectStore.mu.Unlock()
	s.fakeObjectStore.objects[objectID] = buf
	return nil
}

func (s *streamingObjectStore) streamCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamed
}

func (s *streamingObjectStore) readerType(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readerTypes[i]
}

// spillConfig is captureConfig plus a session spill directory, which is what a
// composition root that wired a spill base produces.
func spillConfig(t *testing.T, store ToolResultObjectStore, modelBytes, captureCeiling, materializedMax int) (turnConfig, *captureSpillDirectory) {
	t.Helper()
	session, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	dir, err := newCaptureSpillDirectory(t.TempDir(), session)
	if err != nil {
		t.Fatalf("newCaptureSpillDirectory: %v", err)
	}
	t.Cleanup(func() { _ = dir.Release() })
	cfg := captureConfig(store, modelBytes, captureCeiling, materializedMax)
	cfg.toolResultSpills = dir
	return cfg, dir
}

// TestToolResultRetentionPrefersTheStreamingUploadAndStreamsOffTheSpill covers
// both halves of the upload seam in one property. A store that implements the
// optional streaming capability must receive the capture as a stream READ BACK
// FROM THE LOCAL SPILL FILE — proven by the concrete reader type, which is the
// only thing that distinguishes streaming from copying a materialized buffer —
// and PutToolResultObject must not be called at all. A store without the
// capability must get the materialized Put instead.
func TestToolResultRetentionPrefersTheStreamingUploadAndStreamsOffTheSpill(t *testing.T) {
	t.Parallel()
	const payload = 4096
	streaming := newStreamingObjectStore()
	cfg, _ := spillConfig(t, streaming, 64, payload*2, payload*2)
	text := strings.Repeat("s", payload)
	commit, err := retainToolResults(context.Background(), cfg, []result{textResult(t, "call-1", text)})
	if err != nil {
		t.Fatalf("retainToolResults: %v", err)
	}
	if commit.retention != nil {
		t.Fatalf("retention failed: %+v", commit.retention)
	}
	if streaming.streamCount() != 1 {
		t.Fatalf("stream uploads = %d, want 1", streaming.streamCount())
	}
	if got := streaming.fakeObjectStore.putCount(); got != 0 {
		t.Fatalf("materialized puts = %d, want 0 when the store streams", got)
	}
	if got := streaming.readerType(0); got != "*os.File" {
		t.Fatalf("streamed reader = %s, want *os.File: the upload must read the local spill, not a materialized copy", got)
	}
	stored, ok := streaming.fakeObjectStore.get(commit.captures[0].Reference.ObjectID)
	if !ok || string(stored) != text {
		t.Fatalf("stored object = %d bytes, want the complete %d-byte capture", len(stored), len(text))
	}

	plain := newFakeObjectStore()
	plainCfg, _ := spillConfig(t, plain, 64, payload*2, payload*2)
	plainCommit, err := retainToolResults(context.Background(), plainCfg, []result{textResult(t, "call-1", text)})
	if err != nil || plainCommit.retention != nil {
		t.Fatalf("plain store retention: err=%v retention=%+v", err, plainCommit.retention)
	}
	if plain.putCount() != 1 {
		t.Fatalf("materialized puts = %d, want 1 for a store without the streaming capability", plain.putCount())
	}
}

// TestToolResultRetentionDeletesTheLocalSpill pins step 3's cleanup on BOTH
// outcomes. After a verified upload the local copy has served its purpose; after
// a failed one the turn ends, so the local copy has no reader either. Leaving
// either behind would grow a session's disk without bound across a long run.
func TestToolResultRetentionDeletesTheLocalSpill(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		arrange   func(*fakeObjectStore)
		wantStage ToolResultRetentionStage
	}{
		{name: "verified upload", arrange: func(*fakeObjectStore) {}},
		{name: "put failure", arrange: func(s *fakeObjectStore) { s.putErr = errors.New("put failed") }, wantStage: ToolResultRetentionStagePut},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newFakeObjectStore()
			tt.arrange(store)
			cfg, dir := spillConfig(t, store, 16, 4096, 4096)
			commit, err := retainToolResults(context.Background(), cfg, []result{textResult(t, "call-1", strings.Repeat("y", 1024))})
			if err != nil {
				t.Fatalf("retainToolResults: %v", err)
			}
			if tt.wantStage == "" && commit.retention != nil {
				t.Fatalf("retention failed: %+v", commit.retention)
			}
			if tt.wantStage != "" && (commit.retention == nil || commit.retention.Stage != tt.wantStage) {
				t.Fatalf("retention = %+v, want stage %q", commit.retention, tt.wantStage)
			}
			entries, readErr := os.ReadDir(dir.root)
			if readErr != nil {
				t.Fatalf("read spill root: %v", readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("spill root still holds %d files after retention", len(entries))
			}
		})
	}
}

// TestToolResultRetentionFailsAtTheSpillStage covers the two ways the local
// spill can be unusable — it could not be opened at all (the session released its
// spill root), and it accepted the producer's bytes but the backing failed
// (a full disk, a short write). Both must fail the step at the SPILL stage
// rather than uploading a prefix the counts and digest do not describe.
func TestToolResultRetentionFailsAtTheSpillStage(t *testing.T) {
	t.Parallel()
	t.Run("spill cannot be opened", func(t *testing.T) {
		t.Parallel()
		store := newFakeObjectStore()
		cfg, dir := spillConfig(t, store, 16, 4096, 4096)
		if err := dir.Release(); err != nil {
			t.Fatalf("Release: %v", err)
		}
		commit, err := retainToolResults(context.Background(), cfg, []result{textResult(t, "call-1", strings.Repeat("z", 1024))})
		if err != nil {
			t.Fatalf("retainToolResults: %v", err)
		}
		if commit.retention == nil || commit.retention.Stage != ToolResultRetentionStageSpill {
			t.Fatalf("retention = %+v, want stage %q", commit.retention, ToolResultRetentionStageSpill)
		}
		if store.putCount() != 0 {
			t.Fatalf("puts = %d, want 0: nothing may be uploaded for a capture that was never spilled", store.putCount())
		}
	})
	t.Run("spill write failed", func(t *testing.T) {
		t.Parallel()
		store := newFakeObjectStore()
		cfg := captureConfig(store, 16, 4096, 4096)
		streamed := textResult(t, "call-1", strings.Repeat("w", 1024))
		// The backing accepts a PREFIX and then short-writes, and its prefix reads
		// back cleanly. That is what a disk filling up mid-write leaves behind, and
		// it is the arrangement in which the counts and digest would otherwise
		// AGREE with the stored object: without the sink's latched failure the
		// pipeline would upload two bytes and record them as a successful capture
		// truncated at the ceiling.
		streamed.capture = newCaptureSinkWithBacking(4096, &faultyBacking{shortAfter: 2})
		if _, err := streamed.capture.Write([]byte("partial")); err != nil {
			t.Fatalf("sink Write: %v", err)
		}
		commit, err := retainToolResults(context.Background(), cfg, []result{streamed})
		if err != nil {
			t.Fatalf("retainToolResults: %v", err)
		}
		if commit.retention == nil || commit.retention.Stage != ToolResultRetentionStageSpill {
			t.Fatalf("retention = %+v, want stage %q", commit.retention, ToolResultRetentionStageSpill)
		}
		if !errors.Is(commit.retention, io.ErrShortWrite) {
			t.Fatalf("retention cause = %v, want the latched short write", commit.retention.Cause)
		}
		if store.putCount() != 0 {
			t.Fatalf("puts = %d, want 0", store.putCount())
		}
	})
}

// TestToolResultRetentionUsesAStreamedCaptureRatherThanReEncodingTheResult is
// the streaming path's defining property: when the tool already wrote its
// complete raw stream to the sink, the retained object is THAT stream — not the
// bounded ToolResult the tool also returned. A pipeline that re-encoded the
// returned result would retain the preview and silently lose the tail, which is
// the exact loss the capability exists to prevent.
func TestToolResultRetentionUsesAStreamedCaptureRatherThanReEncodingTheResult(t *testing.T) {
	t.Parallel()
	store := newFakeObjectStore()
	cfg, dir := spillConfig(t, store, 16, 4096, 4096)
	full := strings.Repeat("F", 2048)
	streamed := textResult(t, "call-1", "bounded model preview")
	sink, err := dir.openSink(streamed.ToolExecutionID, 4096)
	if err != nil {
		t.Fatalf("openSink: %v", err)
	}
	if _, err := sink.Write([]byte(full)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	streamed.capture = sink
	commit, err := retainToolResults(context.Background(), cfg, []result{streamed})
	if err != nil {
		t.Fatalf("retainToolResults: %v", err)
	}
	if commit.retention != nil {
		t.Fatalf("retention failed: %+v", commit.retention)
	}
	capture := commit.captures[0]
	if capture.CapturedBytes != uint64(len(full)) {
		t.Fatalf("CapturedBytes = %d, want %d", capture.CapturedBytes, len(full))
	}
	stored, ok := store.get(capture.Reference.ObjectID)
	if !ok || string(stored) != full {
		t.Fatalf("stored object = %q..., want the streamed bytes", string(stored[:min(len(stored), 16)]))
	}
	if got := committedText(t, commit.messages[0]); !strings.HasPrefix(got, "bounded") && !strings.Contains(got, "shaped") {
		t.Fatalf("committed text = %q, want the tool's own bounded preview", got)
	}
}

// TestStreamedCaptureRecordsTheCeilingTruncation pins what a STREAMING producer
// that ran past the ceiling records. The sink kept counting after it stopped
// retaining, so the original size is exact even though the capture is truncated,
// and the reason is capture_ceiling — a Harness ceiling, not a producer that
// bounded itself.
func TestStreamedCaptureRecordsTheCeilingTruncation(t *testing.T) {
	t.Parallel()
	store := newFakeObjectStore()
	const ceiling = 128
	cfg, dir := spillConfig(t, store, 16, ceiling, ceiling)
	streamed := textResult(t, "call-1", "preview")
	sink, err := dir.openSink(streamed.ToolExecutionID, ceiling)
	if err != nil {
		t.Fatalf("openSink: %v", err)
	}
	if _, err := sink.Write([]byte(strings.Repeat("T", 1000))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	streamed.capture = sink
	commit, err := retainToolResults(context.Background(), cfg, []result{streamed})
	if err != nil || commit.retention != nil {
		t.Fatalf("retention: err=%v retention=%+v", err, commit.retention)
	}
	capture := commit.captures[0]
	if !capture.Truncated || capture.TruncationReason != event.ToolResultTruncatedCaptureCeiling {
		t.Fatalf("capture = %+v, want a capture_ceiling truncation", capture)
	}
	if capture.CapturedBytes != ceiling {
		t.Fatalf("CapturedBytes = %d, want %d", capture.CapturedBytes, ceiling)
	}
	original, exact := capture.OriginalSize()
	if original != 1000 || !exact {
		t.Fatalf("OriginalSize = (%d, %v), want (1000, true)", original, exact)
	}
	requireValidStepDone(t, commit)
}

// TestToolResultRetentionTreatsAnAmbiguousUploadAsAFailure covers the upload whose
// outcome the loop cannot know: the store reported an error but stored the object
// anyway — a timed-out write that landed, the classic ambiguous PUT. The loop must
// treat it as a failure, because the only alternative is to record a capture on the
// strength of an error. What is left behind is an object no committed StepDone
// references, which is precisely the orphan a safe garbage collector reclaims.
func TestToolResultRetentionTreatsAnAmbiguousUploadAsAFailure(t *testing.T) {
	t.Parallel()
	store := newFakeObjectStore()
	store.putErr = errAmbiguousUpload
	store.storeDespitePutErr = true
	cfg, _ := spillConfig(t, store, 16, 4096, 4096)
	text := strings.Repeat("A", 1024)
	commit, err := retainToolResults(context.Background(), cfg, []result{textResult(t, "call-1", text)})
	if err != nil {
		t.Fatalf("retainToolResults: %v", err)
	}
	if commit.retention == nil || commit.retention.Stage != ToolResultRetentionStagePut {
		t.Fatalf("retention = %+v, want a put-stage failure", commit.retention)
	}
	if len(commit.captures) != 0 {
		t.Fatalf("captures = %d, want none: an ambiguous upload may not be recorded as retained", len(commit.captures))
	}
	if store.putCount() != 1 {
		t.Fatalf("puts = %d, want exactly 1: an ambiguous upload is not retried", store.putCount())
	}
	sum := sha256.Sum256([]byte(text))
	if _, ok := store.get(captureObjectID(hex.EncodeToString(sum[:]))); !ok {
		t.Fatal("the ambiguously-stored object is absent; this test no longer exercises the ambiguous case")
	}
}

var errAmbiguousUpload = errors.New("upload timed out")

// TestToolResultRetentionCancellationReleasesEveryStreamedSpill pins the local
// cleanup on the one path that commits nothing at all. A cancelled step discards
// its results, so nothing will ever read their spills; leaving them behind would
// let an interrupted session accumulate files with no owner.
func TestToolResultRetentionCancellationReleasesEveryStreamedSpill(t *testing.T) {
	t.Parallel()
	store := newFakeObjectStore()
	store.putErr = errors.New("store unavailable")
	cfg, dir := spillConfig(t, store, 16, 4096, 4096)
	ctx, cancel := context.WithCancel(context.Background())
	results := make([]result, 0, 3)
	for i := 0; i < 3; i++ {
		r := textResult(t, fmt.Sprintf("call-%d", i), strings.Repeat("C", 1024))
		sink, err := dir.openSink(r.ToolExecutionID, 4096)
		if err != nil {
			t.Fatalf("openSink: %v", err)
		}
		if _, err := sink.Write([]byte(strings.Repeat("C", 1024))); err != nil {
			t.Fatalf("Write: %v", err)
		}
		r.capture = sink
		results = append(results, r)
	}
	entries, err := os.ReadDir(dir.root)
	if err != nil || len(entries) != 3 {
		t.Fatalf("spill root holds %d files before retention (err=%v), want 3", len(entries), err)
	}
	cancel()
	if _, err := retainToolResults(ctx, cfg, results); !errors.Is(err, context.Canceled) {
		t.Fatalf("retainToolResults = %v, want context.Canceled", err)
	}
	entries, err = os.ReadDir(dir.root)
	if err != nil {
		t.Fatalf("read spill root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("spill root holds %d files after a cancelled step, want 0", len(entries))
	}
}

// TestRunTurnStreamsACapturingToolThroughTheSessionSpill is the whole-turn half
// of the streaming contract, and it is where turnCaptureSinks has its reader.
// Everything below the turn can be proved with a hand-built sink; only a real
// turn shows that the loop's own config is what opens one, that a capturing tool
// reaches the streaming entry point through runTurn, and that the committed
// StepDone describes the STREAMED bytes rather than the bounded result the tool
// also returned.
func TestRunTurnStreamsACapturingToolThroughTheSessionSpill(t *testing.T) {
	t.Parallel()
	const maxBytes = 128
	raw := strings.Repeat("S", 4096)
	streaming := &turnStreamingTool{name: "Streamer", raw: raw, preview: "short preview"}
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{toolUseChunk(0, "id-stream", "Streamer", `{}`)},
		{textChunk("done")},
	}}
	ts := agenticToolSet([]tool.InvokableTool{streaming}, 25, 100)
	ts.MaxToolResultBytes = maxBytes
	store := newFakeObjectStore()
	cfg, st, rec := newTurnFixture(nil, nil, ts, client, noGateReg())
	cfg.toolResultObjects = store
	session, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	dir, err := newCaptureSpillDirectory(t.TempDir(), session)
	if err != nil {
		t.Fatalf("newCaptureSpillDirectory: %v", err)
	}
	defer func() { _ = dir.Release() }()
	cfg.toolResultSpills = dir
	streaming.spillRoot = dir.root

	if terminal := runTurn(context.Background(), cfg, st); terminal == nil {
		t.Fatal("runTurn returned no terminal")
	}
	if got := streaming.capturedRuns(); got != 1 {
		t.Fatalf("streaming entry point runs = %d, want 1: the turn did not prefer the capability", got)
	}
	if got := streaming.liveSpillFiles(); got != 1 {
		t.Fatalf("spill files present while the tool was streaming = %d, want 1: the turn wired a sink that does not use the session spill", got)
	}
	sds := stepDones(rec.events())
	// Two steps: the tool step, then the final-answer step the model produces
	// once it has seen the tool result. Only the first carries a capture.
	if len(sds) != 2 {
		t.Fatalf("StepDone count = %d, want 2", len(sds))
	}
	if len(sds[1].Captures) != 0 {
		t.Fatalf("the final-answer step carries %d captures, want none", len(sds[1].Captures))
	}
	if len(sds[0].Captures) != 1 {
		t.Fatalf("Captures = %d, want 1", len(sds[0].Captures))
	}
	capture := sds[0].Captures[0]
	if capture.CapturedBytes != uint64(len(raw)) {
		t.Fatalf("CapturedBytes = %d, want the streamed %d", capture.CapturedBytes, len(raw))
	}
	if capture.Reference == nil {
		t.Fatal("the committed capture has no object reference")
	}
	stored, ok := store.get(capture.Reference.ObjectID)
	if !ok || string(stored) != raw {
		t.Fatalf("stored object = %d bytes, want the streamed %d", len(stored), len(raw))
	}
	trm, ok := sds[0].Messages[1].(*content.ToolResultMessage)
	if !ok {
		t.Fatalf("StepDone.Messages[1] = %T, want *content.ToolResultMessage", sds[0].Messages[1])
	}
	if got := committedText(t, trm); len(got) > maxBytes || !strings.Contains(got, "short preview") {
		t.Fatalf("committed tool result = %q (%d bytes), want the tool's bounded preview within the model budget", got, len(got))
	}
	entries, err := os.ReadDir(dir.root)
	if err != nil {
		t.Fatalf("read spill root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("spill root holds %d files after a committed turn", len(entries))
	}
}

// turnStreamingTool is a capturing tool usable in a whole-turn fixture: it
// implements the preparation boundary the runner requires and streams raw bytes
// that are deliberately NOT the bytes it returns.
type turnStreamingTool struct {
	name    string
	raw     string
	preview string

	// spillRoot, when set, is read from INSIDE the streaming call. It is the only
	// place the difference between a file-backed sink and a memory-backed one is
	// observable: by the time the turn ends the spill has been released either
	// way, so an after-the-fact directory listing cannot tell them apart.
	spillRoot string

	mu        sync.Mutex
	captured  int
	liveFiles int
}

func (s *turnStreamingTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: s.name}, nil
}

func (s *turnStreamingTool) PrepareCall(context.Context, uuid.UUID, string) (tool.Request, tool.PreparedArtifact, error) {
	return tool.Request{}, nil, nil
}

func (s *turnStreamingTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult(s.preview), nil
}

func (s *turnStreamingTool) InvokableRunCaptured(_ context.Context, _ string, sink tool.ResultCaptureSink) (*tool.ToolResult, error) {
	s.mu.Lock()
	s.captured++
	s.mu.Unlock()
	if _, err := io.WriteString(sink, s.raw); err != nil {
		return nil, err
	}
	if s.spillRoot != "" {
		entries, err := os.ReadDir(s.spillRoot)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.liveFiles = len(entries)
		s.mu.Unlock()
	}
	return tool.TextResult(s.preview), nil
}

func (s *turnStreamingTool) capturedRuns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captured
}

// liveSpillFiles is how many files existed under the session spill root while
// the tool was still writing to its sink.
func (s *turnStreamingTool) liveSpillFiles() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liveFiles
}
