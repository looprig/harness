package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/llm"
)

// Client is a connection-bound llm.LLM: one Codec (wire dialect) x one Endpoint
// (connection identity + routing) x one Authenticator (credentials). It performs
// the same ordered pre-I/O guards for both Invoke and Stream — binding check,
// then Model.Validate, then auth — before any network call, and maps transport
// vs non-2xx failures to the typed llm.NetworkError / llm.APIError.
type Client struct {
	codec llm.Codec
	ep    Endpoint
	auth  llm.Authenticator
	hc    *http.Client
}

// Compile-time proof that Client honors the llm.LLM contract.
var _ llm.LLM = (*Client)(nil)

// RequestBuildError is a failure to CONSTRUCT the outbound HTTP request — a bad
// method or a malformed endpoint URL. It is a request-configuration error, kept
// strictly distinct from *llm.NetworkError (which is reserved for transport
// failures out of hc.Do) so errors.As never misclassifies a config bug as a
// transport fault. Unwrap exposes the underlying net/http cause.
type RequestBuildError struct {
	Err error
}

func (e *RequestBuildError) Error() string { return "transport: build request: " + e.Err.Error() }
func (e *RequestBuildError) Unwrap() error { return e.Err }

// Timeout budget for the risky phases of a request. These bound connection setup
// and header reception WITHOUT a whole-request http.Client.Timeout, which would
// abort a long-lived streaming body mid-flight. The per-request deadline for the
// body is the caller's context (every I/O call takes ctx).
const (
	dialTimeout           = 10 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	responseHeaderTimeout = 60 * time.Second
	expectContinueTimeout = 1 * time.Second
	idleConnTimeout       = 90 * time.Second
)

// New constructs a Client bound to ep, speaking codec, authenticating with auth.
// auth is required: nil is a programmer error (the explicit "no credentials" value
// is auth.None()), so New panics on a nil auth rather than silently sending
// unauthenticated requests. An empty Endpoint.ChatPath defaults to DefaultChatPath.
func New(codec llm.Codec, ep Endpoint, auth llm.Authenticator) *Client {
	if auth == nil {
		panic("transport.New: auth must not be nil; pass auth.None() for no credentials")
	}
	if ep.ChatPath == "" {
		ep.ChatPath = DefaultChatPath
	}
	return &Client{
		codec: codec,
		ep:    ep,
		auth:  auth,
		hc: &http.Client{
			// No whole-request Timeout: it would kill streaming. Bound the
			// connect/TLS/header phases on the Transport instead; the body is
			// bounded by the request context.
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   dialTimeout,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				// Default TLS with an explicit floor of 1.2 and no
				// InsecureSkipVerify — server certificates are verified.
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

// Invoke sends a non-streaming request and returns the complete response.
// Ordered, all pre-I/O guards first: (1) binding check, (2) Model.Validate,
// (3) encode + build the http.Request, (4) authorize, (5) do + map errors,
// (6) decode.
func (c *Client) Invoke(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if err := c.checkBinding(req.Model); err != nil {
		return nil, err
	}
	if err := req.Model.Validate(); err != nil {
		return nil, err
	}
	httpReq, err := c.buildRequest(ctx, req, llm.RequestModeInvoke)
	if err != nil {
		return nil, err
	}
	if err := c.auth.Authorize(ctx, httpReq); err != nil {
		return nil, err
	}
	httpResp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	defer httpResp.Body.Close()
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	if httpResp.StatusCode/100 != 2 {
		return nil, &llm.APIError{Status: httpResp.StatusCode, Message: string(body), Body: body}
	}
	return c.codec.DecodeResponse(body)
}

// Stream sends a streaming request and returns a StreamReader over the decoded
// chunks. Same ordered guards as Invoke; on a 2xx it hands the still-open body to
// StreamChunks, on a non-2xx it drains and returns an *llm.APIError.
func (c *Client) Stream(ctx context.Context, req llm.Request) (*llm.StreamReader[content.Chunk], error) {
	if err := c.checkBinding(req.Model); err != nil {
		return nil, err
	}
	if err := req.Model.Validate(); err != nil {
		return nil, err
	}
	httpReq, err := c.buildRequest(ctx, req, llm.RequestModeStream)
	if err != nil {
		return nil, err
	}
	if err := c.auth.Authorize(ctx, httpReq); err != nil {
		return nil, err
	}
	httpResp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	if httpResp.StatusCode/100 != 2 {
		defer httpResp.Body.Close()
		body, readErr := io.ReadAll(httpResp.Body)
		if readErr != nil {
			return nil, &llm.NetworkError{Err: fmt.Errorf("transport: reading error body (status %d): %w", httpResp.StatusCode, readErr)}
		}
		return nil, &llm.APIError{Status: httpResp.StatusCode, Message: string(body), Body: body}
	}
	return StreamChunks(httpResp.Body, c.codec), nil
}

// checkBinding fails closed when the request's Model names a provider or endpoint
// other than the one this Client is bound to, before any I/O.
func (c *Client) checkBinding(m llm.Model) error {
	if m.Provider != c.ep.Provider || m.BaseURL != c.ep.BaseURL {
		return &llm.ModelMismatchError{
			BoundProvider:   c.ep.Provider,
			RequestProvider: m.Provider,
			BoundEndpoint:   c.ep.BaseURL,
			RequestEndpoint: m.BaseURL,
		}
	}
	return nil
}

// buildRequest encodes req via the codec and builds the ctx-bound POST. The
// Accept: text/event-stream header is added only for RequestModeStream.
func (c *Client) buildRequest(ctx context.Context, req llm.Request, mode llm.RequestMode) (*http.Request, error) {
	body, err := c.codec.EncodeRequest(req, mode)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(c.ep.BaseURL, "/") + c.ep.ChatPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, &RequestBuildError{Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if mode == llm.RequestModeStream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	return httpReq, nil
}
