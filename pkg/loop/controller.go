package loop

import (
	"context"

	"github.com/looprig/core/uuid"
	"github.com/looprig/inference"
)

// Handle is the read-only identity and inference view of a live loop.
type Handle interface {
	ID() uuid.UUID
	Mode() ModeName
	Model() inference.Model
}

// ModeCatalog is the optional read-only selectable-mode view of a live loop.
// The empty ModeName identifies the base mode. Implementations return a
// defensive copy so callers cannot mutate the bound definition.
type ModeCatalog interface {
	Modes() []ModeName
}

// Controller is the trusted mutation surface of a live loop. Changes are applied
// by the actor at a turn boundary; Task 9 provides that behavior.
type Controller interface {
	Handle
	SetMode(context.Context, ModeName) error
	Change(context.Context, ...Change) error
	// Interrupt cancels this loop's current turn AND every loop below it in the delegate
	// subtree, marking the whole subtree interrupt-pending so a parent whose interrupted
	// delegate wait resolves cannot open a fresh delegate step. It is the subtree-scoped
	// counterpart to the session-wide Session.Interrupt and the single-child Subagent
	// interrupt. The runtime holds an admission barrier over the subtree until it is idle.
	Interrupt(context.Context) error
}

// Change is a sealed immutable loop-inference change. Runtime implementations
// inspect the two read-only projections and validate a whole batch atomically.
type Change interface {
	InferenceModel() (inference.Model, bool)
	InferenceEffort() (inference.Effort, bool)
	change()
}

type modelChange struct{ model inference.Model }

func (modelChange) change()                                   {}
func (c modelChange) InferenceModel() (inference.Model, bool) { return cloneModel(c.model), true }
func (modelChange) InferenceEffort() (inference.Effort, bool) { return inference.EffortNone, false }

type effortChange struct{ effort inference.Effort }

func (effortChange) change()                                     {}
func (effortChange) InferenceModel() (inference.Model, bool)     { return inference.Model{}, false }
func (c effortChange) InferenceEffort() (inference.Effort, bool) { return c.effort, true }

// ChangeModel selects a new secret-free model descriptor. Controllers validate it
// atomically with the other changes before applying anything.
func ChangeModel(model inference.Model) Change { return modelChange{model: cloneModel(model)} }

// ChangeEffort selects a new inference effort. Controllers validate it atomically.
func ChangeEffort(effort inference.Effort) Change { return effortChange{effort: effort} }

// ChangeErrorKind classifies why a SetMode or Change was refused. Every kind is a
// fail-secure refusal: the change is NOT applied (no partial apply) and no enduring
// event is emitted (except when the durable append itself is the failure, in which case
// the append faulted the session and the change is not applied).
type ChangeErrorKind string

const (
	// ChangeInvalidMode: the requested mode name is not a predeclared mode of the loop
	// definition (nor the base mode).
	ChangeInvalidMode ChangeErrorKind = "invalid_mode"
	// ChangeInvalidModel: the requested model descriptor failed structural validation.
	ChangeInvalidModel ChangeErrorKind = "invalid_model"
	// ChangeInvalidEffort: the requested effort is not a known effort level.
	ChangeInvalidEffort ChangeErrorKind = "invalid_effort"
	// ChangeNoChanges: a Change batch selected neither a model nor an effort.
	ChangeNoChanges ChangeErrorKind = "no_changes"
	// ChangeLoopShuttingDown: the loop is shutting down and admits no configuration change.
	ChangeLoopShuttingDown ChangeErrorKind = "loop_shutting_down"
	// ChangeLoopExited: the loop's actor has exited, so no change can be delivered.
	ChangeLoopExited ChangeErrorKind = "loop_exited"
	// ChangeContextDone: the caller's context was cancelled before the change committed.
	ChangeContextDone ChangeErrorKind = "context_done"
	// ChangeDurableAppendFailed: the enduring change event's required durable append
	// failed (the session faulted), so the change was NOT applied.
	ChangeDurableAppendFailed ChangeErrorKind = "durable_append_failed"
)

// ChangeError is the typed refusal returned by Controller.SetMode / Controller.Change.
// Mode carries the offending mode name for ChangeInvalidMode; Cause chains an underlying
// validation or persistence error where one exists. Callers errors.As it to distinguish
// a user error (invalid mode/model/effort) from a lifecycle refusal (shutting down /
// exited) from a persistence fault.
type ChangeError struct {
	Kind  ChangeErrorKind
	Mode  ModeName
	Cause error
}

func (e *ChangeError) Error() string {
	msg := "loop: change refused (" + string(e.Kind) + ")"
	if e.Mode != "" {
		msg += ": mode=" + string(e.Mode)
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *ChangeError) Unwrap() error { return e.Cause }
