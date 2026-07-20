package loopruntime

import "github.com/looprig/harness/pkg/loop"

type (
	AccessGate             = loop.AccessGate
	Effect                 = loop.Effect
	ToolPolicy             = loop.ToolPolicy
	ReadGuard              = loop.ReadGuard
	PermissionFactory      = loop.PermissionFactory
	Delegation             = loop.Delegation
	PermissionDecision     = loop.PermissionDecision
	PermissionGate         = loop.PermissionGate
	RuntimeContextProvider = loop.RuntimeContextProvider
	Provenance             = loop.Provenance
	ConfigError            = loop.ConfigError
	ConfigErrorKind        = loop.ConfigErrorKind
	IDGenerationError      = loop.IDGenerationError
	CommitError            = loop.CommitError
	CommitCancelReason     = loop.CommitCancelReason
	InvalidEffectError     = loop.InvalidEffectError
	Backend                = loop.Backend
)

const (
	EffectAsk              = loop.EffectAsk
	EffectAutoApprove      = loop.EffectAutoApprove
	EffectDeny             = loop.EffectDeny
	ConfigMissingClient    = loop.ConfigMissingClient
	ConfigInvalidModel     = loop.ConfigInvalidModel
	ConfigMissingPublisher = loop.ConfigMissingPublisher
	CommitTurnCancelled    = loop.CommitTurnCancelled
	DelegationManaged      = loop.DelegationManaged
)

var (
	WithProvenance          = loop.WithProvenance
	ProvenanceFrom          = loop.ProvenanceFrom
	WithToolUseID           = loop.WithToolUseID
	ToolUseIDFrom           = loop.ToolUseIDFrom
	WithPreparedCall        = loop.WithPreparedCall
	PreparedCallFromContext = loop.PreparedCallFromContext
	WithUserInputRequester  = loop.WithUserInputRequester
	WithApprovalRequester   = loop.WithApprovalRequester
)
