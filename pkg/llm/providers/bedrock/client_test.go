package bedrock_test

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

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/harness/pkg/llm/auth"
	"github.com/looprig/harness/pkg/llm/providers/bedrock"
)

// Client must satisfy the llm.LLM contract.
var _ llm.LLM = (*bedrock.Client)(nil)

// anthropicResponseJSON is a minimal valid non-streaming Anthropic Messages
// response the anthropicapi codec decodes into a one-text-block AIMessage.
const anthropicResponseJSON = `{
	"id": "msg_1",
	"type": "message",
	"role": "assistant",
	"model": "anthropic.claude-3-5-sonnet-20241022-v2:0",
	"content": [{"type": "text", "text": "Hi there"}],
	"usage": {"input_tokens": 5, "output_tokens": 3}
}`

// capturedRequest records the wire-level facts a Bedrock request must carry.
type capturedRequest struct {
	method      string
	escapedPath string
	authz       string
	amzDate     string
	contentType string
	accept      string
	body        []byte
}

// bodyCaptureServer returns a server that always answers 200 with a valid
// Anthropic response and pushes the raw request body to the returned channel. Used
// by the body-transform tests.
func bodyCaptureServer(t *testing.T) (*httptest.Server, chan []byte) {
	t.Helper()
	bodyCh := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := readAll(t, r)
		bodyCh <- b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, anthropicResponseJSON)
	}))
	return srv, bodyCh
}

func readAll(t *testing.T, r *http.Request) []byte {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	return b
}

// TestBedrockInvoke covers the happy Invoke path end-to-end against an
// httptest.Server: the request is SigV4-signed for the bedrock service, routed to
// /model/<model-id>/invoke, carries JSON content/accept headers, and a 200
// Anthropic response decodes to a message.
func TestBedrockInvoke(t *testing.T) {
	t.Parallel()

	const modelID = "anthropic.claude-3-5-sonnet-20241022-v2:0"

	capCh := make(chan capturedRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capCh <- capturedRequest{
			method:      r.Method,
			escapedPath: r.URL.EscapedPath(),
			authz:       r.Header.Get("Authorization"),
			amzDate:     r.Header.Get("X-Amz-Date"),
			contentType: r.Header.Get("Content-Type"),
			accept:      r.Header.Get("Accept"),
			body:        readAll(t, r),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, anthropicResponseJSON)
	}))
	defer srv.Close()

	c := bedrock.NewWithEndpoint(testCreds(), "us-east-1", srv.URL)
	resp, err := c.Invoke(context.Background(), bedrockRequest(modelID))
	if err != nil {
		t.Fatalf("Invoke() err = %v, want nil", err)
	}
	if resp == nil || resp.Message == nil || len(resp.Message.Blocks) != 1 {
		t.Fatalf("Invoke() resp = %+v, want a one-block message", resp)
	}
	if tb, ok := resp.Message.Blocks[0].(*content.TextBlock); !ok || tb.Text != "Hi there" {
		t.Errorf("decoded block = %+v, want TextBlock{Hi there}", resp.Message.Blocks[0])
	}

	got := <-capCh
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	// ":" stays literal on the wire (url.PathEscape leaves it); the signer encodes
	// it into the canonical URI, not the request line.
	wantPath := "/model/" + modelID + "/invoke"
	if got.escapedPath != wantPath {
		t.Errorf("path = %q, want %q", got.escapedPath, wantPath)
	}
	if !strings.HasPrefix(got.authz, "AWS4-HMAC-SHA256 ") {
		t.Errorf("Authorization = %q, want an AWS4-HMAC-SHA256 signature", got.authz)
	}
	if !strings.Contains(got.authz, "/us-east-1/bedrock/aws4_request") {
		t.Errorf("Authorization credential scope = %q, want .../us-east-1/bedrock/aws4_request", got.authz)
	}
	if got.amzDate == "" {
		t.Error("X-Amz-Date header missing")
	}
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got.contentType)
	}
	if got.accept != "application/json" {
		t.Errorf("Accept = %q, want application/json", got.accept)
	}
}

// TestBedrockInvokeErrors covers the mapped failure modes: a non-2xx status maps
// to *llm.APIError (preserving status + body) and a transport failure (server
// closes the connection) maps to *llm.NetworkError.
func TestBedrockInvokeErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		serverStatus int
		serverBody   string
		hijack       bool
		wantAPIErr   bool
		wantAPICode  int
		wantNetErr   bool
	}{
		{
			name:         "non-2xx maps to APIError",
			serverStatus: http.StatusForbidden,
			serverBody:   `{"message":"not authorized"}`,
			wantAPIErr:   true,
			wantAPICode:  http.StatusForbidden,
		},
		{
			name:         "500 maps to APIError",
			serverStatus: http.StatusInternalServerError,
			serverBody:   `{"message":"boom"}`,
			wantAPIErr:   true,
			wantAPICode:  http.StatusInternalServerError,
		},
		{
			name:       "transport failure maps to NetworkError",
			hijack:     true,
			wantNetErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.hijack {
					hj, ok := w.(http.Hijacker)
					if !ok {
						return
					}
					conn, _, _ := hj.Hijack()
					conn.Close()
					return
				}
				w.WriteHeader(tt.serverStatus)
				fmt.Fprint(w, tt.serverBody)
			}))
			defer srv.Close()

			c := bedrock.NewWithEndpoint(testCreds(), "us-east-1", srv.URL)
			_, err := c.Invoke(context.Background(), bedrockRequest("anthropic.claude-3-5-sonnet-20241022-v2:0"))

			switch {
			case tt.wantAPIErr:
				var apiErr *llm.APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("err = %T, want *llm.APIError", err)
				}
				if apiErr.Status != tt.wantAPICode {
					t.Errorf("APIError.Status = %d, want %d", apiErr.Status, tt.wantAPICode)
				}
				if len(apiErr.Body) == 0 {
					t.Error("APIError.Body is empty, want the raw provider payload")
				}
			case tt.wantNetErr:
				var netErr *llm.NetworkError
				if !errors.As(err, &netErr) {
					t.Fatalf("err = %T, want *llm.NetworkError", err)
				}
			}
		})
	}
}

// TestBedrockStreamNotSupported locks the documented deferral: Stream fails closed
// with *StreamingNotSupportedError and opens no connection.
func TestBedrockStreamNotSupported(t *testing.T) {
	t.Parallel()

	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := bedrock.NewWithEndpoint(testCreds(), "us-east-1", srv.URL)
	reader, err := c.Stream(context.Background(), bedrockRequest("anthropic.claude-3-5-sonnet-20241022-v2:0"))
	if reader != nil {
		t.Error("Stream() returned a non-nil reader alongside the not-implemented error")
	}
	var nse *bedrock.StreamingNotSupportedError
	if !errors.As(err, &nse) {
		t.Fatalf("err = %T, want *bedrock.StreamingNotSupportedError", err)
	}
	if called.Load() {
		t.Error("Stream opened a connection despite being unimplemented")
	}
}

// TestBedrockPreIOGuards verifies the ordered fail-closed guards run before any
// network I/O: a non-Bedrock provider is rejected with *llm.ModelMismatchError,
// and an invalid Model (empty name) with *llm.ValidationError. Neither reaches the
// server.
func TestBedrockPreIOGuards(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		mutate             func(*llm.Request)
		wantMM             bool
		wantValErr         bool
		wantUnsupportedFmt bool
	}{
		{
			name:   "wrong provider is a model mismatch",
			mutate: func(r *llm.Request) { r.Model.Provider = llm.ProviderChutes },
			wantMM: true,
		},
		{
			name:       "empty name fails validation",
			mutate:     func(r *llm.Request) { r.Model.Name = "" },
			wantValErr: true,
		},
		{
			// Bedrock Converse passes Validate (supportsAPIFormat admits it) but this
			// client only encodes the Anthropic dialect, so it must fail closed rather
			// than silently Anthropic-encode a Converse request.
			name:               "bedrock-converse format is unsupported (fail closed, not silent)",
			mutate:             func(r *llm.Request) { r.Model.APIFormat = llm.APIFormatBedrockConverse },
			wantUnsupportedFmt: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var called atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called.Store(true)
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, anthropicResponseJSON)
			}))
			defer srv.Close()

			c := bedrock.NewWithEndpoint(testCreds(), "us-east-1", srv.URL)
			req := bedrockRequest("anthropic.claude-3-5-sonnet-20241022-v2:0")
			tt.mutate(&req)

			_, err := c.Invoke(context.Background(), req)

			if tt.wantMM {
				var mm *llm.ModelMismatchError
				if !errors.As(err, &mm) {
					t.Fatalf("err = %T, want *llm.ModelMismatchError", err)
				}
			}
			if tt.wantValErr {
				var ve *llm.ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("err = %T, want *llm.ValidationError", err)
				}
			}
			if tt.wantUnsupportedFmt {
				var uf *bedrock.UnsupportedAPIFormatError
				if !errors.As(err, &uf) {
					t.Fatalf("err = %T, want *bedrock.UnsupportedAPIFormatError", err)
				}
				if uf.APIFormat != llm.APIFormatBedrockConverse {
					t.Errorf("UnsupportedAPIFormatError.APIFormat = %q, want %q", uf.APIFormat, llm.APIFormatBedrockConverse)
				}
			}
			if called.Load() {
				t.Error("network was called despite a pre-I/O guard failure")
			}
		})
	}
}

// TestBedrockNew covers the fail-closed constructor: empty region or empty
// mandatory credentials yield *bedrock.ConfigError and no client; a full config
// yields a non-nil llm.LLM.
func TestBedrockNew(t *testing.T) {
	t.Parallel()

	full := auth.SigV4Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret", SessionToken: "tok"}

	tests := []struct {
		name    string
		creds   auth.SigV4Credentials
		region  string
		wantErr bool
	}{
		{name: "valid config", creds: full, region: "us-east-1"},
		{name: "valid without session token", creds: auth.SigV4Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret"}, region: "us-west-2"},
		{name: "empty region fails closed", creds: full, region: "", wantErr: true},
		{name: "empty access key fails closed", creds: auth.SigV4Credentials{SecretAccessKey: "secret"}, region: "us-east-1", wantErr: true},
		{name: "empty secret key fails closed", creds: auth.SigV4Credentials{AccessKeyID: "AKID"}, region: "us-east-1", wantErr: true},
		{name: "all empty fails closed", creds: auth.SigV4Credentials{}, region: "", wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := bedrock.New(tt.creds, tt.region)
			if (err != nil) != tt.wantErr {
				t.Fatalf("New() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if got != nil {
					t.Fatalf("New() returned non-nil client (%T) alongside an error", got)
				}
				var ce *bedrock.ConfigError
				if !errors.As(err, &ce) {
					t.Fatalf("err = %T, want *bedrock.ConfigError", err)
				}
				return
			}
			if got == nil {
				t.Fatal("New() = nil, want non-nil client")
			}
		})
	}
}
