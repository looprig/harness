package sessionruntime

import sessionapi "github.com/looprig/harness/pkg/session"

type SessionErrorKind = sessionapi.SessionErrorKind
type SessionError = sessionapi.SessionError
type TurnRejectedError = sessionapi.TurnRejectedError
type ConfigMismatchError = sessionapi.ConfigMismatchError
type AgentNameMismatchError = sessionapi.AgentNameMismatchError
type RestoreDiscoveryErrorKind = sessionapi.RestoreDiscoveryErrorKind
type RestoreDiscoveryError = sessionapi.RestoreDiscoveryError
type RestoreErrorKind = sessionapi.RestoreErrorKind
type RestoreError = sessionapi.RestoreError
type GateErrorKind = sessionapi.GateErrorKind
type GateError = sessionapi.GateError
type WorkspaceNotConfiguredError = sessionapi.WorkspaceNotConfiguredError

const (
	SessionIDGenerationFailed            = sessionapi.SessionIDGenerationFailed
	SessionLoopIDGenerationFailed        = sessionapi.SessionLoopIDGenerationFailed
	SessionLoopExited                    = sessionapi.SessionLoopExited
	SessionLoopNotFound                  = sessionapi.SessionLoopNotFound
	SessionEventChannelClosed            = sessionapi.SessionEventChannelClosed
	SessionContextDone                   = sessionapi.SessionContextDone
	SessionClosing                       = sessionapi.SessionClosing
	SessionFaulted                       = sessionapi.SessionFaulted
	SessionLoopDepthExceeded             = sessionapi.SessionLoopDepthExceeded
	SessionLoopQuotaExceeded             = sessionapi.SessionLoopQuotaExceeded
	SessionForeignBuilderMissing         = sessionapi.SessionForeignBuilderMissing
	SessionDelegateIntentAppendFailed    = sessionapi.SessionDelegateIntentAppendFailed
	SessionDelegateAdmissionCommitFailed = sessionapi.SessionDelegateAdmissionCommitFailed
	RestoreNoSessionStarted              = sessionapi.RestoreNoSessionStarted
	RestoreNoPrimaryLoop                 = sessionapi.RestoreNoPrimaryLoop
	RestoreLeaseFailed                   = sessionapi.RestoreLeaseFailed
	RestoreJournalFailed                 = sessionapi.RestoreJournalFailed
	RestoreReplayFailed                  = sessionapi.RestoreReplayFailed
	RestoreAppendFailed                  = sessionapi.RestoreAppendFailed
	RestoreLoopFailed                    = sessionapi.RestoreLoopFailed
	RestoreContextDone                   = sessionapi.RestoreContextDone
	RestoreIDGenerationFailed            = sessionapi.RestoreIDGenerationFailed
	RestoreForeignSIDMissing             = sessionapi.RestoreForeignSIDMissing
	RestoreForeignBuilderMissing         = sessionapi.RestoreForeignBuilderMissing
	RestoreMaterializeFailed             = sessionapi.RestoreMaterializeFailed
	GateNotFound                         = sessionapi.GateNotFound
	GateNotReady                         = sessionapi.GateNotReady
	GateKindMismatch                     = sessionapi.GateKindMismatch
	GateActionInvalid                    = sessionapi.GateActionInvalid
	GateCapacity                         = sessionapi.GateCapacity
	GateAppendFailed                     = sessionapi.GateAppendFailed
)
