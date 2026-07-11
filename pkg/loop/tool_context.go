package loop

import (
	"context"

	"github.com/looprig/harness/pkg/tool"
)

type toolUseIDKey struct{}
type preparedKey struct{}
type userInputRequesterKey struct{}

// RequestUserInputFunc is the actor-supplied implementation behind RequestUserInput.
type RequestUserInputFunc func(context.Context, string, []string) (string, error)

// UserInputContextError reports that a tool requested user input outside a live loop call.
type UserInputContextError struct{}

func (*UserInputContextError) Error() string {
	return "loop: RequestUserInput called without a live loop requester"
}

// WithUserInputRequester installs the live loop's user-input capability for a tool call.
func WithUserInputRequester(ctx context.Context, requester RequestUserInputFunc) context.Context {
	return context.WithValue(ctx, userInputRequesterKey{}, requester)
}

// RequestUserInput routes a tool request through the live loop capability in ctx.
func RequestUserInput(ctx context.Context, question string, choices []string) (string, error) {
	requester, ok := ctx.Value(userInputRequesterKey{}).(RequestUserInputFunc)
	if !ok || requester == nil {
		return "", &UserInputContextError{}
	}
	return requester(ctx, question, choices)
}

// WithToolUseID carries the provider tool-use id for a running tool call.
func WithToolUseID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, toolUseIDKey{}, id)
}

// ToolUseIDFrom returns the provider tool-use id carried by ctx.
func ToolUseIDFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(toolUseIDKey{}).(string)
	return id, ok
}

// WithPrepared carries the artifact prepared for a tool invocation.
func WithPrepared(ctx context.Context, prepared tool.PreparedArtifact) context.Context {
	return context.WithValue(ctx, preparedKey{}, prepared)
}

// PreparedFromContext returns the prepared artifact carried by ctx.
func PreparedFromContext(ctx context.Context) (tool.PreparedArtifact, bool) {
	value, ok := ctx.Value(preparedKey{}).(tool.PreparedArtifact)
	return value, ok && value != nil
}
