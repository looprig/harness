package hub

import (
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// TestSubscriptionLossErrorError covers the typed loss error's message rendering,
// both with and without a wrapped cause, and that Unwrap exposes the cause.
func TestSubscriptionLossErrorError(t *testing.T) {
	t.Parallel()
	cause := errors.New("boom")
	tests := []struct {
		name    string
		err     *SubscriptionLossError
		wantSub string
		cause   error
	}{
		{
			name:    "no cause",
			err:     &SubscriptionLossError{DroppedClass: event.Enduring},
			wantSub: "subscription lost",
			cause:   nil,
		},
		{
			name:    "with cause",
			err:     &SubscriptionLossError{DroppedClass: event.Enduring, Cause: cause},
			wantSub: "boom",
			cause:   cause,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got == "" {
				t.Fatalf("Error() returned empty string")
			} else if !contains(got, tt.wantSub) {
				t.Errorf("Error() = %q, want substring %q", got, tt.wantSub)
			}
			if !errors.Is(tt.err, tt.cause) && tt.cause != nil {
				t.Errorf("errors.Is(err, cause) = false, want true")
			}
		})
	}
}

// TestEventSubscriptionClose covers the lifecycle of a subscription handle:
// Close is idempotent, Err is nil after an intentional Close, and Events is
// closed after Close.
func TestEventSubscriptionClose(t *testing.T) {
	t.Parallel()
	sub := newSubscription(event.EventFilter{}, nil)

	if err := sub.Err(); err != nil {
		t.Fatalf("Err() before close = %v, want nil", err)
	}

	if err := sub.Close(); err != nil {
		t.Fatalf("first Close() = %v, want nil", err)
	}
	// Idempotent: second Close is a no-op, still nil.
	if err := sub.Close(); err != nil {
		t.Fatalf("second Close() = %v, want nil", err)
	}
	if err := sub.Err(); err != nil {
		t.Errorf("Err() after intentional Close = %v, want nil", err)
	}
	// Events channel is closed.
	if _, ok := <-sub.Events(); ok {
		t.Errorf("Events() yielded a value after Close, want closed channel")
	}
}

// TestEventSubscriptionForceLoss covers the hub-forced termination path: a
// forced loss records the typed error in Err(), closes Events(), and is
// idempotent (a later intentional Close does not clobber the recorded error,
// and a force after Close does not overwrite the nil/Err either).
func TestEventSubscriptionForceLoss(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		afterFunc func(s *EventSubscription)
		wantClass event.Class
	}{
		{
			name:      "force loss alone records typed error",
			afterFunc: func(s *EventSubscription) {},
			wantClass: event.Enduring,
		},
		{
			name:      "Close after force loss keeps the typed error",
			afterFunc: func(s *EventSubscription) { _ = s.Close() },
			wantClass: event.Enduring,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sub := newSubscription(event.EventFilter{}, nil)
			lossErr := &SubscriptionLossError{DroppedClass: tt.wantClass}
			sub.fail(lossErr)
			tt.afterFunc(sub)

			gotErr := sub.Err()
			var lerr *SubscriptionLossError
			if !errors.As(gotErr, &lerr) {
				t.Fatalf("Err() = %v, want *SubscriptionLossError", gotErr)
			}
			if lerr.DroppedClass != tt.wantClass {
				t.Errorf("DroppedClass = %v, want %v", lerr.DroppedClass, tt.wantClass)
			}
			if _, ok := <-sub.Events(); ok {
				t.Errorf("Events() yielded a value after fail, want closed channel")
			}
		})
	}
}

// TestEventSubscriptionFailThenCloseSafe proves fail and Close can be called in
// either order without panicking on a double channel close and that the first
// terminal cause wins.
func TestEventSubscriptionFailThenCloseSafe(t *testing.T) {
	t.Parallel()
	// Intentional Close first, then a (losing) fail: the nil cause stands.
	sub := newSubscription(event.EventFilter{}, nil)
	if err := sub.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	sub.fail(&SubscriptionLossError{DroppedClass: event.Enduring})
	if err := sub.Err(); err != nil {
		t.Errorf("Err() after Close-then-fail = %v, want nil (Close won)", err)
	}
}

// TestEventSubscriptionFilter confirms the subscription retains the filter it was
// built with so the hub can evaluate ShouldDeliver against it.
func TestEventSubscriptionFilter(t *testing.T) {
	t.Parallel()
	loopID := mustID(t)
	want := event.EventFilter{Ephemeral: event.LoopScope{Loops: map[uuid.UUID]struct{}{loopID: {}}}}
	sub := newSubscription(want, nil)
	if !sub.filter.Ephemeral.Matches(loopID) {
		t.Errorf("subscription filter did not retain the configured ephemeral scope")
	}
}

// contains is a tiny substring helper to avoid importing strings just for tests.
func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// mustID mints a non-zero UUID for tests or fails fast.
func mustID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}
