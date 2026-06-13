// internal/llm/openaiapi/lmstudio/client.go
package lmstudio

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi"
)

const defaultBaseURL = "http://localhost:1234/v1"

// Client is a plain HTTP OpenAI-compatible client for LM Studio.
// It sends no Authorization header — LM Studio does not require auth.
type Client struct {
	baseURL string
	http    *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the default http.Client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.http = hc }
}

// New constructs a Client. baseURL defaults to http://localhost:1234/v1 if empty.
func New(baseURL string, opts ...Option) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Invoke sends a non-streaming request and returns the complete response.
func (c *Client) Invoke(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if err := req.Model.Validate(); err != nil {
		return nil, err
	}
	body, err := encodeRequest(req, false)
	if err != nil {
		return nil, err
	}
	respBody, err := c.do(ctx, body)
	if err != nil {
		return nil, err
	}
	return decodeResponse(respBody)
}

// Stream sends a streaming request and returns a StreamReader[content.Chunk].
func (c *Client) Stream(ctx context.Context, req llm.Request) (*llm.StreamReader[content.Chunk], error) {
	if err := req.Model.Validate(); err != nil {
		return nil, err
	}
	body, err := encodeRequest(req, true)
	if err != nil {
		return nil, err
	}
	httpResp, err := c.doStream(ctx, body)
	if err != nil {
		return nil, err
	}
	return openaiapi.NewStream(httpResp.Body), nil
}

func (c *Client) do(ctx context.Context, body []byte) ([]byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := c.http.Do(httpReq)
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
	return respBody, nil
}

func (c *Client) doStream(ctx context.Context, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	if httpResp.StatusCode/100 != 2 {
		defer httpResp.Body.Close()
		b, readErr := io.ReadAll(httpResp.Body)
		if readErr != nil {
			return nil, &llm.NetworkError{Err: fmt.Errorf("reading error body (status %d): %w", httpResp.StatusCode, readErr)}
		}
		return nil, &llm.APIError{Status: httpResp.StatusCode, Message: fmt.Sprintf("lmstudio stream: %s", b), Body: b}
	}
	return httpResp, nil
}
