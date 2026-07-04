package loop

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/llm"
)

// fakeLLM is a controllable llm.LLM for tests.
type fakeLLM struct {
	chunks           []content.Chunk
	streamErr        error // returned from Stream() itself (before any chunk)
	nextErr          error // returned from Next() after all chunks (instead of io.EOF)
	blockUntilCancel bool  // Next() blocks until ctx cancelled, then returns ctx.Err()
	ignoreCtx        bool  // with blockUntilCancel: block forever (provider ignores ctx)
}

func textChunk(s string) content.Chunk {
	return &content.TextChunk{Text: s}
}

// testModel returns a minimal but VALID llm.Model (passes llm.Model.Validate):
// a known provider speaking a supported wire dialect at a loopback endpoint. Tests
// that only need a well-formed descriptor use it in place of the retired ModelSpec.
func testModel() llm.Model {
	return llm.Model{
		Provider:  llm.ProviderLMStudio,
		APIFormat: llm.APIFormatOpenAI,
		BaseURL:   "http://localhost:1234",
		Name:      "m",
	}
}

func (f *fakeLLM) Invoke(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return nil, errors.New("fakeLLM.Invoke not used")
}

func (f *fakeLLM) Stream(ctx context.Context, req llm.Request) (*llm.StreamReader[content.Chunk], error) {
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	i := 0
	next := func() (content.Chunk, error) {
		if i < len(f.chunks) {
			c := f.chunks[i]
			i++
			return c, nil
		}
		if f.blockUntilCancel {
			if f.ignoreCtx {
				select {} // deliberate hang; only safe under a bounded test
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if f.nextErr != nil {
			return nil, f.nextErr
		}
		return nil, io.EOF
	}
	return llm.NewStreamReader(next, nil), nil
}

// recordingLLM records each request it receives, then streams a fixed response.
type recordingLLM struct {
	mu     sync.Mutex
	reqs   []llm.Request
	chunks []content.Chunk
}

func (r *recordingLLM) Invoke(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return nil, errors.New("recordingLLM.Invoke not used")
}
func (r *recordingLLM) Stream(ctx context.Context, req llm.Request) (*llm.StreamReader[content.Chunk], error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	i := 0
	next := func() (content.Chunk, error) {
		if i < len(r.chunks) {
			c := r.chunks[i]
			i++
			return c, nil
		}
		return nil, io.EOF
	}
	return llm.NewStreamReader(next, nil), nil
}
func (r *recordingLLM) lastReq() llm.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reqs[len(r.reqs)-1]
}

// panicLLM panics inside Stream.
type panicLLM struct{}

func (panicLLM) Invoke(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return nil, errors.New("unused")
}
func (panicLLM) Stream(ctx context.Context, req llm.Request) (*llm.StreamReader[content.Chunk], error) {
	panic("boom in Stream")
}
