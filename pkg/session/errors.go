package session

import (
	"strconv"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
)

type SessionErrorKind string

const (
	SessionIDGenerationFailed            SessionErrorKind = "id_generation_failed"
	SessionLoopIDGenerationFailed        SessionErrorKind = "loop_id_generation_failed"
	SessionLoopExited                    SessionErrorKind = "loop_exited"
	SessionLoopNotFound                  SessionErrorKind = "loop_not_found"
	SessionEventChannelClosed            SessionErrorKind = "event_channel_closed"
	SessionContextDone                   SessionErrorKind = "context_done"
	SessionClosing                       SessionErrorKind = "session_closing"
	SessionFaulted                       SessionErrorKind = "session_faulted"
	SessionLoopDepthExceeded             SessionErrorKind = "loop_depth_exceeded"
	SessionLoopQuotaExceeded             SessionErrorKind = "loop_quota_exceeded"
	SessionForeignBuilderMissing         SessionErrorKind = "foreign_builder_missing"
	SessionDelegateIntentAppendFailed    SessionErrorKind = "delegate_intent_append_failed"
	SessionDelegateAdmissionCommitFailed SessionErrorKind = "delegate_admission_commit_failed"
)

type SessionError struct {
	Kind  SessionErrorKind
	Cause error
}

func (e *SessionError) Error() string {
	messages := map[SessionErrorKind]string{
		SessionIDGenerationFailed: "session: id generation failed", SessionLoopIDGenerationFailed: "session: loop id generation failed",
		SessionLoopExited: "session: loop exited", SessionLoopNotFound: "session: loop not found",
		SessionEventChannelClosed: "session: event channel closed without terminal event", SessionContextDone: "session: context done",
		SessionClosing: "session: closing", SessionFaulted: "session: faulted (durable persistence failure)",
		SessionLoopDepthExceeded: "session: loop spawn depth limit exceeded", SessionLoopQuotaExceeded: "session: loop spawn quota exceeded",
		SessionForeignBuilderMissing:         "session: foreign engine selected but no foreign builder wired",
		SessionDelegateIntentAppendFailed:    "session: required delegate intent append failed",
		SessionDelegateAdmissionCommitFailed: "session: delegate admission commit failed after durable intent",
	}
	msg := messages[e.Kind]
	if msg == "" {
		msg = "session: error"
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}
func (e *SessionError) Unwrap() error { return e.Cause }

type TurnRejectedError struct{ Reason event.RejectReason }

func (e *TurnRejectedError) Error() string {
	switch e.Reason {
	case event.RejectQueueFull:
		return "session: turn rejected: queue full"
	case event.RejectShuttingDown:
		return "session: turn rejected: loop shutting down"
	case event.RejectInternal:
		return "session: turn rejected: transient internal failure"
	default:
		return "session: turn rejected"
	}
}

type ConfigMismatchError struct{ Persisted, Live event.ConfigFingerprint }

func (e *ConfigMismatchError) Error() string {
	return "session: restore config mismatch: persisted model=" + e.Persisted.ModelID + " != live model=" + e.Live.ModelID + " (system/tool digests may also differ); pass WithAllowConfigMismatch to override"
}

type AgentNameMismatchError struct{ Persisted, Configured identity.AgentName }

func (e *AgentNameMismatchError) Error() string {
	return "session: restore agent name mismatch: persisted=" + strconv.Quote(string(e.Persisted)) + " != configured=" + strconv.Quote(string(e.Configured)) + "; pass WithAllowConfigMismatch to override"
}

type RestoreDiscoveryErrorKind string

const (
	RestoreNoSessionStarted RestoreDiscoveryErrorKind = "no_session_started"
	RestoreNoPrimaryLoop    RestoreDiscoveryErrorKind = "no_primary_loop"
)

type RestoreDiscoveryError struct {
	Kind      RestoreDiscoveryErrorKind
	SessionID uuid.UUID
}

func (e *RestoreDiscoveryError) Error() string {
	if e.Kind == RestoreNoSessionStarted {
		return "session: restore: no SessionStarted in stream for " + e.SessionID.String()
	}
	if e.Kind == RestoreNoPrimaryLoop {
		return "session: restore: no root LoopStarted in stream for " + e.SessionID.String()
	}
	return "session: restore: discovery failed for " + e.SessionID.String()
}

type RestoreErrorKind string

const (
	RestoreLeaseFailed           RestoreErrorKind = "lease_failed"
	RestoreJournalFailed         RestoreErrorKind = "journal_failed"
	RestoreReplayFailed          RestoreErrorKind = "replay_failed"
	RestoreAppendFailed          RestoreErrorKind = "append_failed"
	RestoreLoopFailed            RestoreErrorKind = "loop_failed"
	RestoreContextDone           RestoreErrorKind = "context_done"
	RestoreIDGenerationFailed    RestoreErrorKind = "id_generation_failed"
	RestoreForeignSIDMissing     RestoreErrorKind = "foreign_sid_missing"
	RestoreForeignBuilderMissing RestoreErrorKind = "foreign_builder_missing"
	RestoreMaterializeFailed     RestoreErrorKind = "materialize_failed"
)

type RestoreError struct {
	Kind  RestoreErrorKind
	Cause error
}

func (e *RestoreError) Error() string {
	msg := "session: restore failed (" + string(e.Kind) + ")"
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}
func (e *RestoreError) Unwrap() error { return e.Cause }

type GateErrorKind string

const (
	GateNotFound      GateErrorKind = "not_found"
	GateNotReady      GateErrorKind = "not_ready"
	GateKindMismatch  GateErrorKind = "kind_mismatch"
	GateActionInvalid GateErrorKind = "action_invalid"
	GateCapacity      GateErrorKind = "capacity"
	GateAppendFailed  GateErrorKind = "append_failed"
)

type GateError struct {
	GateID gate.ID
	Kind   GateErrorKind
	Cause  error
}

func (e *GateError) Error() string {
	prefix := "session: gate"
	if e.GateID != (gate.ID{}) {
		prefix += " " + e.GateID.String()
	}
	switch e.Kind {
	case GateNotFound:
		return prefix + " not found"
	case GateNotReady:
		return prefix + " not ready"
	case GateKindMismatch:
		return prefix + " kind mismatch"
	case GateActionInvalid:
		return prefix + " action invalid"
	case GateCapacity:
		return prefix + " capacity exceeded"
	case GateAppendFailed:
		if e.Cause != nil {
			return prefix + " append failed: " + e.Cause.Error()
		}
		return prefix + " append failed"
	}
	return prefix + " error"
}
func (e *GateError) Unwrap() error         { return e.Cause }
func (e *GateError) GateErrorKind() string { return string(e.Kind) }

type WorkspaceNotConfiguredError struct{}

func (*WorkspaceNotConfiguredError) Error() string {
	return "session: workspace store not configured (WithWorkspaceStore); cannot checkpoint"
}
