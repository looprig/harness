// pkg/llm/codec/sse/sse_test.go
package sse_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm/codec/sse"
)

// errorReader returns an error after emitting a fixed prefix.
type errorReader struct {
	prefix []byte
	pos    int
	err    error
}

func (e *errorReader) Read(p []byte) (int, error) {
	if e.pos >= len(e.prefix) {
		return 0, e.err
	}
	n := copy(p, e.prefix[e.pos:])
	e.pos += n
	return n, nil
}

// collectNext drives r.Next() until error, returning payloads and the terminal error.
func collectNext(r *sse.Reader) ([]string, error) {
	var payloads []string
	for {
		p, err := r.Next()
		if err != nil {
			return payloads, err
		}
		payloads = append(payloads, p)
	}
}

func TestReader_Next(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		body     string
		wantData []string
		wantErr  error
	}{
		{
			name:     "happy path two data lines then DONE",
			body:     "data: {\"a\":1}\n\ndata: {\"b\":2}\n\ndata: [DONE]\n\n",
			wantData: []string{`{"a":1}`, `{"b":2}`},
			wantErr:  io.EOF,
		},
		{
			name:     "single data line then DONE",
			body:     "data: {\"x\":0}\n\ndata: [DONE]\n\n",
			wantData: []string{`{"x":0}`},
			wantErr:  io.EOF,
		},
		{
			name:     "empty stream",
			body:     "",
			wantData: nil,
			wantErr:  io.EOF,
		},
		{
			name:     "DONE immediately no payload",
			body:     "data: [DONE]\n\n",
			wantData: nil,
			wantErr:  io.EOF,
		},
		{
			name:     "non-data lines skipped only data yielded",
			body:     ": comment\n\nevent: start\n\ndata: {\"ok\":true}\n\nid: 1\n\ndata: [DONE]\n\n",
			wantData: []string{`{"ok":true}`},
			wantErr:  io.EOF,
		},
		{
			name:     "first DONE ends stream ignores remainder",
			body:     "data: {\"first\":1}\n\ndata: [DONE]\n\ndata: {\"second\":2}\n\n",
			wantData: []string{`{"first":1}`},
			wantErr:  io.EOF,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := sse.NewReader(strings.NewReader(tc.body))
			got, err := collectNext(r)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("terminal error: got %v, want %v", err, tc.wantErr)
			}
			if len(got) != len(tc.wantData) {
				t.Fatalf("payload count: got %d %v, want %d %v", len(got), got, len(tc.wantData), tc.wantData)
			}
			for i := range got {
				if got[i] != tc.wantData[i] {
					t.Errorf("payload[%d]: got %q, want %q", i, got[i], tc.wantData[i])
				}
			}
		})
	}
}

func TestReader_IgnoresNonDataLines(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		body     string
		wantData []string
	}{
		{
			name:     "keep-alive comment skipped",
			body:     ": keep-alive\n\ndata: {\"p\":1}\n\ndata: [DONE]\n\n",
			wantData: []string{`{"p":1}`},
		},
		{
			name:     "empty lines between events skipped",
			body:     "\n\ndata: {\"p\":2}\n\n\n\ndata: [DONE]\n\n",
			wantData: []string{`{"p":2}`},
		},
		{
			name:     "event lines skipped",
			body:     "event: completion\n\ndata: {\"p\":3}\n\ndata: [DONE]\n\n",
			wantData: []string{`{"p":3}`},
		},
		{
			name:     "id lines skipped",
			body:     "id: 42\n\ndata: {\"p\":4}\n\ndata: [DONE]\n\n",
			wantData: []string{`{"p":4}`},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := sse.NewReader(strings.NewReader(tc.body))
			got, err := collectNext(r)
			if !errors.Is(err, io.EOF) {
				t.Fatalf("unexpected terminal error: %v", err)
			}
			if len(got) != len(tc.wantData) {
				t.Fatalf("payload count: got %d %v, want %d %v", len(got), got, len(tc.wantData), tc.wantData)
			}
			for i := range got {
				if got[i] != tc.wantData[i] {
					t.Errorf("payload[%d]: got %q, want %q", i, got[i], tc.wantData[i])
				}
			}
		})
	}
}

func TestReader_ScannerError(t *testing.T) {
	t.Parallel()

	errConnectionReset := errors.New("connection reset")
	errNetworkError := errors.New("network error")

	cases := []struct {
		name    string
		prefix  string
		readErr error
		wantErr error
	}{
		{
			name:    "error after partial data line",
			prefix:  "data: {\"partial",
			readErr: errConnectionReset,
			wantErr: errConnectionReset,
		},
		{
			name:    "error on first read",
			prefix:  "",
			readErr: errNetworkError,
			wantErr: errNetworkError,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			er := &errorReader{prefix: []byte(tc.prefix), err: tc.readErr}
			r := sse.NewReader(er)
			_, gotErr := collectNext(r)
			if gotErr == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(gotErr, tc.wantErr) {
				t.Errorf("expected error %v, got %v", tc.wantErr, gotErr)
			}
		})
	}
}

func TestNewReader(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{
			name: "non-nil reader returns usable Reader",
			body: "data: {}\n\ndata: [DONE]\n\n",
		},
		{
			name: "empty body reader construction does not panic",
			body: "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := sse.NewReader(strings.NewReader(tc.body))
			if r == nil {
				t.Fatal("NewReader returned nil")
			}
			_, err := r.Next()
			if err == nil && tc.body == "" {
				t.Fatal("empty body: expected error from Next, got nil")
			}
		})
	}
}

func FuzzReader(f *testing.F) {
	f.Add("data: {\"choices\":[]}\n\ndata: [DONE]\n\n")
	f.Add("data: [DONE]\n\n")
	f.Add("")
	f.Add(": keep-alive\n\n")
	f.Fuzz(func(t *testing.T, body string) {
		r := sse.NewReader(strings.NewReader(body))
		for {
			_, err := r.Next()
			if err != nil {
				return
			}
		}
	})
}
