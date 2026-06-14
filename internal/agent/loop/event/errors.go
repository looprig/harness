package event

// EmptyResponseError is the TurnFailed.Err cause when a provider returns a
// successful stream that contains no text or thinking content.
type EmptyResponseError struct{}

func (e *EmptyResponseError) Error() string { return "loop: empty response from provider" }

// TurnPanicError is the TurnFailed.Err cause when the turn goroutine panics.
// Detail is the recovered value rendered as a string.
type TurnPanicError struct{ Detail string }

func (e *TurnPanicError) Error() string {
	return "loop: panic in turn goroutine: " + e.Detail
}
