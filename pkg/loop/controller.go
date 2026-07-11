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

// Controller is the trusted mutation surface of a live loop. Changes are applied
// by the actor at a turn boundary; Task 9 provides that behavior.
type Controller interface {
	Handle
	SetMode(context.Context, ModeName) error
	Change(context.Context, ...Change) error
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
