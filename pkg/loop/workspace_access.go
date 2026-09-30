package loop

import (
	"errors"

	"github.com/looprig/harness/pkg/gate"
)

// WorkspaceAccess configures WithWorkspaceAccess. Only Roots is required; a
// nil Approver or Rules takes the default described on each field.
type WorkspaceAccess struct {
	// Roots are the directories the loop may read and, with approval, write.
	// Each must be an existing directory other than the filesystem root; a
	// relative root is resolved against the working directory when Define
	// runs, and symlinks are resolved then too.
	Roots []string
	// Approver answers each write approval. Nil selects GateApprover(): the
	// loop's own durable permission gate, answered by the TUI, by a headless
	// session's RespondGate, or, on a Host, by Factory's gate_response.
	Approver gate.Approver
	// Rules remembers "Approve always for this workspace" answers. Nil gives
	// every bound loop its own gate.NewSessionRules(): an "always" answer then
	// lasts for that loop in that session while the process runs, and never
	// reaches another session, another loop, or a restored session.
	Rules gate.RuleStore
}

// workspaceAccessState is the frozen WithWorkspaceAccess configuration.
type workspaceAccessState struct {
	access        *gate.WorkspaceAccess
	approver      gate.Approver
	rules         gate.RuleStore // nil: a fresh gate.SessionRules per bind
	rulesRevision string
}

// workspaceAccessIdentity is the PolicyRevision projection of a workspace
// gate. The approver is who answers, not what is allowed, so it is not
// hashed; in-memory rules contribute nothing, so an "always" answer never
// changes the loop's policy identity.
type workspaceAccessIdentity struct {
	Scheme        string
	Roots         []string
	RulesRevision string `json:",omitempty"`
}

func (w *workspaceAccessState) identity() workspaceAccessIdentity {
	return workspaceAccessIdentity{Scheme: "workspace-access/v1", Roots: w.access.Roots(), RulesRevision: w.rulesRevision}
}

func (w *workspaceAccessState) evaluator() (*gate.Evaluator, error) {
	rules := w.rules
	if rules == nil {
		rules = gate.NewSessionRules()
	}
	return gate.NewWorkspaceEvaluator(w.access, w.approver, rules)
}

// WithWorkspaceAccess installs an interactive access gate for an agent that
// works on files in Roots: a filesystem read within Roots runs, a filesystem
// write within Roots opens a permission gate (answered by the TUI, a headless
// Approver such as gate.ApproverFunc, or, on a Host, by Factory's
// gate_response), and everything else is denied: reads and writes outside
// every root, whole-host scopes, command execution and network access. A
// requirement of any other kind fails closed. The answer "Approve always for
// this workspace" is remembered in Rules, so later writes to the same file
// skip the prompt; a remembered rule can never widen access past Roots.
//
// Like WithReadOnlyAccess, the gate is lexical: it judges the canonical path a
// tool prepared and touches no filesystem, so it is sound only with trusted
// tools that resolve symlinks before preparing and confine their own I/O to
// the approved path (the standard EditFile and WriteFile do). It decides tool
// calls; it is not an OS sandbox.
//
// It is the loop's access gate, so combining it with WithAccessGate or
// WithReadOnlyAccess is a DefinitionDuplicateOption. To also gate commands or
// network, compose gate.NewInteractiveEvaluator yourself with
// gate.WorkspaceAccess.Bindings() minus the kinds you re-route, and install it
// with WithAccessGate and WithPolicyRevision.
//
// PolicyRevision is derived, so no WithPolicyRevision is required: it hashes
// "workspace-access/v1", the canonical roots and, when Rules implements
// gate.PolicyRevisioner, its revision, read once when the option is applied.
// The approver and in-memory rules contribute nothing, so an "always" answer
// never changes the loop's policy identity and a restored session does not
// report drift. Invalid roots, or a typed-nil Approver or Rules, fail Define
// with DefinitionInvalidAccessGate.
func WithWorkspaceAccess(access WorkspaceAccess) Option {
	roots := append([]string(nil), access.Roots...)
	return func(o *definitionOptions) error {
		approver, rules := access.Approver, access.Rules
		if err := o.singleton("access_gate"); err != nil {
			return err
		}
		invalid := func(err error) error {
			return &DefinitionError{Kind: DefinitionInvalidAccessGate, Field: "access_gate", Cause: err}
		}
		workspace, err := gate.NewWorkspaceAccess(roots...)
		if err != nil {
			return invalid(err)
		}
		if approver == nil {
			approver = GateApprover()
		} else if nilLike(approver) {
			return invalid(errWorkspaceNilApprover)
		}
		if rules != nil && nilLike(rules) {
			return invalid(errWorkspaceNilRules)
		}
		state := &workspaceAccessState{access: workspace, approver: approver, rules: rules}
		if revisioner, ok := rules.(gate.PolicyRevisioner); ok {
			state.rulesRevision = revisioner.PolicyRevision()
		}
		// Build one evaluator now so a bad configuration fails Define, not Bind.
		if _, err := state.evaluator(); err != nil {
			return invalid(err)
		}
		o.workspace = state
		return nil
	}
}

var (
	errWorkspaceNilApprover = errors.New("loop: workspace access approver is a typed nil")
	errWorkspaceNilRules    = errors.New("loop: workspace access rules are a typed nil")
)
