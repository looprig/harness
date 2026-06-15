// Package hashcache memoizes the parse of a byte slice keyed by its SHA-256.
//
// A Cache[T] remembers the result of parsing the last byte slice it was given.
// A subsequent Load of bytes with an identical SHA-256 returns the remembered
// value without re-invoking the parse function, so repeated reads of an
// unchanged file (e.g. an approvals file) skip re-parsing. When the bytes
// change, the parse runs again and the cache slot is replaced.
//
// The generic [T any] is the unconstrained-type-parameter idiom (precedent:
// llm.StreamReader[T] and registry.Registry[T]); it keeps this package
// domain-free so it can back any parse, not a dynamic any value flowing through
// business logic.
package hashcache

import (
	"crypto/sha256"
	"sync"
)

// Cache memoizes a single parse result keyed by the SHA-256 of its input.
//
// The zero value is not usable; construct a Cache with New. A Cache is safe for
// concurrent use: every access to its state happens under mu.
type Cache[T any] struct {
	mu    sync.Mutex
	sum   [sha256.Size]byte
	val   T
	ok    bool
	parse func([]byte) (T, error)
}

// New returns a Cache that delegates parsing of fresh (uncached) content to
// parse. parse must be non-nil; it is invoked synchronously by Load whenever the
// input's SHA-256 differs from the cached entry (or nothing is cached yet).
func New[T any](parse func([]byte) (T, error)) *Cache[T] {
	return &Cache[T]{parse: parse}
}

// Load returns the parsed value for content.
//
// It computes sha256.Sum256(content). On a cache hit (a value is cached and the
// sum matches the cached sum) it returns the cached value without calling parse.
// Otherwise it calls parse(content); on success it caches the sum and value and
// returns them, and on error it returns the error WITHOUT caching it — so the
// next Load of identical bytes re-attempts the parse. The previously cached
// entry, if any, is left untouched on a parse error.
//
// Load holds the cache mutex for its whole duration, so a slow parse serializes
// concurrent Loads; this trades throughput for a simple, race-free contract.
func (c *Cache[T]) Load(content []byte) (T, error) {
	sum := sha256.Sum256(content)

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ok && sum == c.sum {
		return c.val, nil
	}

	val, err := c.parse(content)
	if err != nil {
		var zero T
		return zero, err
	}

	c.sum = sum
	c.val = val
	c.ok = true
	return val, nil
}
