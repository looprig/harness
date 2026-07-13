package hustle

import (
	"strconv"
	"testing"
)

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

func TestReasonAllowedExhaustive(t *testing.T) {
	t.Parallel()
	allowed := map[Stage]map[ReasonCode]bool{
		StageQueue:           {ReasonRejected: true, ReasonCanceled: true, ReasonTimeout: true, ReasonInternal: true},
		StageModelResolution: {ReasonCanceled: true, ReasonTimeout: true, ReasonModelResolution: true, ReasonInternal: true},
		StageInference:       {ReasonCanceled: true, ReasonTimeout: true, ReasonInference: true, ReasonInternal: true},
		StageOutput:          {ReasonCanceled: true, ReasonTimeout: true, ReasonInvalidOutput: true, ReasonInternal: true},
		StageTerminal:        {ReasonTimeout: true, ReasonTerminal: true, ReasonInternal: true},
		StageFinalization:    {ReasonTimeout: true, ReasonFinalization: true, ReasonInternal: true},
	}
	tests := make([]struct {
		name   string
		stage  Stage
		reason ReasonCode
		want   bool
	}, 0, int(StageFinalization+2)*int(ReasonInternal+2))
	for stage := StageUnknown; stage <= StageFinalization+1; stage++ {
		for reason := ReasonUnknown; reason <= ReasonInternal+1; reason++ {
			tests = append(tests, struct {
				name   string
				stage  Stage
				reason ReasonCode
				want   bool
			}{
				name:   "stage_" + strconv.Itoa(int(stage)) + "_reason_" + strconv.Itoa(int(reason)),
				stage:  stage,
				reason: reason,
				want:   allowed[stage][reason],
			})
		}
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := ReasonAllowed(testCase.stage, testCase.reason); got != testCase.want {
				t.Errorf("ReasonAllowed(%d,%d) = %v, want %v", testCase.stage, testCase.reason, got, testCase.want)
			}
		})
	}
}
