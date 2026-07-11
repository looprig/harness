package rig

import (
	"context"
	"errors"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/session"
)

func (r *Rig) NewSession(ctx context.Context) (session.SessionController, error) {
	runtime, err := r.lifecycle.NewSession(ctx)
	if err != nil {
		return nil, mapRunError(err)
	}
	return runtime, nil
}

func (r *Rig) RestoreSession(ctx context.Context, id uuid.UUID) (session.SessionController, error) {
	runtime, err := r.lifecycle.RestoreSession(ctx, id)
	if err != nil {
		return nil, err
	}
	return runtime, nil
}

func mapRunError(err error) error {
	var run *sessionruntime.NewSessionError
	if !errors.As(err, &run) {
		return &LifecycleError{Kind: LifecycleSessionFailed, Cause: err}
	}
	kinds := map[sessionruntime.NewSessionErrorKind]LifecycleErrorKind{
		sessionruntime.NewSessionContextDone:        LifecycleContextDone,
		sessionruntime.NewSessionIDGenerationFailed: LifecycleIDGenerationFailed,
		sessionruntime.NewSessionLeaseFailed:        LifecycleLeaseFailed,
		sessionruntime.NewSessionJournalFailed:      LifecycleJournalFailed,
		sessionruntime.NewSessionAppenderFailed:     LifecycleAppenderFailed,
		sessionruntime.NewSessionRuntimeFailed:      LifecycleSessionFailed,
	}
	return &LifecycleError{Kind: kinds[run.Kind], Cause: run.Cause}
}
