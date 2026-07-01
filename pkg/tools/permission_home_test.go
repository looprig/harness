package tools

import (
	"errors"
	"testing"
)

func TestHomeUnresolvableError(t *testing.T) {
	t.Parallel()
	err := error(&HomeUnresolvableError{Cause: errors.New("no $HOME")})
	var target *HomeUnresolvableError
	if !errors.As(err, &target) {
		t.Fatalf("errors.As failed for *HomeUnresolvableError")
	}
	if target.Cause == nil {
		t.Errorf("Cause not preserved")
	}
	if got := err.Error(); got == "" {
		t.Errorf("Error() is empty")
	}
}
