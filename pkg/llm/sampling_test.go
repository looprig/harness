package llm

import "testing"

func TestSamplingCloneIsDeep(t *testing.T) {
	t.Parallel()
	temp := 0.7
	topP := 0.9
	max := 4096
	orig := Sampling{Temperature: &temp, TopP: &topP, MaxTokens: &max, Stop: []string{"a"}, Effort: EffortHigh}

	clone := orig.Clone()
	*clone.Temperature = 0.1
	*clone.TopP = 0.5
	*clone.MaxTokens = 1
	clone.Stop[0] = "b"

	if *orig.Temperature != 0.7 {
		t.Errorf("clone mutated original Temperature: got %v", *orig.Temperature)
	}
	if *orig.TopP != 0.9 {
		t.Errorf("clone mutated original TopP: got %v", *orig.TopP)
	}
	if *orig.MaxTokens != 4096 {
		t.Errorf("clone mutated original MaxTokens: got %v", *orig.MaxTokens)
	}
	if orig.Stop[0] != "a" {
		t.Errorf("clone mutated original Stop: got %v", orig.Stop[0])
	}
}

func TestSamplingCloneNilSafe(t *testing.T) {
	t.Parallel()
	clone := Sampling{Effort: EffortLow}.Clone()
	if clone.Temperature != nil || clone.MaxTokens != nil || clone.Stop != nil {
		t.Errorf("nil fields should clone to nil, got %+v", clone)
	}
	if clone.Effort != EffortLow {
		t.Errorf("Effort not preserved: got %q", clone.Effort)
	}
}
