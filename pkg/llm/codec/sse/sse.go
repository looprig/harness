// pkg/llm/codec/sse/sse.go

// Package sse de-frames OpenAI-style Server-Sent Event streams into their
// per-event JSON payloads. It is transport-agnostic (reads any io.Reader) and
// shared across OpenAI-compatible dialects.
package sse

import (
	"bufio"
	"io"
	"strings"
)

// Reader reads OpenAI-style Server-Sent Events from an io.Reader.
// Each call to Next returns the JSON payload from one "data: <json>" line.
// Returns io.EOF after "data: [DONE]" or end of stream.
type Reader struct {
	scanner *bufio.Scanner
}

// NewReader constructs a Reader from an io.Reader.
func NewReader(r io.Reader) *Reader {
	return &Reader{scanner: bufio.NewScanner(r)}
}

// Next returns the next SSE data payload, stripping the "data: " prefix.
// Returns io.EOF when the stream ends (either [DONE] or connection close).
func (s *Reader) Next() (string, error) {
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
