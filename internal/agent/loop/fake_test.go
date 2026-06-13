package loop

import (
	"context"
	"errors"
	"io"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
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
	return content.Chunk{Type: content.ChunkTypeText, Text: &content.TextChunk{Text: s}}
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
			return content.Chunk{}, ctx.Err()
		}
		if f.nextErr != nil {
			return content.Chunk{}, f.nextErr
		}
		return content.Chunk{}, io.EOF
	}
	return llm.NewStreamReader(next, nil), nil
}
