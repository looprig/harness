package tui

import (
	"bytes"
	"fmt"
	"sync"
)

// syncBuf is a goroutine-safe bytes.Buffer wrapper: a tea.Program writes frames from
// its render goroutine while the test reads length/contents. Shared by the tests that
// drive a real program through a captured io.Writer (e.g. tool_handoff_test.go).
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Len()
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// blockingReader blocks Read until closed, keeping a driven tea.Program alive until
// the test calls Quit (so the program never exits on an early input EOF).
type blockingReader struct {
	mu     sync.Mutex
	ch     chan struct{}
	closed bool
}

func newBlockingReader() *blockingReader { return &blockingReader{ch: make(chan struct{})} }

func (b *blockingReader) Read(p []byte) (int, error) {
	<-b.ch
	return 0, fmt.Errorf("input closed")
}

func (b *blockingReader) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.closed = true
		close(b.ch)
	}
}
