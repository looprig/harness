package gate

import (
	"context"
	"fmt"

	"github.com/looprig/harness/pkg/tool"
)

// CurrentGrantVersion is the structural grant ABI version understood by Gate.
const CurrentGrantVersion uint16 = 1

// RuleMatcher checks independently stored deny and allow rules for one
// normalized requirement. Evaluator always checks every deny before consulting
// any allow.
type RuleMatcher interface {
	MatchesDeny(context.Context, tool.Requirement) (bool, error)
	MatchesAllow(context.Context, tool.Requirement) (bool, error)
}

// RuleWriter atomically persists a complete batch of displayed reusable allow
// candidates. Returning an error means none of the candidates were persisted.
type RuleWriter interface {
	WriteRules(context.Context, []tool.RuleCandidate) error
}

// GrantIssuer mints one structural, execution-bound grant token. The signature
// is intentionally dependency-free and is satisfied structurally by an
// enforcing executor without importing harness.
type GrantIssuer interface {
	GrantVersion() uint16
	IssueGrant(ctx context.Context, executionID, command, cwd, kind, scope, class, target string, expiryUnixMilli int64) (string, error)
}

// Evaluation is the result of evaluating one complete prepared request. Denied
// and Unmet are combined sets in original request order. Candidates contains
// every reusable candidate displayed for the unmet set.
type Evaluation struct {
	Denied     []tool.Requirement
	Unmet      []tool.Requirement
	Candidates []tool.RuleCandidate

	request           tool.Request
	grantRequirements []tool.Requirement
	candidates        []tool.RuleCandidate
}

// Resolution is the live result of applying an approval action. Grants are
// deliberately excluded from JSON: minted tokens may only travel through the
// prepared execution path, never a prompt, display, journal, or audit payload.
type Resolution struct {
	Approved bool     `json:"approved"`
	Grants   []string `json:"-"`
}

// Evaluator combines structural access, durable rules, approval persistence,
// and post-decision grant issuance without importing an enforcement package.
type Evaluator struct {
	access  AccessBindings
	matcher RuleMatcher
	writer  RuleWriter
	issuer  GrantIssuer
}

// EvaluationErrorKind classifies a fail-closed evaluation dependency failure.
type EvaluationErrorKind string

const (
	EvaluationRuleMatchFailed         EvaluationErrorKind = "rule_match_failed"
	EvaluationDenied                  EvaluationErrorKind = "denied"
	EvaluationActionInvalid           EvaluationErrorKind = "action_invalid"
	EvaluationWriterMissing           EvaluationErrorKind = "writer_missing"
	EvaluationWriteFailed             EvaluationErrorKind = "write_failed"
	EvaluationIssuerMissing           EvaluationErrorKind = "issuer_missing"
	EvaluationGrantVersionUnsupported EvaluationErrorKind = "grant_version_unsupported"
	EvaluationGrantFailed             EvaluationErrorKind = "grant_failed"
)

// EvaluationError reports a dependency failure during evaluation.
type EvaluationError struct {
	Kind        EvaluationErrorKind
	Requirement string
	Cause       error
}

func (e *EvaluationError) Error() string {
	return fmt.Sprintf("gate: evaluate %s for %s: %v", e.Kind, e.Requirement, e.Cause)
}

func (e *EvaluationError) Unwrap() error { return e.Cause }

// NewEvaluator validates access routing, rejects an unsupported grant ABI, and
// binds the optional rule, writer, and grant services. Nil services remain
// fail-closed at the point their capability is needed.
func NewEvaluator(bindings []AccessBinding, matcher RuleMatcher, writer RuleWriter, issuer GrantIssuer) (*Evaluator, error) {
	access, err := NewAccessBindings(bindings)
	if err != nil {
		return nil, err
	}
	if issuer != nil {
		if version := issuer.GrantVersion(); version != CurrentGrantVersion {
			return nil, &EvaluationError{
				Kind:  EvaluationGrantVersionUnsupported,
				Cause: fmt.Errorf("got %d, want %d", version, CurrentGrantVersion),
			}
		}
	}
	return &Evaluator{access: access, matcher: matcher, writer: writer, issuer: issuer}, nil
}

// Evaluate resolves every access state, then every stored deny, then every
// stored allow. It never serializes approval gates: all unmatched gated
// requirements are returned together as one combined unmet set.
func (e *Evaluator) Evaluate(ctx context.Context, request tool.Request) (Evaluation, error) {
	if err := ctx.Err(); err != nil {
		return Evaluation{}, err
	}
	if err := tool.ValidateRequest(request); err != nil {
		return Evaluation{}, err
	}
	result := Evaluation{request: request.Clone()}
	gated := make([]tool.Requirement, 0, len(request.Requirements))
	for _, requirement := range request.Requirements {
		access, err := e.access.AccessFor(requirement)
		if err != nil {
			return Evaluation{}, err
		}
		switch access {
		case AccessDeny:
			result.Denied = append(result.Denied, requirement.Clone())
		case AccessGated:
			gated = append(gated, requirement)
		case AccessAllow:
			// An enforcing source configured Allow needs no grant token.
		}
	}
	if len(result.Denied) != 0 {
		return result, nil
	}

	for _, requirement := range gated {
		if e.matcher == nil {
			continue
		}
		matched, err := e.matcher.MatchesDeny(ctx, requirement)
		if err != nil {
			return Evaluation{}, &EvaluationError{Kind: EvaluationRuleMatchFailed, Requirement: requirement.Kind, Cause: err}
		}
		if matched {
			result.Denied = append(result.Denied, requirement.Clone())
		}
	}
	if len(result.Denied) != 0 {
		return result, nil
	}

	for _, requirement := range gated {
		matched := false
		if e.matcher != nil {
			var err error
			matched, err = e.matcher.MatchesAllow(ctx, requirement)
			if err != nil {
				return Evaluation{}, &EvaluationError{Kind: EvaluationRuleMatchFailed, Requirement: requirement.Kind, Cause: err}
			}
		}
		result.grantRequirements = append(result.grantRequirements, requirement.Clone())
		if matched {
			continue
		}
		result.Unmet = append(result.Unmet, requirement.Clone())
		candidates := cloneCandidates(requirement.Candidates)
		result.Candidates = append(result.Candidates, candidates...)
		result.candidates = append(result.candidates, candidates...)
	}
	return result, nil
}

// Resolve applies one exact approval action. Workspace approval persists the
// entire displayed candidate set in one RuleWriter call before any grant is
// minted; a persistence failure blocks execution. Approve writes nothing. Deny
// and evaluated denials mint no grants.
func (e *Evaluator) Resolve(ctx context.Context, evaluation Evaluation, action ApprovalAction) (Resolution, error) {
	if err := ctx.Err(); err != nil {
		return Resolution{}, err
	}
	if len(evaluation.Denied) != 0 {
		if action == ApprovalDeny {
			return Resolution{}, nil
		}
		return Resolution{}, &EvaluationError{Kind: EvaluationDenied, Requirement: evaluation.Denied[0].Kind, Cause: fmt.Errorf("configured or stored deny")}
	}
	switch action {
	case ApprovalDeny:
		return Resolution{}, nil
	case ApprovalApprove:
		// Once approval is intentionally ephemeral: nothing is persisted.
	case ApprovalApproveAlwaysWorkspace:
		if e.writer == nil {
			return Resolution{}, &EvaluationError{Kind: EvaluationWriterMissing, Cause: fmt.Errorf("workspace rule writer is not configured")}
		}
		if err := e.writer.WriteRules(ctx, cloneCandidates(evaluation.candidates)); err != nil {
			return Resolution{}, &EvaluationError{Kind: EvaluationWriteFailed, Cause: err}
		}
	default:
		return Resolution{}, &EvaluationError{Kind: EvaluationActionInvalid, Cause: fmt.Errorf("unknown action %q", action)}
	}

	grantCount := 0
	for _, requirement := range evaluation.grantRequirements {
		if requirement.GrantClass != "" {
			grantCount++
		}
	}
	if grantCount == 0 {
		return Resolution{Approved: true}, nil
	}
	if e.issuer == nil {
		return Resolution{}, &EvaluationError{Kind: EvaluationIssuerMissing, Cause: fmt.Errorf("grant issuer is not configured")}
	}
	grants := make([]string, 0, grantCount)
	for _, requirement := range evaluation.grantRequirements {
		if requirement.GrantClass == "" {
			continue
		}
		token, err := e.issuer.IssueGrant(
			ctx,
			evaluation.request.ExecutionID,
			evaluation.request.Command,
			evaluation.request.WorkingDirectory,
			requirement.Kind,
			requirement.Scope,
			requirement.GrantClass,
			requirement.GrantTarget,
			evaluation.request.ExpiresAtUnixMilli,
		)
		if err != nil {
			return Resolution{}, &EvaluationError{Kind: EvaluationGrantFailed, Requirement: requirement.Kind, Cause: err}
		}
		if token == "" {
			return Resolution{}, &EvaluationError{Kind: EvaluationGrantFailed, Requirement: requirement.Kind, Cause: fmt.Errorf("issuer returned empty token")}
		}
		grants = append(grants, token)
	}
	return Resolution{Approved: true, Grants: grants}, nil
}

func cloneCandidates(candidates []tool.RuleCandidate) []tool.RuleCandidate {
	if candidates == nil {
		return nil
	}
	out := make([]tool.RuleCandidate, len(candidates))
	copy(out, candidates)
	return out
}
