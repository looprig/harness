package evaltest

import (
	"strconv"
	"strings"

	"github.com/looprig/eval"
)

// This file holds the concise, deterministic rendering of a report and its parts
// for Go test output. Rendering is deliberately narrow: it emits only facts that
// are safe to place in a test log or a CI transcript — statuses, safe numeric
// measurements, finding codes and severities, and evidence *references* reduced
// to a count and safe integer message indexes.
//
// It NEVER renders untrusted content: conversation text, tool arguments or
// output, judge or evaluator prose (Finding.Message, DiagnosticEvidence.Message),
// redacted excerpts, content hashes, or caller-supplied evidence IDs. The eval
// package itself withholds caller-supplied identifiers from diagnostics because a
// hostile value must not leak through them; this presenter honours the same rule.
//
// Ordering is stable: samples arrive in the runner's deterministic
// scenario-major order, assessments in evaluator order, and measurements and
// findings in the order the evaluator authored them. The one map in the model —
// Summary.Assessments — is rendered through a fixed status order, never by
// ranging the map.

// statusOrder is the fixed, deterministic order in which assessment statuses are
// rendered in a summary. It is independent of severity and never depends on map
// iteration.
var statusOrder = [...]eval.AssessmentStatus{
	eval.StatusPass,
	eval.StatusFail,
	eval.StatusUnverified,
	eval.StatusError,
	eval.StatusSkipped,
}

// scenarioName derives the subtest (and flat-line) name for a sample from its
// stable identity: the scenario ID, plus a trial suffix when more than one trial
// per scenario is present. Both components are validated domain identifiers and
// safe to render.
func scenarioName(s eval.SampleReport) string {
	name := s.ScenarioID
	if name == "" {
		name = "(unnamed)"
	}
	if s.TrialIndex > 0 {
		name += "#" + strconv.Itoa(s.TrialIndex)
	}
	return name
}

// renderSummary renders a report summary as a single concise, deterministic line.
// Status counts are emitted in the fixed statusOrder, so the line is identical
// across runs regardless of map iteration order.
func renderSummary(s eval.Summary) string {
	var b strings.Builder
	b.WriteString("samples=")
	b.WriteString(strconv.Itoa(s.Samples))
	b.WriteString(" target_errors=")
	b.WriteString(strconv.Itoa(s.TargetErrors))
	for _, st := range statusOrder {
		if n := s.Assessments[st]; n > 0 {
			b.WriteByte(' ')
			b.WriteString(string(st))
			b.WriteByte('=')
			b.WriteString(strconv.Itoa(n))
		}
	}
	return b.String()
}

// renderAssessment renders one assessment as a single concise line: the
// evaluator identity, the status, the (optional) duration, each safe numeric
// measurement, and each finding reduced to its code, severity, and a concise
// evidence reference. It never renders the finding message or any untrusted
// content.
func renderAssessment(a eval.Assessment) string {
	var b strings.Builder
	b.WriteString(string(a.Evaluator))
	b.WriteByte('@')
	b.WriteString(string(a.Revision))
	b.WriteString(" status=")
	b.WriteString(string(a.Status))
	if a.Duration > 0 {
		b.WriteString(" dur=")
		b.WriteString(a.Duration.String())
	}
	for _, m := range a.Measurements {
		b.WriteString(" [")
		b.WriteString(string(m.Name))
		b.WriteByte('=')
		b.WriteString(strconv.FormatFloat(m.Value, 'g', -1, 64))
		b.WriteByte(' ')
		b.WriteString(string(m.Unit))
		b.WriteByte(']')
	}
	for _, f := range a.Findings {
		b.WriteString(" {")
		b.WriteString(string(f.Code))
		b.WriteByte('/')
		b.WriteString(string(f.Severity))
		b.WriteString(renderFindingEvidence(f.Evidence))
		b.WriteByte('}')
	}
	return b.String()
}

// renderFindingEvidence renders a finding's evidence references concisely: the
// count of references and, when any reference addresses a message, the safe
// integer message indexes in reference order. The caller-supplied EvidenceID is
// deliberately withheld — it may be untrusted.
func renderFindingEvidence(refs []eval.EvidenceRef) string {
	if len(refs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(" ev=")
	b.WriteString(strconv.Itoa(len(refs)))
	msgs := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.MessageIndex != nil {
			msgs = append(msgs, strconv.Itoa(*r.MessageIndex))
		}
	}
	if len(msgs) > 0 {
		b.WriteString(" msg=[")
		b.WriteString(strings.Join(msgs, ","))
		b.WriteByte(']')
	}
	return b.String()
}
