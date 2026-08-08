package tool

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

type modelFacingErrorTestMarker struct{ detail string }

func (e modelFacingErrorTestMarker) Error() string { return "ordinary" }

func (e modelFacingErrorTestMarker) ModelFacingError() string { return e.detail }

type modelFacingErrorTestAs struct{ detail string }

func (e modelFacingErrorTestAs) Error() string { return "ordinary" }

func (e modelFacingErrorTestAs) As(target any) bool {
	marker, ok := target.(*ModelFacingError)
	if !ok {
		return false
	}
	*marker = modelFacingErrorTestMarker(e)
	return true
}

type modelFacingErrorTestCycle struct{ next error }

func (e *modelFacingErrorTestCycle) Error() string { return "cycle" }

func (e *modelFacingErrorTestCycle) Unwrap() error { return e.next }

type modelFacingErrorTestFanout struct{ children []error }

func (e modelFacingErrorTestFanout) Error() string { return "fanout" }

func (e modelFacingErrorTestFanout) Unwrap() []error { return e.children }

type modelFacingErrorTestCountingMarker struct {
	calls *atomic.Int64
	id    int
	safe  bool
}

func (e modelFacingErrorTestCountingMarker) Error() string { return "counted marker" }

func (e modelFacingErrorTestCountingMarker) ModelFacingError() string {
	e.calls.Add(1)
	if e.safe {
		return "safe fanout detail"
	}
	panic("marker probe")
}

func TestModelFacingErrorDetailTraversesOnlyRealChains(t *testing.T) {
	t.Parallel()
	const secret = "secret"
	const safe = "safe detail"
	tests := []struct {
		name   string
		err    error
		want   string
		marked bool
	}{
		{name: "ordinary", err: errors.New(secret)},
		{name: "wrapper", err: fmtWrappedError{cause: modelFacingErrorTestMarker{detail: safe}}, want: safe, marked: true},
		{name: "join keeps secret sibling private", err: errors.Join(errors.New(secret), modelFacingErrorTestAs{detail: secret})},
		{name: "custom As cannot fabricate marker", err: modelFacingErrorTestAs{detail: secret}},
	}
	for _, tt := range tests {
		tc := tt
		t.Run(tc.name, func(t *testing.T) {
			got, marked := ModelFacingErrorDetail(tc.err)
			if got != tc.want || marked != tc.marked {
				t.Fatalf("ModelFacingErrorDetail() = %q, %v; want %q, %v", got, marked, tc.want, tc.marked)
			}
		})
	}

	cycle := &modelFacingErrorTestCycle{}
	cycle.next = cycle
	if got, marked := ModelFacingErrorDetail(cycle); got != "" || marked {
		t.Fatalf("cycle detail = %q, %v; want empty, false", got, marked)
	}
}

func TestModelFacingErrorDetailBoundsHostileFanout(t *testing.T) {
	t.Parallel()
	const childCount = maxModelFacingErrorNodes * 1024
	calls := new(atomic.Int64)
	children := make([]error, childCount)
	for i := range children {
		children[i] = modelFacingErrorTestCountingMarker{calls: calls, id: i}
	}
	children[maxModelFacingErrorNodes-2] = modelFacingErrorTestCountingMarker{calls: calls, id: maxModelFacingErrorNodes - 2, safe: true}
	root := modelFacingErrorTestFanout{children: children}

	bounded := unwrapErrors(root, maxModelFacingErrorNodes-1)
	if got, want := len(bounded), maxModelFacingErrorNodes-1; got != want {
		t.Fatalf("unwrapErrors() returned %d children, want exactly the %d-child budget", got, want)
	}

	detail, marked := ModelFacingErrorDetail(root)
	if detail != "safe fanout detail" || !marked {
		t.Fatalf("ModelFacingErrorDetail() = %q, %v; want safe fanout detail, true", detail, marked)
	}
	if got, want := calls.Load(), int64(maxModelFacingErrorNodes-1); got != want {
		t.Fatalf("ModelFacingError() calls = %d, want exactly %d allowed children", got, want)
	}
}

func TestBoundModelFacingErrorDetailNormalizesUTF8AndRuneBounds(t *testing.T) {
	t.Parallel()
	value := strings.Repeat("界", MaxModelFacingErrorBytes) + "\xff"
	got := BoundModelFacingErrorDetail(value)
	if len(got) > MaxModelFacingErrorBytes {
		t.Fatalf("bounded bytes = %d, want <= %d", len(got), MaxModelFacingErrorBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("bounded detail is not valid UTF-8")
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatal("bounded detail contains a replacement rune from partial truncation")
	}
}

type fmtWrappedError struct{ cause error }

func (e fmtWrappedError) Error() string { return "wrapped" }

func (e fmtWrappedError) Unwrap() error { return e.cause }
