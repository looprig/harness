package event_test

import (
	"testing"

	"github.com/looprig/harness/pkg/event"
)

func TestEventErrorMessages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"empty response", &event.EmptyResponseError{}, "loop: empty response from provider"},
		{"tool limit", &event.ToolLimitError{Iterations: 32, MaxIterations: 100, Calls: 101, MaxCalls: 200}, "tool limit reached: 32/100 steps, 101/200 calls"},
		{"turn panic", &event.TurnPanicError{Detail: "x"}, "loop: panic in turn goroutine: x"},
		{"turn panic empty detail", &event.TurnPanicError{Detail: ""}, "loop: panic in turn goroutine: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}
