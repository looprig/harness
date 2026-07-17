package hustleruntime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
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
	executionCtx, cancelExecutions := context.WithCancel(sessionCtx)
	runtime := &runtimeController{
		sessionCtx: sessionCtx, sessionID: config.SessionID, definitions: definitions,
		executionCtx: executionCtx, cancelExecutions: cancelExecutions,
		auditTimeout: config.AuditTimeout, finalizationTimeout: config.FinalizationTimeout,
		workerDrainTimeout: config.WorkerDrainTimeout, stamper: config.Stamper,
		audit: config.Audit, faults: config.Faults, activity: config.Activity,
		finalizerContext: config.FinalizerContext, after: time.After,
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
	result, runtime, usage, runErr := c.runtime.execute(ctx, definition, request.Name, run.id, request.Cause.LoopID, input, validate)
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

func (r *runtimeController) execute(ctx context.Context, definition hustle.BoundDefinition, name hustle.Name, runID hustle.RunID, loopID uuid.UUID, input json.RawMessage, validate ValidateResult) (hustle.Result, event.ModelRuntime, *content.Usage, *RunError) {
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
		return hustle.Result{}, runtime, usage, executionError(name, runID, hustle.StageOutput, reason, executionCtx, &OutputError{Cause: err})
	}
	if err := executionCtx.Err(); err != nil {
		return hustle.Result{}, runtime, usage, executionError(name, runID, hustle.StageOutput, hustle.ReasonInvalidOutput, executionCtx, err)
	}
	return result, runtime, usage, nil
}

func callValidator(ctx context.Context, validate ValidateResult, result hustle.Result) (err error) {
	defer func() {
		if recover() != nil {
			err = &CallbackPanicError{Stage: hustle.StageOutput}
		}
	}()
	return validate(ctx, result)
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
	results := make(chan invokeResult, 1)
	go func() {
		defer func() {
			if recover() != nil {
				results <- invokeResult{err: &WorkerPanicError{RunID: runID}}
			}
		}()
		response, err := client.Invoke(ctx, request)
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
	if response != nil && response.FinishReason == stream.FinishReasonToolUse {
		return hustle.Result{}, &OutputError{Cause: &inference.StructuredOutputFinishError{Reason: stream.FinishReasonToolUse}}
	}
	output, err := inference.StructuredResult(response)
	if err != nil {
		return hustle.Result{}, &OutputError{Cause: err}
	}
	if !nativeStructuredText(response.Message) {
		return hustle.Result{}, &OutputError{Reason: OutputFailureInvalidShape}
	}
	if len(output) > outputLimit {
		return hustle.Result{}, &OutputError{Reason: OutputFailureTooLarge}
	}
	return hustle.Result{Output: output, Usage: usage}, nil
}

func nativeStructuredText(message *content.AIMessage) bool {
	if message == nil {
		return false
	}
	text := false
	for _, block := range message.Blocks {
		switch typed := block.(type) {
		case *content.TextBlock:
			if typed != nil {
				text = true
			}
		case *content.ToolUseBlock:
			return false
		}
	}
	return text
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
