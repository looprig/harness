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
