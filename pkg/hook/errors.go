package hook

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxDenialCodeBytes   = 64
	maxDenialReasonBytes = 1024
)

// ConfigErrorKind identifies an invalid hook declaration or typed payload.
type ConfigErrorKind string

const (
	ConfigUnknownOperation         ConfigErrorKind = "unknown_operation"
	ConfigOperationNotGuardable    ConfigErrorKind = "operation_not_guardable"
	ConfigNilGuard                 ConfigErrorKind = "nil_guard"
	ConfigNilAround                ConfigErrorKind = "nil_around"
	ConfigMissingPolicyRevision    ConfigErrorKind = "missing_policy_revision"
	ConfigUnexpectedPolicyRevision ConfigErrorKind = "unexpected_policy_revision"
	ConfigInvalidCall              ConfigErrorKind = "invalid_call"
	ConfigInvalidDenial            ConfigErrorKind = "invalid_denial"
)

// ConfigError reports an invalid hook configuration or call boundary.
type ConfigError struct {
	Kind      ConfigErrorKind
	Operation Operation
	Index     int
	Field     string
}

func (e *ConfigError) Error() string {
	message := "hook: invalid configuration: " + string(e.Kind)
	if e.Field != "" {
		message += " (" + e.Field + ")"
	}
	if e.Operation != 0 {
		message += fmt.Sprintf(" for operation %d", e.Operation)
	}
	return message
}

// Denial is an intentional, bounded guard refusal.
type Denial struct {
	Code   string
	Reason string
}

func (e *Denial) Error() string {
	return "hook: denied: " + e.Code + ": " + e.Reason
}

// Deny constructs an intentional denial or returns ConfigError when its
// diagnostic fields violate the bounded public contract.
func Deny(code, reason string) error {
	if !validDenialText(code, maxDenialCodeBytes) ||
		!validDenialText(reason, maxDenialReasonBytes) {
		return &ConfigError{Kind: ConfigInvalidDenial, Field: "denial"}
	}
	return &Denial{Code: code, Reason: reason}
}

func validDenialText(value string, maxBytes int) bool {
	if strings.TrimSpace(value) == "" || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
