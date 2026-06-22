package swe

import (
	"github.com/inventivepotter/urvi/agents/explorer"
	"github.com/inventivepotter/urvi/agents/operator"
	"github.com/inventivepotter/urvi/agents/researcher"
	"github.com/inventivepotter/urvi/agents/reviewer"
	"github.com/inventivepotter/urvi/internal/agent/loop"
)

// leafRegistry builds the SWE-Swarm's registry of spawnable LEAF agents from the
// four leaf packages, adapting each leaf's raw-signature BuildTools into the
// swe.Agent shape (func(LeafToolDeps) loop.ToolSet) at the composition root — so
// the leaf packages never import swarms/swe (no import cycle). The orchestrator is
// deliberately absent: it is the primary loop, not a spawnable leaf. AllowsRuntimeSkills
// is left false in P1. A duplicate name fails secure with a *DuplicateAgentError.
//
// The deps parameter is deliberately NOT captured by the adapters: each adapter
// re-invokes the leaf's BuildTools with the deps the swarm passes PER SPAWN (the
// registry stores the closure, not its result), so every spawn gets a FRESH
// PermissionChecker — the per-loop approval-isolation guarantee. The parameter is
// retained for signature symmetry with the spawn-time call and for a future phase
// that may bind registry-scoped deps; in P1 it is unused.
func leafRegistry(_ LeafToolDeps) (*Registry, error) {
	return NewRegistry(
		Agent{
			Name:        operator.Name,
			Description: operator.Description,
			Role:        operator.Role,
			Skills:      []string{"code-style"},
			BuildTools:  func(d LeafToolDeps) loop.ToolSet { return operator.BuildTools(d.Root, nil) },
		},
		Agent{
			Name:        researcher.Name,
			Description: researcher.Description,
			Role:        researcher.Role,
			BuildTools:  func(d LeafToolDeps) loop.ToolSet { return researcher.BuildTools(d.Root, d.HTTPCl, nil) },
		},
		Agent{
			Name:        explorer.Name,
			Description: explorer.Description,
			Role:        explorer.Role,
			BuildTools:  func(d LeafToolDeps) loop.ToolSet { return explorer.BuildTools(d.Root, nil) },
		},
		Agent{
			Name:        reviewer.Name,
			Description: reviewer.Description,
			Role:        reviewer.Role,
			BuildTools:  func(d LeafToolDeps) loop.ToolSet { return reviewer.BuildTools(d.Root, nil) },
		},
	)
}
