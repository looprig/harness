package loopruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

	puts []string
}

func newFakeObjectStore() *fakeObjectStore {
	return &fakeObjectStore{objects: map[string][]byte{}}
}

var errFakeObjectIdentityReused = errors.New("fake object store: identity reused with different bytes")

func (f *fakeObjectStore) PutToolResultObject(_ context.Context, objectID string, content []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, objectID)
	if f.putErr != nil {
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
	if f.statErr != nil {
		return ToolResultObjectStat{}, f.statErr
	}
	stored, ok := f.objects[objectID]
	if !ok {
		return ToolResultObjectStat{}, errors.New("fake object store: no such object")
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
	for _, tt := range tests {
		for _, failingFirst := range []bool{true, false} {
			tt, failingFirst := tt, failingFirst
			name := tt.name + "/failing result last"
			if failingFirst {
				name = tt.name + "/failing result first"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				store := newFakeObjectStore()
				tt.arrange(store)
				cfg := captureConfig(store, 128, 1<<20, 1<<20)
				// "short" is below the model budget, so it needs no object and its
				// capture is recorded without touching the store — which is what
				// makes it a survivor that can only differ by ordering.
				failing := textResult(t, "tu-big", strings.Repeat("w", 4096))
				survivor := textResult(t, "tu-small", "short")
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
				// The surviving result keeps its ordinary text and must NOT claim a
				// retention that this step did not record.
				if got := committedText(t, commit.messages[survivorIndex]); got != "short" {
					t.Fatalf("surviving result committed %q, want %q", got, "short")
				}
				if strings.Contains(committedText(t, commit.messages[survivorIndex]), toolResultRetainedMarkerPrefix) {
					t.Fatal("a surviving result carries a retention marker although no capture was recorded")
				}
			})
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
