// Package exact provides deterministic, programmatic evaluators over the typed
// core/content conversation and eval evidence. Every evaluator here satisfies
// eval.Evaluator, declares a stable Descriptor with Method eval.MethodProgrammatic,
// and reaches its verdict from observable facts alone — never from a model judge.
//
// The evaluators are constructor-parameterized: each carries its own target
// (the required substrings, the tool name, the threshold) rather than reading
// it from the sample's scenario expectation. This matches the inline usage in
// the framework design, e.g. exact.RequiredTool("lookup_account").
//
// Discipline honored throughout this package:
//
//   - No conversation flattening except the text evaluators' private
//     flattenAssistantText projection, whose result never leaves the package.
//   - Every failure carries at least one eval.EvidenceRef that resolves to an
//     eval.Evidence the assessment also includes, so eval.Assessment.Validate
//     (which rejects dangling references) passes.
//   - Diagnostic strings never echo untrusted conversation or tool content; they
//     carry only safe counts, closed-enum tokens, and trusted constructor
//     arguments. Untrusted material is referenced by evidence (message index,
//     redacted excerpt, hash, byte count), never inlined.
//   - A vacuously configured evaluator (for example RequiredText() with no
//     substrings) yields an eval.Errored assessment with a config_error finding.
//     It never passes.
//   - Missing required evidence yields eval.Unverified via Descriptor.CheckRequires
//     before any evaluation — never a pass and never a fail.
package exact

import (
	"context"
	"strconv"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/eval"
)

// evaluatorRevision is the shared revision of the Phase-1 exact evaluators. A
// behavioral change to any evaluator must bump this so reports can distinguish
// verdicts produced by different logic.
const evaluatorRevision eval.Revision = "v1"

// codeConfigError is the finding code attached to the Errored assessment a
// misconfigured (vacuously constructed) evaluator produces.
const codeConfigError eval.FindingCode = "config_error"

// Config-error reasons. Each is a fixed, safe string — never derived from
// untrusted input — so it is safe to place in a finding message.
const (
	reasonNoSubstrings = "requires at least one substring"
)

// configErrored builds an Errored assessment for a misconfigured evaluator. An
// Errored status means the evaluator could not reach a verdict; it carries no
// measurement and never reads as a pass.
func configErrored(d eval.Descriptor, reason string) eval.Assessment {
	return eval.Errored(d, eval.Finding{
		Code:     codeConfigError,
		Severity: eval.SeverityHigh,
		Message:  reason,
	})
}

// diagnosticEvidence builds an evaluator_diagnostic evidence entry. code is a
// trusted constant and msg carries only safe counts or closed-enum tokens, so
// the redacted message never leaks untrusted content.
func diagnosticEvidence(id eval.EvidenceID, code eval.Name, msg string) eval.Evidence {
	return eval.Evidence{
		ID:   id,
		Kind: eval.EvidenceDiagnostic,
		Diagnostic: &eval.DiagnosticEvidence{
			Code:     code,
			Severity: eval.SeverityHigh,
			Message:  eval.RedactedExcerpt(msg),
		},
	}
}

// flattenAssistantText is the text evaluators' private projection: it joins the
// text blocks of every assistant message into one string, separating blocks with
// a newline. It is deliberately unexported and its result never leaves the
// package. Only *content.AIMessage *content.TextBlock content contributes — user,
// system, and tool-result text (including blocks nested inside a tool result),
// and assistant thinking blocks, are not assistant output and are excluded.
func flattenAssistantText(conv content.AgenticMessages) string {
	var b strings.Builder
	first := true
	for _, msg := range conv {
		ai, ok := msg.(*content.AIMessage)
		if !ok {
			continue
		}
		for _, blk := range ai.Blocks {
			tb, ok := blk.(*content.TextBlock)
			if !ok {
				continue
			}
			if !first {
				b.WriteByte('\n')
			}
			b.WriteString(tb.Text)
			first = false
		}
	}
	return b.String()
}

// assistantMessageText joins one assistant message's text blocks with a newline.
// It is used to locate the specific message a forbidden substring appears in.
func assistantMessageText(ai *content.AIMessage) string {
	var b strings.Builder
	first := true
	for _, blk := range ai.Blocks {
		tb, ok := blk.(*content.TextBlock)
		if !ok {
			continue
		}
		if !first {
			b.WriteByte('\n')
		}
		b.WriteString(tb.Text)
		first = false
	}
	return b.String()
}

// requiredText asserts that every configured substring appears in the assistant's
// text output. Matching is over the private flatten of assistant text; because
// UTF-8 is self-synchronizing, byte-level substring search (strings.Contains) is
// correct for Unicode substrings.
type requiredText struct {
	desc      eval.Descriptor
	required  []string
	configErr string
}

// RequiredText returns an evaluator asserting that every given substring appears
// somewhere in the assistant's text output. Constructing it with no substrings
// is a configuration error: the resulting evaluator yields Errored, never pass.
func RequiredText(substrings ...string) eval.Evaluator {
	e := requiredText{
		desc: eval.Descriptor{
			Name:        "exact/required_text",
			Revision:    evaluatorRevision,
			Method:      eval.MethodProgrammatic,
			Description: "asserts every required substring appears in the assistant's text output",
		},
		required: substrings,
	}
	if len(substrings) == 0 {
		e.configErr = reasonNoSubstrings
	}
	return e
}

func (e requiredText) Descriptor() eval.Descriptor { return e.desc }

func (e requiredText) Evaluate(_ context.Context, s eval.Sample) (eval.Assessment, error) {
	if e.configErr != "" {
		return configErrored(e.desc, e.configErr), nil
	}
	if a, ok := e.desc.CheckRequires(s); !ok {
		return a, nil
	}
	flat := flattenAssistantText(s.Observation.Conversation)
	missing := 0
	for _, sub := range e.required {
		if !strings.Contains(flat, sub) {
			missing++
		}
	}
	if missing == 0 {
		return eval.Pass(e.desc), nil
	}
	summary := strconv.Itoa(missing) + " of " + strconv.Itoa(len(e.required)) + " required substrings absent"
	ev := diagnosticEvidence("exact/required_text/absent", "required_text_absent", summary)
	a := eval.Fail(e.desc, eval.Finding{
		Code:     codeRequiredTextMissing,
		Severity: eval.SeverityHigh,
		Message:  summary,
		Evidence: []eval.EvidenceRef{{Evidence: ev.ID}},
	})
	a.Evidence = []eval.Evidence{ev}
	return a, nil
}

// codeRequiredTextMissing identifies the required-substring-absent failure.
const codeRequiredTextMissing eval.FindingCode = "required_text_missing"

// forbiddenText asserts that no configured substring appears in the assistant's
// text output. It matches per assistant message so the offending message index
// can be cited as evidence; a match that would only span the boundary between two
// messages is therefore not a hit.
type forbiddenText struct {
	desc      eval.Descriptor
	forbidden []string
	configErr string
}

// ForbiddenText returns an evaluator asserting that none of the given substrings
// appears in the assistant's text output. Constructing it with no substrings is a
// configuration error: the resulting evaluator yields Errored, never pass.
func ForbiddenText(substrings ...string) eval.Evaluator {
	e := forbiddenText{
		desc: eval.Descriptor{
			Name:        "exact/forbidden_text",
			Revision:    evaluatorRevision,
			Method:      eval.MethodProgrammatic,
			Description: "asserts no forbidden substring appears in the assistant's text output",
		},
		forbidden: substrings,
	}
	if len(substrings) == 0 {
		e.configErr = reasonNoSubstrings
	}
	return e
}

func (e forbiddenText) Descriptor() eval.Descriptor { return e.desc }

func (e forbiddenText) Evaluate(_ context.Context, s eval.Sample) (eval.Assessment, error) {
	if e.configErr != "" {
		return configErrored(e.desc, e.configErr), nil
	}
	if a, ok := e.desc.CheckRequires(s); !ok {
		return a, nil
	}
	for idx, msg := range s.Observation.Conversation {
		ai, ok := msg.(*content.AIMessage)
		if !ok {
			continue
		}
		text := assistantMessageText(ai)
		for _, sub := range e.forbidden {
			if strings.Contains(text, sub) {
				return e.fail(idx), nil
			}
		}
	}
	return eval.Pass(e.desc), nil
}

// fail builds the forbidden-text failure, citing the offending message by index
// through a message-index evidence entry. The forbidden text itself is never
// echoed — only its location is disclosed.
func (e forbiddenText) fail(msgIndex int) eval.Assessment {
	idx := msgIndex
	ev := eval.Evidence{
		ID:           "exact/forbidden_text/hit",
		Kind:         eval.EvidenceMessageIndex,
		MessageIndex: &eval.MessageIndexRef{Index: msgIndex},
	}
	a := eval.Fail(e.desc, eval.Finding{
		Code:     codeForbiddenTextPresent,
		Severity: eval.SeverityHigh,
		Message:  "a forbidden substring appears in assistant message index " + strconv.Itoa(msgIndex),
		Evidence: []eval.EvidenceRef{{Evidence: ev.ID, MessageIndex: &idx}},
	})
	a.Evidence = []eval.Evidence{ev}
	return a
}

// codeForbiddenTextPresent identifies the forbidden-substring-present failure.
const codeForbiddenTextPresent eval.FindingCode = "forbidden_text_present"
