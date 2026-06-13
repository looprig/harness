package llm

// StreamReader is a pull-based iterator over streaming values of type T.
// Call Next to advance; it returns (zero, io.EOF) when the stream is exhausted.
// Always call Close when done — even after io.EOF — to release the underlying connection.
type StreamReader[T any] struct {
	next  func() (T, error)
	close func() error
}

// NewStreamReader constructs a StreamReader from a next function and an optional
// closer. If closer is nil, Close is a no-op.
// next must return (zero, io.EOF) when the stream is exhausted.
func NewStreamReader[T any](next func() (T, error), closer func() error) *StreamReader[T] {
	if closer == nil {
		closer = func() error { return nil }
	}
	return &StreamReader[T]{next: next, close: closer}
}

// Next returns the next value. Returns (zero, io.EOF) when exhausted.
func (r *StreamReader[T]) Next() (T, error) {
	return r.next()
}

// Close releases the underlying connection. Safe to call multiple times if the closer is idempotent.
func (r *StreamReader[T]) Close() error {
	return r.close()
}
