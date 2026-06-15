package hashcache

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// errParse is a typed parse failure used to assert that errors propagate with
// their identity intact (errors.As-able) and are never cached.
type errParse struct {
	content string
}

func (e *errParse) Error() string {
	return fmt.Sprintf("parse failed for %q", e.content)
}

// countingParse returns a parse func that records each invocation in calls and
// maps content to len(content) (a deterministic, content-derived int value).
func countingParse(calls *int64) func([]byte) (int, error) {
	return func(b []byte) (int, error) {
		atomic.AddInt64(calls, 1)
		return len(b), nil
	}
}

// TestCacheLoad exercises the memoization contract step by step: a fresh parse,
// a cache hit on identical bytes, a re-parse on changed bytes, an error that
// propagates without being cached, and the empty-input boundary.
func TestCacheLoad(t *testing.T) {
	t.Parallel()

	type step struct {
		name      string
		content   []byte
		want      int
		wantErr   bool
		wantCalls int64 // cumulative parse-call count expected after this step
	}

	tests := []struct {
		name  string
		parse func(calls *int64) func([]byte) (int, error)
		steps []step
	}{
		{
			name:  "first load parses, identical load is a cache hit, changed load re-parses",
			parse: countingParse,
			steps: []step{
				{name: "first load parses", content: []byte("hello"), want: 5, wantCalls: 1},
				{name: "identical load is cache hit", content: []byte("hello"), want: 5, wantCalls: 1},
				{name: "identical load again still cache hit", content: []byte("hello"), want: 5, wantCalls: 1},
				{name: "changed load re-parses", content: []byte("hi"), want: 2, wantCalls: 2},
				{name: "back to original re-parses (only last sum cached)", content: []byte("hello"), want: 5, wantCalls: 3},
				{name: "new identical load is cache hit", content: []byte("hello"), want: 5, wantCalls: 3},
			},
		},
		{
			name:  "empty input boundary: first empty parses, second empty is a cache hit",
			parse: countingParse,
			steps: []step{
				{name: "first empty load parses", content: []byte{}, want: 0, wantCalls: 1},
				{name: "second empty load is cache hit", content: []byte{}, want: 0, wantCalls: 1},
				{name: "nil load is a cache hit (same sha256 as empty)", content: nil, want: 0, wantCalls: 1},
				{name: "non-empty after empty re-parses", content: []byte("x"), want: 1, wantCalls: 2},
				{name: "empty after non-empty re-parses", content: []byte{}, want: 0, wantCalls: 3},
			},
		},
		{
			name: "parse error propagates and is not cached, then identical load retries",
			parse: func(calls *int64) func([]byte) (int, error) {
				return func(b []byte) (int, error) {
					atomic.AddInt64(calls, 1)
					return 0, &errParse{content: string(b)}
				}
			},
			steps: []step{
				{name: "first load errors", content: []byte("boom"), wantErr: true, wantCalls: 1},
				{name: "identical load retries (error not cached)", content: []byte("boom"), wantErr: true, wantCalls: 2},
				{name: "another identical load retries again", content: []byte("boom"), wantErr: true, wantCalls: 3},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var calls int64
			c := New(tt.parse(&calls))

			for _, s := range tt.steps {
				got, err := c.Load(s.content)
				if (err != nil) != s.wantErr {
					t.Fatalf("step %q: Load() error = %v, wantErr %v", s.name, err, s.wantErr)
				}
				if s.wantErr {
					var pe *errParse
					if !errors.As(err, &pe) {
						t.Fatalf("step %q: Load() error = %v, want errors.As(*errParse)", s.name, err)
					}
				} else if got != s.want {
					t.Errorf("step %q: Load() = %d, want %d", s.name, got, s.want)
				}
				if n := atomic.LoadInt64(&calls); n != s.wantCalls {
					t.Errorf("step %q: parse calls = %d, want %d", s.name, n, s.wantCalls)
				}
			}
		})
	}
}

// TestCacheLoadHitSkipsParse pins the core guarantee in isolation: once a value
// is cached, a subsequent identical Load must never invoke parse again — proven
// by a parse that fails the test if it is ever called a second time.
func TestCacheLoadHitSkipsParse(t *testing.T) {
	t.Parallel()

	var calls int64
	c := New(func(b []byte) (string, error) {
		if atomic.AddInt64(&calls, 1) > 1 {
			t.Errorf("parse called %d times for identical input; want exactly 1", calls)
		}
		return string(b), nil
	})

	const content = "payload"
	for i := 0; i < 5; i++ {
		got, err := c.Load([]byte(content))
		if err != nil {
			t.Fatalf("Load() iteration %d unexpected error = %v", i, err)
		}
		if got != content {
			t.Errorf("Load() iteration %d = %q, want %q", i, got, content)
		}
	}
	if calls != 1 {
		t.Errorf("parse calls = %d, want 1", calls)
	}
}

// TestCacheLoadConcurrent hammers Load from many goroutines with a mix of
// identical and differing inputs. It must be race-free under -race, never
// panic, and always return the correct value for each input.
func TestCacheLoadConcurrent(t *testing.T) {
	t.Parallel()

	var calls int64
	// parse maps content -> its length; len is a content-derived invariant we can
	// always re-verify regardless of cache hits/misses.
	c := New(func(b []byte) (int, error) {
		atomic.AddInt64(&calls, 1)
		return len(b), nil
	})

	// A small set of distinct inputs so concurrent goroutines collide on the
	// same keys (forcing real contention on the mutex and cache slot).
	inputs := [][]byte{
		[]byte(""),
		[]byte("a"),
		[]byte("bb"),
		[]byte("ccc"),
		[]byte("dddd"),
	}

	const goroutines = 64
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				in := inputs[(seed+i)%len(inputs)]
				got, err := c.Load(in)
				if err != nil {
					t.Errorf("Load(%q) unexpected error = %v", in, err)
					return
				}
				if got != len(in) {
					t.Errorf("Load(%q) = %d, want %d", in, got, len(in))
					return
				}
			}
		}(g)
	}
	wg.Wait()

	// At least one parse must have happened; with caching it should be far fewer
	// than the total Load count. We only assert the lower bound to avoid being
	// flaky on scheduling — correctness of values is already checked above.
	if n := atomic.LoadInt64(&calls); n < 1 {
		t.Errorf("parse calls = %d, want >= 1", n)
	}
}
