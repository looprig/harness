package loop

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// TestTryAck asserts the non-blocking reply helper over the surviving reply type
// (command.CancelResult — the submit outcome is now a published event, not a tryAck
// reply): a buffered(1) channel receives the value; a nil/unbuffered/already-full
// channel hits the default branch without blocking the caller (the actor). The
// unbuffered/nil/full cases would deadlock a blocking send, so a test that returns at
// all proves tryAck did not block.
func TestTryAck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		makeChan    func() chan command.CancelResult
		wantDeliver bool
	}{
		{
			name:        "buffered(1) delivers",
			makeChan:    func() chan command.CancelResult { return make(chan command.CancelResult, 1) },
			wantDeliver: true,
		},
		{
			name:        "nil channel hits default without blocking",
			makeChan:    func() chan command.CancelResult { return nil },
			wantDeliver: false,
		},
		{
			name:        "unbuffered channel with no reader hits default without blocking",
			makeChan:    func() chan command.CancelResult { return make(chan command.CancelResult) },
			wantDeliver: false,
		},
		{
			name: "already-full buffered(1) hits default without blocking",
			makeChan: func() chan command.CancelResult {
				ch := make(chan command.CancelResult, 1)
				ch <- command.Cancelled{} // pre-fill
				return ch
			},
			wantDeliver: false,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ch := tt.makeChan()
			want := command.AlreadyCommitted{TurnID: mustID(t)}

			// If tryAck blocked, this call would never return and the test would
			// time out — so reaching the next line already proves non-blocking.
			tryAck[command.CancelResult](ch, want)

			if !tt.wantDeliver {
				return
			}
			select {
			case got := <-ch:
				ac, ok := got.(command.AlreadyCommitted)
				if !ok || ac != want {
					t.Errorf("delivered %+v, want %+v", got, want)
				}
			default:
				t.Error("buffered(1) channel did not receive the value")
			}
		})
	}
}

func mustID(t *testing.T) uuid.UUID {
	t.Helper()
	u, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return u
}
