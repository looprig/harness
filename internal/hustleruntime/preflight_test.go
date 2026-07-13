package hustleruntime

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
)

func validRuntimeConfig(t *testing.T, definition hustle.BoundDefinition) RuntimeConfig {
	t.Helper()
	return RuntimeConfig{
		SessionID:           mustRuntimeTestID(t),
		Definitions:         []hustle.BoundDefinition{definition},
		AuditTimeout:        time.Second,
		FinalizationTimeout: time.Second,
		WorkerDrainTimeout:  time.Second,
		Stamper:             event.NewFactory(uuid.New, time.Now),
		Audit:               &runtimeTestAudit{},
		Faults:              &runtimeTestFaults{},
		Activity:            &runtimeTestActivity{},
	}
}

func TestNewValidatesRuntimeConfig(t *testing.T) {
	t.Parallel()
	client := successfulRuntimeClient(nil)
	definition := runtimeTestBoundDefinition(t, "test.config", hustle.ParticipationBlocking, client, hustle.ModelSourceNamed, nil)
	valid := validRuntimeConfig(t, definition)
	tests := []struct {
		name       string
		mutate     func(*RuntimeConfig)
		wantReason ConfigErrorReason
		wantField  string
	}{
		{name: "valid", mutate: func(*RuntimeConfig) {}},
		{name: "zero session id", mutate: func(config *RuntimeConfig) { config.SessionID = uuid.UUID{} }, wantReason: ConfigInvalidSessionID, wantField: "runtime.session_id"},
		{name: "no definitions", mutate: func(config *RuntimeConfig) { config.Definitions = nil }, wantReason: ConfigInvalidDefinitions, wantField: "runtime.definitions"},
		{name: "nil definition", mutate: func(config *RuntimeConfig) { config.Definitions = []hustle.BoundDefinition{nil} }, wantReason: ConfigInvalidDefinitions, wantField: "runtime.definitions"},
		{name: "duplicate definition", mutate: func(config *RuntimeConfig) { config.Definitions = append(config.Definitions, definition) }, wantReason: ConfigInvalidDefinitions, wantField: "runtime.definitions"},
		{name: "zero audit timeout", mutate: func(config *RuntimeConfig) { config.AuditTimeout = 0 }, wantReason: ConfigInvalidTimeout, wantField: "runtime.audit_timeout"},
		{name: "zero finalization timeout", mutate: func(config *RuntimeConfig) { config.FinalizationTimeout = 0 }, wantReason: ConfigInvalidTimeout, wantField: "runtime.finalization_timeout"},
		{name: "zero worker drain timeout", mutate: func(config *RuntimeConfig) { config.WorkerDrainTimeout = 0 }, wantReason: ConfigInvalidTimeout, wantField: "runtime.worker_drain_timeout"},
		{name: "nil stamper", mutate: func(config *RuntimeConfig) { config.Stamper = nil }, wantReason: ConfigMissingCollaborator, wantField: "runtime.stamper"},
		{name: "nil audit", mutate: func(config *RuntimeConfig) { config.Audit = nil }, wantReason: ConfigMissingCollaborator, wantField: "runtime.audit"},
		{name: "typed nil audit", mutate: func(config *RuntimeConfig) { config.Audit = (*runtimeTestAudit)(nil) }, wantReason: ConfigMissingCollaborator, wantField: "runtime.audit"},
		{name: "nil faults", mutate: func(config *RuntimeConfig) { config.Faults = nil }, wantReason: ConfigMissingCollaborator, wantField: "runtime.faults"},
		{name: "typed nil faults", mutate: func(config *RuntimeConfig) { config.Faults = (*runtimeTestFaults)(nil) }, wantReason: ConfigMissingCollaborator, wantField: "runtime.faults"},
		{name: "nil activity", mutate: func(config *RuntimeConfig) { config.Activity = nil }, wantReason: ConfigMissingCollaborator, wantField: "runtime.activity"},
		{name: "typed nil activity", mutate: func(config *RuntimeConfig) { config.Activity = (*runtimeTestActivity)(nil) }, wantReason: ConfigMissingCollaborator, wantField: "runtime.activity"},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			runtimeConfig := valid
			runtimeConfig.Definitions = append([]hustle.BoundDefinition(nil), valid.Definitions...)
			testCase.mutate(&runtimeConfig)
			controller, err := New(context.Background(), Config{
				Blocking: LaneLimits{Concurrent: 1}, Background: LaneLimits{Concurrent: 1}, Runtime: &runtimeConfig,
			})
			if testCase.wantReason == "" {
				if err != nil || controller == nil {
					t.Fatalf("New() = (%v,%v), want controller,nil", controller, err)
				}
				return
			}
			var configErr *ConfigError
			if controller != nil || !errors.As(err, &configErr) || configErr.Reason != testCase.wantReason || configErr.Field != testCase.wantField {
				t.Fatalf("New() = (%v,%T %v), want ConfigError reason=%q field=%q", controller, err, err, testCase.wantReason, testCase.wantField)
			}
		})
	}
}

func TestRunAndFinalizeRejectsInvalidRequestBeforeOwnership(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		ctx           func() context.Context
		mutate        func(*hustle.Request)
		nilRuntime    bool
		nilValidate   bool
		nilFinalizer  bool
		wantReason    RequestErrorReason
		wantAdmission AdmissionErrorReason
	}{
		{name: "nil context", wantReason: RequestInvalidContext},
		{name: "runtime unavailable", ctx: context.Background, nilRuntime: true, wantReason: RequestRuntimeUnavailable},
		{name: "unknown definition", ctx: context.Background, mutate: func(request *hustle.Request) { request.Name = "missing" }, wantReason: RequestUnknownDefinition},
		{name: "missing cause", ctx: context.Background, mutate: func(request *hustle.Request) { request.Cause = identity.Cause{} }, wantReason: RequestInvalidCause},
		{name: "empty input", ctx: context.Background, mutate: func(request *hustle.Request) { request.Input = nil }, wantReason: RequestInvalidInput},
		{name: "malformed input", ctx: context.Background, mutate: func(request *hustle.Request) { request.Input = []byte(`{"broken"`) }, wantReason: RequestInvalidInput},
		{name: "multiple input values", ctx: context.Background, mutate: func(request *hustle.Request) { request.Input = []byte(`{} {}`) }, wantReason: RequestInvalidInput},
		{name: "oversized input", ctx: context.Background, mutate: func(request *hustle.Request) { request.Input = []byte(`"` + strings.Repeat("x", 1024) + `"`) }, wantReason: RequestInputTooLarge},
		{name: "oversized malformed input is bounded before parsing", ctx: context.Background, mutate: func(request *hustle.Request) { request.Input = []byte(strings.Repeat("{", 1025)) }, wantReason: RequestInputTooLarge},
		{name: "nil validator", ctx: context.Background, nilValidate: true, wantReason: RequestNilValidator},
		{name: "nil finalizer", ctx: context.Background, nilFinalizer: true, wantAdmission: AdmissionNilFinalizer},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			client := successfulRuntimeClient(nil)
			definition := runtimeTestBoundDefinition(t, "test.preflight", hustle.ParticipationBlocking, client, hustle.ModelSourceNamed, nil)
			audit := &runtimeTestAudit{}
			activity := &runtimeTestActivity{}
			controller := runtimeTestController(t, definition, audit, &runtimeTestFaults{}, activity)
			if testCase.nilRuntime {
				var err error
				controller, err = New(context.Background(), testConfig())
				if err != nil {
					t.Fatal(err)
				}
			}
			request := runtimeRequest(t, "test.preflight")
			if testCase.mutate != nil {
				testCase.mutate(&request)
			}
			validate := ValidateResult(func(context.Context, hustle.Result) error { return nil })
			if testCase.nilValidate {
				validate = nil
			}
			var finalizers atomic.Int32
			finalizer := Finalizer(func(context.Context, hustle.Outcome) error { finalizers.Add(1); return nil })
			if testCase.nilFinalizer {
				finalizer = nil
			}
			var ctx context.Context
			if testCase.ctx != nil {
				ctx = testCase.ctx()
			}
			err := controller.RunAndFinalize(ctx, request, validate, finalizer)
			if testCase.wantAdmission != "" {
				var admissionErr *AdmissionError
				if !errors.As(err, &admissionErr) || admissionErr.Reason != testCase.wantAdmission {
					t.Fatalf("error = %T %v, want AdmissionError reason %q", err, err, testCase.wantAdmission)
				}
			} else {
				var requestErr *RequestError
				if !errors.As(err, &requestErr) || requestErr.Reason != testCase.wantReason {
					t.Fatalf("error = %T %v, want RequestError reason %q", err, err, testCase.wantReason)
				}
			}
			if finalizers.Load() != 0 || client.invocations.Load() != 0 || activity.acquires.Load() != 0 || len(audit.snapshot()) != 0 {
				t.Fatalf("preownership side effects = finalizers:%d invokes:%d activity:%d audit:%d", finalizers.Load(), client.invocations.Load(), activity.acquires.Load(), len(audit.snapshot()))
			}
		})
	}
}
