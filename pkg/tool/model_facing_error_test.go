package tool

import (
	"errors"
	"strings"
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
