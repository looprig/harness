package llm_test

import (
	"errors"
	"io"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
)

func TestStreamReader_Next(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		items     []string
		wantItems []string
	}{
		{
			name:      "three item stream reads all items then EOF",
			items:     []string{"alpha", "beta", "gamma"},
			wantItems: []string{"alpha", "beta", "gamma"},
		},
		{
			name:      "single item stream",
			items:     []string{"only"},
			wantItems: []string{"only"},
		},
		{
			name:      "empty stream immediately returns EOF",
			items:     []string{},
			wantItems: []string{},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			idx := 0
			next := func() (string, error) {
				if idx >= len(tc.items) {
					return "", io.EOF
				}
				v := tc.items[idx]
				idx++
				return v, nil
			}

			r := llm.NewStreamReader(next, nil)

			var got []string
			for {
				v, err := r.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("unexpected error before EOF: %v", err)
				}
				got = append(got, v)
			}

			if len(got) != len(tc.wantItems) {
				t.Fatalf("got %d items, want %d", len(got), len(tc.wantItems))
			}
			for i, want := range tc.wantItems {
				if got[i] != want {
					t.Errorf("item[%d]: got %q, want %q", i, got[i], want)
				}
			}
		})
	}
}

// TestStreamReader_CloseIdempotent asserts Close runs the wrapped closer at most
// once across repeated calls, and every call returns the first call's result.
func TestStreamReader_CloseIdempotent(t *testing.T) {
	t.Parallel()

	errClose := errors.New("close failed")

	cases := []struct {
		name     string
		closer   func(calls *int) func() error
		calls    int // number of Close() invocations
		wantRuns int // how many times the wrapped closer must run
		wantErr  error
	}{
		{
			name:     "double close on nil closer runs nothing, returns nil",
			closer:   nil,
			calls:    2,
			wantRuns: 0,
			wantErr:  nil,
		},
		{
			name: "double close runs closer once, returns nil twice",
			closer: func(calls *int) func() error {
				return func() error { *calls++; return nil }
			},
			calls:    2,
			wantRuns: 1,
			wantErr:  nil,
		},
		{
			name: "triple close runs error-closer once, returns same err each call",
			closer: func(calls *int) func() error {
				return func() error { *calls++; return errClose }
			},
			calls:    3,
			wantRuns: 1,
			wantErr:  errClose,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			runs := 0
			var closer func() error
			if tc.closer != nil {
				closer = tc.closer(&runs)
			}
			next := func() (string, error) { return "", io.EOF }
			r := llm.NewStreamReader(next, closer)

			for i := 0; i < tc.calls; i++ {
				if err := r.Close(); !errors.Is(err, tc.wantErr) {
					t.Errorf("Close() call %d error = %v, want %v", i, err, tc.wantErr)
				}
			}
			if runs != tc.wantRuns {
				t.Errorf("closer ran %d times, want %d", runs, tc.wantRuns)
			}
		})
	}
}

func TestStreamReader_Close(t *testing.T) {
	t.Parallel()

	errClose := errors.New("close failed")

	cases := []struct {
		name       string
		closer     func() error
		wantCalled bool
		wantErr    error
	}{
		{
			name:       "nil closer returns nil and is a no-op",
			closer:     nil,
			wantCalled: false,
			wantErr:    nil,
		},
		{
			name: "explicit closer sets flag and returns nil",
			closer: func() error {
				return nil
			},
			wantCalled: true,
			wantErr:    nil,
		},
		{
			name: "closer that returns error propagates it",
			closer: func() error {
				return errClose
			},
			wantCalled: true,
			wantErr:    errClose,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			called := false
			var closer func() error
			if tc.closer != nil {
				original := tc.closer
				closer = func() error {
					called = true
					return original()
				}
			}

			next := func() (string, error) { return "", io.EOF }
			r := llm.NewStreamReader(next, closer)

			err := r.Close()

			if tc.closer != nil && !called {
				t.Error("closer was not called")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Close() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestStreamReader_ErrorPropagates(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("stream error")

	cases := []struct {
		name      string
		items     []string
		errOnCall int
		wantItems []string
		wantErr   error
	}{
		{
			name:      "error on first call returns no items",
			items:     []string{"a", "b"},
			errOnCall: 0,
			wantItems: nil,
			wantErr:   sentinel,
		},
		{
			name:      "error after two items returns those items then error",
			items:     []string{"x", "y", "z"},
			errOnCall: 2,
			wantItems: []string{"x", "y"},
			wantErr:   sentinel,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			call := 0
			next := func() (string, error) {
				if call == tc.errOnCall {
					return "", sentinel
				}
				if call >= len(tc.items) {
					return "", io.EOF
				}
				v := tc.items[call]
				call++
				return v, nil
			}

			r := llm.NewStreamReader(next, nil)

			var got []string
			var finalErr error
			for {
				v, err := r.Next()
				if err != nil {
					finalErr = err
					break
				}
				got = append(got, v)
			}

			if !errors.Is(finalErr, tc.wantErr) {
				t.Errorf("final error = %v, want %v", finalErr, tc.wantErr)
			}
			if len(got) != len(tc.wantItems) {
				t.Fatalf("got %d items, want %d", len(got), len(tc.wantItems))
			}
			for i, want := range tc.wantItems {
				if got[i] != want {
					t.Errorf("item[%d]: got %q, want %q", i, got[i], want)
				}
			}
		})
	}
}
