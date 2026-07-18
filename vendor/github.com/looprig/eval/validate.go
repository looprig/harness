package eval

import (
	"strconv"
	"unicode/utf8"
)

// This file holds the Validate methods for the domain identity and enum types.
// Each enum uses a closed switch: every known member has a case and the default
// branch returns a typed error, so an unrecognized value is always rejected and
// never falls through. Validation is fail-secure — on any doubt it denies.

// Validate reports whether n is a well-formed Name.
func (n Name) Validate() error {
	return validateIdentifier("Name", string(n), MaxNameBytes)
}

// Validate reports whether r is a well-formed Revision.
func (r Revision) Validate() error {
	return validateIdentifier("Revision", string(r), MaxRevisionBytes)
}

// validateIdentifier enforces the shared identity-string rules: non-empty,
// within the byte bound, and valid UTF-8. Its diagnostics reference only the
// field name and the constant bound, never the offending value.
func validateIdentifier(field, s string, maxBytes int) error {
	if s == "" {
		return &ValidationError{Field: field, Reason: "must not be empty"}
	}
	if len(s) > maxBytes {
		return &ValidationError{Field: field, Reason: "exceeds " + strconv.Itoa(maxBytes) + " bytes"}
	}
	if !utf8.ValidString(s) {
		return &ValidationError{Field: field, Reason: "must be valid UTF-8"}
	}
	return nil
}

// Validate reports whether s is a known Scope member.
func (s Scope) Validate() error {
	switch s {
	case ScopeCase, ScopeTurn, ScopeSession, ScopeRun:
		return nil
	default:
		return &InvalidEnumError{Enum: "Scope", Value: strconv.Itoa(int(s))}
	}
}

// Validate reports whether m is a known Method member.
func (m Method) Validate() error {
	switch m {
	case MethodProgrammatic, MethodModel, MethodComposite:
		return nil
	default:
		return &InvalidEnumError{Enum: "Method", Value: strconv.Itoa(int(m))}
	}
}

// Validate reports whether s is a known AssessmentStatus. The zero value is not
// a member, so an unset status is rejected and can never read as a pass.
func (s AssessmentStatus) Validate() error {
	switch s {
	case StatusPass, StatusFail, StatusUnverified, StatusError, StatusSkipped:
		return nil
	default:
		// The token may be untrusted; withhold it from the diagnostic.
		return &InvalidEnumError{Enum: "AssessmentStatus"}
	}
}

// Validate reports whether s is a known Severity. The zero value is not a
// member.
func (s Severity) Validate() error {
	switch s {
	case SeverityInfo, SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical:
		return nil
	default:
		return &InvalidEnumError{Enum: "Severity"}
	}
}

// Validate reports whether u is a known Unit. The zero value is not a member.
func (u Unit) Validate() error {
	switch u {
	case UnitCount, UnitRatio, UnitSecond, UnitToken, UnitByte:
		return nil
	default:
		return &InvalidEnumError{Enum: "Unit"}
	}
}
