package loopruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// fakePublisher is an in-memory ToolResultPublisher that MINTS its own opaque
// reference, the way a session object store does. Its identities deliberately
// share no grammar with captureObjectID, so a test can tell a store-issued
// reference from one the loop minted.
type fakePublisher struct {
	mu      sync.Mutex
	objects map[string][]byte
	next    int

	publishErr error
	// lieSize / lieDigest / lieReference make the store report metadata that
	// does not describe what it was handed.
	lieSize      bool
	lieDigest    bool
	lieReference bool
}

func newFakePublisher() *fakePublisher { return &fakePublisher{objects: map[string][]byte{}} }

func (p *fakePublisher) PublishToolResultObject(_ context.Context, body io.Reader, size uint64, sum [32]byte) (sessionwire.ObjectMetadata, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return sessionwire.ObjectMetadata{}, err
	}
	if p.publishErr != nil {
		return sessionwire.ObjectMetadata{}, p.publishErr
	}
	if uint64(len(data)) != size || sha256.Sum256(data) != sum {
		return sessionwire.ObjectMetadata{}, errors.New("fake publisher: body does not match the declared size and digest")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	id := "v1:tool-result:gen" + strings.Repeat("0", p.next) + ":" + hex.EncodeToString(sum[:])
	p.objects[id] = data
	metadata := sessionwire.ObjectMetadata{
		Reference: sessionwire.ObjectReference{ObjectID: id},
		SizeBytes: size,
		Digest:    "sha256:" + hex.EncodeToString(sum[:]),
	}
	if p.lieSize {
		metadata.SizeBytes++
	}
	if p.lieDigest {
		metadata.Digest = "sha256:" + strings.Repeat("0", 64)
	}
	if p.lieReference {
		metadata.Reference = sessionwire.ObjectReference{}
	}
	return metadata, nil
}

func (p *fakePublisher) get(id string) ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	data, ok := p.objects[id]
	return data, ok
}

func publishConfig(publisher ToolResultPublisher, modelBytes int, tools ...tool.InvokableTool) turnConfig {
	cfg := captureConfig(nil, modelBytes, 0, 0)
	cfg.toolResultPublisher = publisher
	cfg.tools.Registry = tools
	return cfg
}

// namedTool is an InvokableTool that exists only for its name: it lets a test
// put the reader tool into a loop's registry.
type namedTool struct{ name string }

func (n namedTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: n.name, Schema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (namedTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult(""), nil
}

// TestPublishedRetentionRecordsTheStoreIssuedReference is the I2.2 defect-1 fix
// at the loop layer: the recorded reference is exactly the one the store
// returned, never a harness-minted content id, and it resolves to the full
// captured bytes.
func TestPublishedRetentionRecordsTheStoreIssuedReference(t *testing.T) {
	t.Parallel()
	publisher := newFakePublisher()
	big := strings.Repeat("p", 4096)
	r := textResult(t, "tu-1", big)
	commit, err := retainToolResults(context.Background(), publishConfig(publisher, 512), []result{r})
	if err != nil {
		t.Fatalf("retainToolResults: %v", err)
	}
	if commit.retention != nil {
		t.Fatalf("retention failed: %v", commit.retention)
	}
	requireValidStepDone(t, commit)
	if len(commit.captures) != 1 || commit.captures[0].Reference == nil {
		t.Fatalf("captures = %+v, want one referenced capture", commit.captures)
	}
	ref := commit.captures[0].Reference.ObjectID
	if strings.HasPrefix(ref, captureObjectIDPrefix) {
		t.Fatalf("reference %q was minted by harness, not issued by the store", ref)
	}
	stored, ok := publisher.get(ref)
	if !ok {
		t.Fatalf("the recorded reference %q names nothing the store holds", ref)
	}
	if string(stored) != big {
		t.Fatalf("stored %d bytes, want the full %d-byte result", len(stored), len(big))
	}
}

// TestPublishedRetentionVerifiesTheReturnedMetadata enumerates every way a store
// can misdescribe what it was handed; each must fail the retention at its own
// stage rather than commit a reference to the wrong object.
func TestPublishedRetentionVerifiesTheReturnedMetadata(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		arrange func(*fakePublisher)
		stage   ToolResultRetentionStage
	}{
		{name: "publish error", arrange: func(p *fakePublisher) { p.publishErr = errors.New("down") }, stage: ToolResultRetentionStagePut},
		{name: "wrong size", arrange: func(p *fakePublisher) { p.lieSize = true }, stage: ToolResultRetentionStageSize},
		{name: "wrong digest", arrange: func(p *fakePublisher) { p.lieDigest = true }, stage: ToolResultRetentionStageDigest},
		{name: "invalid reference", arrange: func(p *fakePublisher) { p.lieReference = true }, stage: ToolResultRetentionStageReference},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			publisher := newFakePublisher()
			tt.arrange(publisher)
			r := textResult(t, "tu-1", strings.Repeat("q", 4096))
			commit, err := retainToolResults(context.Background(), publishConfig(publisher, 512), []result{r})
			if err != nil {
				t.Fatalf("retainToolResults: %v", err)
			}
			if commit.retention == nil || commit.retention.Stage != tt.stage {
				t.Fatalf("retention = %+v, want stage %q", commit.retention, tt.stage)
			}
			if len(commit.captures) != 0 {
				t.Fatalf("captures = %d, want none after a failed retention", len(commit.captures))
			}
		})
	}
}

// TestPublishedRetentionMarkerNamesTheReaderOnlyWhenBound pins that the marker
// instructs a read_tool_result call only when the calling loop can make it.
func TestPublishedRetentionMarkerNamesTheReaderOnlyWhenBound(t *testing.T) {
	t.Parallel()
	for _, bound := range []bool{false, true} {
		publisher := newFakePublisher()
		var tools []tool.InvokableTool
		if bound {
			tools = append(tools, namedTool{name: loop.ReadToolResultToolName})
		}
		r := textResult(t, "tu-1", strings.Repeat("m", 4096))
		commit, err := retainToolResults(context.Background(), publishConfig(publisher, 512, tools...), []result{r})
		if err != nil || commit.retention != nil {
			t.Fatalf("bound=%v: retainToolResults = %v / %v", bound, err, commit.retention)
		}
		text := committedText(t, commit.messages[0])
		wantInstruction := loop.ReadToolResultToolName + ` capture_id="` + r.ToolExecutionID.String() + `"`
		if got := strings.Contains(text, wantInstruction); got != bound {
			t.Fatalf("bound=%v: marker instruction present = %v in %q", bound, got, text)
		}
		if len(text) > 512 {
			t.Fatalf("bound=%v: preview is %d bytes, above the model budget", bound, len(text))
		}
	}
}

// TestLegacyRetentionNeverNamesTheReader pins D3: a capture retained through
// the deprecated caller-minted seam is unreadable, so even a loop with the
// reader bound must not be told to read it.
func TestLegacyRetentionNeverNamesTheReader(t *testing.T) {
	t.Parallel()
	cfg := captureConfig(newFakeObjectStore(), 512, 0, 0)
	cfg.tools.Registry = []tool.InvokableTool{namedTool{name: loop.ReadToolResultToolName}}
	commit, err := retainToolResults(context.Background(), cfg, []result{textResult(t, "tu-1", strings.Repeat("l", 4096))})
	if err != nil || commit.retention != nil {
		t.Fatalf("retainToolResults = %v / %v", err, commit.retention)
	}
	if text := committedText(t, commit.messages[0]); strings.Contains(text, loop.ReadToolResultToolName) {
		t.Fatalf("legacy marker %q instructs a read of an unreadable capture", text)
	}
}

// TestRunTurnPublishedRetentionFailureEndsTheTurn is D8 on the readable path: a
// failed publish commits the notice with zero captures and ends the turn on the
// typed cause, with no further inference.
func TestRunTurnPublishedRetentionFailureEndsTheTurn(t *testing.T) {
	t.Parallel()
	rawTool := &rawGraphTool{name: "RawGraph", raw: []content.Block{&content.TextBlock{Text: strings.Repeat("g", 4096)}}}
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{toolUseChunk(0, "id-raw", "RawGraph", `{}`)},
		{textChunk("done")},
	}}
	ts := agenticToolSet([]tool.InvokableTool{rawTool}, 25, 100)
	ts.MaxToolResultBytes = 128
	publisher := newFakePublisher()
	publisher.publishErr = errors.New("object store unavailable")
	cfg, st, rec := newTurnFixture(nil, nil, ts, client, noGateReg())
	cfg.toolResultPublisher = publisher

	terminal := runTurn(context.Background(), cfg, st)
	failed, ok := terminal.(event.TurnFailed)
	if !ok {
		t.Fatalf("terminal = %T, want TurnFailed", terminal)
	}
	var retention *ToolResultRetentionError
	if !errors.As(failed.Err, &retention) || retention.Stage != ToolResultRetentionStagePut {
		t.Fatalf("terminal error = %v, want a put-stage *ToolResultRetentionError", failed.Err)
	}
	sds := stepDones(rec.events())
	if len(sds) != 1 || len(sds[0].Captures) != 0 {
		t.Fatalf("StepDones = %d (captures %v), want one with no captures", len(sds), sds)
	}
	if got := len(client.requests()); got != 1 {
		t.Fatalf("inference requests = %d, want 1", got)
	}
}
