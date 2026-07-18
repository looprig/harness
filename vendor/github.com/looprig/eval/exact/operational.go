package exact

import (
	"context"
	"math"
	"strconv"
	"time"

	"github.com/looprig/eval"
)

// Config-error reasons for the operational evaluators.
const (
	reasonThresholdRange   = "error-rate threshold must be within [0,1]"
	reasonNonPositiveLimit = "duration limit must be positive"
)

// Finding codes and measurement names for the operational evaluators.
const (
	codeToolErrorRateExceeded eval.FindingCode = "tool_error_rate_exceeded"
	codeMaxDurationExceeded   eval.FindingCode = "max_duration_exceeded"

	measToolErrorRate eval.Name = "tool_error_rate"
	measDurationSecs  eval.Name = "duration_seconds"
)

// --- ToolErrorRate ---

// toolErrorRate measures the proportion of tool operations that errored, taken
// from tool-operation evidence, and optionally fails when that ratio exceeds a
// configured threshold. It requires EvidenceToolOperation, so a sample with no
// recorded tool operations (a structurally undefined rate) yields Unverified via
// CheckRequires rather than a division by zero — never a pass.
type toolErrorRate struct {
	desc      eval.Descriptor
	threshold *float64 // nil means "measure only, never fail"
	configErr string
}

// RateOption configures a ToolErrorRate evaluator.
type RateOption func(*toolErrorRate)

// MaxErrorRate sets the maximum acceptable tool error rate. When the observed
// rate strictly exceeds r the evaluator fails. r must lie within [0,1]; a value
// outside that range (or NaN) is a configuration error surfaced as Errored.
func MaxErrorRate(r float64) RateOption {
	return func(t *toolErrorRate) {
		v := r
		t.threshold = &v
	}
}

// ToolErrorRate returns an evaluator that measures the errored-tool-operation
// ratio. With no MaxErrorRate option it only reports the measurement and passes;
// with one it fails when the ratio exceeds the threshold.
func ToolErrorRate(opts ...RateOption) eval.Evaluator {
	e := toolErrorRate{
		desc: eval.Descriptor{
			Name:        "exact/tool_error_rate",
			Revision:    evaluatorRevision,
			Method:      eval.MethodProgrammatic,
			Description: "measures the proportion of tool operations that errored",
			Requires:    []eval.EvidenceKind{eval.EvidenceToolOperation},
		},
	}
	for _, o := range opts {
		o(&e)
	}
	if e.threshold != nil {
		r := *e.threshold
		if math.IsNaN(r) || r < 0 || r > 1 {
			e.configErr = reasonThresholdRange
		}
	}
	return e
}

func (e toolErrorRate) Descriptor() eval.Descriptor { return e.desc }

func (e toolErrorRate) Evaluate(_ context.Context, s eval.Sample) (eval.Assessment, error) {
	if e.configErr != "" {
		return configErrored(e.desc, e.configErr), nil
	}
	if a, ok := e.desc.CheckRequires(s); !ok {
		return a, nil
	}
	total := 0
	var errored []eval.Evidence
	for _, ev := range s.Observation.Trace.Evidence {
		if ev.Kind != eval.EvidenceToolOperation || ev.ToolOperation == nil {
			continue
		}
		total++
		if ev.ToolOperation.IsError {
			errored = append(errored, ev)
		}
	}
	// CheckRequires guarantees at least one tool-operation evidence, so total >= 1
	// and the ratio is well defined.
	rate := float64(len(errored)) / float64(total)
	m := eval.Measurement{Name: measToolErrorRate, Value: rate, Unit: eval.UnitRatio}

	if e.threshold != nil && rate > *e.threshold {
		refs := make([]eval.EvidenceRef, len(errored))
		for i, ev := range errored {
			refs[i] = eval.EvidenceRef{Evidence: ev.ID}
		}
		a := eval.Fail(e.desc, eval.Finding{
			Code:     codeToolErrorRateExceeded,
			Severity: eval.SeverityHigh,
			Message: "tool error rate " + formatFloat(rate) +
				" exceeds limit " + formatFloat(*e.threshold),
			Evidence: refs,
		})
		a.Measurements = []eval.Measurement{m}
		a.Evidence = errored
		return a, nil
	}
	return eval.Pass(e.desc, m), nil
}

// --- MaxDuration ---

// maxDuration measures the largest recorded timed span from timing evidence and
// fails when it exceeds a configured limit. It requires EvidenceTiming, so a
// sample without recorded timing yields Unverified via CheckRequires — never a
// pass. A non-positive limit is a configuration error surfaced as Errored.
type maxDuration struct {
	desc      eval.Descriptor
	limit     time.Duration
	configErr string
}

// MaxDuration returns an evaluator that fails when the largest recorded timed
// span exceeds limit. limit must be positive.
func MaxDuration(limit time.Duration) eval.Evaluator {
	e := maxDuration{
		desc: eval.Descriptor{
			Name:        "exact/max_duration",
			Revision:    evaluatorRevision,
			Method:      eval.MethodProgrammatic,
			Description: "fails when the longest recorded timed span exceeds a limit",
			Requires:    []eval.EvidenceKind{eval.EvidenceTiming},
		},
		limit: limit,
	}
	if limit <= 0 {
		e.configErr = reasonNonPositiveLimit
	}
	return e
}

func (e maxDuration) Descriptor() eval.Descriptor { return e.desc }

func (e maxDuration) Evaluate(_ context.Context, s eval.Sample) (eval.Assessment, error) {
	if e.configErr != "" {
		return configErrored(e.desc, e.configErr), nil
	}
	if a, ok := e.desc.CheckRequires(s); !ok {
		return a, nil
	}
	var longest time.Duration
	var longestEv eval.Evidence
	found := false
	for _, ev := range s.Observation.Trace.Evidence {
		if ev.Kind != eval.EvidenceTiming || ev.Timing == nil {
			continue
		}
		if !found || ev.Timing.Duration > longest {
			longest = ev.Timing.Duration
			longestEv = ev
			found = true
		}
	}
	// CheckRequires guarantees at least one timing evidence entry.
	m := eval.Measurement{Name: measDurationSecs, Value: longest.Seconds(), Unit: eval.UnitSecond}

	if longest > e.limit {
		a := eval.Fail(e.desc, eval.Finding{
			Code:     codeMaxDurationExceeded,
			Severity: eval.SeverityHigh,
			Message: "duration " + formatFloat(longest.Seconds()) +
				"s exceeds limit " + formatFloat(e.limit.Seconds()) + "s",
			Evidence: []eval.EvidenceRef{{Evidence: longestEv.ID}},
		})
		a.Measurements = []eval.Measurement{m}
		a.Evidence = []eval.Evidence{longestEv}
		return a, nil
	}
	return eval.Pass(e.desc, m), nil
}

// formatFloat renders a float64 compactly for a safe diagnostic message. The
// value is a bounded ratio or a duration in seconds — never untrusted content.
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
