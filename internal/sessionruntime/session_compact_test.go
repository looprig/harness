package sessionruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	contextcount "github.com/looprig/inference/contextcount"
)

func compactDispatchDefinition(engine loop.Engine) loop.Definition {
	definition := engineCfg(&stubLLM{}, engine, "system")
	if engine == loop.EngineNative {
		capability := contextcount.CounterCapability{
			Transport: contextcount.CounterTransportLocal, Retention: contextcount.RetentionNone,
			TokenizerRev: "dispatch-test-v1", Quality: contextcount.CountQualityExactLocal,
		}
		model := validModel("compact-dispatch")
		model.Limits = testContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
		definition = mustDefine(
			loop.WithName("agent"), loop.WithInference(&stubLLM{}, model), loop.WithSystem("system"),
			loop.WithContextCounter(&liveCompactionCounter{capability: capability, counts: []content.TokenCount{20}}),
			loop.WithInferenceCapability(contextcount.InferenceCapability{Transport: contextcount.InferenceTransportLocal, Retention: contextcount.RetentionNone}),
			loop.WithCompaction(loop.CompactionPolicy{
				CounterPolicy: loop.CounterPolicyRequireExact, ReservedOutput: 20,
				MaxSummaryTokens: 10, CountTimeout: time.Second, Hustle: "context.compact",
			}),
		)
	}
	return definition
}

func compactDispatchSession(t *testing.T, engine loop.Engine) (*Session, uuid.UUID, *channelBackend) {
	t.Helper()
	sessionID := uuid.UUID{0xe1}
	loopID := uuid.UUID{0xe2}
	backend := &channelBackend{Commands: make(chan command.Command, 1), Done: make(chan struct{})}
	definition := compactDispatchDefinition(engine)
	bound := bindCfg(definition, sessionID, loopID)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Session{
		sessionID: sessionID, sessionCtx: ctx, sessionCancel: cancel,
		loops: map[uuid.UUID]*loopHandle{
			loopID: {id: loopID, bound: bound, backend: backend},
		},
		activeLoopID: loopID,
		newID:        func() (uuid.UUID, error) { return uuid.UUID{0xe3}, nil },
		now:          func() time.Time { return time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC) },
		cmdAppender:  nopCommandAppender{},
	}, loopID, backend
}

func TestSessionDispatchesPublicCompactCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		invoke   func(context.Context, *Session, uuid.UUID) (uuid.UUID, error)
		mutate   func(*Session, uuid.UUID, *channelBackend) uuid.UUID
		wantKind SessionErrorKind
		wantSend bool
	}{
		{
			name: "Compact targets active native loop",
			invoke: func(ctx context.Context, session *Session, _ uuid.UUID) (uuid.UUID, error) {
				return session.Compact(ctx)
			},
			wantSend: true,
		},
		{
			name: "CompactToLoop targets exact native loop without active redirect",
			invoke: func(ctx context.Context, session *Session, target uuid.UUID) (uuid.UUID, error) {
				return session.CompactToLoop(ctx, target)
			},
			mutate: func(session *Session, current uuid.UUID, _ *channelBackend) uuid.UUID {
				target := uuid.UUID{0xe4}
				backend := &channelBackend{Commands: make(chan command.Command, 1), Done: make(chan struct{})}
				session.loops[target] = &loopHandle{id: target, bound: bindCfg(compactDispatchDefinition(loop.EngineNative), session.sessionID, target), backend: backend}
				session.activeLoopID = current
				return target
			},
			wantSend: true,
		},
		{
			name: "unknown exact target is rejected",
			invoke: func(ctx context.Context, session *Session, target uuid.UUID) (uuid.UUID, error) {
				return session.CompactToLoop(ctx, target)
			},
			mutate:   func(_ *Session, _ uuid.UUID, _ *channelBackend) uuid.UUID { return uuid.UUID{0xff} },
			wantKind: SessionLoopNotFound,
		},
		{
			name: "exited exact target is rejected",
			invoke: func(ctx context.Context, session *Session, target uuid.UUID) (uuid.UUID, error) {
				return session.CompactToLoop(ctx, target)
			},
			mutate: func(_ *Session, current uuid.UUID, backend *channelBackend) uuid.UUID {
				close(backend.Done)
				return current
			},
			wantKind: SessionLoopExited,
		},
		{
			name: "native loop without compaction policy is rejected",
			invoke: func(ctx context.Context, session *Session, target uuid.UUID) (uuid.UUID, error) {
				return session.CompactToLoop(ctx, target)
			},
			mutate: func(session *Session, current uuid.UUID, _ *channelBackend) uuid.UUID {
				session.loops[current].bound = bindCfg(cfg(&stubLLM{}), session.sessionID, current)
				return current
			},
			wantKind: SessionCompactionUnsupported,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			session, loopID, originalBackend := compactDispatchSession(t, loop.EngineNative)
			target := loopID
			if tt.mutate != nil {
				target = tt.mutate(session, loopID, originalBackend)
			}
			id, err := tt.invoke(context.Background(), session, target)
			var sessionErr *SessionError
			if errors.As(err, &sessionErr) != (tt.wantKind != "") {
				t.Fatalf("error = %T %v, want kind %q", err, err, tt.wantKind)
			}
			if sessionErr != nil && sessionErr.Kind != tt.wantKind {
				t.Fatalf("error kind = %q, want %q", sessionErr.Kind, tt.wantKind)
			}
			if !tt.wantSend {
				if !id.IsZero() {
					t.Fatalf("rejected command id = %v, want zero", id)
				}
				return
			}
			if id != (uuid.UUID{0xe3}) {
				t.Fatalf("command id = %v, want trusted minted id", id)
			}
			targetBackend := session.loops[target].backend.(*channelBackend)
			select {
			case received := <-targetBackend.Commands:
				compact, ok := received.(command.Compact)
				if !ok {
					t.Fatalf("command = %T, want command.Compact", received)
				}
				if compact.CommandID != id || compact.Agency != identity.AgencyUser || compact.SessionID != session.sessionID || compact.LoopID != target || compact.CreatedAt != session.now() {
					t.Fatalf("compact = %+v, want trusted user command for session/loop", compact)
				}
			default:
				t.Fatal("target loop received no compact command")
			}
			if target != session.activeLoopID {
				select {
				case got := <-originalBackend.Commands:
					t.Fatalf("active loop received redirected command %T", got)
				default:
				}
			}
		})
	}
}

func TestSessionRejectsForeignCompactTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		engine loop.Engine
	}{
		{name: "foreign conversational backend is not redirected to native compaction", engine: loop.EngineForeignClaude},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			session, loopID, backend := compactDispatchSession(t, tt.engine)
			id, err := session.CompactToLoop(context.Background(), loopID)
			var sessionErr *SessionError
			if !errors.As(err, &sessionErr) || sessionErr.Kind != SessionCompactionUnsupported {
				t.Fatalf("error = %T %v, want *SessionError{%q}", err, err, SessionCompactionUnsupported)
			}
			if !id.IsZero() {
				t.Fatalf("foreign command id = %v, want zero", id)
			}
			select {
			case got := <-backend.Commands:
				t.Fatalf("foreign backend received %T", got)
			default:
			}
		})
	}
}
