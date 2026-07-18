package rig

type DefinitionErrorKind string

const (
	DefinitionNilOption                    DefinitionErrorKind = "nil_option"
	DefinitionMissingLoop                  DefinitionErrorKind = "missing_loop"
	DefinitionInvalidLoop                  DefinitionErrorKind = "invalid_loop"
	DefinitionDuplicateLoop                DefinitionErrorKind = "duplicate_loop"
	DefinitionMissingPrimer                DefinitionErrorKind = "missing_primer"
	DefinitionInvalidPrimer                DefinitionErrorKind = "invalid_primer"
	DefinitionInvalidActivePrimer          DefinitionErrorKind = "invalid_active_primer"
	DefinitionMissingSessionStore          DefinitionErrorKind = "missing_session_store"
	DefinitionInvalidSessionStore          DefinitionErrorKind = "invalid_session_store"
	DefinitionInvalidDelegationLimits      DefinitionErrorKind = "invalid_delegation_limits"
	DefinitionInvalidForeignBuilders       DefinitionErrorKind = "invalid_foreign_builders"
	DefinitionInvalidGateCaps              DefinitionErrorKind = "invalid_gate_caps"
	DefinitionInvalidSecurityLimitFactory  DefinitionErrorKind = "invalid_ceiling_factory"
	DefinitionInvalidRestoreDecider        DefinitionErrorKind = "invalid_restore_decider"
	DefinitionDuplicateOption              DefinitionErrorKind = "duplicate_option"
	DefinitionInvalidHustle                DefinitionErrorKind = "invalid_hustle"
	DefinitionDuplicateHustle              DefinitionErrorKind = "duplicate_hustle"
	DefinitionMissingHustleLimits          DefinitionErrorKind = "missing_hustle_limits"
	DefinitionUnusedHustleLimits           DefinitionErrorKind = "unused_hustle_limits"
	DefinitionInvalidHustleLimits          DefinitionErrorKind = "invalid_hustle_limits"
	DefinitionMissingCompactionHustle      DefinitionErrorKind = "missing_compaction_hustle"
	DefinitionIncompatibleCompactionHustle DefinitionErrorKind = "incompatible_compaction_hustle"
)

type DefinitionError struct {
	Kind  DefinitionErrorKind
	Name  string
	Cause error
}

func (e *DefinitionError) Error() string {
	msg := "rig: invalid definition (" + string(e.Kind) + ")"
	if e.Name != "" {
		msg += ": " + e.Name
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *DefinitionError) Unwrap() error { return e.Cause }

type LifecycleErrorKind string

const (
	LifecycleContextDone         LifecycleErrorKind = "context_done"
	LifecycleIDGenerationFailed  LifecycleErrorKind = "id_generation_failed"
	LifecycleLeaseFailed         LifecycleErrorKind = "lease_failed"
	LifecycleJournalFailed       LifecycleErrorKind = "journal_failed"
	LifecycleAppenderFailed      LifecycleErrorKind = "appender_failed"
	LifecycleSecurityLimitFailed LifecycleErrorKind = "ceiling_failed"
	LifecycleSessionFailed       LifecycleErrorKind = "session_failed"
)

type LifecycleError struct {
	Kind  LifecycleErrorKind
	Cause error
}

func (e *LifecycleError) Error() string {
	msg := "rig: session lifecycle failed (" + string(e.Kind) + ")"
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *LifecycleError) Unwrap() error { return e.Cause }
