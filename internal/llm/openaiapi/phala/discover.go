package phala

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"sync"
	"time"

	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/tee"
)

// sessionTTL caps how long an attested session is reused before re-attesting.
// Matches llm/chutes (50s) on the same posture: stay comfortably under any
// server-side per-instance freshness window.
const sessionTTL = 50 * time.Second

// attestedSession captures the verified-and-bound state of a per-model TEE
// instance: the signing key the receipt must carry, the algo it signs with,
// and when we last attested it.
type attestedSession struct {
	signingAddr []byte
	algo        string
	attestedAt  time.Time
}

// Client is the phala TEE-attested chat client. Task 9 carries only the state
// the attestation flow needs; Task 10 fleshes out construction, options, and
// the embedded OpenAI-compatible chat path.
type Client struct {
	http    *http.Client
	apiBase string
	apiKey  string
	nrasURL string
	jwksURL string

	mu       sync.Mutex
	sessions map[string]*attestedSession // keyed by model name

	// attestFn lets tests bypass the real TDX+NRAS chain — the synthetic
	// fixture's signing_address is recoverable from report_data by
	// construction, but the GPU evidence cannot be re-signed against a fake
	// NRAS without rebuilding the SPDM chain. Defaults to (*Client).attest in
	// production; tests overwrite with a fake. The (ctx, *Client, model)
	// shape matches chutes' attestFn pattern so a reader of one knows the
	// other.
	attestFn func(ctx context.Context, c *Client, model string) (*attestedSession, error)
}

// attestModel returns a fresh-or-cached attested session for the named model.
// On first call (or after sessionTTL elapses) it fetches the attestation
// report, verifies the TDX quote + GPU evidence + report_data binding, and
// caches the result. Failures are NOT cached — the next call re-attests.
//
// Concurrent callers for the same model may all observe a cache miss and run
// attestFn in parallel; the last writer wins. This matches chutes and is
// acceptable for v1 (attestation is read-only and idempotent).
func (c *Client) attestModel(ctx context.Context, model string) (*attestedSession, error) {
	c.mu.Lock()
	s := c.sessions[model]
	c.mu.Unlock()
	if s != nil && time.Since(s.attestedAt) < sessionTTL {
		return s, nil
	}
	s, err := c.attestFn(ctx, c, model)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.sessions[model] = s
	c.mu.Unlock()
	return s, nil
}

// attest performs one full attestation round-trip and verification for the
// named model. It is the default value of attestFn; tests override.
//
// Steps (see WIRE.md sections 3-4):
//  1. Generate a fresh 32-byte nonce.
//  2. GET /v1/attestation/report?model=...&nonce=...
//  3. parseAttestationReport -> parsedAttestation
//  4. tee.VerifyTDXQuote(rawQuote) -> report_data
//  5. verifyBinding (Shapes A/B: signingAddress||nonce; Shape C:
//     sha256(nonceHex+pubKeyB64))
//  6. tee.VerifyGPUEvidence on the gpu_evidence array
//  7. return *attestedSession
//
// For Shape A/B the nonce we sent is the binding nonce. For Shape C, RedPill
// generates its own per-instance nonces and echoes them in each
// all_attestations[].nonce; the binding is checked against THAT nonce, not
// ours. (Our outgoing nonce is still useful for replay protection at the HTTP
// layer.)
func (c *Client) attest(ctx context.Context, _ *Client, model string) (*attestedSession, error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	nonceHex := hex.EncodeToString(nonce)

	body, err := c.fetchAttestationReport(ctx, model, nonceHex)
	if err != nil {
		return nil, err
	}
	parsed, err := parseAttestationReport(body, model)
	if err != nil {
		return nil, err
	}
	rd, err := tee.VerifyTDXQuote(parsed.RawQuote)
	if err != nil {
		return nil, err // *tee.Error propagates; callers errors.As on it
	}
	switch parsed.Algo {
	case "chutes":
		// Shape C: chutes-style binding over the per-instance nonce.
		if err := verifyChutesBinding(rd, parsed.NonceHex, parsed.PubKeyB64); err != nil {
			return nil, err
		}
	default:
		// Shape A/B: phala layout against the nonce we just sent.
		if err := verifyBinding(rd, parsed.SigningAddress, nonce); err != nil {
			return nil, err
		}
	}
	var gpus []tee.GPUEvidence
	if err := json.Unmarshal(parsed.NvidiaPayload, &gpus); err != nil {
		return nil, attestErr(ReasonAttestationMalformed,
			fmt.Errorf("decode nvidia_payload: %w", err))
	}
	if err := tee.VerifyGPUEvidence(ctx, c.http, c.nrasURL, c.jwksURL, gpus); err != nil {
		return nil, err
	}
	return &attestedSession{
		signingAddr: parsed.SigningAddress,
		algo:        parsed.Algo,
		attestedAt:  time.Now(),
	}, nil
}

// fetchAttestationReport issues GET {apiBase}/v1/attestation/report?model=...&nonce=...
// with a Bearer token. Returns the raw response body, *llm.NetworkError on
// transport failure, or *llm.APIError on a non-2xx status.
func (c *Client) fetchAttestationReport(ctx context.Context, model, nonceHex string) ([]byte, error) {
	u := c.apiBase + "/v1/attestation/report" +
		"?model=" + neturl.QueryEscape(model) +
		"&nonce=" + nonceHex
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
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
