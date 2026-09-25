package sessionruntime

import (
	"context"
	"time"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/present"
	"github.com/looprig/harness/pkg/runtimecommand"
)

// presentTimeout is a deadline for cooperative presenters and their bounded
// lookups. It cannot forcibly stop product code that ignores ctx.
const presentTimeout = 5 * time.Second

type attribution struct {
	principal *sessionwire.Principal
	metadata  sessionwire.MessageMetadata
	presented *command.Presented
}

// presentUserInput is the only place a session invokes its presenter. Its
// callers gate on AgencyUser; machine input and restored re-offers bypass it.
func (s *Session) presentUserInput(
	ctx context.Context,
	loopID uuid.UUID,
	kind runtimecommand.Kind,
	blocks []content.Block,
	principal *sessionwire.Principal,
	metadata sessionwire.MessageMetadata,
) (attribution, error) {
	attr := attribution{principal: principal}
	if len(metadata) > 0 {
		attr.metadata = metadata
	}
	if s.presenter == nil {
		return attr, nil
	}
	presentCtx, cancel := context.WithTimeout(ctx, presentTimeout)
	defer cancel()
	frame, err := present.Run(presentCtx, s.presenter, present.Input{
		SessionID: s.sessionID, LoopID: loopID, AgentName: s.agentNameFor(loopID),
		Kind: kind, Principal: principal, Metadata: attr.metadata, Blocks: blocks,
	})
	if err != nil {
		return attribution{}, err
	}
	if !frame.Empty() {
		attr.presented = &command.Presented{Prefix: frame.Prefix, Suffix: frame.Suffix}
	}
	return attr, nil
}

// agentNameFor returns the loop's immutable attribution name. A fixture or
// unbound loop has no name.
func (s *Session) agentNameFor(loopID uuid.UUID) identity.AgentName {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	if handle, ok := s.loops[loopID]; ok && handle != nil && handle.bound != nil {
		return handle.bound.Name()
	}
	return ""
}
