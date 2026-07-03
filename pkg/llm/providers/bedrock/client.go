// Package bedrock is an AWS Bedrock Runtime client for Anthropic-on-Bedrock. It
// satisfies llm.LLM for the non-streaming InvokeModel path: it encodes the
// Anthropic Messages body (via the anthropicapi codec), rewrites it into the
// Bedrock body (drop "model", add "anthropic_version"), routes to the
// region-derived bedrock-runtime endpoint with the model id in the URL path, and
// signs the request with AWS Signature Version 4. Streaming (AWS eventstream) is a
// documented follow-up and returns a typed StreamingNotSupportedError.
//
// Credentials are AWS SigV4, not a bearer key, so a Bedrock client is constructed
// directly via New (auto.New cannot supply SigV4 credentials and errors to here).
package bedrock

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/harness/pkg/llm/auth"
	"github.com/looprig/harness/pkg/llm/codec/anthropicapi"
)

// Compile-time proof that Client honors the llm.LLM contract.
var _ llm.LLM = (*Client)(nil)

const (
	// bedrockService is the SigV4 service name; the signer keys its non-s3
	// canonical-URI double-encoding on it (the model path's ":" -> "%3A").
	bedrockService = "bedrock"
	// endpointScheme and hostFormat build the region-routed Bedrock Runtime endpoint.
	endpointScheme = "https"
	hostFormat     = "bedrock-runtime.%s.amazonaws.com"
	// path fragments: /model/<model-id>/invoke.
	pathModelPrefix  = "/model/"
	pathInvokeSuffix = "/invoke"

	contentTypeJSON = "application/json"
)

// Timeout budget for the connect/TLS/header phases, mirroring the generic
// transport client's hygiene. There is deliberately no whole-request
// http.Client.Timeout: the per-request deadline is the caller's context (Invoke
// takes ctx), and omitting it keeps the client forward-compatible with a future
// long-lived streaming body.
const (
	dialTimeout           = 10 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	responseHeaderTimeout = 60 * time.Second
	expectContinueTimeout = 1 * time.Second
	idleConnTimeout       = 90 * time.Second
)

// Client is a region-bound Anthropic-on-Bedrock inference client. It owns one
// SigV4 signer (built from the caller's credentials) and one http.Client, and is
// safe for concurrent use (both are immutable after construction). Connection
// binding is by provider+region: a request whose Model.Provider is not
// ProviderBedrock is rejected pre-I/O with *llm.ModelMismatchError.
type Client struct {
	region   string
	endpoint string // scheme://host base, e.g. https://bedrock-runtime.us-east-1.amazonaws.com
	signer   llm.Authenticator
	codec    anthropicapi.Codec
	hc       *http.Client
}

// New constructs a Bedrock client bound to region, signing with creds. It fails
// closed with *ConfigError when the region or either mandatory credential field
// (AccessKeyID, SecretAccessKey) is empty — no Client and no network object are
// created. The session token is optional (used for temporary credentials).
func New(creds auth.SigV4Credentials, region string) (llm.LLM, error) {
	if region == "" {
		return nil, &ConfigError{Field: "region", Reason: "AWS region must not be empty"}
	}
	if creds.AccessKeyID == "" {
		return nil, &ConfigError{Field: "AccessKeyID", Reason: "SigV4 AccessKeyID must not be empty"}
	}
	if creds.SecretAccessKey == "" {
		return nil, &ConfigError{Field: "SecretAccessKey", Reason: "SigV4 SecretAccessKey must not be empty"}
	}
	return newClient(creds, region, defaultEndpoint(region)), nil
}

// newClient wires a Client for a validated region + endpoint. endpoint is the
// scheme://host base; New derives it from the region, tests override it to reach
// an httptest.Server. It builds the SigV4 signer once and the phase-bounded,
// TLS>=1.2 http.Client.
func newClient(creds auth.SigV4Credentials, region, endpoint string) *Client {
	return &Client{
		region:   region,
		endpoint: endpoint,
		signer:   auth.SigV4(creds, region, bedrockService),
		hc: &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   dialTimeout,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
				TLSHandshakeTimeout:   tlsHandshakeTimeout,
				ResponseHeaderTimeout: responseHeaderTimeout,
				ExpectContinueTimeout: expectContinueTimeout,
				IdleConnTimeout:       idleConnTimeout,
				ForceAttemptHTTP2:     true,
			},
		},
	}
}

// defaultEndpoint returns the region-routed Bedrock Runtime endpoint base.
func defaultEndpoint(region string) string {
	return endpointScheme + "://" + fmt.Sprintf(hostFormat, region)
}

// Invoke sends a non-streaming InvokeModel request and returns the decoded
// Anthropic response. Ordered, all pre-I/O guards first: (1) provider binding,
// (2) Model.Validate, (3) API-format guard (Anthropic-only), (4) encode Anthropic
// body + rewrite to the Bedrock body, (5) build the ctx-bound POST to the
// per-model path, (6) SigV4-sign, (7) do + map transport/non-2xx failures,
// (8) decode the Anthropic response.
func (c *Client) Invoke(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if err := c.checkBinding(req.Model); err != nil {
		return nil, err
	}
	if err := req.Model.Validate(); err != nil {
		return nil, err
	}
	// supportsAPIFormat(bedrock) admits both Anthropic and Bedrock Converse, so a
	// Converse Model passes Validate; this client only implements the Anthropic
	// dialect, so fail closed rather than silently Anthropic-encode a Converse call.
	if req.Model.APIFormat != llm.APIFormatAnthropic {
		return nil, &UnsupportedAPIFormatError{APIFormat: req.Model.APIFormat}
	}

	anthropicBody, err := c.codec.EncodeRequest(req, llm.RequestModeInvoke)
	if err != nil {
		return nil, err
	}
	body, err := toBedrockBody(anthropicBody)
	if err != nil {
		return nil, err
	}

	httpReq, err := c.buildRequest(ctx, req.Model.Name, body)
	if err != nil {
		return nil, err
	}
	if err := c.signer.Authorize(ctx, httpReq); err != nil {
		return nil, err
	}

	httpResp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	defer httpResp.Body.Close()
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	if httpResp.StatusCode/100 != 2 {
		return nil, &llm.APIError{Status: httpResp.StatusCode, Message: string(respBody), Body: respBody}
	}
	return c.codec.DecodeResponse(respBody)
}

// Stream is not implemented: Bedrock streaming uses AWS eventstream framing, a
// documented follow-up. It fails closed with *StreamingNotSupportedError and opens
// no connection.
func (c *Client) Stream(_ context.Context, _ llm.Request) (*llm.StreamReader[content.Chunk], error) {
	return nil, &StreamingNotSupportedError{}
}

// checkBinding fails closed when the request's Model names a provider other than
// Bedrock, before any I/O. Bedrock is region-bound (the Model carries no region
// and, by convention, an empty BaseURL), so the enforceable binding is the
// provider; the region is fixed at construction.
func (c *Client) checkBinding(m llm.Model) error {
	if m.Provider != llm.ProviderBedrock {
		return &llm.ModelMismatchError{
			BoundProvider:   llm.ProviderBedrock,
			RequestProvider: m.Provider,
			BoundEndpoint:   c.endpoint,
			RequestEndpoint: m.BaseURL,
		}
	}
	return nil
}

// buildRequest constructs the ctx-bound POST to
// <endpoint>/model/<escaped model id>/invoke with the JSON content/accept headers.
// The model id is path-escaped so it is a single URL path segment; its ":" stays
// literal on the wire (url.PathEscape does not escape ":"), and the SigV4 signer
// then double-encodes it into the canonical URI ("%3A"). Headers are set before
// signing so they are covered by the signature.
func (c *Client) buildRequest(ctx context.Context, modelID string, body []byte) (*http.Request, error) {
	rawURL := c.endpoint + pathModelPrefix + url.PathEscape(modelID) + pathInvokeSuffix
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, &RequestBuildError{Err: err}
	}
	httpReq.Header.Set("Content-Type", contentTypeJSON)
	httpReq.Header.Set("Accept", contentTypeJSON)
	return httpReq, nil
}
