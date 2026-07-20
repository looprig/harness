package loop

import (
	"context"
	"errors"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
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

func (*fakeLLM) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, errors.New("unused")
}

func testModel() model.Model {
	return model.Model{
		Provider: model.ProviderName("lmstudio"), APIFormat: model.APIFormatOpenAI,
		BaseURL: "http://localhost:1234", Name: "m",
	}
}
