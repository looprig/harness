package delegationtool

import (
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
)

const (
	validDelegateID = "55555555-5555-4555-8555-555555555555"
	validRequestID  = "66666666-6666-4666-8666-666666666666"
)

func TestPrepareEnvelopeActions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args string
		want agentAction
	}{
		{name: "omitted action starts", args: `{"description":"label","prompt":"do work","subagent_type":"worker"}`, want: actionStart},
		{name: "start", args: `{"action":"start","description":"label","prompt":"do work","subagent_type":"worker","run_in_background":false}`, want: actionStart},
		{name: "send", args: `{"action":"send","delegate_id":"` + validDelegateID + `","prompt":"do more","run_in_background":false}`, want: actionSend},
		{name: "wait", args: `{"action":"wait","delegate_id":"` + validDelegateID + `","request_id":"` + validRequestID + `","timeout_seconds":0}`, want: actionWait},
		{name: "interrupt", args: `{"action":"interrupt","delegate_id":"` + validDelegateID + `"}`, want: actionInterrupt},
		{name: "status", args: `{"action":"status"}`, want: actionStatus},
		{name: "status one delegate", args: `{"action":"status","delegate_id":"` + validDelegateID + `"}`, want: actionStatus},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := prepareEnvelope(tt.args)
			if err != nil {
				t.Fatalf("prepareEnvelope() error = %v", err)
			}
			if got.Action != tt.want {
				t.Fatalf("Action = %q, want %q", got.Action, tt.want)
			}
		})
	}
}

func TestPrepareEnvelopeDefaultsAndValues(t *testing.T) {
	t.Parallel()

	got, err := prepareEnvelope(`{"description":"label","prompt":"do work","subagent_type":"worker"}`)
	if err != nil {
		t.Fatalf("prepareEnvelope() error = %v", err)
	}
	if !got.RunInBackground {
		t.Fatal("omitted run_in_background = false, want managed default true")
	}
	if got.Effort != nil {
		t.Fatalf("omitted effort = %v, want nil", *got.Effort)
	}

	got, err = prepareEnvelope(`{"action":"start","description":"label","prompt":"do work","subagent_type":"worker","agent_harness":"codex","model":"luna","effort":"none","run_in_background":false,"timeout_seconds":86400}`)
	if err != nil {
		t.Fatalf("prepareEnvelope() explicit error = %v", err)
	}
	if got.RunInBackground {
		t.Fatal("explicit run_in_background = true, want false")
	}
	if got.Effort == nil || *got.Effort != "none" {
		t.Fatalf("effort = %v, want explicit none", got.Effort)
	}
	if got.TimeoutSeconds == nil || *got.TimeoutSeconds != maxTimeoutSeconds {
		t.Fatalf("timeout_seconds = %v, want %d", got.TimeoutSeconds, maxTimeoutSeconds)
	}
}

func TestPrepareEnvelopeActionBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args string
		cat  string
	}{
		{name: "unknown action", args: `{"action":"explode"}`, cat: errCategoryInvalidValue},
		{name: "start missing description", args: `{"action":"start","prompt":"p","subagent_type":"worker"}`, cat: errCategoryMissingField},
		{name: "start missing prompt", args: `{"action":"start","description":"d","subagent_type":"worker"}`, cat: errCategoryMissingField},
		{name: "start missing role", args: `{"action":"start","description":"d","prompt":"p"}`, cat: errCategoryMissingField},
		{name: "send missing delegate", args: `{"action":"send","prompt":"p","run_in_background":false}`, cat: errCategoryMissingField},
		{name: "send missing prompt", args: `{"action":"send","delegate_id":"` + validDelegateID + `","run_in_background":false}`, cat: errCategoryMissingField},
		{name: "wait missing request", args: `{"action":"wait","delegate_id":"` + validDelegateID + `"}`, cat: errCategoryMissingField},
		{name: "interrupt extra field", args: `{"action":"interrupt","delegate_id":"` + validDelegateID + `","prompt":"p"}`, cat: errCategoryFieldNotAllowed},
		{name: "status extra field", args: `{"action":"status","prompt":"p"}`, cat: errCategoryFieldNotAllowed},
		{name: "send relabel", args: `{"action":"send","delegate_id":"` + validDelegateID + `","prompt":"p","description":"d","run_in_background":false}`, cat: errCategoryFieldNotAllowed},
		{name: "background timeout", args: `{"action":"start","description":"d","prompt":"p","subagent_type":"worker","timeout_seconds":1}`, cat: errCategoryFieldNotAllowed},
		{name: "background send timeout", args: `{"action":"send","delegate_id":"` + validDelegateID + `","prompt":"p","timeout_seconds":1}`, cat: errCategoryFieldNotAllowed},
		{name: "negative timeout", args: `{"action":"start","description":"d","prompt":"p","subagent_type":"worker","run_in_background":false,"timeout_seconds":-1}`, cat: errCategoryInvalidValue},
		{name: "too long timeout", args: `{"action":"start","description":"d","prompt":"p","subagent_type":"worker","run_in_background":false,"timeout_seconds":86401}`, cat: errCategoryInvalidValue},
		{name: "zero delegate", args: `{"action":"send","delegate_id":"00000000-0000-0000-0000-000000000000","prompt":"p","run_in_background":false}`, cat: errCategoryInvalidValue},
		{name: "zero request", args: `{"action":"wait","delegate_id":"` + validDelegateID + `","request_id":"00000000-0000-0000-0000-000000000000"}`, cat: errCategoryInvalidValue},
		{name: "bad delegate", args: `{"action":"send","delegate_id":"not-a-uuid","prompt":"p","run_in_background":false}`, cat: errCategoryInvalidValue},
		{name: "xhigh effort", args: `{"action":"start","description":"d","prompt":"p","subagent_type":"worker","effort":"xhigh"}`, cat: errCategoryInvalidValue},
		{name: "ultra effort", args: `{"action":"start","description":"d","prompt":"p","subagent_type":"worker","effort":"ultra"}`, cat: errCategoryInvalidValue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := prepareEnvelope(tt.args)
			assertPrepareCategory(t, err, tt.cat)
		})
	}
}

func TestPrepareEnvelopeLimitsAndJSONDiscipline(t *testing.T) {
	t.Parallel()

	base := `{"action":"start","description":"d","prompt":"p","subagent_type":"worker","run_in_background":false}`
	tests := []struct {
		name string
		args string
		cat  string
	}{
		{name: "description boundary", args: `{"action":"start","description":"` + strings.Repeat("d", maxDescriptionBytes) + `","prompt":"p","subagent_type":"worker"}`},
		{name: "description over boundary", args: `{"action":"start","description":"` + strings.Repeat("d", maxDescriptionBytes+1) + `","prompt":"p","subagent_type":"worker"}`, cat: errCategoryInvalidValue},
		{name: "prompt boundary", args: `{"action":"start","description":"d","prompt":"` + strings.Repeat("p", maxPromptBytes) + `","subagent_type":"worker"}`},
		{name: "prompt over boundary", args: `{"action":"start","description":"d","prompt":"` + strings.Repeat("p", maxPromptBytes+1) + `","subagent_type":"worker"}`, cat: errCategoryInvalidValue},
		{name: "oversized before decode", args: strings.Repeat("{", maxSubagentArgsBytes+1), cat: errCategoryOversized},
		{name: "trailing object", args: base + `{}`, cat: errCategoryMalformed},
		{name: "trailing scalar", args: base + ` true`, cat: errCategoryMalformed},
		{name: "array", args: `[]`, cat: errCategoryMalformed},
		{name: "null", args: `null`, cat: errCategoryMalformed},
		{name: "unknown field", args: base[:len(base)-1] + `,"message":"old"}`, cat: errCategoryUnknownField},
		{name: "old agent alias", args: `{"action":"start","agent":"worker","message":"p"}`, cat: errCategoryUnknownField},
		{name: "old wait alias", args: `{"action":"start","description":"d","prompt":"p","subagent_type":"worker","wait":true}`, cat: errCategoryUnknownField},
		{name: "malformed", args: `{"action":"start"`, cat: errCategoryMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := prepareEnvelope(tt.args)
			if tt.cat == "" {
				if err != nil {
					t.Fatalf("prepareEnvelope() error = %v", err)
				}
				return
			}
			assertPrepareCategory(t, err, tt.cat)
		})
	}
}

func TestPrepareEnvelopeErrorsDoNotEchoInput(t *testing.T) {
	t.Parallel()

	secret := "do-not-echo-this-prompt"
	_, err := prepareEnvelope(`{"action":"start","description":"d","prompt":"` + secret + `","subagent_type":"worker","effort":"xhigh"}`)
	if err == nil {
		t.Fatal("prepareEnvelope() error = nil, want rejection")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "xhigh") {
		t.Fatalf("error = %q, contains user input", err)
	}
}

func assertPrepareCategory(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("prepareEnvelope() error = nil, want category %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want category %q", err, want)
	}
	if len(err.Error()) > 96 {
		t.Fatalf("error length = %d, want bounded", len(err.Error()))
	}
}

func TestPrepareEnvelopeUUIDValues(t *testing.T) {
	t.Parallel()

	got, err := prepareEnvelope(`{"action":"wait","delegate_id":"` + validDelegateID + `","request_id":"` + validRequestID + `"}`)
	if err != nil {
		t.Fatalf("prepareEnvelope() error = %v", err)
	}
	wantDelegate := uuid.MustParse(validDelegateID)
	wantRequest := uuid.MustParse(validRequestID)
	if got.DelegateID == nil || *got.DelegateID != wantDelegate {
		t.Fatalf("DelegateID = %v, want %v", got.DelegateID, wantDelegate)
	}
	if got.RequestID == nil || *got.RequestID != wantRequest {
		t.Fatalf("RequestID = %v, want %v", got.RequestID, wantRequest)
	}
}
