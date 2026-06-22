// internal/llm/openaiapi/sse.go
package openaiapi

import (
	"bufio"
	"io"
	"strings"
)

// SSEReader reads OpenAI-style Server-Sent Events from an HTTP response body.
// Each call to Next returns the JSON payload from one "data: <json>" line.
// Returns io.EOF after "data: [DONE]" or end of stream.
type SSEReader struct {
	scanner *bufio.Scanner
}

// NewSSEReader constructs an SSEReader from an HTTP response body.
func NewSSEReader(r io.Reader) *SSEReader {
	return &SSEReader{scanner: bufio.NewScanner(r)}
}

// Next returns the next SSE data payload, stripping the "data: " prefix.
// Returns io.EOF when the stream ends (either [DONE] or connection close).
func (s *SSEReader) Next() (string, error) {
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			return "", io.EOF
		}
		return payload, nil
	}
	if err := s.scanner.Err(); err != nil {
		return "", err
	}
	return "", io.EOF
}
