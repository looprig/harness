package loopruntime

import (
	"context"
	"errors"
	"sync"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
)

type compactionExecutorConfig struct {
	Compactor           Compactor
	Counter             inference.ContextCounter
	CounterCapability   inference.CounterCapability
	InferenceCapability inference.InferenceCapability
	Settings            contextAdmissionSettings
	MaxSummaryTokens    content.TokenCount
}

type compactionExecutionCandidate struct {
	Measurement     event.ContextMeasurement
	Request         inference.Request
	RuntimeTail     *content.UserMessage
	RuntimeRevision string
	Transcript      content.AgenticMessages
}

type compactionExecutorError struct{ Field string }

func (e *compactionExecutorError) Error() string {
	return "loopruntime: invalid compaction executor field " + e.Field
}

type compactionExecutionResult struct {
	outcome contextCompactionAwaitResult
	err     error
}

type compactionExecutionRun struct {
	result chan compactionExecutionResult
	cancel context.CancelFunc
}

type compactionExecutor struct {
	ctx    context.Context
	config compactionExecutorConfig
	mu     sync.Mutex
	runs   map[event.CompactAttemptID]compactionExecutionRun
}

func newCompactionExecutor(ctx context.Context, config compactionExecutorConfig) (*compactionExecutor, error) {
	if ctx == nil {
		return nil, &compactionExecutorError{Field: "context"}
	}
	if config.Compactor == nil {
		return nil, &compactionExecutorError{Field: "compactor"}
	}
	if config.Counter == nil {
		return nil, &compactionExecutorError{Field: "counter"}
	}
	if config.Settings.CountTimeout <= 0 || config.MaxSummaryTokens == 0 {
		return nil, &compactionExecutorError{Field: "policy"}
	}
	if err := config.CounterCapability.Validate(); err != nil {
		return nil, &compactionExecutorError{Field: "counter_capability"}
	}
	if err := config.InferenceCapability.Validate(); err != nil {
		return nil, &compactionExecutorError{Field: "inference_capability"}
	}
	return &compactionExecutor{ctx: ctx, config: config, runs: make(map[event.CompactAttemptID]compactionExecutionRun)}, nil
}

func installCompactionExecutor(ctx context.Context, config *runtimeConfig, compactor Compactor) error {
	if compactor == nil {
		return nil
	}
	if config == nil || config.Compaction == nil {
		return &compactionExecutorError{Field: "policy"}
	}
	executor, err := newCompactionExecutor(ctx, compactionExecutorConfig{
		Compactor: compactor, Counter: config.ContextCounter,
		CounterCapability: config.CounterCapability, InferenceCapability: config.InferenceCapability,
		Settings: compactionAdmissionSettings(*config.Compaction), MaxSummaryTokens: config.Compaction.MaxSummaryTokens,
	})
	if err != nil {
		return err
	}
	config.compactionSink = executor
	return nil
}

func (e *compactionExecutor) CoordinateCompaction(context.Context, compactionDisposition) error {
	return &compactionExecutorError{Field: "candidate"}
}

func (e *compactionExecutor) CoordinateCompactionCandidate(
	_ context.Context,
	disposition compactionDisposition,
	candidate compactionExecutionCandidate,
) error {
	if disposition.Kind != compactionDispositionStart || disposition.Attempt == nil {
		return &compactionExecutorError{Field: "disposition"}
	}
	attempt := *disposition.Attempt
	result := make(chan compactionExecutionResult, 1)
	runCtx, cancel := context.WithCancel(e.ctx)
	e.mu.Lock()
	if _, exists := e.runs[attempt.AttemptID]; exists {
		e.mu.Unlock()
		cancel()
		return &compactionExecutorError{Field: "attempt"}
	}
	e.runs[attempt.AttemptID] = compactionExecutionRun{result: result, cancel: cancel}
	e.mu.Unlock()
	candidate.Request.Messages = cloneMessages(candidate.Request.Messages)
	candidate.RuntimeTail = cloneUserMessage(candidate.RuntimeTail)
	candidate.Transcript = cloneMessages(candidate.Transcript)
	go func() { result <- e.execute(runCtx, attempt, candidate) }()
	return nil
}

func (e *compactionExecutor) AwaitCompaction(ctx context.Context, attemptID event.CompactAttemptID) (contextCompactionAwaitResult, error) {
	e.mu.Lock()
	run, exists := e.runs[attemptID]
	e.mu.Unlock()
	if !exists {
		return contextCompactionAwaitResult{}, &compactionExecutorError{Field: "attempt"}
	}
	select {
	case completed := <-run.result:
		run.cancel()
		e.mu.Lock()
		delete(e.runs, attemptID)
		e.mu.Unlock()
		return completed.outcome, completed.err
	case <-ctx.Done():
		run.cancel()
		e.mu.Lock()
		delete(e.runs, attemptID)
		e.mu.Unlock()
		return rejectedCompactionResult(event.CompactRejectCanceled), nil
	}
}

func (e *compactionExecutor) execute(ctx context.Context, attempt compactionAttempt, candidate compactionExecutionCandidate) compactionExecutionResult {
	input := loop.CompactionInput{
		Basis: attempt.Basis, Model: candidate.Measurement.Model,
		RequestFingerprint: candidate.Measurement.RequestFingerprint,
		Transcript:         cloneMessages(candidate.Transcript), MaxSummaryTokens: e.config.MaxSummaryTokens,
	}
	var prepared contextCompactionAwaitResult
	finalized := false
	err := e.config.Compactor.CompactAndFinalize(ctx, input, func(finalizeCtx context.Context, outcome CompactionOutcome) error {
		finalized = true
		prepared = e.prepare(finalizeCtx, attempt, candidate, outcome)
		return nil
	})
	if !finalized {
		prepared = rejectedCompactionResultWithError(compactionRejectReason(err), err)
	}
	return compactionExecutionResult{outcome: prepared}
}

func (e *compactionExecutor) prepare(
	ctx context.Context,
	attempt compactionAttempt,
	candidate compactionExecutionCandidate,
	outcome CompactionOutcome,
) contextCompactionAwaitResult {
	if err := outcome.Validate(); err != nil {
		return rejectedCompactionResult(event.CompactRejectInternal)
	}
	if outcome.Err != nil {
		return rejectedCompactionResultWithError(compactionRejectReason(outcome.Err), outcome.Err)
	}
	value := outcome.Value
	if value.Basis != attempt.Basis || value.Model != candidate.Measurement.Model ||
		value.RequestFingerprint != candidate.Measurement.RequestFingerprint {
		return rejectedCompactionResult(event.CompactRejectInvalidSummary)
	}
	request := candidate.Request
	request.Messages = content.AgenticMessages{cloneUserMessage(value.Summary)}
	if candidate.RuntimeTail != nil {
		request.Messages = append(request.Messages, cloneUserMessage(candidate.RuntimeTail))
	}
	measurement, err := measureRequestContext(
		ctx, e.config.Counter, e.config.CounterCapability, e.config.InferenceCapability,
		e.config.Settings, attempt.Basis, request, candidate.RuntimeRevision,
	)
	if err != nil {
		return rejectedCompactionResultWithError(compactionRejectReason(err), err)
	}
	if measurement.InputTokens >= measurement.InputLimit {
		return rejectedCompactionResultWithError(
			event.CompactRejectSummaryTooLarge,
			&loop.SummaryTooLargeError{Measurement: measurement},
		)
	}
	template, err := contextFingerprintTemplateForRequest(
		request, candidate.RuntimeRevision, e.config.CounterCapability, e.config.InferenceCapability,
	)
	if err != nil {
		return rejectedCompactionResult(event.CompactRejectInternal)
	}
	return contextCompactionAwaitResult{
		Disposition: contextCompactionAwaitCommitted,
		Proposal: compactionFinalizationProposal{Success: &compactionPreparedSuccess{
			Model: candidate.Measurement.Model, RequestFingerprint: candidate.Measurement.RequestFingerprint,
			Summary: cloneUserMessage(value.Summary),
			PostCount: compactionPostCount{
				Model: measurement.Model, InputTokens: measurement.InputTokens, InputLimit: measurement.InputLimit,
				Quality: measurement.Quality, Fingerprint: template,
			},
		}},
	}
}

func rejectedCompactionResult(reason event.CompactRejectReason) contextCompactionAwaitResult {
	return rejectedCompactionResultWithError(reason, nil)
}

func rejectedCompactionResultWithError(reason event.CompactRejectReason, continuationError error) contextCompactionAwaitResult {
	return contextCompactionAwaitResult{
		Disposition:       contextCompactionAwaitRejected,
		Proposal:          compactionFinalizationProposal{RejectReason: reason},
		ContinuationError: continuationError,
	}
}

func compactionRejectReason(err error) event.CompactRejectReason {
	if err == nil {
		return event.CompactRejectInternal
	}
	var invalid *loop.InvalidSummaryError
	if errors.As(err, &invalid) {
		return event.CompactRejectInvalidSummary
	}
	var unknown *loop.ContextLimitUnknownError
	if errors.As(err, &unknown) {
		return event.CompactRejectContextLimitUnknown
	}
	var count *inference.ContextCountError
	if errors.As(err, &count) {
		return event.CompactRejectContextCountFailed
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return event.CompactRejectCanceled
	}
	return event.CompactRejectExecutionFailed
}
