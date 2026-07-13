package hustle

import "testing"

func TestStageAndReasonCodeValidity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		stage       Stage
		reason      ReasonCode
		stageValid  bool
		reasonValid bool
	}{
		{name: "zero sentinels invalid", stage: StageUnknown, reason: ReasonUnknown},
		{name: "minimum values valid", stage: StageQueue, reason: ReasonRejected, stageValid: true, reasonValid: true},
		{name: "maximum values valid", stage: StageFinalization, reason: ReasonInternal, stageValid: true, reasonValid: true},
		{name: "above maximum invalid", stage: StageFinalization + 1, reason: ReasonInternal + 1},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := testCase.stage.Valid(); got != testCase.stageValid {
				t.Errorf("Stage.Valid() = %v, want %v", got, testCase.stageValid)
			}
			if got := testCase.reason.Valid(); got != testCase.reasonValid {
				t.Errorf("ReasonCode.Valid() = %v, want %v", got, testCase.reasonValid)
			}
		})
	}
}
