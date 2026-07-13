package hustleruntime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/inference"
)

type runtimeSelectiveAudit struct {
	mu       sync.Mutex
	events   []event.Event
	failType string
	cause    error
}

func (a *runtimeSelectiveAudit) PublishInternalEventChecked(_ context.Context, ev event.Event) error {
	typeName := eventTypeName(ev)
	if typeName == a.failType {
		return a.cause
	}
	a.mu.Lock()
	a.events = append(a.events, ev)
	a.mu.Unlock()
	return nil
}

func (a *runtimeSelectiveAudit) snapshot() []event.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]event.Event(nil), a.events...)
}

func TestAuditFailuresFaultAndReachFinalizerAsTypedRunErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		failType         string
		response         *inference.Response
		wantStage        hustle.Stage
		wantReason       hustle.ReasonCode
		wantPersisted    int
		wantPrimaryStage hustle.Stage
	}{
		{name: "started append failure stops inference", failType: "started", response: runtimeResponse(`{"ok":true}`, nil), wantStage: hustle.StageQueue, wantReason: hustle.ReasonInternal},
		{name: "completed append failure becomes terminal failure", failType: "completed", response: runtimeResponse(`{"ok":true}`, nil), wantStage: hustle.StageTerminal, wantReason: hustle.ReasonTerminal, wantPersisted: 1},
		{name: "failed append failure retains output primary", failType: "failed", response: runtimeResponse(`{"broken"`, nil), wantStage: hustle.StageTerminal, wantReason: hustle.ReasonTerminal, wantPersisted: 1, wantPrimaryStage: hustle.StageOutput},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			auditCause := &runtimeFailureCause{label: "audit unavailable"}
			audit := &runtimeSelectiveAudit{failType: testCase.failType, cause: auditCause}
			faults := &runtimeTestFaults{}
			client := &runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
				return testCase.response, nil
			}}
			definition := runtimeTestBoundDefinition(t, "test.audit", hustle.ParticipationBlocking, client, hustle.ModelSourceNamed, nil)
			controller := runtimeTestControllerWithAudit(t, definition, audit, faults, &runtimeTestActivity{})
			var finalOutcome hustle.Outcome
			err := controller.RunAndFinalize(context.Background(), runtimeRequest(t, "test.audit"), func(context.Context, hustle.Result) error { return nil }, func(_ context.Context, outcome hustle.Outcome) error {
				finalOutcome = outcome
				return nil
			})
			var runErr *RunError
			if !errors.As(err, &runErr) {
				t.Fatalf("error = %T %v, want RunError", err, err)
			}
			if testCase.wantPrimaryStage == hustle.StageUnknown {
				if runErr.Stage != testCase.wantStage || runErr.ReasonCode != testCase.wantReason {
					t.Fatalf("run error = %#v, want stage=%v reason=%v", runErr, testCase.wantStage, testCase.wantReason)
				}
			} else {
				if runErr.Stage != testCase.wantPrimaryStage || runErr.TerminalErr == nil {
					t.Fatalf("primary run error = %#v, want stage=%v with terminal error", runErr, testCase.wantPrimaryStage)
				}
				var terminalErr *AuditError
				if !errors.As(runErr.TerminalErr, &terminalErr) || terminalErr.Stage != testCase.wantStage || terminalErr.ReasonCode != testCase.wantReason {
					t.Fatalf("terminal error = %#v, want typed stage=%v reason=%v", runErr.TerminalErr, testCase.wantStage, testCase.wantReason)
				}
			}
			if finalOutcome.Err != runErr || finalOutcome.Result != nil || !errors.Is(err, auditCause) {
				t.Fatalf("final outcome = %#v error=%v, want same run error with audit cause", finalOutcome, err)
			}
			if len(audit.snapshot()) != testCase.wantPersisted {
				t.Fatalf("persisted events = %#v, want %d", audit.snapshot(), testCase.wantPersisted)
			}
			if testCase.failType == "started" && client.invocations.Load() != 0 {
				t.Fatalf("invoke calls after Started failure = %d, want 0", client.invocations.Load())
			}
			faults.mu.Lock()
			gotFaults := append([]error(nil), faults.faults...)
			faults.mu.Unlock()
			var auditErr *AuditError
			if len(gotFaults) != 1 || !errors.As(gotFaults[0], &auditErr) {
				t.Fatalf("faults = %#v, want one AuditError", gotFaults)
			}
		})
	}
}
