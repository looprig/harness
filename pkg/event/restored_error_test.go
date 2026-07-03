package event_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/event"
)

// TestRestoredErrorError pins the Error() rendering: "<kind>: <message>".
func TestRestoredErrorError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  event.RestoredError
		want string
	}{
		{
			name: "happy path",
			err:  event.RestoredError{Kind: "tool_limit", Message: "tool limit reached"},
			want: "tool_limit: tool limit reached",
		},
		{
			name: "empty message",
			err:  event.RestoredError{Kind: "unknown", Message: ""},
			want: "unknown: ",
		},
		{
			name: "empty kind",
			err:  event.RestoredError{Kind: "", Message: "boom"},
			want: ": boom",
		},
		{
			name: "both empty",
			err:  event.RestoredError{},
			want: ": ",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRestoredErrorIsError proves *RestoredError satisfies the error interface and
// is recoverable via errors.As at a call site.
func TestRestoredErrorIsError(t *testing.T) {
	t.Parallel()
	var err error = &event.RestoredError{Kind: "k", Message: "m"}
	var got *event.RestoredError
	if !errors.As(err, &got) {
		t.Fatalf("errors.As(*RestoredError) = false, want true")
	}
	if got.Kind != "k" || got.Message != "m" {
		t.Errorf("recovered = %+v, want {k m}", got)
	}
}

// TestRestoredErrorJSONRoundTrip proves the {kind,message} wire form round-trips
// deep-equal — this is the shape the event codec persists for TurnFailed.Err.
func TestRestoredErrorJSONRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  event.RestoredError
		want string // expected wire JSON
	}{
		{
			name: "happy path",
			err:  event.RestoredError{Kind: "empty_response", Message: "loop: empty response from provider"},
			want: `{"kind":"empty_response","message":"loop: empty response from provider"}`,
		},
		{
			name: "empty message",
			err:  event.RestoredError{Kind: "unknown", Message: ""},
			want: `{"kind":"unknown","message":""}`,
		},
		{
			name: "zero value",
			err:  event.RestoredError{},
			want: `{"kind":"","message":""}`,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(&tt.err)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			if string(data) != tt.want {
				t.Errorf("json.Marshal() = %s, want %s", data, tt.want)
			}
			var got event.RestoredError
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if got != tt.err {
				t.Errorf("round-trip = %+v, want %+v", got, tt.err)
			}
		})
	}
}

// TestErrKind pins ErrKind's stable kind strings for every concrete error type the
// event package itself produces for TurnFailed.Err, the idempotent re-projection of
// an already-restored *RestoredError, and the "unknown" fallback for everything
// else (including the open-ended provider/stream errors that flow through
// streamFailure, whose message is still preserved losslessly by the codec).
func TestErrKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "empty response",
			err:  &event.EmptyResponseError{},
			want: "empty_response",
		},
		{
			name: "tool limit",
			err:  &event.ToolLimitError{Iterations: 5, MaxIterations: 4, Calls: 9, MaxCalls: 8},
			want: "tool_limit",
		},
		{
			name: "turn panic",
			err:  &event.TurnPanicError{Detail: "nil pointer"},
			want: "turn_panic",
		},
		{
			name: "restored error re-projects to its own kind",
			err:  &event.RestoredError{Kind: "tool_limit", Message: "tool limit reached"},
			want: "tool_limit",
		},
		{
			name: "restored error with empty kind stays empty",
			err:  &event.RestoredError{Kind: "", Message: "m"},
			want: "",
		},
		{
			name: "unknown sentinel falls back",
			err:  errors.New("some provider failure"),
			want: "unknown",
		},
		{
			name: "nil falls back",
			err:  nil,
			want: "unknown",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := event.ErrKind(tt.err); got != tt.want {
				t.Errorf("ErrKind() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestErrKindMatchesWrapped proves ErrKind uses errors.As (not a bare type switch),
// so a wrapped known error is still classified by its concrete kind.
func TestErrKindMatchesWrapped(t *testing.T) {
	t.Parallel()
	wrapped := wrapErr{cause: &event.ToolLimitError{}}
	if got := event.ErrKind(wrapped); got != "tool_limit" {
		t.Errorf("ErrKind(wrapped ToolLimitError) = %q, want %q", got, "tool_limit")
	}
}

// wrapErr is a tiny Unwrap-able wrapper used only to prove ErrKind unwraps.
type wrapErr struct{ cause error }

func (w wrapErr) Error() string { return "wrapped: " + w.cause.Error() }
func (w wrapErr) Unwrap() error { return w.cause }
