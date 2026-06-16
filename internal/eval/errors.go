package eval

import "fmt"

// RunError wraps a Runner failure for a named case.
type RunError struct {
	Case  string
	Cause error
}

func (e *RunError) Error() string { return fmt.Sprintf("eval: run case %q: %v", e.Case, e.Cause) }
func (e *RunError) Unwrap() error { return e.Cause }

// MeasureError wraps a Metric failure for a named case.
type MeasureError struct {
	Metric string
	Case   string
	Cause  error
}

func (e *MeasureError) Error() string {
	return fmt.Sprintf("eval: metric %q on case %q: %v", e.Metric, e.Case, e.Cause)
}
func (e *MeasureError) Unwrap() error { return e.Cause }

// LoadError wraps a golden-set load failure for a path.
type LoadError struct {
	Path  string
	Cause error
}

func (e *LoadError) Error() string { return fmt.Sprintf("eval: load %q: %v", e.Path, e.Cause) }
func (e *LoadError) Unwrap() error { return e.Cause }

// JudgeParseError reports an unparseable judge response.
type JudgeParseError struct {
	Raw   string
	Cause error
}

func (e *JudgeParseError) Error() string {
	return fmt.Sprintf("eval: parse judge response: %v", e.Cause)
}
func (e *JudgeParseError) Unwrap() error { return e.Cause }
