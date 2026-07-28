package hustleruntime

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/stream"
)

type runtimeController struct {
	owner               *Controller
	sessionCtx          context.Context
	executionCtx        context.Context
	cancelExecutions    context.CancelFunc
	sessionID           uuid.UUID
	definitions         map[hustle.Name]hustle.BoundDefinition
	auditTimeout        time.Duration
	finalizationTimeout time.Duration
	workerDrainTimeout  time.Duration
	stamper             HeaderStamper
	audit               AuditPublisher
	faults              FaultReporter
	activity            ActivityTracker
	finalizerContext    FinalizerContextDecorator
	after               func(time.Duration) <-chan time.Time
	newExecutionContext func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	evidenceRunner      *evidenceRunner
	evidenceWorkspace   *tool.ReadWorkspaceBinding
}

func newRuntimeController(sessionCtx context.Context, config RuntimeConfig) (*runtimeController, error) {
	if config.SessionID.IsZero() {
		return nil, &ConfigError{Reason: ConfigInvalidSessionID, Field: "runtime.session_id"}
	}
	if len(config.Definitions) == 0 {
		return nil, &ConfigError{Reason: ConfigInvalidDefinitions, Field: "runtime.definitions"}
	}
	if config.AuditTimeout <= 0 {
		return nil, &ConfigError{Reason: ConfigInvalidTimeout, Field: "runtime.audit_timeout"}
	}
	if config.FinalizationTimeout <= 0 {
		return nil, &ConfigError{Reason: ConfigInvalidTimeout, Field: "runtime.finalization_timeout"}
	}
	if config.WorkerDrainTimeout <= 0 {
		return nil, &ConfigError{Reason: ConfigInvalidTimeout, Field: "runtime.worker_drain_timeout"}
	}
	if config.Stamper == nil || nilRuntimeValue(reflect.ValueOf(config.Stamper)) {
		return nil, &ConfigError{Reason: ConfigMissingCollaborator, Field: "runtime.stamper"}
	}
	if config.Audit == nil || nilRuntimeValue(reflect.ValueOf(config.Audit)) {
		return nil, &ConfigError{Reason: ConfigMissingCollaborator, Field: "runtime.audit"}
	}
	if config.Faults == nil || nilRuntimeValue(reflect.ValueOf(config.Faults)) {
		return nil, &ConfigError{Reason: ConfigMissingCollaborator, Field: "runtime.faults"}
	}
	if config.Activity == nil || nilRuntimeValue(reflect.ValueOf(config.Activity)) {
		return nil, &ConfigError{Reason: ConfigMissingCollaborator, Field: "runtime.activity"}
	}
	if config.FinalizerContext != nil && nilRuntimeValue(reflect.ValueOf(config.FinalizerContext)) {
		return nil, &ConfigError{Reason: ConfigMissingCollaborator, Field: "runtime.finalizer_context"}
	}
	definitions := make(map[hustle.Name]hustle.BoundDefinition, len(config.Definitions))
	for _, definition := range config.Definitions {
		if definition == nil || nilRuntimeValue(reflect.ValueOf(definition)) || definition.Name() == "" {
			return nil, &ConfigError{Reason: ConfigInvalidDefinitions, Field: "runtime.definitions"}
		}
		if _, exists := definitions[definition.Name()]; exists {
			return nil, &ConfigError{Reason: ConfigInvalidDefinitions, Field: "runtime.definitions"}
		}
		definitions[definition.Name()] = definition
	}
	var runner *evidenceRunner
	var readWorkspace *tool.ReadWorkspaceBinding
	for _, definition := range definitions {
		if _, enabled := definition.EvidenceToolPolicy(); !enabled {
			continue
		}
		if config.Evidence == nil || config.Evidence.ReadWorkspace == nil ||
			config.Evidence.ReadWorkspace.Root == "" {
			return nil, &ConfigError{Reason: ConfigMissingCollaborator, Field: "runtime.evidence"}
		}
		var err error
		runner, err = newEvidenceRunner(
			config.Evidence.Access,
			config.Evidence.Containment,
			EvidenceContainmentPolicy{
				ReadRoot:        config.Evidence.ReadWorkspace.Root,
				SecurityCeiling: config.Evidence.SecurityCeiling,
			},
			config.Evidence.AllowedKinds,
			config.Evidence.NewExecutionID,
		)
		if err != nil {
			return nil, &ConfigError{Reason: ConfigMissingCollaborator, Field: "runtime.evidence"}
		}
		readWorkspace = &tool.ReadWorkspaceBinding{Root: config.Evidence.ReadWorkspace.Root}
		break
	}
	executionCtx, cancelExecutions := context.WithCancel(sessionCtx)
	runtime := &runtimeController{
		sessionCtx: sessionCtx, sessionID: config.SessionID, definitions: definitions,
		executionCtx: executionCtx, cancelExecutions: cancelExecutions,
		auditTimeout: config.AuditTimeout, finalizationTimeout: config.FinalizationTimeout,
		workerDrainTimeout: config.WorkerDrainTimeout, stamper: config.Stamper,
		audit: config.Audit, faults: config.Faults, activity: config.Activity,
		finalizerContext: config.FinalizerContext, after: time.After,
		evidenceRunner: runner, evidenceWorkspace: readWorkspace,
	}
	runtime.newExecutionContext = runtime.executionContextWithTimeout
	return runtime, nil
}

func nilRuntimeValue(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// RunAndFinalize executes one registered definition and retains ownership until
// the consumer's required finalizer returns.
func (c *Controller) RunAndFinalize(ctx context.Context, request hustle.Request, validate ValidateResult, finalizer Finalizer) error {
	definition, input, err := c.preflight(ctx, request, validate, finalizer)
	if err != nil {
		return err
	}
	run, err := c.ownWithEligibility(ctx, definition.Participation(), finalizer, false)
	if err != nil {
		return err
	}
	audit := auditRun{descriptor: definition.Descriptor(), runID: run.id, cause: request.Cause}
	var lease ActivityLease
	var cleanup func() error
	if definition.Participation() == hustle.ParticipationBlocking {
		activityCtx, cancel := c.runtime.newAuditContext()
		lease, err = c.runtime.activity.AcquireHustleActivity(activityCtx, run.id)
		cancel()
		acquisitionErr := err
		if lease != nil && !nilRuntimeValue(reflect.ValueOf(lease)) {
			cleanup = func() error {
				releaseCtx, releaseCancel := c.runtime.newAuditContext()
				defer releaseCancel()
				if releaseErr := lease.Release(releaseCtx); releaseErr != nil {
					if sameErrorValue(releaseErr, acquisitionErr) {
						return nil
					}
					return &ActivityError{RunID: run.id, Operation: ActivityRelease, Cause: releaseErr}
				}
				return nil
			}
		}
		if err == nil && cleanup == nil {
			err = &ActivityError{RunID: run.id, Operation: ActivityAcquire}
		}
		if err != nil {
			activityErr, ok := err.(*ActivityError)
			if !ok {
				activityErr = &ActivityError{RunID: run.id, Operation: ActivityAcquire, Cause: err}
			}
			c.runtime.reportFault(activityErr)
			runErr := &RunError{Name: request.Name, RunID: run.id, Stage: hustle.StageQueue, ReasonCode: hustle.ReasonInternal, Cause: activityErr}
			run.completeSetup(runErr, cleanup, nil)
			run.lane.cancelQueued(run)
			return run.finish(context.Background(), hustle.Outcome{Err: runErr}, nil, true)
		}
	}
	if err := c.runtime.publishStarted(audit); err != nil {
		c.runtime.reportFault(err)
		runErr := &RunError{Name: request.Name, RunID: run.id, Stage: hustle.StageQueue, ReasonCode: hustle.ReasonInternal, Cause: err}
		run.completeSetup(runErr, cleanup, nil)
		run.lane.cancelQueued(run)
		return run.finish(context.Background(), hustle.Outcome{Err: runErr}, nil, true)
	}
	run.completeSetup(nil, cleanup, func(failure *QueueFailureError) error { return c.runtime.publishQueueFailure(audit, failure) })
	if !run.lane.makeEligible(run) {
		return run.awaitExecution()
	}
	if err := run.awaitExecution(); err != nil {
		return err
	}
	startedAt := time.Now()
	result, runtime, usage, runErr := c.runtime.execute(ctx, definition, request, run.id, input, validate)
	duration := time.Since(startedAt)
	if runErr == nil {
		err = c.runtime.publishCompleted(audit, runtime, result, duration)
		if err != nil {
			c.runtime.reportFault(err)
			runErr = &RunError{Name: request.Name, RunID: run.id, Stage: hustle.StageTerminal, ReasonCode: hustle.ReasonTerminal, Cause: err}
			err = runErr
		}
	} else {
		runErr.TerminalErr = c.runtime.publishFailed(audit, runtime, usage, runErr, duration)
		if runErr.TerminalErr != nil {
			c.runtime.reportFault(runErr.TerminalErr)
		}
		err = runErr
	}
	outcome := hustle.Outcome{Result: &result}
	if err != nil {
		outcome = hustle.Outcome{Err: err}
	}
	return run.finalize(context.Background(), outcome)
}

func sameErrorValue(left, right error) bool {
	if left == nil || right == nil {
		return false
	}
	leftType := reflect.TypeOf(left)
	rightType := reflect.TypeOf(right)
	return leftType.Comparable() && rightType.Comparable() && left == right
}

func (c *Controller) preflight(ctx context.Context, request hustle.Request, validate ValidateResult, finalizer Finalizer) (hustle.BoundDefinition, json.RawMessage, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, nil, &RequestError{Reason: RequestInvalidContext, Name: request.Name}
	}
	if c.runtime == nil {
		return nil, nil, &RequestError{Reason: RequestRuntimeUnavailable, Name: request.Name}
	}
	definition := c.runtime.definitions[request.Name]
	if definition == nil {
		return nil, nil, &RequestError{Reason: RequestUnknownDefinition, Name: request.Name}
	}
	if request.Cause.LoopID.IsZero() {
		return nil, nil, &RequestError{Reason: RequestInvalidCause, Name: request.Name}
	}
	if len(request.Input) == 0 {
		return nil, nil, &RequestError{Reason: RequestInvalidInput, Name: request.Name}
	}
	if len(request.Input) > definition.Limits().InputBytes {
		return nil, nil, &RequestError{Reason: RequestInputTooLarge, Name: request.Name}
	}
	if !json.Valid(request.Input) {
		return nil, nil, &RequestError{Reason: RequestInvalidInput, Name: request.Name}
	}
	if validate == nil {
		return nil, nil, &RequestError{Reason: RequestNilValidator, Name: request.Name}
	}
	if finalizer == nil {
		return nil, nil, &AdmissionError{Reason: AdmissionNilFinalizer, Participation: definition.Participation()}
	}
	return definition, append(json.RawMessage(nil), request.Input...), nil
}

func (r *runtimeController) execute(ctx context.Context, definition hustle.BoundDefinition, request hustle.Request, runID hustle.RunID, input json.RawMessage, validate ValidateResult) (hustle.Result, event.ModelRuntime, *content.Usage, *RunError) {
	if _, enabled := definition.EvidenceToolPolicy(); enabled {
		return r.executeWithEvidence(ctx, definition, request, runID, input, validate)
	}
	return r.executeSingle(ctx, definition, request.Name, runID, request.Cause.LoopID, input, validate)
}

func (r *runtimeController) executeSingle(ctx context.Context, definition hustle.BoundDefinition, name hustle.Name, runID hustle.RunID, loopID uuid.UUID, input json.RawMessage, validate ValidateResult) (hustle.Result, event.ModelRuntime, *content.Usage, *RunError) {
	executionCtx, cancel := r.newExecutionContext(ctx, definition.Timeout())
	defer cancel()
	binding, err := definition.ResolveInference(executionCtx, loopID)
	if err != nil {
		return hustle.Result{}, event.ModelRuntime{}, nil, executionError(name, runID, hustle.StageModelResolution, hustle.ReasonModelResolution, executionCtx, err)
	}
	if err := executionCtx.Err(); err != nil {
		return hustle.Result{}, event.ModelRuntime{}, nil, executionError(name, runID, hustle.StageModelResolution, hustle.ReasonModelResolution, executionCtx, err)
	}
	runtime := event.ModelRuntime{Key: binding.Model.Key(), Limits: binding.Model.Limits, Effort: binding.Model.Sampling.Effort}
	output, _ := definition.OutputSchema()
	request := inference.Request{
		Model:  binding.Model.Clone(),
		System: definition.SystemPrompt(),
		Messages: content.AgenticMessages{&content.UserMessage{Message: content.Message{
			Role:   content.RoleUser,
			Blocks: []content.Block{&content.TextBlock{Text: string(input)}},
		}}},
		Output: output,
	}
	if err := inference.ValidateRequestFeatures(request); err != nil {
		return hustle.Result{}, runtime, nil, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, &OutputError{Cause: err})
	}
	response, err := r.invoke(executionCtx, runID, binding.Client, request)
	usage, usageErr := responseUsage(response)
	if err != nil {
		reason := hustle.ReasonInference
		var panicErr *WorkerPanicError
		if errors.As(err, &panicErr) {
			reason = hustle.ReasonInternal
			r.reportFault(panicErr)
		}
		return hustle.Result{}, runtime, usage, executionError(name, runID, hustle.StageInference, reason, executionCtx, err)
	}
	if usageErr != nil {
		return hustle.Result{}, runtime, nil, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, &OutputError{Cause: usageErr})
	}
	var result hustle.Result
	if output == nil {
		result, err = extractResult(response, usage, definition.Limits().OutputBytes)
	} else {
		result, err = extractStructuredResult(response, usage, definition.Limits().OutputBytes)
	}
	if err != nil {
		return hustle.Result{}, runtime, usage, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, err)
	}
	if err := callValidator(executionCtx, validate, result); err != nil {
		reason := hustle.ReasonInvalidOutput
		var panicErr *CallbackPanicError
		if errors.As(err, &panicErr) {
			reason = hustle.ReasonInternal
			r.reportFault(panicErr)
		}
		err = normalizeValidatorError(err)
		return hustle.Result{}, runtime, usage, executionError(name, runID, hustle.StageOutput, reason, executionCtx, &OutputError{Cause: err})
	}
	if err := executionCtx.Err(); err != nil {
		return hustle.Result{}, runtime, usage, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, err)
	}
	return result, runtime, usage, nil
}

func (r *runtimeController) executeWithEvidence(
	ctx context.Context,
	definition hustle.BoundDefinition,
	runRequest hustle.Request,
	runID hustle.RunID,
	input json.RawMessage,
	validate ValidateResult,
) (hustle.Result, event.ModelRuntime, *content.Usage, *RunError) {
	executionCtx, cancel := r.newExecutionContext(ctx, definition.Timeout())
	defer cancel()
	evidenceCtx, cleanupEvidenceCtx := newEvidenceAttemptContext(executionCtx)
	defer cleanupEvidenceCtx()
	plan, runtime, runErr := r.prepareEvidenceExecution(
		evidenceCtx, definition, runRequest, runID, input,
	)
	if runErr != nil {
		return hustle.Result{}, runtime, nil, runErr
	}
	var aggregate *content.Usage
	for attempt := 0; ; attempt++ {
		result, usage, runErr := r.executeEvidenceAttempt(
			evidenceCtx, definition, runRequest, runID, plan, validate,
		)
		var usageErr error
		aggregate, usageErr = addUsage(aggregate, usage)
		if usageErr != nil {
			return hustle.Result{}, runtime, aggregate, executionError(
				runRequest.Name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput,
				evidenceCtx, &OutputError{Cause: usageErr},
			)
		}
		if runErr == nil {
			result.Usage = cloneUsage(aggregate)
			return result, runtime, aggregate, nil
		}
		if !shouldRetry(
			definition.RetryPolicy(), attempt, evidenceCtx.Err(), runErr,
			r.owner.isPoisoned(),
		) {
			return hustle.Result{}, runtime, aggregate, runErr
		}
	}
}

type evidenceExecutionPlan struct {
	binding    hustle.InferenceBinding
	policy     hustle.EvidenceToolPolicy
	knownTools map[string]struct{}
	request    inference.Request
}

func (r *runtimeController) prepareEvidenceExecution(
	executionCtx context.Context,
	definition hustle.BoundDefinition,
	runRequest hustle.Request,
	runID hustle.RunID,
	input json.RawMessage,
) (evidenceExecutionPlan, event.ModelRuntime, *RunError) {
	name := runRequest.Name
	if runRequest.Cause.SessionID.IsZero() || runRequest.Cause.SessionID != r.sessionID {
		return evidenceExecutionPlan{}, event.ModelRuntime{}, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, evidenceError(EvidenceFailureInvalidBinding))
	}
	binding, err := definition.ResolveInference(executionCtx, runRequest.Cause.LoopID)
	if err != nil {
		return evidenceExecutionPlan{}, event.ModelRuntime{}, executionError(name, runID, hustle.StageModelResolution, hustle.ReasonModelResolution, executionCtx, err)
	}
	if err := executionCtx.Err(); err != nil {
		return evidenceExecutionPlan{}, event.ModelRuntime{}, executionError(name, runID, hustle.StageModelResolution, hustle.ReasonModelResolution, executionCtx, err)
	}
	runtime := event.ModelRuntime{Key: binding.Model.Key(), Limits: binding.Model.Limits, Effort: binding.Model.Sampling.Effort}
	output, outputEnabled := definition.OutputSchema()
	policy, policyEnabled := definition.EvidenceToolPolicy()
	if !outputEnabled || !policyEnabled || r.evidenceRunner == nil || r.evidenceWorkspace == nil {
		return evidenceExecutionPlan{}, runtime, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, evidenceError(EvidenceFailureInvalidBinding))
	}
	tools, knownTools, err := staticEvidenceTools(policy)
	if err != nil {
		return evidenceExecutionPlan{}, runtime, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, err)
	}
	request := inference.Request{
		Model: binding.Model.Clone(), System: definition.SystemPrompt(),
		Messages: content.AgenticMessages{&content.UserMessage{Message: content.Message{
			Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: string(input)}},
		}}},
		Tools: tools, Output: output, ToolChoice: inference.ToolChoiceAuto,
	}
	if err := validateEvidenceRequestFeatures(request); err != nil {
		return evidenceExecutionPlan{}, runtime, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, &OutputError{Cause: err})
	}
	return evidenceExecutionPlan{
		binding: binding, policy: policy, knownTools: knownTools, request: request,
	}, runtime, nil
}

func (r *runtimeController) executeEvidenceAttempt(
	executionCtx context.Context,
	definition hustle.BoundDefinition,
	runRequest hustle.Request,
	runID hustle.RunID,
	plan evidenceExecutionPlan,
	validate ValidateResult,
) (hustle.Result, *content.Usage, *RunError) {
	name := runRequest.Name
	inferenceRequest, err := ownInferenceRequest(plan.request)
	if err != nil {
		return hustle.Result{}, nil, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, err)
	}
	catalog, err := r.bindEvidence(
		executionCtx, runID, definition, runRequest, r.evidenceWorkspace,
	)
	if err != nil {
		return hustle.Result{}, nil, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, err)
	}
	var aggregate *content.Usage
	rounds, calls, evidenceBytes := 0, 0, 0
	seenCallIDs := make(map[string]struct{})
	for {
		if rounds >= plan.policy.Limits.MaxRounds {
			return hustle.Result{}, aggregate, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, evidenceError(EvidenceFailureRoundsExceeded))
		}
		if err := validateEvidenceRequestFeatures(inferenceRequest); err != nil {
			return hustle.Result{}, aggregate, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, &OutputError{Cause: err})
		}
		response, invokeErr := r.invoke(executionCtx, runID, plan.binding.Client, inferenceRequest)
		rounds++
		var classified classifiedToolResponse
		var classifyErr error
		if invokeErr == nil {
			classified, classifyErr = classifyToolResponse(response, plan.knownTools, toolResponseLimits{
				outputBytes:      definition.Limits().OutputBytes,
				maxCallsPerRound: plan.policy.Limits.MaxCallsPerRound,
			})
		}
		roundUsage, usageErr := responseUsage(response)
		if usageErr == nil {
			var addErr error
			aggregate, addErr = addUsage(aggregate, roundUsage)
			if addErr != nil {
				return hustle.Result{}, aggregate, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, &OutputError{Cause: addErr})
			}
		}
		if invokeErr != nil {
			reason := hustle.ReasonInference
			var panicErr *WorkerPanicError
			if errors.As(invokeErr, &panicErr) {
				reason = hustle.ReasonInternal
				r.reportFault(panicErr)
			}
			return hustle.Result{}, aggregate, executionError(name, runID, hustle.StageInference, reason, executionCtx, invokeErr)
		}
		if usageErr != nil {
			return hustle.Result{}, aggregate, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, &OutputError{Cause: usageErr})
		}
		if classifyErr != nil {
			return hustle.Result{}, aggregate, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, classifyErr)
		}
		providerMessage := response.Message
		switch response := classified.(type) {
		case terminalToolResponse:
			result := hustle.Result{Output: append(json.RawMessage(nil), response.output...), Usage: cloneUsage(aggregate)}
			if err := callValidator(executionCtx, validate, result); err != nil {
				reason := hustle.ReasonInvalidOutput
				var panicErr *CallbackPanicError
				if errors.As(err, &panicErr) {
					reason = hustle.ReasonInternal
					r.reportFault(panicErr)
				}
				err = normalizeValidatorError(err)
				return hustle.Result{}, aggregate, executionError(name, runID, hustle.StageOutput, reason, executionCtx, &OutputError{Cause: err})
			}
			if err := executionCtx.Err(); err != nil {
				return hustle.Result{}, aggregate, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, err)
			}
			return result, aggregate, nil
		case evidenceToolResponse:
			// The provider response is borrowed. Own the validated assistant
			// message before preparation, authorization, or execution gives
			// another collaborator a chance to mutate it.
			assistant, err := ownAIMessage(providerMessage)
			if err != nil {
				return hustle.Result{}, aggregate, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, err)
			}
			if len(response.calls) > plan.policy.Limits.MaxCallsPerRound {
				return hustle.Result{}, aggregate, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, evidenceError(EvidenceFailureCallsPerRoundExceeded))
			}
			if calls > plan.policy.Limits.MaxCalls-len(response.calls) {
				return hustle.Result{}, aggregate, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, evidenceError(EvidenceFailureCallsExceeded))
			}
			for _, call := range response.calls {
				if _, duplicate := seenCallIDs[call.id]; duplicate {
					return hustle.Result{}, aggregate, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, toolResponseError(ToolResponseFailureDuplicateCallID))
				}
			}
			for _, call := range response.calls {
				seenCallIDs[call.id] = struct{}{}
			}
			remaining := plan.policy.Limits.MaxEvidenceBytes - evidenceBytes
			if remaining <= 0 {
				return hustle.Result{}, aggregate, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, evidenceError(EvidenceFailureEvidenceTooLarge))
			}
			roundLimits := plan.policy.Limits
			roundLimits.MaxEvidenceBytes = remaining
			results, err := r.runEvidence(executionCtx, runID, catalog, response.calls, roundLimits)
			if err != nil {
				return hustle.Result{}, aggregate, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, err)
			}
			calls += len(response.calls)
			for _, result := range results {
				encoded, marshalErr := content.MarshalBlocks(result.content)
				if marshalErr != nil || len(encoded) > remaining {
					return hustle.Result{}, aggregate, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, evidenceError(EvidenceFailureEvidenceTooLarge))
				}
				evidenceBytes += len(encoded)
				remaining -= len(encoded)
			}
			inferenceRequest.Messages = append(inferenceRequest.Messages, assistant)
			for _, result := range results {
				blocks, err := ownBlocks(result.content)
				if err != nil {
					return hustle.Result{}, aggregate, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, err)
				}
				inferenceRequest.Messages = append(inferenceRequest.Messages, &content.ToolResultMessage{
					Message:   content.Message{Role: content.RoleTool, Blocks: blocks},
					ToolUseID: result.callID,
				})
			}
		default:
			return hustle.Result{}, aggregate, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, toolResponseError(ToolResponseFailureInvalidShape))
		}
	}
}

func validateEvidenceRequestFeatures(request inference.Request) error {
	if !request.Model.Caps.Tools || !request.Model.Caps.StructuredOutput ||
		!request.Model.Caps.StructuredOutputWithTools {
		return &inference.StructuredOutputWithToolsUnsupportedError{Model: request.Model.Name}
	}
	return inference.ValidateRequestFeatures(request)
}

func staticEvidenceTools(policy hustle.EvidenceToolPolicy) ([]inference.Tool, map[string]struct{}, error) {
	tools := make([]inference.Tool, 0)
	known := make(map[string]struct{})
	for _, definition := range policy.Definitions {
		for _, info := range definition.ToolInfos() {
			if _, duplicate := known[info.Name]; duplicate {
				return nil, nil, evidenceError(EvidenceFailureInvalidBinding)
			}
			known[info.Name] = struct{}{}
			tools = append(tools, inference.Tool{
				Name: info.Name, Description: info.Desc, Schema: append(json.RawMessage(nil), info.Schema...),
			})
		}
	}
	if len(tools) == 0 {
		return nil, nil, evidenceError(EvidenceFailureInvalidBinding)
	}
	return tools, known, nil
}

func addUsage(aggregate, current *content.Usage) (*content.Usage, error) {
	if current == nil {
		return cloneUsage(aggregate), nil
	}
	if aggregate == nil {
		return cloneUsage(current), nil
	}
	sum, err := aggregate.Add(*current)
	if err != nil {
		return cloneUsage(aggregate), err
	}
	return &sum, nil
}

type evidenceRunResult struct {
	results []evidenceToolResult
	err     error
	panic   bool
}

func (r *runtimeController) runEvidence(
	ctx context.Context,
	runID hustle.RunID,
	catalog []hustle.BoundEvidenceTool,
	calls []evidenceToolCall,
	limits hustle.ToolLoopLimits,
) ([]evidenceToolResult, error) {
	done := make(chan evidenceRunResult, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- evidenceRunResult{
					err: evidenceError(EvidenceFailureInternal), panic: true,
				}
			}
		}()
		results, err := r.evidenceRunner.run(ctx, catalog, calls, limits)
		done <- evidenceRunResult{results: results, err: err}
	}()
	select {
	case result := <-done:
		var collaboratorPanic *evidencePanicError
		if result.panic || errors.As(result.err, &collaboratorPanic) {
			r.reportFault(&EvidenceWorkerPanicError{RunID: runID})
			return nil, evidenceError(EvidenceFailureInternal)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return result.results, result.err
	case <-ctx.Done():
	}
	select {
	case result := <-done:
		var collaboratorPanic *evidencePanicError
		if result.panic || errors.As(result.err, &collaboratorPanic) {
			r.reportFault(&EvidenceWorkerPanicError{RunID: runID})
			return nil, evidenceError(EvidenceFailureInternal)
		}
		return nil, ctx.Err()
	case <-r.after(r.workerDrainTimeout):
		r.owner.poison(&WorkerPoisonError{RunID: runID, Cause: ctx.Err()})
		return nil, ctx.Err()
	}
}

type evidenceBindResult struct {
	catalog []hustle.BoundEvidenceTool
	err     error
	panic   bool
}

// bindEvidence places invocation-scoped factory construction and Info
// validation under the same bounded worker ownership policy as inference and
// evidence execution. A factory that ignores cancellation poisons admission;
// a panic is recovered and reported without retaining its value.
func (r *runtimeController) bindEvidence(
	ctx context.Context,
	runID hustle.RunID,
	definition hustle.BoundDefinition,
	request hustle.Request,
	readWorkspace *tool.ReadWorkspaceBinding,
) ([]hustle.BoundEvidenceTool, error) {
	done := make(chan evidenceBindResult, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- evidenceBindResult{
					err: evidenceError(EvidenceFailureInternal), panic: true,
				}
			}
		}()
		catalog, err := bindEvidenceInvocation(
			ctx, definition, request, readWorkspace,
		)
		done <- evidenceBindResult{catalog: catalog, err: err}
	}()
	select {
	case result := <-done:
		var collaboratorPanic *evidencePanicError
		if result.panic || errors.As(result.err, &collaboratorPanic) {
			r.reportFault(&EvidenceWorkerPanicError{RunID: runID})
			return nil, evidenceError(EvidenceFailureInternal)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return result.catalog, result.err
	case <-ctx.Done():
	}
	select {
	case result := <-done:
		var collaboratorPanic *evidencePanicError
		if result.panic || errors.As(result.err, &collaboratorPanic) {
			r.reportFault(&EvidenceWorkerPanicError{RunID: runID})
			return nil, evidenceError(EvidenceFailureInternal)
		}
		return nil, ctx.Err()
	case <-r.after(r.workerDrainTimeout):
		r.owner.poison(&WorkerPoisonError{RunID: runID, Cause: ctx.Err()})
		return nil, ctx.Err()
	}
}

func ownAIMessage(message *content.AIMessage) (*content.AIMessage, error) {
	if message == nil {
		return nil, nil
	}
	blocks, err := ownBlocks(message.Blocks)
	if err != nil {
		return nil, err
	}
	return &content.AIMessage{
		Message: content.Message{Role: message.Role, Blocks: blocks},
		Usage:   cloneUsage(message.Usage),
	}, nil
}

func ownBlocks(blocks []content.Block) ([]content.Block, error) {
	encoded, err := content.MarshalBlocks(blocks)
	if err != nil {
		return nil, err
	}
	return content.UnmarshalBlocks(encoded)
}

func callValidator(ctx context.Context, validate ValidateResult, result hustle.Result) (err error) {
	defer func() {
		if recover() != nil {
			err = &CallbackPanicError{Stage: hustle.StageOutput}
		}
	}()
	return validate(ctx, result)
}

func normalizeValidatorError(err error) error {
	if hustle.IsRecoverableTerminalValidationError(err) {
		return hustle.NewRecoverableTerminalValidationError()
	}
	return err
}

func executionError(name hustle.Name, runID hustle.RunID, stage hustle.Stage, reason hustle.ReasonCode, ctx context.Context, cause error) *RunError {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		reason = hustle.ReasonTimeout
	} else if errors.Is(ctx.Err(), context.Canceled) {
		reason = hustle.ReasonCanceled
	}
	return &RunError{Name: name, RunID: runID, Stage: stage, ReasonCode: reason, Cause: cause}
}

type invokeResult struct {
	response *inference.Response
	err      error
}

func (r *runtimeController) invoke(ctx context.Context, runID hustle.RunID, client inference.Client, request inference.Request) (*inference.Response, error) {
	ownedRequest, err := ownInferenceRequest(request)
	if err != nil {
		return nil, err
	}
	results := make(chan invokeResult, 1)
	go func() {
		defer func() {
			if recover() != nil {
				results <- invokeResult{err: &WorkerPanicError{RunID: runID}}
			}
		}()
		response, err := client.Invoke(ctx, ownedRequest)
		results <- invokeResult{response: response, err: err}
	}()
	select {
	case result := <-results:
		if err := ctx.Err(); err != nil {
			return result.response, err
		}
		return result.response, result.err
	case <-ctx.Done():
	}
	select {
	case result := <-results:
		return result.response, ctx.Err()
	case <-r.after(r.workerDrainTimeout):
		poisonErr := &WorkerPoisonError{RunID: runID, Cause: ctx.Err()}
		r.owner.poison(poisonErr)
		return nil, ctx.Err()
	}
}

func ownInferenceRequest(request inference.Request) (inference.Request, error) {
	owned := inference.Request{
		Model: request.Model.Clone(), System: request.System, ToolChoice: request.ToolChoice,
	}
	if request.Output != nil {
		output := request.Output.Clone()
		owned.Output = &output
	}
	if request.Override != nil {
		override := request.Override.Clone()
		owned.Override = &override
	}
	if request.Tools != nil {
		owned.Tools = make([]inference.Tool, len(request.Tools))
		for index, candidate := range request.Tools {
			owned.Tools[index] = inference.Tool{
				Name: candidate.Name, Description: candidate.Description,
				Schema: append(json.RawMessage(nil), candidate.Schema...),
			}
		}
	}
	owned.Messages = make(content.AgenticMessages, 0, len(request.Messages))
	for _, message := range request.Messages {
		switch typed := message.(type) {
		case *content.UserMessage:
			blocks, err := ownBlocks(typed.Blocks)
			if err != nil {
				return inference.Request{}, err
			}
			owned.Messages = append(owned.Messages, &content.UserMessage{Message: content.Message{Role: typed.Role, Blocks: blocks}})
		case *content.AIMessage:
			copy, err := ownAIMessage(typed)
			if err != nil {
				return inference.Request{}, err
			}
			owned.Messages = append(owned.Messages, copy)
		case *content.SystemMessage:
			blocks, err := ownBlocks(typed.Blocks)
			if err != nil {
				return inference.Request{}, err
			}
			owned.Messages = append(owned.Messages, &content.SystemMessage{Message: content.Message{Role: typed.Role, Blocks: blocks}})
		case *content.ToolResultMessage:
			blocks, err := ownBlocks(typed.Blocks)
			if err != nil {
				return inference.Request{}, err
			}
			owned.Messages = append(owned.Messages, &content.ToolResultMessage{
				Message:   content.Message{Role: typed.Role, Blocks: blocks},
				ToolUseID: typed.ToolUseID, IsError: typed.IsError,
			})
		default:
			return inference.Request{}, toolResponseError(ToolResponseFailureInvalidShape)
		}
	}
	return owned, nil
}

func extractResult(response *inference.Response, usage *content.Usage, outputLimit int) (hustle.Result, error) {
	if response == nil || response.Message == nil || response.Message.Role != content.RoleAssistant || len(response.Message.Blocks) != 1 {
		return hustle.Result{}, &OutputError{Reason: OutputFailureInvalidShape}
	}
	block, ok := response.Message.Blocks[0].(*content.TextBlock)
	if !ok || block == nil {
		return hustle.Result{}, &OutputError{Reason: OutputFailureInvalidShape}
	}
	if len(block.Text) == 0 {
		return hustle.Result{}, &OutputError{Reason: OutputFailureEmptyText}
	}
	if len(block.Text) > outputLimit {
		return hustle.Result{}, &OutputError{Reason: OutputFailureTooLarge}
	}
	if !json.Valid([]byte(block.Text)) {
		return hustle.Result{}, &OutputError{Reason: OutputFailureInvalidJSON}
	}
	return hustle.Result{Output: append(json.RawMessage(nil), block.Text...), Usage: usage}, nil
}

func extractStructuredResult(response *inference.Response, usage *content.Usage, outputLimit int) (hustle.Result, error) {
	if err := nativeStructuredFinishError(response); err != nil {
		return hustle.Result{}, &OutputError{Cause: err}
	}
	output, err := inference.StructuredResult(response)
	if err != nil {
		return hustle.Result{}, &OutputError{Cause: err}
	}
	rawBytes, nativeText, overflow := nativeStructuredTextSize(response)
	if !nativeText {
		return hustle.Result{}, &OutputError{Reason: OutputFailureInvalidShape}
	}
	if overflow || rawBytes > outputLimit {
		return hustle.Result{}, &OutputError{Reason: OutputFailureTooLarge}
	}
	return hustle.Result{Output: output, Usage: usage}, nil
}

func nativeStructuredFinishError(response *inference.Response) error {
	if response == nil {
		return nil
	}
	switch response.FinishReason {
	case stream.FinishReasonLength, stream.FinishReasonContentFilter, stream.FinishReasonToolUse:
		return &inference.StructuredOutputFinishError{Reason: response.FinishReason}
	case stream.FinishReasonStop:
		if containsNonNilToolUse(response.Message) {
			return &inference.StructuredOutputFinishError{Reason: stream.FinishReasonStop}
		}
	case stream.FinishReasonUnknown:
	default:
		return &inference.StructuredOutputFinishError{Reason: inference.StructuredOutputFinishReasonOther}
	}
	return nil
}

func containsNonNilToolUse(message *content.AIMessage) bool {
	if message == nil {
		return false
	}
	for _, block := range message.Blocks {
		if tool, ok := block.(*content.ToolUseBlock); ok && tool != nil {
			return true
		}
	}
	return false
}

func nativeStructuredTextSize(response *inference.Response) (int, bool, bool) {
	if response == nil || response.Message == nil || response.Message.Role != content.RoleAssistant {
		return 0, false, false
	}
	textSeen := false
	total := 0
	for _, block := range response.Message.Blocks {
		switch typed := block.(type) {
		case *content.TextBlock:
			if typed == nil {
				return 0, false, false
			}
			textSeen = true
			fragmentLength := len(typed.Text)
			if fragmentLength > math.MaxInt-total {
				return 0, true, true
			}
			total += fragmentLength
		case *content.ThinkingBlock:
			if typed == nil {
				return 0, false, false
			}
		case *content.ToolUseBlock:
			return 0, false, false
		default:
			return 0, false, false
		}
	}
	return total, textSeen, false
}

func responseUsage(response *inference.Response) (*content.Usage, error) {
	if response == nil || response.Usage == nil {
		return nil, nil
	}
	usage := cloneUsage(response.Usage)
	if err := usage.Validate(); err != nil {
		return nil, err
	}
	return usage, nil
}

func cloneUsage(usage *content.Usage) *content.Usage {
	if usage == nil {
		return nil
	}
	copy := *usage
	return &copy
}

func (r *runtimeController) newAuditContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.sessionCtx), r.auditTimeout)
}

func (r *runtimeController) newFinalizationContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.sessionCtx), r.finalizationTimeout)
	if r.finalizerContext != nil {
		ctx = r.finalizerContext.DecorateFinalizerContext(ctx)
	}
	return ctx, cancel
}

func (r *runtimeController) executionContextWithTimeout(caller context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	combined, cancelCombined := context.WithCancel(caller)
	stopSessionCancel := context.AfterFunc(r.executionCtx, cancelCombined)
	if r.executionCtx.Err() != nil {
		cancelCombined()
	}
	execution, cancelTimeout := context.WithTimeout(combined, timeout)
	return execution, func() {
		cancelTimeout()
		stopSessionCancel()
		cancelCombined()
	}
}

func (r *runtimeController) reportFault(err error) {
	ctx, cancel := r.newAuditContext()
	defer cancel()
	r.faults.ReportFault(ctx, err)
}
