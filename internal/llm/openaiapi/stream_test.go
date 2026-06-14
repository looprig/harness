package openaiapi_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi"
)

// closerSpy wraps an io.Reader and records whether Close was called.
type closerSpy struct {
	io.Reader
	closed bool
}

func (c *closerSpy) Close() error {
	c.closed = true
	return nil
}

func TestNewStream_TextChunks(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		body      string
		wantTexts []string
		wantEOF   bool
	}{
		{
			name:      "single text chunk then DONE",
			body:      "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\ndata: [DONE]\n",
			wantTexts: []string{"hello"},
			wantEOF:   true,
		},
		{
			name:      "multiple text chunks",
			body:      "data: {\"choices\":[{\"delta\":{\"content\":\"foo\"}}]}\ndata: {\"choices\":[{\"delta\":{\"content\":\"bar\"}}]}\ndata: [DONE]\n",
			wantTexts: []string{"foo", "bar"},
			wantEOF:   true,
		},
		{
			name:      "role-only delta skipped",
			body:      "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\ndata: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\ndata: [DONE]\n",
			wantTexts: []string{"hi"},
			wantEOF:   true,
		},
		{
			name:      "after DONE returns EOF",
			body:      "data: [DONE]\n",
			wantTexts: []string{},
			wantEOF:   true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stream := openaiapi.NewStream(io.NopCloser(strings.NewReader(tc.body)))
			defer stream.Close()

			var got []string
			for {
				chunk, err := stream.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				tc, ok := chunk.(*content.TextChunk)
				if !ok {
					t.Fatalf("expected *content.TextChunk, got %T", chunk)
				}
				got = append(got, tc.Text)
			}

			if len(got) != len(tc.wantTexts) {
				t.Fatalf("got %d chunks, want %d: %v", len(got), len(tc.wantTexts), got)
			}
			for i, want := range tc.wantTexts {
				if got[i] != want {
					t.Errorf("chunk[%d]: got %q, want %q", i, got[i], want)
				}
			}

			if tc.wantEOF {
				_, err := stream.Next()
				if !errors.Is(err, io.EOF) {
					t.Errorf("expected io.EOF after stream end, got %v", err)
				}
			}
		})
	}
}

func TestNewStream_ThinkingChunks(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		body       string
		wantTypes  []string
		wantValues []string
	}{
		{
			name:       "reasoning content yields thinking chunk",
			body:       "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"let me think\"}}]}\ndata: [DONE]\n",
			wantTypes:  []string{"thinking"},
			wantValues: []string{"let me think"},
		},
		{
			name:       "thinking then text in sequence",
			body:       "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"plan\"}}]}\ndata: {\"choices\":[{\"delta\":{\"content\":\"result\"}}]}\ndata: [DONE]\n",
			wantTypes:  []string{"thinking", "text"},
			wantValues: []string{"plan", "result"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stream := openaiapi.NewStream(io.NopCloser(strings.NewReader(tc.body)))
			defer stream.Close()

			var gotTypes []string
			var gotValues []string

			for {
				chunk, err := stream.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				switch c := chunk.(type) {
				case *content.ThinkingChunk:
					gotTypes = append(gotTypes, "thinking")
					gotValues = append(gotValues, c.Thinking)
				case *content.TextChunk:
					gotTypes = append(gotTypes, "text")
					gotValues = append(gotValues, c.Text)
				default:
					t.Fatalf("unexpected chunk type: %T", chunk)
				}
			}

			if len(gotTypes) != len(tc.wantTypes) {
				t.Fatalf("got %d chunks, want %d", len(gotTypes), len(tc.wantTypes))
			}
			for i := range tc.wantTypes {
				if gotTypes[i] != tc.wantTypes[i] {
					t.Errorf("chunk[%d] type: got %q, want %q", i, gotTypes[i], tc.wantTypes[i])
				}
				if gotValues[i] != tc.wantValues[i] {
					t.Errorf("chunk[%d] value: got %q, want %q", i, gotValues[i], tc.wantValues[i])
				}
			}
		})
	}
}

func TestNewStream_BodyClosed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
	}{
		{name: "Close sets closed flag"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spy := &closerSpy{Reader: strings.NewReader("data: [DONE]\n")}
			stream := openaiapi.NewStream(spy)

			if err := stream.Close(); err != nil {
				t.Fatalf("Close returned error: %v", err)
			}
			if !spy.closed {
				t.Error("expected underlying body to be closed after Close()")
			}
		})
	}
}

func TestNewStream_MalformedJSON(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		body     string
		wantText string
	}{
		{
			name:     "malformed line skipped, valid line yielded",
			body:     "data: not-json\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\ndata: [DONE]\n",
			wantText: "ok",
		},
		{
			name:     "multiple malformed lines then valid",
			body:     "data: {\ndata: }\ndata: {\"choices\":[{\"delta\":{\"content\":\"good\"}}]}\ndata: [DONE]\n",
			wantText: "good",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stream := openaiapi.NewStream(io.NopCloser(strings.NewReader(tc.body)))
			defer stream.Close()

			chunk, err := stream.Next()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			c, ok := chunk.(*content.TextChunk)
			if !ok {
				t.Fatalf("expected *content.TextChunk, got %T", chunk)
			}
			if c.Text != tc.wantText {
				t.Errorf("got text %q, want %q", c.Text, tc.wantText)
			}
		})
	}
}

func TestNewStream_EmptyChoices(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		body     string
		wantText string
	}{
		{
			name:     "empty choices skipped",
			body:     "data: {\"choices\":[]}\ndata: {\"choices\":[{\"delta\":{\"content\":\"yes\"}}]}\ndata: [DONE]\n",
			wantText: "yes",
		},
		{
			name:     "missing choices field skipped",
			body:     "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\ndata: {\"choices\":[{\"delta\":{\"content\":\"pass\"}}]}\ndata: [DONE]\n",
			wantText: "pass",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stream := openaiapi.NewStream(io.NopCloser(strings.NewReader(tc.body)))
			defer stream.Close()

			chunk, err := stream.Next()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			c, ok := chunk.(*content.TextChunk)
			if !ok {
				t.Fatalf("expected *content.TextChunk, got %T", chunk)
			}
			if c.Text != tc.wantText {
				t.Errorf("got text %q, want %q", c.Text, tc.wantText)
			}
		})
	}
}
