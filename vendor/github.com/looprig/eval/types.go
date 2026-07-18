package eval

// This file declares the strict domain identity and enumeration types shared
// across the eval framework. Every value that carries domain meaning is a named
// type rather than a bare primitive, and every enumerated set is closed: values
// outside the declared members are rejected by Validate (see validate.go),
// never treated as a default. There is deliberately no valid zero value for the
// assessment status, severity, or unit enums — an unset value is a validation
// failure, so missing information can never masquerade as a pass.

// Byte bounds for the identity string types. Identifiers are short by design;
// these caps reject absurd or hostile inputs before they reach evaluators,
// reports, or sinks. They are byte counts, not rune counts.
const (
	// MaxNameBytes bounds a Name in UTF-8 bytes.
	MaxNameBytes = 256
	// MaxRevisionBytes bounds a Revision in UTF-8 bytes.
	MaxRevisionBytes = 256
)

// Name identifies a scenario, evaluator, measurement, or similar domain object.
// A valid Name is non-empty, valid UTF-8, and no longer than MaxNameBytes bytes.
type Name string

// Revision identifies a specific version of a named object, such as an
// evaluator revision, a model revision, or a prompt revision. A valid Revision
// is non-empty, valid UTF-8, and no longer than MaxRevisionBytes bytes.
type Revision string

// Scope names the observational granularity an assessment applies to.
type Scope uint8

const (
	// ScopeCase covers a single qualification case. It is the valid zero value.
	ScopeCase Scope = iota
	// ScopeTurn covers one completed turn.
	ScopeTurn
	// ScopeSession covers a full session.
	ScopeSession
	// ScopeRun covers an entire run of many sessions or cases.
	ScopeRun
)

// Method is descriptive metadata describing how an evaluator reaches its
// assessment. It supports filtering, cost accounting, and reporting.
type Method uint8

const (
	// MethodProgrammatic evaluates observable facts deterministically. It is
	// the valid zero value.
	MethodProgrammatic Method = iota
	// MethodModel evaluates genuinely ambiguous meaning with a model judge.
	MethodModel
	// MethodComposite grounds a semantic conclusion in operational facts by
	// combining component evaluators.
	MethodComposite
)

// AssessmentStatus is the terminal disposition of an assessment. It is a string
// type so it renders directly in wire envelopes and reports. There is no valid
// zero value: an unset status must not validate, so missing evidence can never
// be reported as a pass.
type AssessmentStatus string

const (
	// StatusPass indicates the expectation was met.
	StatusPass AssessmentStatus = "pass"
	// StatusFail indicates the expectation was not met.
	StatusFail AssessmentStatus = "fail"
	// StatusUnverified indicates no authoritative evidence was available. It is
	// never an inferred pass.
	StatusUnverified AssessmentStatus = "unverified"
	// StatusError indicates the evaluator itself failed to produce a verdict.
	StatusError AssessmentStatus = "error"
	// StatusSkipped indicates the evaluator was intentionally not run.
	StatusSkipped AssessmentStatus = "skipped"
)

// Severity ranks a Finding. The members are ordered from least to most serious
// as declared here (info < low < medium < high < critical). It is a string type
// for direct rendering. There is no valid zero value: an unset severity does
// not validate.
type Severity string

const (
	// SeverityInfo is informational, not a defect.
	SeverityInfo Severity = "info"
	// SeverityLow is a minor issue.
	SeverityLow Severity = "low"
	// SeverityMedium is a moderate issue.
	SeverityMedium Severity = "medium"
	// SeverityHigh is a serious issue.
	SeverityHigh Severity = "high"
	// SeverityCritical is the most serious issue.
	SeverityCritical Severity = "critical"
)

// Unit names the dimension of a Measurement.Value. The member set covers the
// dimensions the Phase-1 evaluators produce and nothing more. There is no valid
// zero value: every measurement must declare a known unit.
type Unit string

const (
	// UnitCount is a dimensionless whole-number tally.
	UnitCount Unit = "count"
	// UnitRatio is a dimensionless proportion or rate.
	UnitRatio Unit = "ratio"
	// UnitSecond is a duration in seconds.
	UnitSecond Unit = "second"
	// UnitToken is a count of model tokens.
	UnitToken Unit = "token"
	// UnitByte is a size in bytes.
	UnitByte Unit = "byte"
)
