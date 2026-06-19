package loop

// tryAck is the loop's single non-blocking reply helper for every
// loop-originated reply channel: command.Disposition (UserInput/SubagentResult),
// command.CancelResult (CancelQueuedInput), and command.Command (gate.reply).
//
// Session-created reply channels are buffered with capacity 1, so the send
// succeeds on the normal path. The select/default exists ONLY to protect the
// actor from a contract violation (a nil, unbuffered, or already-filled channel):
// such a send would block the single actor goroutine, wedging the whole loop, so
// it is dropped instead. The default branch is therefore NOT a normal drop policy
// — a correct caller always provides a buffered(1) channel and always receives the
// reply. Reaching default is a bug in the caller.
func tryAck[T any](ack chan<- T, v T) {
	select {
	case ack <- v:
	default:
		// Contract violation: nil/unbuffered/already-filled reply channel. The
		// actor must not block, so the reply is dropped. Callers MUST pass a
		// buffered(1) channel; reaching here means they did not.
	}
}
