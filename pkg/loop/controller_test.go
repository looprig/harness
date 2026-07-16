package loop

import (
	"context"
	"testing"

	"github.com/looprig/core/uuid"
	model "github.com/looprig/inference/model"
)

func TestControllerContractsCompile(t *testing.T) {
	t.Parallel()
	var _ Handle = controllerStub{}
	var _ Controller = controllerStub{}
	changes := []Change{ChangeModel(testModel()), ChangeEffort(model.EffortHigh)}
	if len(changes) != 2 {
		t.Fatal("changes missing")
	}
	if model, ok := changes[0].InferenceModel(); !ok || model.Name != testModel().Name {
		t.Fatalf("model change = %+v, %v", model, ok)
	}
	if effort, ok := changes[1].InferenceEffort(); !ok || effort != model.EffortHigh {
		t.Fatalf("effort change = %q, %v", effort, ok)
	}
}

type controllerStub struct{}

func (controllerStub) ID() uuid.UUID                           { return uuid.UUID{} }
func (controllerStub) Mode() ModeName                          { return "" }
func (controllerStub) Model() model.Model                      { return model.Model{} }
func (controllerStub) SetMode(context.Context, ModeName) error { return nil }
func (controllerStub) Change(context.Context, ...Change) error { return nil }
func (controllerStub) Interrupt(context.Context) error         { return nil }
