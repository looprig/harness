package hustleruntime

import (
	"context"
	"reflect"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/tool"
)

// evidenceToolResult is one defensively owned provider-call/result pair.
type evidenceToolResult struct {
	callID  string
	name    string
	content []content.Block
}

// evidenceRunner owns only sequential, read-only evidence execution.
type evidenceRunner struct {
	access         evidenceAccessEvaluator
	allowedKinds   map[string]struct{}
	newExecutionID evidenceExecutionIDFactory
}

func newEvidenceRunner(
	access evidenceAccessEvaluator,
	allowedKinds []string,
	newExecutionID evidenceExecutionIDFactory,
) (*evidenceRunner, error) {
	if nilEvidenceRuntimeValue(reflect.ValueOf(access)) || newExecutionID == nil || len(allowedKinds) == 0 {
		return nil, evidenceError(EvidenceFailureInvalidBinding)
	}
	allowed := make(map[string]struct{}, len(allowedKinds))
	for _, kind := range allowedKinds {
		if kind == "" || kind != strings.TrimSpace(kind) || strings.ContainsRune(kind, '\x00') {
			return nil, evidenceError(EvidenceFailureInvalidBinding)
		}
		if _, duplicate := allowed[kind]; duplicate {
			return nil, evidenceError(EvidenceFailureInvalidBinding)
		}
		allowed[kind] = struct{}{}
	}
	return &evidenceRunner{access: access, allowedKinds: allowed, newExecutionID: newExecutionID}, nil
}

// bindEvidenceInvocation builds and validates the concrete catalog from the
// request's explicit causal origin. No construction-time active-loop state is
// accepted by this seam.
func bindEvidenceInvocation(
	ctx context.Context,
	definition hustle.BoundDefinition,
	request hustle.Request,
	readWorkspace *tool.ReadWorkspaceBinding,
) (catalog []hustle.BoundEvidenceTool, err error) {
	if ctx == nil || nilEvidenceRuntimeValue(reflect.ValueOf(definition)) ||
		request.Cause.SessionID.IsZero() || request.Cause.LoopID.IsZero() ||
		readWorkspace == nil {
		return nil, evidenceError(EvidenceFailureInvalidBinding)
	}
	if failure := evidenceContextFailure(ctx); failure != nil {
		return nil, failure
	}
	defer func() {
		if recover() != nil {
			catalog = nil
			err = evidenceError(EvidenceFailureInternal)
		}
	}()
	catalog, bindErr := definition.BindEvidenceTools(ctx, hustle.EvidenceBindings{
		SessionID: request.Cause.SessionID,
		LoopID:    request.Cause.LoopID,
		ReadWorkspace: &tool.ReadWorkspaceBinding{
			Root: readWorkspace.Root,
		},
	})
	if bindErr != nil {
		return nil, evidenceError(EvidenceFailureInvalidBinding)
	}
	return append([]hustle.BoundEvidenceTool(nil), catalog...), nil
}

func (r *evidenceRunner) run(
	ctx context.Context,
	catalog []hustle.BoundEvidenceTool,
	calls []evidenceToolCall,
	limits hustle.ToolLoopLimits,
) ([]evidenceToolResult, error) {
	if r == nil || nilEvidenceRuntimeValue(reflect.ValueOf(r.access)) || r.newExecutionID == nil ||
		len(r.allowedKinds) == 0 || len(catalog) == 0 || len(calls) == 0 ||
		limits.MaxResultBytes <= 0 || limits.MaxEvidenceBytes <= 0 ||
		limits.MaxResultBytes > limits.MaxEvidenceBytes {
		return nil, evidenceError(EvidenceFailureInvalidBinding)
	}
	if failure := evidenceContextFailure(ctx); failure != nil {
		return nil, failure
	}
	byName, err := indexEvidenceCatalog(catalog)
	if err != nil {
		return nil, err
	}
	executionIDs, err := preflightEvidenceIdentities(calls, byName, r.newExecutionID)
	if err != nil {
		return nil, err
	}
	results := make([]evidenceToolResult, 0, len(calls))
	evidenceBytes := 0
	for index, call := range calls {
		if failure := evidenceContextFailure(ctx); failure != nil {
			return nil, failure
		}
		bound := byName[call.name]
		concrete := bound.Tool()
		if nilEvidenceRuntimeValue(reflect.ValueOf(concrete)) {
			return nil, evidenceError(EvidenceFailureUnprepared)
		}
		preparer, ok := concrete.(tool.CallPreparer)
		if !ok || nilEvidenceRuntimeValue(reflect.ValueOf(preparer)) {
			return nil, evidenceError(EvidenceFailureUnprepared)
		}
		executionID := executionIDs[index]
		request, artifact, err := prepareEvidenceCall(ctx, preparer, executionID, string(call.input))
		if err != nil {
			return nil, err
		}
		if request.ExecutionID != executionID.String() || request.ToolName != call.name {
			return nil, evidenceError(EvidenceFailureAmbiguousIdentity)
		}
		if err := authorizeEvidenceRequest(ctx, r.access, r.allowedKinds, request); err != nil {
			return nil, err
		}
		prepared := tool.PreparedCall{
			ExecutionID: executionID,
			Request:     request,
			Artifact:    artifact,
		}
		result, err := executeEvidenceCall(ctx, concrete, string(call.input), prepared)
		if err != nil {
			return nil, err
		}
		owned, encodedBytes, err := ownEvidenceResult(result)
		if err != nil {
			return nil, err
		}
		if encodedBytes > limits.MaxResultBytes {
			return nil, evidenceError(EvidenceFailureResultTooLarge)
		}
		if evidenceBytes > limits.MaxEvidenceBytes-encodedBytes {
			return nil, evidenceError(EvidenceFailureEvidenceTooLarge)
		}
		evidenceBytes += encodedBytes
		results = append(results, evidenceToolResult{
			callID: call.id, name: call.name, content: owned,
		})
	}
	return results, nil
}

func preflightEvidenceIdentities(
	calls []evidenceToolCall,
	byName map[string]hustle.BoundEvidenceTool,
	newExecutionID evidenceExecutionIDFactory,
) ([]uuid.UUID, error) {
	seenCallIDs := make(map[string]struct{}, len(calls))
	seenExecutionIDs := make(map[uuid.UUID]struct{}, len(calls))
	executionIDs := make([]uuid.UUID, len(calls))
	for index, call := range calls {
		if call.id == "" {
			return nil, evidenceError(EvidenceFailureAmbiguousIdentity)
		}
		if _, duplicate := seenCallIDs[call.id]; duplicate {
			return nil, evidenceError(EvidenceFailureAmbiguousIdentity)
		}
		seenCallIDs[call.id] = struct{}{}
		if _, known := byName[call.name]; !known {
			return nil, evidenceError(EvidenceFailureUnknownTool)
		}
		executionID, err := newExecutionID()
		if err != nil {
			return nil, evidenceError(EvidenceFailureInternal)
		}
		if executionID.IsZero() {
			return nil, evidenceError(EvidenceFailureAmbiguousIdentity)
		}
		if _, duplicate := seenExecutionIDs[executionID]; duplicate {
			return nil, evidenceError(EvidenceFailureAmbiguousIdentity)
		}
		seenExecutionIDs[executionID] = struct{}{}
		executionIDs[index] = executionID
	}
	return executionIDs, nil
}

func indexEvidenceCatalog(catalog []hustle.BoundEvidenceTool) (map[string]hustle.BoundEvidenceTool, error) {
	byName := make(map[string]hustle.BoundEvidenceTool, len(catalog))
	for _, bound := range catalog {
		name := bound.Name()
		if name == "" || nilEvidenceRuntimeValue(reflect.ValueOf(bound.Tool())) {
			return nil, evidenceError(EvidenceFailureInvalidBinding)
		}
		if _, duplicate := byName[name]; duplicate {
			return nil, evidenceError(EvidenceFailureInvalidBinding)
		}
		byName[name] = bound
	}
	return byName, nil
}

func prepareEvidenceCall(
	ctx context.Context,
	preparer tool.CallPreparer,
	executionID uuid.UUID,
	args string,
) (request tool.Request, artifact tool.PreparedArtifact, err error) {
	defer func() {
		if recover() != nil {
			request = tool.Request{}
			artifact = nil
			err = evidenceError(EvidenceFailureInternal)
		}
	}()
	request, artifact, err = preparer.PrepareCall(ctx, executionID, args)
	if err != nil {
		return tool.Request{}, nil, evidenceError(EvidenceFailurePreparation)
	}
	if failure := evidenceContextFailure(ctx); failure != nil {
		return tool.Request{}, nil, failure
	}
	return request, artifact, nil
}

func authorizeEvidenceRequest(
	ctx context.Context,
	access evidenceAccessEvaluator,
	allowedKinds map[string]struct{},
	request tool.Request,
) (err error) {
	for _, requirement := range request.Requirements {
		if _, allowed := allowedKinds[requirement.Kind]; !allowed ||
			requirement.GrantClass != "" || requirement.GrantTarget != "" ||
			len(requirement.Candidates) != 0 {
			return evidenceError(EvidenceFailureForbiddenCapability)
		}
	}
	if err := tool.ValidateRequest(request); err != nil {
		return evidenceError(EvidenceFailureInvalidRequest)
	}
	defer func() {
		if recover() != nil {
			err = evidenceError(EvidenceFailureInternal)
		}
	}()
	for _, requirement := range request.Requirements {
		if failure := evidenceContextFailure(ctx); failure != nil {
			return failure
		}
		configured, accessErr := access.AccessFor(requirement)
		if accessErr != nil || configured != gate.AccessAllow {
			return evidenceError(EvidenceFailureAccessRefused)
		}
	}
	return nil
}

func executeEvidenceCall(
	ctx context.Context,
	concrete tool.InvokableTool,
	args string,
	prepared tool.PreparedCall,
) (result *tool.ToolResult, err error) {
	defer func() {
		if recover() != nil {
			result = nil
			err = evidenceError(EvidenceFailureInternal)
		}
	}()
	result, err = concrete.InvokableRun(withPreparedEvidenceCall(ctx, prepared), args)
	if err != nil {
		return nil, evidenceError(EvidenceFailureExecution)
	}
	if failure := evidenceContextFailure(ctx); failure != nil {
		return nil, failure
	}
	return result, nil
}

func ownEvidenceResult(result *tool.ToolResult) ([]content.Block, int, error) {
	if result == nil || len(result.Content) == 0 {
		return nil, 0, evidenceError(EvidenceFailureInvalidResult)
	}
	encoded, err := content.MarshalBlocks(result.Content)
	if err != nil {
		return nil, 0, evidenceError(EvidenceFailureInvalidResult)
	}
	owned, err := content.UnmarshalBlocks(encoded)
	if err != nil || len(owned) == 0 {
		return nil, 0, evidenceError(EvidenceFailureInvalidResult)
	}
	return owned, len(encoded), nil
}

func evidenceContextFailure(ctx context.Context) error {
	if ctx == nil {
		return evidenceError(EvidenceFailureInvalidBinding)
	}
	switch ctx.Err() {
	case context.Canceled:
		return evidenceError(EvidenceFailureCanceled)
	case context.DeadlineExceeded:
		return evidenceError(EvidenceFailureDeadline)
	default:
		return nil
	}
}

func nilEvidenceRuntimeValue(value reflect.Value) bool {
	if !value.IsValid() {
		return true
	}
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func evidenceError(reason EvidenceFailureReason) *EvidenceError {
	return &EvidenceError{Reason: reason}
}
