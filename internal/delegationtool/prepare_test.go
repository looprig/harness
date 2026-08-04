package delegationtool

import (
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
)

const validAgentID = "55555555-5555-4555-8555-555555555555"

func TestPrepareStartAgentDefaultsAndValues(t *testing.T) {
	t.Parallel()

	got, err := prepareStartAgent(`{"agent_type":"worker","instructions":"do work"}`)
	if err != nil {
		t.Fatalf("prepareStartAgent() error = %v", err)
	}
	if !got.WaitForResponse {
		t.Fatal("omitted wait_for_response = false, want true")
	}
	if got.TimeoutSeconds != nil {
		t.Fatalf("omitted timeout_seconds = %v, want nil", got.TimeoutSeconds)
	}

	got, err = prepareStartAgent(`{"agent_type":"worker","name":"builder","instructions":"do work","wait_for_response":false,"timeout_seconds":86400,"agent_harness":"codex","agent_source":"gateway","model":"luna","effort":"none","agent_mode":"build"}`)
	if err != nil {
		t.Fatalf("prepareStartAgent() explicit error = %v", err)
	}
	if got.AgentType != "worker" || got.Name != "builder" || got.Instructions != "do work" || got.WaitForResponse || got.TimeoutSeconds == nil || *got.TimeoutSeconds != maxTimeoutSeconds {
		t.Fatalf("prepared start = %+v", got)
	}
	if got.AgentHarness != "codex" || got.AgentSource != "gateway" || got.Model != "luna" || got.Effort == nil || *got.Effort != "none" || got.AgentMode != "build" {
		t.Fatalf("prepared selectors = %+v", got)
	}
}

func TestPrepareMessageAgentDefaultsAndValues(t *testing.T) {
	t.Parallel()

	got, err := prepareMessageAgent(`{"agent_id":"` + validAgentID + `","message":"continue"}`)
	if err != nil {
		t.Fatalf("prepareMessageAgent() error = %v", err)
	}
	if got.AgentID != uuid.MustParse(validAgentID) || got.Message != "continue" || !got.WaitForResponse || got.TimeoutSeconds != nil {
		t.Fatalf("prepared message = %+v", got)
	}

	got, err = prepareMessageAgent(`{"agent_id":"` + validAgentID + `","message":"continue","wait_for_response":false,"timeout_seconds":0}`)
	if err != nil {
		t.Fatalf("prepareMessageAgent() explicit error = %v", err)
	}
	if got.WaitForResponse || got.TimeoutSeconds == nil || *got.TimeoutSeconds != 0 {
		t.Fatalf("prepared message controls = %+v", got)
	}
}

func TestPrepareListAgentAndStopAgentIDs(t *testing.T) {
	t.Parallel()

	listed, err := prepareListAgents(`{}`)
	if err != nil || listed.AgentID != nil {
		t.Fatalf("prepareListAgents({}) = %+v, %v", listed, err)
	}
	listed, err = prepareListAgents(`{"agent_id":"` + validAgentID + `"}`)
	if err != nil || listed.AgentID == nil || *listed.AgentID != uuid.MustParse(validAgentID) {
		t.Fatalf("prepareListAgents(id) = %+v, %v", listed, err)
	}
	stopped, err := prepareStopAgent(`{"agent_id":"` + validAgentID + `"}`)
	if err != nil || stopped.AgentID != uuid.MustParse(validAgentID) {
		t.Fatalf("prepareStopAgent() = %+v, %v", stopped, err)
	}
}

func TestPrepareAgentToolsRejectUnknownAndCrossToolFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prepare func(string) error
		args    string
		cat     string
	}{
		{name: "start action", prepare: startPreparationError, args: `{"agent_type":"worker","instructions":"p","action":"start"}`, cat: errCategoryUnknownField},
		{name: "start legacy", prepare: startPreparationError, args: `{"subagent_type":"worker","prompt":"p"}`, cat: errCategoryUnknownField},
		{name: "start message field", prepare: startPreparationError, args: `{"agent_type":"worker","instructions":"p","message":"x"}`, cat: errCategoryUnknownField},
		{name: "message start field", prepare: messagePreparationError, args: `{"agent_id":"` + validAgentID + `","message":"p","agent_type":"worker"}`, cat: errCategoryUnknownField},
		{name: "message legacy id", prepare: messagePreparationError, args: `{"delegate_id":"` + validAgentID + `","message":"p"}`, cat: errCategoryUnknownField},
		{name: "list message", prepare: listPreparationError, args: `{"message":"p"}`, cat: errCategoryUnknownField},
		{name: "stop wait", prepare: stopPreparationError, args: `{"agent_id":"` + validAgentID + `","wait_for_response":true}`, cat: errCategoryUnknownField},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) { assertPrepareCategory(t, tt.prepare(tt.args), tt.cat) })
	}
}

func TestPrepareAgentRequiredFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prepare func(string) error
		args    string
		cat     string
	}{
		{name: "start missing type", prepare: startPreparationError, args: `{"instructions":"p"}`, cat: errCategoryMissingField},
		{name: "start missing instructions", prepare: startPreparationError, args: `{"agent_type":"worker"}`, cat: errCategoryMissingField},
		{name: "message missing id", prepare: messagePreparationError, args: `{"message":"p"}`, cat: errCategoryMissingField},
		{name: "message missing message", prepare: messagePreparationError, args: `{"agent_id":"` + validAgentID + `"}`, cat: errCategoryMissingField},
		{name: "stop missing id", prepare: stopPreparationError, args: `{}`, cat: errCategoryMissingField},
		{name: "zero id", prepare: stopPreparationError, args: `{"agent_id":"00000000-0000-0000-0000-000000000000"}`, cat: errCategoryInvalidValue},
		{name: "bad id", prepare: stopPreparationError, args: `{"agent_id":"not-a-uuid"}`, cat: errCategoryInvalidValue},
		{name: "overlong id", prepare: stopPreparationError, args: `{"agent_id":"` + strings.Repeat("1", 37) + `"}`, cat: errCategoryInvalidValue},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) { assertPrepareCategory(t, tt.prepare(tt.args), tt.cat) })
	}
}

func TestPrepareAgentLimitsUTF8AndJSONDiscipline(t *testing.T) {
	t.Parallel()
	invalidUTF8 := string([]byte{0xff})
	tests := []struct {
		name    string
		prepare func(string) error
		args    string
		cat     string
	}{
		{name: "name boundary", prepare: startPreparationError, args: `{"agent_type":"worker","name":"` + strings.Repeat("n", maxAgentNameBytes) + `","instructions":"p"}`},
		{name: "name over boundary", prepare: startPreparationError, args: `{"agent_type":"worker","name":"` + strings.Repeat("n", maxAgentNameBytes+1) + `","instructions":"p"}`, cat: errCategoryInvalidValue},
		{name: "instructions boundary", prepare: startPreparationError, args: `{"agent_type":"worker","instructions":"` + strings.Repeat("p", maxAgentMessageBytes) + `"}`},
		{name: "instructions over boundary", prepare: startPreparationError, args: `{"agent_type":"worker","instructions":"` + strings.Repeat("p", maxAgentMessageBytes+1) + `"}`, cat: errCategoryInvalidValue},
		{name: "message over boundary", prepare: messagePreparationError, args: `{"agent_id":"` + validAgentID + `","message":"` + strings.Repeat("p", maxAgentMessageBytes+1) + `"}`, cat: errCategoryInvalidValue},
		{name: "invalid UTF-8", prepare: startPreparationError, args: `{"agent_type":"worker","instructions":"` + invalidUTF8 + `"}`, cat: errCategoryMalformed},
		{name: "invalid UTF-8 id", prepare: stopPreparationError, args: `{"agent_id":"` + invalidUTF8 + `"}`, cat: errCategoryMalformed},
		{name: "negative timeout", prepare: startPreparationError, args: `{"agent_type":"worker","instructions":"p","timeout_seconds":-1}`, cat: errCategoryInvalidValue},
		{name: "timeout over boundary", prepare: messagePreparationError, args: `{"agent_id":"` + validAgentID + `","message":"p","timeout_seconds":86401}`, cat: errCategoryInvalidValue},
		{name: "oversized", prepare: startPreparationError, args: strings.Repeat("{", maxAgentArgsBytes+1), cat: errCategoryOversized},
		{name: "trailing", prepare: stopPreparationError, args: `{"agent_id":"` + validAgentID + `"}{}`, cat: errCategoryMalformed},
		{name: "array", prepare: listPreparationError, args: `[]`, cat: errCategoryMalformed},
		{name: "null", prepare: listPreparationError, args: `null`, cat: errCategoryMalformed},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			err := tt.prepare(tt.args)
			if tt.cat == "" {
				if err != nil {
					t.Fatalf("prepare error = %v", err)
				}
				return
			}
			assertPrepareCategory(t, err, tt.cat)
		})
	}
}

func TestPrepareAgentErrorsDoNotEchoInput(t *testing.T) {
	secret := "do-not-echo-this-message"
	err := startPreparationError(`{"agent_type":"worker","instructions":"` + secret + `","effort":"xhigh"}`)
	if err == nil {
		t.Fatal("prepare error = nil, want rejection")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "xhigh") {
		t.Fatalf("error = %q, contains user input", err)
	}
}

func startPreparationError(args string) error   { _, err := prepareStartAgent(args); return err }
func messagePreparationError(args string) error { _, err := prepareMessageAgent(args); return err }
func listPreparationError(args string) error    { _, err := prepareListAgents(args); return err }
func stopPreparationError(args string) error    { _, err := prepareStopAgent(args); return err }

func assertPrepareCategory(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("prepare error = nil, want category %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want category %q", err, want)
	}
	if len(err.Error()) > 96 {
		t.Fatalf("error length = %d, want bounded", len(err.Error()))
	}
}
