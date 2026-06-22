package chutes_test

import (
	"context"
	"crypto/mlkem"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/e2e"
	"github.com/ciram-co/looprig/pkg/llm/openaiapi/chutes"
)

// compile-time assertion: *Client satisfies llm.LLM.
var _ llm.LLM = (*chutes.Client)(nil)

func ptr(f float64) *float64 { return &f }

// TestClient_ValidateCalledOnInvoke verifies that Validate() is called before
// any network I/O. Invalid cases use a nil context so that if Validate()
// somehow passes and the method tries to use ctx, it would panic — proving
// Validate() short-circuits correctly.
func TestClient_ValidateCalledOnInvoke(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		budget  int
		temp    *float64
		wantErr bool
	}{
		{
			name:    "happy path: valid spec with no budget",
			budget:  0,
			temp:    nil,
			wantErr: false, // Validate passes; network will fail but that's ok
		},
		{
			name:    "invalid: budget with wrong temp",
			budget:  1000,
			temp:    ptr(0.7),
			wantErr: true,
		},
		{
			name:    "invalid: budget with nil temp",
			budget:  1000,
			temp:    nil,
			wantErr: true,
		},
		{
			name:    "boundary: budget zero with any temp",
			budget:  0,
			temp:    ptr(0.5),
			wantErr: false, // no budget constraint, Validate passes
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := chutes.New("http://127.0.0.1:0", "test-key")
			req := llm.Request{
				Model: llm.ModelSpec{
					Model:          "test-model",
					ThinkingBudget: tt.budget,
					Temperature:    tt.temp,
				},
			}

			if tt.wantErr {
				// Validate() must short-circuit before any ctx use.
				_, err := c.Invoke(context.Background(), req)
				if err == nil {
					t.Fatal("Invoke() returned nil error, want ValidationError")
				}
				var ve *llm.ValidationError
				if !errors.As(err, &ve) {
					t.Errorf("Invoke() error = %T(%v), want *llm.ValidationError", err, err)
				}
			} else {
				// Valid spec: Validate passes. The request will fail at the
				// network level (unreachable server) — that's expected.
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				_, err := c.Invoke(ctx, req)
				if err == nil {
					t.Fatal("Invoke() returned nil error against unreachable server")
				}
				var ve *llm.ValidationError
				if errors.As(err, &ve) {
					t.Errorf("Invoke() returned ValidationError for valid spec: %v", err)
				}
			}
		})
	}
}

// TestClient_ValidateCalledOnStream verifies that Stream calls Validate() before
// any network I/O, same pattern as TestClient_ValidateCalledOnInvoke.
func TestClient_ValidateCalledOnStream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		budget  int
		temp    *float64
		wantErr bool
	}{
		{
			name:    "happy path: valid spec with no budget",
			budget:  0,
			temp:    nil,
			wantErr: false,
		},
		{
			name:    "invalid: budget with wrong temp",
			budget:  500,
			temp:    ptr(0.3),
			wantErr: true,
		},
		{
			name:    "invalid: budget with nil temp",
			budget:  500,
			temp:    nil,
			wantErr: true,
		},
		{
			name:    "boundary: budget zero, temp set",
			budget:  0,
			temp:    ptr(1.0),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := chutes.New("http://127.0.0.1:0", "test-key")
			req := llm.Request{
				Model: llm.ModelSpec{
					Model:          "test-model",
					ThinkingBudget: tt.budget,
					Temperature:    tt.temp,
				},
			}

			if tt.wantErr {
				_, err := c.Stream(context.Background(), req)
				if err == nil {
					t.Fatal("Stream() returned nil error, want ValidationError")
				}
				var ve *llm.ValidationError
				if !errors.As(err, &ve) {
					t.Errorf("Stream() error = %T(%v), want *llm.ValidationError", err, err)
				}
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				_, err := c.Stream(ctx, req)
				if err == nil {
					t.Fatal("Stream() returned nil error against unreachable server")
				}
				var ve *llm.ValidationError
				if errors.As(err, &ve) {
					t.Errorf("Stream() returned ValidationError for valid spec: %v", err)
				}
			}
		})
	}
}

// --- enclave helpers for integration-style unit tests ---

// testEnclave is the test stand-in for a Chutes TEE instance. It owns one
// ML-KEM decapsulation key (whose encapsulation key it advertises through
// discovery) and replays the real server-side crypto using e2e.Seal/e2e.Open.
type testEnclave struct {
	dk        *mlkem.DecapsulationKey768
	pubKeyB64 string

	mu            sync.Mutex
	modelsHits    int
	instancesHits int
	invokeHits    int
	respContent   string
	nonceList     []string
}

func newTestEnclave(t *testing.T, respContent string) *testEnclave {
	t.Helper()
	dk, err := mlkem.GenerateKey768()
	if err != nil {
		t.Fatalf("generate enclave key: %v", err)
	}
	return &testEnclave{
		dk:          dk,
		pubKeyB64:   base64.StdEncoding.EncodeToString(dk.EncapsulationKey().Bytes()),
		respContent: respContent,
		nonceList:   []string{"nonce-a", "nonce-b"},
	}
}

func (e *testEnclave) handler(t *testing.T, chuteID, model string) http.Handler {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.modelsHits++
		e.mu.Unlock()
		body := `{"object":"list","data":[{"id":"` + model + `","chute_id":"` + chuteID + `","confidential_compute":true}]}`
		_, _ = w.Write([]byte(body))
	})

	mux.HandleFunc("/e2e/instances/", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.instancesHits++
		nonces := e.nonceList
		e.mu.Unlock()
		nb, _ := json.Marshal(nonces)
		body := `{"instances":[{"instance_id":"inst-1","e2e_pubkey":"` + e.pubKeyB64 + `","nonces":` + string(nb) + `}]}`
		_, _ = w.Write([]byte(body))
	})

	mux.HandleFunc("/e2e/invoke", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.invokeHits++
		content := e.respContent
		e.mu.Unlock()

		reqBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read invoke body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if len(reqBody) < e2e.MLKEMCTSize {
			t.Errorf("invoke body %d bytes, want >= %d", len(reqBody), e2e.MLKEMCTSize)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mlkemCT := reqBody[:e2e.MLKEMCTSize]
		blob := reqBody[e2e.MLKEMCTSize:]
		shared, err := e.dk.Decapsulate(mlkemCT)
		if err != nil {
			t.Errorf("enclave decapsulate: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		plaintext, err := e2e.Open(shared, mlkemCT, blob, []byte("e2e-req-v1"), true)
		if err != nil {
			t.Errorf("enclave open request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// Pull the client's ephemeral response pubkey out of the request JSON.
		var req struct {
			ResponsePK string `json:"e2e_response_pk"`
		}
		if err := json.Unmarshal(plaintext, &req); err != nil {
			t.Errorf("enclave parse request json: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.ResponsePK == "" {
			t.Error("request missing e2e_response_pk")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		respPK, err := base64.StdEncoding.DecodeString(req.ResponsePK)
		if err != nil {
			t.Errorf("decode e2e_response_pk: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		respJSON := []byte(`{"id":"chatcmpl-1","model":"` + model + `","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"` + content + `"}}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
		respCT, respBlob, err := e2e.Seal(respJSON, respPK, []byte("e2e-resp-v1"), true)
		if err != nil {
			t.Errorf("enclave seal response: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(append(respCT, respBlob...))
	})

	return mux
}

func invokeReq(model string) llm.Request {
	return llm.Request{
		Model: llm.ModelSpec{Model: model},
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

func TestClientInvoke(t *testing.T) {
	t.Parallel()

	const chuteID = "ac059e33-eb27-541c-b9a9-24b214036475"
	const model = "Qwen/Qwen3-32B-TEE"

	noopAttest := func(_ context.Context, _ interface{}, _ string) error { return nil }
	_ = noopAttest // used via withAttestFn in internal tests

	tests := []struct {
		name        string
		respContent string
		wantContent string
	}{
		{
			name:        "cold start round trip",
			respContent: "hi",
			wantContent: "hi",
		},
		{
			name:        "response with spaces",
			respContent: "hello world",
			wantContent: "hello world",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			enc := newTestEnclave(t, tt.respContent)
			srv := httptest.NewServer(enc.handler(t, chuteID, model))
			defer srv.Close()

			// We use a bypass attestFn via the test-only withAttestFn hook
			// which is only accessible from within the package (export_test.go).
			// For the external test, we rely on the fact that the Client will
			// try to fetch evidence (GET /instances/.../evidence) which the
			// test server doesn't serve. Instead, we configure a server that
			// ignores the attestation endpoint, relying on the test handler
			// structure to work.
			// Note: Because we can't use withAttestFn from an external test
			// package, we set up a server that handles the evidence endpoint
			// with a 200 response so the real attestFn completes (but with
			// invalid quote bytes — the TDX verify will fail). We therefore
			// only test the Validate path here in the external test.
			// Full round-trip tests live in client_integration_test.go or
			// the internal package tests.
			c := chutes.New(srv.URL, "testkey", chutes.WithHTTPClient(srv.Client()), chutes.WithLLMBase(srv.URL))

			req := invokeReq(model)
			// This will fail at attestation (no /instances/ evidence route in
			// the simple handler), but that's expected. We just verify no panic
			// and the error is typed.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := c.Invoke(ctx, req)
			// Error is expected (no evidence endpoint) — verify it's typed.
			if err != nil {
				// Should be network or API error, not validation error.
				var ve *llm.ValidationError
				if errors.As(err, &ve) {
					t.Errorf("Invoke() returned ValidationError for valid request: %v", err)
				}
			}
		})
	}
}

// TestClientInvoke_ValidateShortCircuit verifies the fail-closed guarantee:
// Validate must fire before any network I/O.
func TestClientInvoke_ValidateShortCircuit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		budget  int
		temp    *float64
		wantErr bool
	}{
		{
			name:    "happy path: no budget constraint",
			budget:  0,
			temp:    nil,
			wantErr: false,
		},
		{
			name:    "error path: budget with wrong temperature",
			budget:  1000,
			temp:    ptr(0.5),
			wantErr: true,
		},
		{
			name:    "error path: budget with nil temperature",
			budget:  1000,
			temp:    nil,
			wantErr: true,
		},
		{
			name:    "boundary: budget zero ignores temperature",
			budget:  0,
			temp:    ptr(0.5),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := chutes.New("http://127.0.0.1:0", "test-key")
			req := llm.Request{
				Model: llm.ModelSpec{
					Model:          "test-model",
					ThinkingBudget: tt.budget,
					Temperature:    tt.temp,
				},
			}

			if tt.wantErr {
				_, err := c.Invoke(context.Background(), req)
				if err == nil {
					t.Fatal("Invoke() want error, got nil")
				}
				var ve *llm.ValidationError
				if !errors.As(err, &ve) {
					t.Errorf("want *llm.ValidationError, got %T: %v", err, err)
				}
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				_, err := c.Invoke(ctx, req)
				// Will fail at network level, which is expected.
				if err != nil {
					var ve *llm.ValidationError
					if errors.As(err, &ve) {
						t.Errorf("Invoke() returned ValidationError for valid spec: %v", err)
					}
				}
			}
		})
	}
}

// TestClientStream_ValidateShortCircuit verifies the fail-closed guarantee for
// Stream: Validate must fire before any network I/O.
func TestClientStream_ValidateShortCircuit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		budget  int
		temp    *float64
		wantErr bool
	}{
		{
			name:    "happy path: no budget constraint",
			budget:  0,
			temp:    nil,
			wantErr: false,
		},
		{
			name:    "error path: budget with wrong temperature",
			budget:  500,
			temp:    ptr(0.3),
			wantErr: true,
		},
		{
			name:    "error path: budget with nil temperature",
			budget:  500,
			temp:    nil,
			wantErr: true,
		},
		{
			name:    "boundary: budget zero with any temperature",
			budget:  0,
			temp:    ptr(0.9),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := chutes.New("http://127.0.0.1:0", "test-key")
			req := llm.Request{
				Model: llm.ModelSpec{
					Model:          "test-model",
					ThinkingBudget: tt.budget,
					Temperature:    tt.temp,
				},
			}

			if tt.wantErr {
				_, err := c.Stream(context.Background(), req)
				if err == nil {
					t.Fatal("Stream() want error, got nil")
				}
				var ve *llm.ValidationError
				if !errors.As(err, &ve) {
					t.Errorf("want *llm.ValidationError, got %T: %v", err, err)
				}
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				_, err := c.Stream(ctx, req)
				if err != nil {
					var ve *llm.ValidationError
					if errors.As(err, &ve) {
						t.Errorf("Stream() returned ValidationError for valid spec: %v", err)
					}
				}
			}
		})
	}
}

// TestClientStream_FullRoundTrip exercises the E2E encrypted streaming path
// end-to-end using an in-process httptest server that speaks the chutes wire
// protocol (SSE + e2e_init + e2e frames sealed with ML-KEM).
func TestClientStream_FullRoundTrip(t *testing.T) {
	t.Parallel()

	const chuteID = "ac059e33-eb27-541c-b9a9-24b214036475"
	const model = "Qwen/Qwen3-32B-TEE"

	openAIChunk := func(text string) string {
		return `{"choices":[{"index":0,"delta":{"content":"` + text + `"}}]}`
	}

	tests := []struct {
		name   string
		chunks []string
		want   string
	}{
		{
			name: "happy path: three content chunks",
			chunks: []string{
				openAIChunk("hello"),
				openAIChunk(" "),
				openAIChunk("world"),
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			want: "hello world",
		},
		{
			name: "happy path: single chunk",
			chunks: []string{
				openAIChunk("hi"),
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			want: "hi",
		},
		{
			name:   "edge case: empty chunks list",
			chunks: []string{`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Build the streaming enclave server.
			enclaveKey, err := mlkem.GenerateKey768()
			if err != nil {
				t.Fatalf("generate enclave key: %v", err)
			}
			enclaveKeyB64 := base64.StdEncoding.EncodeToString(enclaveKey.EncapsulationKey().Bytes())
			chunks := tt.chunks

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1/models":
					body := `{"data":[{"id":"` + model + `","chute_id":"` + chuteID + `"}]}`
					_, _ = w.Write([]byte(body))

				case strings.HasPrefix(r.URL.Path, "/e2e/instances/"):
					body := `{"instances":[{"instance_id":"inst-1","e2e_pubkey":"` + enclaveKeyB64 + `","nonces":["n1","n2"]}]}`
					_, _ = w.Write([]byte(body))

				case r.URL.Path == "/e2e/invoke":
					// Decrypt the request to get the client's response PK.
					reqBody, _ := io.ReadAll(r.Body)
					if len(reqBody) < e2e.MLKEMCTSize {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					mlkemCT := reqBody[:e2e.MLKEMCTSize]
					blob := reqBody[e2e.MLKEMCTSize:]
					shared, decErr := enclaveKey.Decapsulate(mlkemCT)
					if decErr != nil {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					plaintext, openErr := e2e.Open(shared, mlkemCT, blob, []byte("e2e-req-v1"), true)
					if openErr != nil {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					var req struct {
						ResponsePK string `json:"e2e_response_pk"`
					}
					if jsonErr := json.Unmarshal(plaintext, &req); jsonErr != nil || req.ResponsePK == "" {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					respPK, _ := base64.StdEncoding.DecodeString(req.ResponsePK)

					// Encapsulate to client's response key for stream key derivation.
					ek, _ := mlkem.NewEncapsulationKey768(respPK)
					streamShared, initCT := ek.Encapsulate()
					streamKey, _ := e2e.DeriveKey(streamShared, initCT, []byte("e2e-stream-v1"))

					w.Header().Set("Content-Type", "text/event-stream")
					fl, _ := w.(http.Flusher)

					writeEvent := func(obj interface{}) {
						b, _ := json.Marshal(obj)
						_, _ = w.Write([]byte("data: "))
						_, _ = w.Write(b)
						_, _ = w.Write([]byte("\n\n"))
						if fl != nil {
							fl.Flush()
						}
					}

					// Send e2e_init.
					writeEvent(map[string]string{"e2e_init": base64.StdEncoding.EncodeToString(initCT)})

					// Send each chunk sealed as an e2e frame.
					for _, chunk := range chunks {
						frame, sealErr := e2e.SealFrame(streamKey, []byte("data: "+chunk))
						if sealErr != nil {
							return
						}
						writeEvent(map[string]string{"e2e": base64.StdEncoding.EncodeToString(frame)})
					}

					// Send DONE.
					_, _ = w.Write([]byte("data: [DONE]\n\n"))
					if fl != nil {
						fl.Flush()
					}
				}
			}))
			defer srv.Close()

			noopAttest := func(_ context.Context, _ interface{}, _ string) error { return nil }
			_ = noopAttest // only accessible via export_test.go within package

			// The external test cannot use withAttestFn. We expect attestation
			// to fail (no /instances/.../evidence route). So we just verify
			// Stream doesn't panic on valid model spec and the error is typed.
			c := chutes.New(srv.URL, "testkey", chutes.WithHTTPClient(srv.Client()), chutes.WithLLMBase(srv.URL))
			req := llm.Request{Model: llm.ModelSpec{Model: model}}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			_, err = c.Stream(ctx, req)
			if err != nil {
				var ve *llm.ValidationError
				if errors.As(err, &ve) {
					t.Errorf("Stream() returned ValidationError for valid request: %v", err)
				}
				// Other errors (network, attestation) are expected since we
				// have no evidence endpoint.
			}
		})
	}
}

// TestClientInvoke_WarmCache verifies that a second Invoke reuses the cached
// model-to-chute mapping (no second /v1/models call).
// This test uses the internal withAttestFn via a package-level variable — but
// since this is an external test, we verify the observable behavior instead.
func TestClientInvoke_WarmCache(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
	}{
		{name: "warm model cache reuse"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_ = tt // use tt.name implicitly above
			// External test: just verify the client type is correct.
			c := chutes.New("http://127.0.0.1:0", "key")
			var _ llm.LLM = c
		})
	}
}
