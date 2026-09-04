package loop_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
)

// stubInferenceClient satisfies the inference seam a loop definition requires. No
// test here runs a turn, so both methods refuse.
type stubInferenceClient struct{}

func (stubInferenceClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("unused")
}

func (stubInferenceClient) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, errors.New("unused")
}

func testModel() model.Model {
	return model.Model{
		Provider: model.ProviderName("lmstudio"), APIFormat: model.APIFormatOpenAI,
		BaseURL: "http://localhost:1234", Name: "m",
	}
}

// externalObjectStore is what a composition root outside harness writes: it
// names only pkg/loop and the standard library, and it is the whole reason the
// store types moved out of internal/loopruntime.
type externalObjectStore struct {
	objects map[string][]byte
	streams int
}

func (s *externalObjectStore) PutToolResultObject(_ context.Context, objectID string, content []byte) error {
	s.objects[objectID] = append([]byte(nil), content...)
	return nil
}

func (s *externalObjectStore) StatToolResultObject(_ context.Context, objectID string) (loop.ToolResultObjectStat, error) {
	return loop.ToolResultObjectStat{SizeBytes: uint64(len(s.objects[objectID]))}, nil
}

func (s *externalObjectStore) PutToolResultObjectStream(_ context.Context, objectID string, content io.Reader, _ uint64) error {
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, content); err != nil {
		return err
	}
	s.streams++
	s.objects[objectID] = buf.Bytes()
	return nil
}

// TestToolResultObjectStoreIsSatisfiableFromOutsideTheRuntime pins the seam an
// external composition root actually uses. It is written in the EXTERNAL test
// package so the assertion is made from a file that imports pkg/loop the way a
// consumer does, rather than from inside the package where an unexported helper
// could stand in.
func TestToolResultObjectStoreIsSatisfiableFromOutsideTheRuntime(t *testing.T) {
	t.Parallel()
	store := &externalObjectStore{objects: map[string][]byte{}}
	var narrow loop.ToolResultObjectStore = store
	if err := narrow.PutToolResultObject(context.Background(), "id", []byte("payload")); err != nil {
		t.Fatalf("PutToolResultObject: %v", err)
	}
	stat, err := narrow.StatToolResultObject(context.Background(), "id")
	if err != nil {
		t.Fatalf("StatToolResultObject: %v", err)
	}
	if stat.SizeBytes != 7 {
		t.Fatalf("SizeBytes = %d, want 7", stat.SizeBytes)
	}
}

// TestToolResultObjectStreamStoreIsOptionalAndProbedByAssertion covers both
// directions: a store that implements only the two required methods must FAIL
// the streaming assertion (so the pipeline falls back rather than nil-derefs),
// and one that adds the streaming method must satisfy it.
func TestToolResultObjectStreamStoreIsOptionalAndProbedByAssertion(t *testing.T) {
	t.Parallel()
	var streaming loop.ToolResultObjectStore = &externalObjectStore{objects: map[string][]byte{}}
	if _, ok := streaming.(loop.ToolResultObjectStreamStore); !ok {
		t.Fatal("a store declaring PutToolResultObjectStream failed the streaming assertion")
	}
	var plain loop.ToolResultObjectStore = plainStore{}
	if _, ok := plain.(loop.ToolResultObjectStreamStore); ok {
		t.Fatal("a store without PutToolResultObjectStream satisfied the streaming assertion")
	}
}

type plainStore struct{}

func (plainStore) PutToolResultObject(context.Context, string, []byte) error { return nil }

func (plainStore) StatToolResultObject(context.Context, string) (loop.ToolResultObjectStat, error) {
	return loop.ToolResultObjectStat{}, nil
}

// TestDefaultMaterializedToolResultBytesIsAFiniteDeclaredMaximum pins the value
// the capture-safety descriptor is projected against. A non-positive default
// would make every high-output materialized definition unsafe by construction,
// which is the fact the descriptor exists to report rather than to cause.
func TestDefaultMaterializedToolResultBytesIsAFiniteDeclaredMaximum(t *testing.T) {
	t.Parallel()
	if loop.DefaultMaterializedToolResultBytes <= 0 {
		t.Fatalf("DefaultMaterializedToolResultBytes = %d, want a positive finite maximum", loop.DefaultMaterializedToolResultBytes)
	}
	if loop.DefaultMaterializedToolResultBytes <= loop.DefaultToolResultCaptureBytes {
		t.Fatalf("DefaultMaterializedToolResultBytes = %d must exceed DefaultToolResultCaptureBytes = %d so the retention ceiling ordinarily binds",
			loop.DefaultMaterializedToolResultBytes, loop.DefaultToolResultCaptureBytes)
	}
	if reflect.TypeOf(loop.DefaultMaterializedToolResultBytes).Kind() != reflect.Int {
		t.Fatalf("DefaultMaterializedToolResultBytes is %v, want an int", reflect.TypeOf(loop.DefaultMaterializedToolResultBytes))
	}
}

// TestToolDefinitionsUnionsBaseAndModesWithoutDuplicates pins the new public
// accessor's own contract, independently of the capture-safety projection that
// motivated it. The projection merges by name and would therefore hide a missing
// deduplication here; a later reader of ToolDefinitions would not.
func TestToolDefinitionsUnionsBaseAndModesWithoutDuplicates(t *testing.T) {
	t.Parallel()
	shared := tool.NewDefinition("shared", 0, nil)
	baseOnly := tool.NewDefinition("base-only", 0, nil)
	modeOnly := tool.NewDefinition("mode-only", 0, nil)
	definition, err := loop.Define(
		loop.WithName(identity.AgentName("planner")),
		loop.WithInference(stubInferenceClient{}, testModel()),
		loop.WithTools(shared, baseOnly),
		loop.WithModes(loop.Mode{Name: "deep", Tools: []tool.Definition{shared, modeOnly}}),
		loop.WithInitialMode("deep"),
	)
	if err != nil {
		t.Fatalf("loop.Define: %v", err)
	}
	var names []string
	for _, d := range definition.ToolDefinitions() {
		names = append(names, d.Name())
	}
	if got := strings.Join(names, ","); got != "shared,base-only,mode-only" {
		t.Fatalf("ToolDefinitions = %q, want base-first then mode, each name once", got)
	}
	var zero loop.Definition
	if got := zero.ToolDefinitions(); got != nil {
		t.Fatalf("zero Definition ToolDefinitions = %v, want nil", got)
	}
}
