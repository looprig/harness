package loop

import "testing"

func TestEngineZeroValueIsNative(t *testing.T) {
	t.Parallel()
	var c Config
	if c.Engine != EngineNative {
		t.Fatalf("zero Config.Engine = %v, want EngineNative", c.Engine)
	}
	if EngineNative != 0 {
		t.Fatalf("EngineNative = %d, want 0 (zero value must be native)", EngineNative)
	}
}
