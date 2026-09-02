package hub

import (
	"errors"
	"fmt"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/hustle"
)

// PublishBoundaryReason identifies why an event was rejected at a hub publication
// boundary. The closed reason set lets callers inspect the denial without parsing
// an error string or retaining the event payload in the error.
type PublishBoundaryReason string

const (
	PublishBoundaryNilEvent   PublishBoundaryReason = "nil_event"
	PublishBoundaryVisibility PublishBoundaryReason = "visibility"
	PublishBoundaryClass      PublishBoundaryReason = "class"
	PublishBoundarySession    PublishBoundaryReason = "session"
	PublishBoundaryType       PublishBoundaryReason = "type"
	PublishBoundaryInvalid    PublishBoundaryReason = "invalid"
)

// PublishBoundaryError reports a fail-closed event publication denial.
type PublishBoundaryError struct {
	Reason    PublishBoundaryReason
	EventType string
	Cause     error
}

func (e *PublishBoundaryError) Error() string {
	message := "hub: event publication denied"
	if e.EventType != "" {
		message += " for " + e.EventType
	}
	if e.Reason != "" {
		message += ": " + string(e.Reason)
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *PublishBoundaryError) Unwrap() error { return e.Cause }

// HustleActivityReason identifies an activity-acquisition contract violation.
type HustleActivityReason string

const (
	HustleActivityInvalidRunID HustleActivityReason = "invalid_run_id"
	HustleActivityDuplicate    HustleActivityReason = "duplicate_run_id"
	HustleActivityStopped      HustleActivityReason = "session_stopped"
)

// HustleActivityError reports that a blocking hustle activity could not be
// inserted into the hub's quiescence set.
type HustleActivityError struct {
	Reason HustleActivityReason
	RunID  hustle.RunID
}

func (e *HustleActivityError) Error() string {
	return fmt.Sprintf("hub: hustle activity denied: %s", e.Reason)
}

// TurnStartReservationReason identifies why a loop could not reserve the Hub's
// activity transition for its opening TurnStarted publication.
type TurnStartReservationReason string

const (
	TurnStartReservationInvalidLoop TurnStartReservationReason = "invalid_loop_id"
	TurnStartReservationStopped     TurnStartReservationReason = "session_stopped"
	TurnStartReservationMismatch    TurnStartReservationReason = "publication_mismatch"
	TurnStartReservationReleased    TurnStartReservationReason = "released"
	TurnStartReservationReused      TurnStartReservationReason = "reused"
)

// TurnStartReservationError reports a denied or mismatched one-shot turn-start
// activity reservation.
type TurnStartReservationError struct {
	Reason TurnStartReservationReason
	LoopID uuid.UUID
}

func (e *TurnStartReservationError) Error() string {
	return fmt.Sprintf("hub: turn-start activity reservation denied: %s", e.Reason)
}

// ErrCommittedBodyMissing is the cause recorded on a committed-public-event
// subscription the hub failed because an ENDURING public event reached its fan-out
// without the canonical bytes its durable append was supposed to report. It is a
// broken invariant of the committed stream, not congestion, and naming it separately
// is what keeps the loss legible: without a cause, this failure is indistinguishable
// from an egress overflow, and a Host consumer that treated it as backpressure would
// resubscribe forever against a hub that can never satisfy the contract.
//
// Its text carries no "hub:" prefix because it is always surfaced through
// *SubscriptionLossError, which supplies one; prefixing here would double it.
var ErrCommittedBodyMissing = errors.New("enduring public event carried no committed public body")

// ErrCommitEventMismatch is the cause recorded when a delivery's committed append
// result belongs to a DIFFERENT event than the one being delivered. It is a hub
// programming error, not a runtime condition: sessionwire.Project stamps a public
// EventID from the event's own header, so a committed result and its event always agree
// unless a delivery path paired the wrong two values.
//
// It is guarded rather than trusted because the failure is silent where it lands. A
// consumer joining a durable tail to this stream dedupes on (sequence, EventID); two
// deliveries carrying one identity make it either drop an event as a duplicate or
// render another twice, with nothing anywhere reporting an error. The compiler cannot
// help — every append result is the same type — so the pairing is checked at the one
// place every delivery passes through.
var ErrCommitEventMismatch = errors.New("committed append result does not belong to the delivered event")
