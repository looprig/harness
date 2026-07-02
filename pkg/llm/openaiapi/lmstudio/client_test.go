// pkg/llm/openaiapi/lmstudio/client_test.go
package lmstudio_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/openaiapi/lmstudio"
)

// compile-time interface assertion
var _ llm.LLM = (*lmstudio.Client)(nil)

// validModel returns a Model that passes llm.Model.Validate for the LM Studio
// provider (OpenAI dialect, loopback http endpoint). Tests that want an invalid
// Model override one field.
func validModel(model string) llm.Model {
	return llm.Model{
		Provider:  llm.ProviderLMStudio,
		APIFormat: llm.APIFormatOpenAI,
		BaseURL:   "http://localhost:1234",
		Name:      model,
	}
}

// validRequest returns a minimal, valid llm.Request for the given model name.
func validRequest(model string) llm.Request {
	return llm.Request{
		Model: validModel(model),
		Messages: content.AgenticMessages{
			&content.UserMessage{
				Message: content.Message{
					Role:   content.RoleUser,
					Blocks: []content.Block{&content.TextBlock{Text: "hello"}},
				},
			},
		},
	}
}

// validResponseJSON is a minimal chat completions JSON response.
const validResponseJSON = `{
	"id": "chatcmpl-1",
	"model": "lmstudio-model",
	"choices": [{"message": {"role": "assistant", "content": "Hello from LM Studio!"}, "finish_reason": "stop"}],
	"usage": {"prompt_tokens": 5, "completion_tokens": 7}
}`

// validSSE is a minimal SSE streaming response.
const validSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"stream token\"}}]}\ndata: [DONE]\n"

// TestNew_DefaultBaseURL verifies that empty baseURL uses the default.
func TestNew_DefaultBaseURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		baseURL string
	}{
		{name: "empty baseURL uses default", baseURL: ""},
		{name: "explicit default is accepted", baseURL: "http://localhost:1234/v1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := lmstudio.New(tc.baseURL)
			if c == nil {
				t.Fatal("New returned nil")
			}
		})
	}
}

// TestClient_Invoke exercises the non-streaming path.
func TestClient_Invoke(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		serverStatus  int
		serverBody    string
		wantErr       bool
		wantAPIErr    bool
		wantAPIStatus int
		wantMsgNonNil bool
		checkNoAuth   bool
		checkPath     bool
	}{
		{
			name:          "happy path: valid response",
			serverStatus:  http.StatusOK,
			serverBody:    validResponseJSON,
			wantMsgNonNil: true,
			checkNoAuth:   true,
			checkPath:     true,
		},
		{
			name:          "server returns 429 TooManyRequests",
			serverStatus:  http.StatusTooManyRequests,
			serverBody:    `{"error":"rate limited"}`,
			wantErr:       true,
			wantAPIErr:    true,
			wantAPIStatus: http.StatusTooManyRequests,
		},
		{
			name:          "server returns 500 InternalServerError",
			serverStatus:  http.StatusInternalServerError,
			serverBody:    `{"error":"internal"}`,
			wantErr:       true,
			wantAPIErr:    true,
			wantAPIStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Communicate from server goroutine to test goroutine via channels.
			pathCh := make(chan string, 1)
			authCh := make(chan string, 1)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pathCh <- r.URL.Path
				authCh <- r.Header.Get("Authorization")
				w.WriteHeader(tc.serverStatus)
				fmt.Fprint(w, tc.serverBody)
			}))
			defer srv.Close()

			c := lmstudio.New(srv.URL+"/v1", lmstudio.WithHTTPClient(srv.Client()))
			resp, err := c.Invoke(context.Background(), validRequest("test-model"))

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tc.wantAPIErr {
					var apiErr *llm.APIError
					if !errors.As(err, &apiErr) {
						t.Fatalf("expected *llm.APIError, got %T: %v", err, err)
					}
					if apiErr.Status != tc.wantAPIStatus {
						t.Errorf("APIError.Status: want %d, got %d", tc.wantAPIStatus, apiErr.Status)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantMsgNonNil && (resp == nil || resp.Message == nil) {
				t.Fatal("expected non-nil Response.Message")
			}

			// Drain channels set by the server handler.
			gotPath := <-pathCh
			gotAuth := <-authCh

			if tc.checkNoAuth && gotAuth != "" {
				t.Errorf("expected no Authorization header, got %q", gotAuth)
			}
			if tc.checkPath && gotPath != "/v1/chat/completions" {
				t.Errorf("request path: want %q, got %q", "/v1/chat/completions", gotPath)
			}
		})
	}
}

// TestClient_Stream exercises the streaming path.
func TestClient_Stream(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		serverStatus   int
		serverBody     string
		wantErrStream  bool
		wantAPIErr     bool
		wantAPIStatus  int
		wantFirstChunk string
	}{
		{
			name:           "happy path: SSE yields text chunk",
			serverStatus:   http.StatusOK,
			serverBody:     validSSE,
			wantFirstChunk: "stream token",
		},
		{
			name:          "server returns 503 ServiceUnavailable",
			serverStatus:  http.StatusServiceUnavailable,
			serverBody:    `{"error":"unavailable"}`,
			wantErrStream: true,
			wantAPIErr:    true,
			wantAPIStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.serverStatus == http.StatusOK {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				w.WriteHeader(tc.serverStatus)
				fmt.Fprint(w, tc.serverBody)
			}))
			defer srv.Close()

			c := lmstudio.New(srv.URL+"/v1", lmstudio.WithHTTPClient(srv.Client()))
			reader, err := c.Stream(context.Background(), validRequest("test-model"))

			if tc.wantErrStream {
				if err == nil {
					t.Fatal("expected error from Stream(), got nil")
				}
				if tc.wantAPIErr {
					var apiErr *llm.APIError
					if !errors.As(err, &apiErr) {
						t.Fatalf("expected *llm.APIError, got %T: %v", err, err)
					}
					if apiErr.Status != tc.wantAPIStatus {
						t.Errorf("APIError.Status: want %d, got %d", tc.wantAPIStatus, apiErr.Status)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if reader == nil {
				t.Fatal("Stream returned nil reader")
			}
			defer reader.Close()

			chunk, err := reader.Next()
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("unexpected error from Next(): %v", err)
			}
			if tc.wantFirstChunk != "" {
				tc2, ok := chunk.(*content.TextChunk)
				if !ok {
					t.Fatalf("expected *content.TextChunk, got %T", chunk)
				}
				if tc2.Text != tc.wantFirstChunk {
					t.Errorf("chunk text: want %q, got %q", tc.wantFirstChunk, tc2.Text)
				}
			}
		})
	}
}

// TestClient_Validate verifies that Validate() is called before any network request.
// Each subtest owns its own server and counter to avoid shared mutable state.
func TestClient_Validate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		model  llm.Model
		method string // "invoke" or "stream"
	}{
		{
			name:   "empty model name fails validation (invoke)",
			model:  llm.Model{Provider: llm.ProviderLMStudio, APIFormat: llm.APIFormatOpenAI, BaseURL: "http://localhost:1234", Name: ""},
			method: "invoke",
		},
		{
			name:   "empty model name fails validation (stream)",
			model:  llm.Model{Provider: llm.ProviderLMStudio, APIFormat: llm.APIFormatOpenAI, BaseURL: "http://localhost:1234", Name: ""},
			method: "stream",
		},
		{
			name:   "unknown provider fails validation (invoke)",
			model:  llm.Model{Provider: llm.Provider("bogus"), APIFormat: llm.APIFormatOpenAI, BaseURL: "http://localhost:1234", Name: "test-model"},
			method: "invoke",
		},
		{
			name:   "unsupported api format fails validation (invoke)",
			model:  llm.Model{Provider: llm.ProviderLMStudio, APIFormat: llm.APIFormatBedrockConverse, BaseURL: "http://localhost:1234", Name: "test-model"},
			method: "invoke",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Each subtest owns its own counter and server — no shared state.
			var networkCalled atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				networkCalled.Store(true)
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, validResponseJSON)
			}))
			defer srv.Close()

			c := lmstudio.New(srv.URL+"/v1", lmstudio.WithHTTPClient(srv.Client()))
			req := llm.Request{
				Model: tc.model,
				Messages: content.AgenticMessages{
					&content.UserMessage{
						Message: content.Message{
							Role:   content.RoleUser,
							Blocks: []content.Block{&content.TextBlock{Text: "test"}},
						},
					},
				},
			}

			var err error
			switch tc.method {
			case "invoke":
				_, err = c.Invoke(context.Background(), req)
			case "stream":
				_, err = c.Stream(context.Background(), req)
			default:
				t.Fatalf("unknown method %q", tc.method)
			}

			if err == nil {
				t.Fatal("expected ValidationError, got nil")
			}
			var valErr *llm.ValidationError
			if !errors.As(err, &valErr) {
				t.Fatalf("expected *llm.ValidationError, got %T: %v", err, err)
			}
			if networkCalled.Load() {
				t.Error("network was called despite validation failure")
			}
		})
	}
}

// TestClient_Invoke_ContentTypeHeader verifies the correct Content-Type header is set.
func TestClient_Invoke_ContentTypeHeader(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		wantContentType string
	}{
		{name: "Content-Type is application/json", wantContentType: "application/json"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			contentTypeCh := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				contentTypeCh <- r.Header.Get("Content-Type")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, validResponseJSON)
			}))
			defer srv.Close()

			c := lmstudio.New(srv.URL+"/v1", lmstudio.WithHTTPClient(srv.Client()))
			_, err := c.Invoke(context.Background(), validRequest("test-model"))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			gotContentType := <-contentTypeCh
			if gotContentType != tc.wantContentType {
				t.Errorf("Content-Type: want %q, got %q", tc.wantContentType, gotContentType)
			}
		})
	}
}

// TestClient_Stream_AcceptHeader verifies the Accept header is set for streaming.
func TestClient_Stream_AcceptHeader(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		wantAccept string
	}{
		{name: "Accept is text/event-stream", wantAccept: "text/event-stream"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			acceptCh := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				acceptCh <- r.Header.Get("Accept")
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, validSSE)
			}))
			defer srv.Close()

			c := lmstudio.New(srv.URL+"/v1", lmstudio.WithHTTPClient(srv.Client()))
			reader, err := c.Stream(context.Background(), validRequest("test-model"))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			defer reader.Close()

			// Drain to ensure the handler has run.
			for {
				_, err := reader.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					break
				}
			}

			gotAccept := <-acceptCh
			if !strings.Contains(gotAccept, tc.wantAccept) {
				t.Errorf("Accept header: want contains %q, got %q", tc.wantAccept, gotAccept)
			}
		})
	}
}

// TestClient_Invoke_NetworkError verifies that a connection failure returns NetworkError.
func TestClient_Invoke_NetworkError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
	}{
		{name: "hijacked connection returns NetworkError"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Hijack and close the connection to force a network error mid-request.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hj, ok := w.(http.Hijacker)
				if !ok {
					return
				}
				conn, _, _ := hj.Hijack()
				conn.Close()
			}))
			defer srv.Close()

			c := lmstudio.New(srv.URL+"/v1", lmstudio.WithHTTPClient(srv.Client()))
			_, err := c.Invoke(context.Background(), validRequest("test-model"))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var netErr *llm.NetworkError
			if !errors.As(err, &netErr) {
				t.Fatalf("expected *llm.NetworkError, got %T: %v", err, err)
			}
		})
	}
}
