package transcript

// stageRead is the ReconstructError.Stage value for a failure reading the record
// source. It is the only stage Reconstruct can fail at: every other anomaly
// degrades to a Warning rather than an error.
const stageRead = "read"

// ReconstructError is returned by Reconstruct when the underlying record source
// fails to read. Reconstruction is otherwise best-effort: malformed or unpaired
// records degrade to Warnings, so this error signals only a source read failure.
// Stage names the reconstruction phase that failed; Cause is the wrapped source
// error, recoverable via errors.As / Unwrap.
type ReconstructError struct {
	Stage string
	Cause error
}

func (e *ReconstructError) Error() string {
	if e.Cause == nil {
		return "transcript: reconstruct: " + e.Stage
	}
	return "transcript: reconstruct: " + e.Stage + ": " + e.Cause.Error()
}

func (e *ReconstructError) Unwrap() error { return e.Cause }
