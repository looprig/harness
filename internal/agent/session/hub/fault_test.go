package hub

import (
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
)

// errAppend is a leaf cause used to populate SessionPersistenceFault.Cause in tests.
var errAppend = errors.New("append failed")

// TestSessionPersistenceFaultError covers the typed fault's Error/Unwrap surface:
// it names the offending event type, chains the underlying cause, and errors.As
// recovers the concrete type with both fields intact.
func TestSessionPersistenceFaultError(t *testing.T) {
	t.Parallel()
	ev := event.SessionActive{Header: event.Header{Coordinates: identity.Coordinates{}}}
	tests := []struct {
		name        string
		fault       *SessionPersistenceFault
		wantContain string
		wantCause   error
	}{
		{
			name:        "with cause chains the underlying error",
			fault:       &SessionPersistenceFault{Event: ev, Cause: errAppend},
			wantContain: errAppend.Error(),
			wantCause:   errAppend,
		},
		{
			name:        "nil cause still reports the offending event",
			fault:       &SessionPersistenceFault{Event: ev, Cause: nil},
			wantContain: "SessionActive",
			wantCause:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			msg := tt.fault.Error()
			if !contains(msg, tt.wantContain) {
				t.Errorf("Error() = %q, want it to contain %q", msg, tt.wantContain)
			}
			if got := errors.Unwrap(tt.fault); !errors.Is(got, tt.wantCause) {
				t.Errorf("Unwrap() = %v, want %v", got, tt.wantCause)
			}
			// errors.As recovers the concrete type with its fields.
			var as *SessionPersistenceFault
			if !errors.As(error(tt.fault), &as) {
				t.Fatalf("errors.As did not match *SessionPersistenceFault")
			}
			if as.Event == nil {
				t.Errorf("recovered fault has nil Event")
			}
		})
	}
}

// TestSessionPersistenceFaultEventType proves Error names the dynamic event type so
// an operator log distinguishes a failed SessionStopped append from a SessionIdle one.
func TestSessionPersistenceFaultEventType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ev   event.Event
		want string
	}{
		{name: "session active", ev: event.SessionActive{}, want: "SessionActive"},
		{name: "session idle", ev: event.SessionIdle{}, want: "SessionIdle"},
		{name: "session stopped", ev: event.SessionStopped{}, want: "SessionStopped"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &SessionPersistenceFault{Event: tt.ev, Cause: errAppend}
			if !contains(f.Error(), tt.want) {
				t.Errorf("Error() = %q, want it to name %q", f.Error(), tt.want)
			}
		})
	}
}
