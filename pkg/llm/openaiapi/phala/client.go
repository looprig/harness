package phala

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/llm"
)

// compile-time assertion: *Client satisfies llm.LLM.
var _ llm.LLM = (*Client)(nil)

// Default attestation-service URLs (WIRE.md sections 1 & 7). NRAS/JWKS are
// shared with chutes; tests override via WithNRAS / WithAPIBase.
const (
	defaultAPIBase = "https://api.redpill.ai"
	defaultNRASURL = "https://nras.attestation.nvidia.com/v3/attest/gpu"
	defaultJWKSURL = "https://nras.attestation.nvidia.com/.well-known/jwks.json"
)

// Option configures a Client at construction.
type Option func(*Client)

// WithHTTPClient sets the HTTP client for every request (attestation, chat,
// receipt, NRAS, JWKS). Useful for timeouts, proxies, and httptest.
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.http = hc } }

// WithAPIBase overrides https://api.redpill.ai. Tests use this to route to
// an httptest.Server.
func WithAPIBase(base string) Option { return func(c *Client) { c.apiBase = base } }

// WithNRAS overrides the NRAS verify URL and JWKS URL.
func WithNRAS(nrasURL, jwksURL string) Option {
	return func(c *Client) {
		c.nrasURL = nrasURL
		c.jwksURL = jwksURL
	}
}

// New constructs a phala Client. apiBase empty defaults to the production URL.
// apiKey is the PHALA_API_KEY bearer token. Defaults (an http.Client, the
// NRAS/JWKS URLs, the session cache, and the real attestFn) are applied
// first; options override.
func New(apiBase, apiKey string, opts ...Option) *Client {
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	c := &Client{
		http:     &http.Client{},
		apiBase:  apiBase,
		apiKey:   apiKey,
		nrasURL:  defaultNRASURL,
		jwksURL:  defaultJWKSURL,
		sessions: map[string]*attestedSession{},
	}
	c.attestFn = c.attest
	for _, o := range opts {
		o(c)
	}
	return c
}

// Invoke sends a non-streaming chat completion to /v1/chat/completions.
// It calls req.Model.Validate() before any network I/O — fail closed.
func (c *Client) Invoke(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if err := req.Model.Validate(); err != nil {
		return nil, err
	}
	sess, err := c.attestModel(ctx, req.Model.Model)
	if err != nil {
		return nil, err
	}
	_ = sess // session is verified; no per-response receipt for non-streaming

	body, err := encodeRequest(req, false)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiBase+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	if resp.StatusCode/100 != 2 {
		return nil, &llm.APIError{Status: resp.StatusCode, Body: respBody}
	}
	return decodeResponse(respBody)
}

// Stream sends a streaming chat completion to /v1/chat/completions and
// returns a *llm.StreamReader[content.Chunk] whose Close also performs
// per-response receipt verification (Shapes A/B) or is a no-op (Shape C).
// It calls req.Model.Validate() before any network I/O — fail closed.
//
// The returned reader MUST be Closed by the caller. The TeeReader captures
// every byte the server sends so Close can recompute response_hash and
// extract chat_id without forking openaiapi.
func (c *Client) Stream(ctx context.Context, req llm.Request) (*llm.StreamReader[content.Chunk], error) {
	if err := req.Model.Validate(); err != nil {
		return nil, err
	}
	sess, err := c.attestModel(ctx, req.Model.Model)
	if err != nil {
		return nil, err
	}

	body, err := encodeRequest(req, true)
	if err != nil {
		return nil, err
	}
	requestHashHex := sha256Hex(body)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiBase+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return nil, &llm.APIError{Status: resp.StatusCode, Body: respBody}
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		defer resp.Body.Close()
		return nil, fmt.Errorf("phala: expected text/event-stream, got %q", ct)
	}

	return newCaptureStream(resp.Body, c, sess, req.Model.Model, requestHashHex), nil
}

// evict deletes the cached session for the named model so the next call
// re-attests from scratch.
func (c *Client) evict(model string) {
	c.mu.Lock()
	delete(c.sessions, model)
	c.mu.Unlock()
}

// receipt is the JSON body of GET /v1/signature/{chat_id}. See WIRE.md section 5.
type receipt struct {
	Text           string `json:"text"`
	Signature      string `json:"signature"`
	SigningAddress string `json:"signing_address"`
	SigningAlgo    string `json:"signing_algo"`
}

// fetchReceipt issues GET {apiBase}/v1/signature/{chatID}?model=...&signing_algo=ed25519
// with the Bearer token, and returns the raw response body.
//
// Returns *llm.NetworkError on transport or body-read failure, or
// *llm.APIError on a non-2xx response.
func fetchReceipt(ctx context.Context, hc *http.Client, apiBase, apiKey, chatID, model string) ([]byte, error) {
	u := apiBase + "/v1/signature/" + neturl.PathEscape(chatID) +
		"?model=" + neturl.QueryEscape(model) +
		"&signing_algo=ed25519"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := hc.Do(req)
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	if resp.StatusCode/100 != 2 {
		return nil, &llm.APIError{Status: resp.StatusCode, Body: body}
	}
	return body, nil
}

// verifyReceipt parses the /v1/signature/{id} response body and verifies it
// against the attested signing address. Hard-fails on algo downgrade — we
// request ed25519 to stay on stdlib.
func verifyReceipt(body []byte, attestedAddr []byte, model, requestHashHex, responseHashHex string) error {
	var r receipt
	if err := json.Unmarshal(body, &r); err != nil {
		return &llm.AttestationError{Reason: string(ReasonReceiptInvalid), Err: fmt.Errorf("decode receipt body: %w", err)}
	}
	if r.SigningAlgo != "ed25519" {
		return &llm.AttestationError{
			Reason: string(ReasonUnsupportedSigAlgo),
			Err:    fmt.Errorf("got signing_algo=%q; want ed25519", r.SigningAlgo),
		}
	}
	addr, err := hex.DecodeString(strings.TrimPrefix(r.SigningAddress, "0x"))
	if err != nil {
		return &llm.AttestationError{Reason: string(ReasonReceiptInvalid), Err: fmt.Errorf("decode signing_address: %w", err)}
	}
	if !bytes.Equal(addr, attestedAddr) {
		return &llm.AttestationError{
			Reason: string(ReasonSignerMismatch),
			Err:    errors.New("receipt signing_address differs from attested address"),
		}
	}
	longForm := model + ":" + requestHashHex + ":" + responseHashHex
	shortForm := requestHashHex + ":" + responseHashHex
	if r.Text != longForm && r.Text != shortForm {
		return &llm.AttestationError{
			Reason: string(ReasonReceiptInvalid),
			Err:    errors.New("receipt text does not match request/response hashes"),
		}
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(r.Signature, "0x"))
	if err != nil {
		return &llm.AttestationError{Reason: string(ReasonReceiptInvalid), Err: fmt.Errorf("decode signature: %w", err)}
	}
	if len(addr) != ed25519.PublicKeySize {
		return &llm.AttestationError{
			Reason: string(ReasonReceiptInvalid),
			Err:    fmt.Errorf("signing_address is %d bytes; want %d", len(addr), ed25519.PublicKeySize),
		}
	}
	if len(sig) != ed25519.SignatureSize {
		return &llm.AttestationError{
			Reason: string(ReasonReceiptInvalid),
			Err:    fmt.Errorf("signature is %d bytes; want %d", len(sig), ed25519.SignatureSize),
		}
	}
	if !ed25519.Verify(ed25519.PublicKey(addr), []byte(r.Text), sig) {
		return &llm.AttestationError{Reason: string(ReasonReceiptInvalid), Err: errors.New("ed25519 signature did not verify")}
	}
	return nil
}
