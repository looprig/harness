package loop

import "testing"

func TestEngineZeroValueIsNative(t *testing.T) {
	t.Parallel()
	if EngineNative != 0 {
		t.Fatalf("EngineNative = %d, want 0 (zero value must be native)", EngineNative)
	}
}
