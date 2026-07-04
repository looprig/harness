package transport_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/harness/pkg/llm/auth"
	"github.com/looprig/harness/pkg/llm/codec/openaiapi"
	"github.com/looprig/harness/pkg/llm/transport"
)

// Client must satisfy the llm.LLM contract.
var _ llm.LLM = (*transport.Client)(nil)

const validResponseJSON = `{
	"id": "chatcmpl-1",
	"model": "test-model",
	"choices": [{"message": {"role": "assistant", "content": "Hello!"}, "finish_reason": "stop"}],
	"usage": {"prompt_tokens": 5, "completion_tokens": 7}
}`

// boundModel returns a Model that both passes Validate and matches an Endpoint
// bound to baseURL (loopback http, OpenAI dialect, LM Studio provider).
func boundModel(baseURL string) llm.Model {
	return llm.Model{
		Provider:  llm.ProviderLMStudio,
		APIFormat: llm.APIFormatOpenAI,
		BaseURL:   baseURL,
		Name:      "test-model",
	}
}

func boundEndpoint(baseURL string) transport.Endpoint {
	return transport.Endpoint{Provider: llm.ProviderLMStudio, BaseURL: baseURL}
}

func validRequest(m llm.Model) llm.Request {
	return llm.Request{
		Model: m,
		Messages: content.AgenticMessages{
			&content.UserMessage{Message: content.Message{
				Role:   content.RoleUser,
				Blocks: []content.Block{&content.TextBlock{Text: "hello"}},
			}},
		},
	}
}

// TestNew_AuthContract locks the "auth is required; auth.None() is the explicit
// no-credentials value" contract: New panics on a nil Authenticator and does not
// panic when given auth.None().
func TestNew_AuthContract(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		auth      llm.Authenticator
		wantPanic bool
	}{
		{name: "nil auth panics", auth: nil, wantPanic: true},
		{name: "auth.None() does not panic", auth: auth.None(), wantPanic: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				r := recover()
				if (r != nil) != tc.wantPanic {
					t.Fatalf("New panic = %v, wantPanic %v", r, tc.wantPanic)
				}
			}()
			c := transport.New(openaiapi.Codec{}, boundEndpoint("https://example.com"), tc.auth)
			if !tc.wantPanic && c == nil {
				t.Fatal("New returned nil for a valid authenticator")
			}
		})
	}
}

// TestClient_Invoke covers the non-streaming path: the happy path under both
// no-auth and Bearer authenticators (asserting the header that reached the
// server), plus non-2xx and transport failures.
func TestClient_Invoke(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		authFn       func() llm.Authenticator
		serverStatus int
		serverBody   string
		hijack       bool // close the connection to force a transport error
		wantAuth     string
		wantMsg      bool
		wantAPIErr   bool
		wantAPICode  int
		wantNetErr   bool
	}{
		{
			name:         "happy path with no auth sends no Authorization header",
			authFn:       auth.None,
			serverStatus: http.StatusOK,
			serverBody:   validResponseJSON,
			wantAuth:     "",
			wantMsg:      true,
		},
		{
			name:         "happy path with Bearer key sends Authorization header",
			authFn:       func() llm.Authenticator { return auth.Key("secret-key") },
			serverStatus: http.StatusOK,
			serverBody:   validResponseJSON,
			wantAuth:     "Bearer secret-key",
			wantMsg:      true,
		},
		{
			name:         "non-2xx maps to APIError",
			authFn:       auth.None,
			serverStatus: http.StatusTooManyRequests,
			serverBody:   `{"error":"rate limited"}`,
			wantAPIErr:   true,
			wantAPICode:  http.StatusTooManyRequests,
		},
		{
			name:       "transport failure maps to NetworkError",
			authFn:     auth.None,
			hijack:     true,
			wantNetErr: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			authCh := make(chan string, 1)
			pathCh := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.hijack {
					hj, ok := w.(http.Hijacker)
					if !ok {
						return
					}
					conn, _, _ := hj.Hijack()
					conn.Close()
					return
				}
				authCh <- r.Header.Get("Authorization")
				pathCh <- r.URL.Path
				w.WriteHeader(tc.serverStatus)
				fmt.Fprint(w, tc.serverBody)
			}))
			defer srv.Close()

			c := transport.New(openaiapi.Codec{}, boundEndpoint(srv.URL), tc.authFn())

			resp, err := c.Invoke(context.Background(), validRequest(boundModel(srv.URL)))

			switch {
			case tc.wantAPIErr:
				var apiErr *llm.APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("expected *llm.APIError, got %T: %v", err, err)
				}
				if apiErr.Status != tc.wantAPICode {
					t.Errorf("APIError.Status = %d, want %d", apiErr.Status, tc.wantAPICode)
				}
			case tc.wantNetErr:
				var netErr *llm.NetworkError
				if !errors.As(err, &netErr) {
					t.Fatalf("expected *llm.NetworkError, got %T: %v", err, err)
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if tc.wantMsg && (resp == nil || resp.Message == nil) {
					t.Fatal("expected non-nil Response.Message")
				}
				if gotAuth := <-authCh; gotAuth != tc.wantAuth {
					t.Errorf("Authorization header = %q, want %q", gotAuth, tc.wantAuth)
				}
				if gotPath := <-pathCh; gotPath != "/chat/completions" {
					t.Errorf("request path = %q, want %q", gotPath, "/chat/completions")
				}
			}
		})
	}
}

// TestClient_ModelMismatch verifies the pre-I/O binding guard: a request whose
// model names a different provider or endpoint than the bound Endpoint is
// rejected with *llm.ModelMismatchError and no network call is made.
func TestClient_ModelMismatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(m *llm.Model)
		method string
	}{
		{
			name:   "provider differs (invoke)",
			mutate: func(m *llm.Model) { m.Provider = llm.ProviderChutes },
			method: "invoke",
		},
		{
			name:   "baseURL differs (invoke)",
			mutate: func(m *llm.Model) { m.BaseURL = "https://elsewhere.example.com" },
			method: "invoke",
		},
		{
			name:   "provider differs (stream)",
			mutate: func(m *llm.Model) { m.Provider = llm.ProviderChutes },
			method: "stream",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var networkCalled atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				networkCalled.Store(true)
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, validResponseJSON)
			}))
			defer srv.Close()

			c := transport.New(openaiapi.Codec{}, boundEndpoint(srv.URL), auth.None())

			m := boundModel(srv.URL)
			tc.mutate(&m)
			req := validRequest(m)

			var err error
			switch tc.method {
			case "invoke":
				_, err = c.Invoke(context.Background(), req)
			case "stream":
				_, err = c.Stream(context.Background(), req)
			}

			var mm *llm.ModelMismatchError
			if !errors.As(err, &mm) {
				t.Fatalf("expected *llm.ModelMismatchError, got %T: %v", err, err)
			}
			if networkCalled.Load() {
				t.Error("network was called despite model mismatch")
			}
		})
	}
}

// TestClient_Validate verifies Validate is enforced after the binding guard but
// before any network call.
func TestClient_Validate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(m *llm.Model)
		method string
	}{
		{
			name:   "empty name fails validation (invoke)",
			mutate: func(m *llm.Model) { m.Name = "" },
			method: "invoke",
		},
		{
			name:   "unsupported api format fails validation (stream)",
			mutate: func(m *llm.Model) { m.APIFormat = llm.APIFormatBedrockConverse },
			method: "stream",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var networkCalled atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				networkCalled.Store(true)
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, validResponseJSON)
			}))
			defer srv.Close()

			c := transport.New(openaiapi.Codec{}, boundEndpoint(srv.URL), auth.None())

			// The mutated field must keep provider+baseURL matching the Endpoint so
			// the binding guard passes and Validate is what rejects the request.
			m := boundModel(srv.URL)
			tc.mutate(&m)
			req := validRequest(m)

			var err error
			switch tc.method {
			case "invoke":
				_, err = c.Invoke(context.Background(), req)
			case "stream":
				_, err = c.Stream(context.Background(), req)
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

// TestClient_Stream covers the streaming path: an SSE server yielding several
// events plus [DONE] decodes to the expected chunk sequence (including a
// multi-tool-call event fanned out one chunk per read), and a non-2xx status
// before streaming maps to *llm.APIError.
func TestClient_Stream(t *testing.T) {
	t.Parallel()

	const sseBody = "data: {\"choices\":[{\"delta\":{\"content\":\"foo\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"bar\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}},{\"index\":1,\"id\":\"b\",\"function\":{\"name\":\"g\",\"arguments\":\"{}\"}}]}}]}\n" +
		"data: [DONE]\n"

	cases := []struct {
		name         string
		serverStatus int
		serverBody   string
		wantChunks   []string // "text:foo", "tool:name|args"
		wantAccept   string
		wantAPIErr   bool
		wantAPICode  int
	}{
		{
			name:         "happy path decodes text and fanned-out tool chunks",
			serverStatus: http.StatusOK,
			serverBody:   sseBody,
			wantChunks:   []string{"text:foo", "text:bar", "tool:f|{}", "tool:g|{}"},
			wantAccept:   "text/event-stream",
		},
		{
			name:         "non-2xx before streaming maps to APIError",
			serverStatus: http.StatusServiceUnavailable,
			serverBody:   `{"error":"unavailable"}`,
			wantAPIErr:   true,
			wantAPICode:  http.StatusServiceUnavailable,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			acceptCh := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				acceptCh <- r.Header.Get("Accept")
				if tc.serverStatus == http.StatusOK {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				w.WriteHeader(tc.serverStatus)
				fmt.Fprint(w, tc.serverBody)
			}))
			defer srv.Close()

			c := transport.New(openaiapi.Codec{}, boundEndpoint(srv.URL), auth.None())

			reader, err := c.Stream(context.Background(), validRequest(boundModel(srv.URL)))

			if tc.wantAPIErr {
				var apiErr *llm.APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("expected *llm.APIError, got %T: %v", err, err)
				}
				if apiErr.Status != tc.wantAPICode {
					t.Errorf("APIError.Status = %d, want %d", apiErr.Status, tc.wantAPICode)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			defer reader.Close()

			var got []string
			for {
				chunk, err := reader.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("unexpected Next() error: %v", err)
				}
				switch ch := chunk.(type) {
				case *content.TextChunk:
					got = append(got, "text:"+ch.Text)
				case *content.ToolUseChunk:
					got = append(got, "tool:"+ch.Name+"|"+ch.InputJSON)
				default:
					t.Fatalf("unexpected chunk type %T", chunk)
				}
			}

			if len(got) != len(tc.wantChunks) {
				t.Fatalf("got %d chunks %v, want %d %v", len(got), got, len(tc.wantChunks), tc.wantChunks)
			}
			for i := range tc.wantChunks {
				if got[i] != tc.wantChunks[i] {
					t.Errorf("chunk[%d] = %q, want %q", i, got[i], tc.wantChunks[i])
				}
			}

			if gotAccept := <-acceptCh; gotAccept != tc.wantAccept {
				t.Errorf("Accept header = %q, want %q", gotAccept, tc.wantAccept)
			}
		})
	}
}
