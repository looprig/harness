package loopruntime

import (
	"context"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
)

// CompactionFinalizationErrorKind identifies the actor-owned transition that
// could not be completed. Journal failures remain infrastructure failures and
// are never rewritten as a false durable rejection.
type CompactionFinalizationErrorKind string

const (
	CompactionFinalizationProposal       CompactionFinalizationErrorKind = "proposal"
	CompactionFinalizationTerminalMint   CompactionFinalizationErrorKind = "terminal_mint"
	CompactionFinalizationTerminalClone  CompactionFinalizationErrorKind = "terminal_clone"
	CompactionFinalizationTerminalAppend CompactionFinalizationErrorKind = "terminal_append"
	CompactionFinalizationWaiterMint     CompactionFinalizationErrorKind = "waiter_mint"
	CompactionFinalizationWaiterAppend   CompactionFinalizationErrorKind = "waiter_append"
)

// CompactionFinalizationError preserves the failing transition and its cause so
// the session can fail closed on journal infrastructure errors.
type CompactionFinalizationError struct {
	Kind      CompactionFinalizationErrorKind
	AttemptID event.CompactAttemptID
	CommandID uuid.UUID
	Cause     error
}

func (e *CompactionFinalizationError) Error() string {
	message := "loopruntime: compaction finalization failed"
	switch e.Kind {
	case CompactionFinalizationProposal:
		message = "loopruntime: invalid compaction finalization proposal"
	case CompactionFinalizationTerminalMint:
		message = "loopruntime: compaction terminal construction failed"
	case CompactionFinalizationTerminalClone:
		message = "loopruntime: compaction terminal ownership copy failed"
	case CompactionFinalizationTerminalAppend:
		message = "loopruntime: compaction terminal append failed"
	case CompactionFinalizationWaiterMint:
		message = "loopruntime: compaction waiter outcome construction failed"
	case CompactionFinalizationWaiterAppend:
		message = "loopruntime: compaction waiter outcome append failed"
	}
	if e.Cause != nil {
		return message + ": " + e.Cause.Error()
	}
	return message
}

func (e *CompactionFinalizationError) Unwrap() error { return e.Cause }

// compactionPreparedSuccess is the immutable success material prepared before
// the actor is asked to own the terminal transition. Applying it to live
// conversation state remains the responsibility of the replacement handshake.
type compactionPreparedSuccess struct {
	Model              inference.ModelKey
	RequestFingerprint [32]byte
	Summary            *content.UserMessage
	PostContext        event.ContextMeasurement
}

// compactionFinalizationProposal carries exactly one terminal disposition.
// RejectReason zero denotes a prepared success; Success nil denotes rejection.
type compactionFinalizationProposal struct {
	Success      *compactionPreparedSuccess
	RejectReason event.CompactRejectReason
}

func (p compactionFinalizationProposal) validate() error {
	if (p.Success == nil) == (p.RejectReason == event.CompactRejectUnspecified) {
		return &CompactionFinalizationError{Kind: CompactionFinalizationProposal}
	}
	if p.Success != nil {
		if err := p.Success.Model.Validate(); err != nil {
			return &CompactionFinalizationError{Kind: CompactionFinalizationProposal, Cause: err}
		}
		if p.Success.RequestFingerprint == ([32]byte{}) || p.Success.Summary == nil || p.Success.PostContext.Model != p.Success.Model {
			return &CompactionFinalizationError{Kind: CompactionFinalizationProposal}
		}
		postContext := p.Success.PostContext
		postContext.Basis = event.ContextBasis{Revision: 1, ThroughEventID: uuid.UUID{1}}
		if err := postContext.Validate(); err != nil {
			return &CompactionFinalizationError{Kind: CompactionFinalizationProposal, Cause: err}
		}
		return nil
	}
	if !p.RejectReason.Valid() {
		return &CompactionFinalizationError{Kind: CompactionFinalizationProposal}
	}
	return nil
}

type compactionFinalizerConfig struct {
	Publisher eventPublisher
	Factory   *event.Factory
	SessionID uuid.UUID
	LoopID    uuid.UUID
	Now       event.Clock
}

type compactionTerminalRecord struct {
	event event.Event
}

// compactionFinalizer is actor-owned. Its record is written only after the
// canonical terminal append succeeds, making retry/fallback idempotent even
// when the invoking callback panics after ownership transfers to the actor.
type compactionFinalizer struct {
	config  compactionFinalizerConfig
	records map[event.CompactAttemptID]compactionTerminalRecord
}

func newCompactionFinalizer(config compactionFinalizerConfig) *compactionFinalizer {
	return &compactionFinalizer{config: config, records: make(map[event.CompactAttemptID]compactionTerminalRecord)}
}

// Finalize appends one canonical terminal and then one deterministic enduring
// waiter outcome per frozen waiter. Calling it again for the same attempt
// returns the first terminal without appending anything else.
func (f *compactionFinalizer) Finalize(
	ctx context.Context,
	attempt compactionAttempt,
	proposal compactionFinalizationProposal,
) (event.Event, error) {
	if record, ok := f.records[attempt.AttemptID]; ok {
		return cloneCompactionTerminal(record.event, attempt.AttemptID)
	}
	if err := proposal.validate(); err != nil {
		return nil, finalizationError(err, attempt.AttemptID)
	}
	duration := f.config.Now().Sub(attempt.StartedAt)
	if duration < 0 {
		duration = 0
	}
	terminal, err := f.buildTerminal(attempt, proposal, duration)
	if err != nil {
		return nil, err
	}
	published, err := cloneCompactionTerminal(terminal, attempt.AttemptID)
	if err != nil {
		return nil, err
	}
	if err := f.config.Publisher.PublishEventChecked(ctx, published); err != nil {
		return nil, &CompactionFinalizationError{
			Kind: CompactionFinalizationTerminalAppend, AttemptID: attempt.AttemptID, Cause: err,
		}
	}
	f.records[attempt.AttemptID] = compactionTerminalRecord{event: terminal}
	if err := f.publishWaiterOutcomes(ctx, attempt, terminal); err != nil {
		returned, cloneErr := cloneCompactionTerminal(terminal, attempt.AttemptID)
		if cloneErr != nil {
			return nil, cloneErr
		}
		return returned, err
	}
	return cloneCompactionTerminal(terminal, attempt.AttemptID)
}

func (f *compactionFinalizer) buildTerminal(
	attempt compactionAttempt,
	proposal compactionFinalizationProposal,
	duration time.Duration,
) (event.Event, error) {
	var terminal event.Event
	if proposal.Success != nil {
		terminal = event.CompactionCommitted{
			AttemptID: attempt.AttemptID, WaiterCommandIDs: append([]uuid.UUID(nil), attempt.WaiterCommandIDs...),
			Reason: attempt.Reason, Basis: attempt.Basis, Summary: cloneUserMessage(proposal.Success.Summary),
			PostContext: proposal.Success.PostContext, Duration: duration,
		}
	} else {
		terminal = event.CompactionRejected{
			AttemptID: attempt.AttemptID, WaiterCommandIDs: append([]uuid.UUID(nil), attempt.WaiterCommandIDs...),
			Reason: attempt.Reason, Basis: attempt.Basis, RejectReason: proposal.RejectReason, Duration: duration,
		}
	}
	stamped, err := stampLoopEvent(terminal, f.config.Factory, f.config.SessionID, f.config.LoopID, uuid.UUID{})
	if err != nil {
		return nil, &CompactionFinalizationError{Kind: CompactionFinalizationTerminalMint, AttemptID: attempt.AttemptID, Cause: err}
	}
	if committed, ok := stamped.(event.CompactionCommitted); ok {
		if attempt.Basis.Revision == ^event.ContextRevision(0) {
			return nil, &CompactionFinalizationError{
				Kind: CompactionFinalizationTerminalMint, AttemptID: attempt.AttemptID, Cause: &contextRevisionOverflowError{},
			}
		}
		committed.PostContext.Basis = event.ContextBasis{
			Revision: attempt.Basis.Revision + 1, ThroughEventID: committed.EventID,
		}
		stamped = committed
	}
	if err := event.ValidateEvent(stamped); err != nil {
		return nil, &CompactionFinalizationError{Kind: CompactionFinalizationTerminalMint, AttemptID: attempt.AttemptID, Cause: err}
	}
	return stamped, nil
}

func (f *compactionFinalizer) publishWaiterOutcomes(ctx context.Context, attempt compactionAttempt, terminal event.Event) error {
	committed, resolved := terminal.(event.CompactionCommitted)
	for _, commandID := range attempt.WaiterCommandIDs {
		reply, err := f.buildWaiterOutcome(attempt.AttemptID, commandID, committed, resolved, terminal)
		if err != nil {
			return err
		}
		if err := f.config.Publisher.PublishEventChecked(ctx, reply); err != nil {
			return &CompactionFinalizationError{
				Kind: CompactionFinalizationWaiterAppend, AttemptID: attempt.AttemptID, CommandID: commandID, Cause: err,
			}
		}
	}
	return nil
}

func (f *compactionFinalizer) buildWaiterOutcome(
	attemptID event.CompactAttemptID,
	commandID uuid.UUID,
	committed event.CompactionCommitted,
	resolved bool,
	terminal event.Event,
) (event.Event, error) {
	header := event.Header{Cause: identity.Cause{CommandID: commandID}}
	var reply event.Event
	if resolved {
		header.EventID = event.CompactWaiterReplyID(attemptID, commandID, true)
		reply = event.CompactWaiterResolved{
			Header: header, AttemptID: attemptID, CommittedEventID: committed.EventID,
		}
	} else {
		rejected := terminal.(event.CompactionRejected)
		header.EventID = event.CompactWaiterReplyID(attemptID, commandID, false)
		reply = event.CompactWaiterRejected{Header: header, AttemptID: attemptID, Reason: rejected.RejectReason}
	}
	stamped, err := stampLoopEvent(reply, f.config.Factory, f.config.SessionID, f.config.LoopID, uuid.UUID{})
	if err != nil {
		return nil, &CompactionFinalizationError{
			Kind: CompactionFinalizationWaiterMint, AttemptID: attemptID, CommandID: commandID, Cause: err,
		}
	}
	if err := event.ValidateEvent(stamped); err != nil {
		return nil, &CompactionFinalizationError{
			Kind: CompactionFinalizationWaiterMint, AttemptID: attemptID, CommandID: commandID, Cause: err,
		}
	}
	return stamped, nil
}

func finalizationError(err error, attemptID event.CompactAttemptID) error {
	finalization, ok := err.(*CompactionFinalizationError)
	if !ok {
		return &CompactionFinalizationError{Kind: CompactionFinalizationProposal, AttemptID: attemptID, Cause: err}
	}
	finalization.AttemptID = attemptID
	return finalization
}
