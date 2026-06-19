package loop

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// TestTryAck asserts the non-blocking reply helper: a buffered(1) channel
// receives the value; a nil/unbuffered/already-full channel hits the default
// branch without blocking the caller (the actor). The unbuffered/nil/full cases
// would deadlock a blocking send, so a test that returns at all proves tryAck did
// not block.
func TestTryAck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		makeChan    func() chan command.Disposition
		wantDeliver bool
	}{
		{
			name:        "buffered(1) delivers",
			makeChan:    func() chan command.Disposition { return make(chan command.Disposition, 1) },
			wantDeliver: true,
		},
		{
			name:        "nil channel hits default without blocking",
			makeChan:    func() chan command.Disposition { return nil },
			wantDeliver: false,
		},
		{
			name:        "unbuffered channel with no reader hits default without blocking",
			makeChan:    func() chan command.Disposition { return make(chan command.Disposition) },
			wantDeliver: false,
		},
		{
			name: "already-full buffered(1) hits default without blocking",
			makeChan: func() chan command.Disposition {
				ch := make(chan command.Disposition, 1)
				ch <- command.TurnRejected{Reason: command.RejectBusy} // pre-fill
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
			want := command.Started{TurnID: mustID(t), InputID: mustID(t)}

			// If tryAck blocked, this call would never return and the test would
			// time out — so reaching the next line already proves non-blocking.
			tryAck[command.Disposition](ch, want)

			if !tt.wantDeliver {
				return
			}
			select {
			case got := <-ch:
				if got != command.Disposition(want) {
					t.Errorf("delivered %+v, want %+v", got, want)
				}
			default:
				t.Error("buffered(1) channel did not receive the value")
			}
		})
	}
}

// TestTryAckCancelResult exercises tryAck with the other reply type (CancelResult)
// to prove the generic helper works for every loop reply channel, not just
// Disposition.
func TestTryAckCancelResult(t *testing.T) {
	t.Parallel()
	ch := make(chan command.CancelResult, 1)
	tryAck[command.CancelResult](ch, command.Cancelled{})
	select {
	case got := <-ch:
		if _, ok := got.(command.Cancelled); !ok {
			t.Errorf("got %T, want Cancelled", got)
		}
	default:
		t.Error("CancelResult not delivered")
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
