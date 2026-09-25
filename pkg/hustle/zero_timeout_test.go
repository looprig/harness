package hustle

import (
	"testing"
	"time"
)

func TestZeroTimeoutDefinesNoDeadline(t *testing.T) {
	t.Parallel()
	d, err := Define(replaceOption(validCurrentOptions(), 2, WithTimeout(0))...)
	if err != nil {
		t.Fatalf("Define(WithTimeout(0)) = %v, want nil", err)
	}
	if got := d.Timeout(); got != 0 {
		t.Fatalf("Timeout() = %v, want 0", got)
	}
	descriptor := d.Descriptor()
	if descriptor.TimeoutNanos != 0 {
		t.Fatalf("TimeoutNanos = %d, want 0", descriptor.TimeoutNanos)
	}
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("Descriptor().Validate() = %v, want nil", err)
	}
	if _, err := Define(replaceOption(validCurrentOptions(), 2, WithTimeout(-time.Nanosecond))...); err == nil {
		t.Fatal("Define(WithTimeout(-1ns)) = nil, want invalid timeout")
	}
}
