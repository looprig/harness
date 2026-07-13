package loop

import (
	"context"
	"errors"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

type fakeLLM struct{}

func (*fakeLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("unused")
}

func mustUUID(t interface {
	Helper()
	Fatalf(string, ...any)
}) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return id
}

type permissionGateStub struct{}

func (permissionGateStub) Check(context.Context, tool.InvokableTool, string, string) Effect {
	return EffectAsk
}

func (permissionGateStub) Grant(context.Context, string, string, tool.ApprovalScope) error {
	return nil
}

func (*fakeLLM) Stream(context.Context, inference.Request) (*inference.StreamReader[content.Chunk], error) {
	return nil, errors.New("unused")
}

func testModel() inference.Model {
	return inference.Model{
		Provider: inference.ProviderName("lmstudio"), APIFormat: inference.APIFormatOpenAI,
		BaseURL: "http://localhost:1234", Name: "m",
	}
}
