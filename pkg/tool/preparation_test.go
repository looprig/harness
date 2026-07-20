package tool

import "testing"

func validCommandRequest() Request {
	return Request{
		ToolName:           "Bash",
		Summary:            "run git status",
		ExecutionID:        "exec-1",
		Command:            "git status",
		WorkingDirectory:   "/workspace",
		ExpiresAtUnixMilli: 1_800_000_000_000,
		Requirements: []Requirement{{
			Kind:        CapabilityCommandExecute,
			Scope:       "",
			Match:       "git status",
			Description: "run command: git status",
			GrantClass:  GrantClassCommandStart,
			GrantTarget: "git status",
			Candidates: []RuleCandidate{{
				Kind:        CapabilityCommandExecute,
				Match:       "Bash(git status)",
				Description: "Bash(git status)",
				GrantClass:  GrantClassCommandStart,
				GrantTarget: "git status",
			}},
		}},
	}
}

func TestPreparationContractIdentifiers(t *testing.T) {
	if CapabilityCommandExecute != "command.execute" {
		t.Fatalf("CapabilityCommandExecute = %q, want command.execute", CapabilityCommandExecute)
	}
	if GrantClassCommandStart != "command.start.v1" {
		t.Fatalf("GrantClassCommandStart = %q, want command.start.v1", GrantClassCommandStart)
	}
}

func TestValidateRequestAcceptsNormalizedCommandRequirement(t *testing.T) {
	if err := ValidateRequest(validCommandRequest()); err != nil {
		t.Fatalf("ValidateRequest() error = %v", err)
	}
}

func TestValidateRequestAcceptsEmptyPureToolRequest(t *testing.T) {
	// Pure tools may prepare an empty request: no requirements, no binding.
	if err := ValidateRequest(Request{ToolName: "Echo"}); err != nil {
		t.Fatalf("ValidateRequest(empty) error = %v", err)
	}
	if err := ValidateRequest(Request{}); err != nil {
		t.Fatalf("ValidateRequest(zero) error = %v", err)
	}
}

func TestValidateRequestRejectsMalformedGrantPairAndCommandInvariant(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{name: "grant class without target", mutate: func(r *Request) { r.Requirements[0].GrantTarget = "" }},
		{name: "grant target without class", mutate: func(r *Request) { r.Requirements[0].GrantClass = "" }},
		{name: "wrong command grant class", mutate: func(r *Request) { r.Requirements[0].GrantClass = "command.other.v1" }},
		{name: "wrong command grant target", mutate: func(r *Request) { r.Requirements[0].GrantTarget = "git diff" }},
		{name: "command access scope", mutate: func(r *Request) { r.Requirements[0].Scope = "/workspace" }},
		{name: "command request field mismatch", mutate: func(r *Request) { r.Command = "git diff" }},
		{name: "command requirement without grant pair", mutate: func(r *Request) {
			r.Requirements[0].GrantClass = ""
			r.Requirements[0].GrantTarget = ""
			r.Requirements[0].Candidates = nil
		}},
		{name: "candidate grant pair mismatch", mutate: func(r *Request) { r.Requirements[0].Candidates[0].GrantTarget = "git diff" }},
		{name: "candidate kind mismatch", mutate: func(r *Request) { r.Requirements[0].Candidates[0].Kind = "network" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := validCommandRequest()
			tt.mutate(&request)
			if err := ValidateRequest(request); err == nil {
				t.Fatal("ValidateRequest() error = nil, want rejection")
			}
		})
	}
}

func TestValidateRequestRejectsNonNormalizedAndDuplicateMatchingFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{name: "kind whitespace", mutate: func(r *Request) { r.Requirements[0].Kind = " command.execute" }},
		{name: "match whitespace", mutate: func(r *Request) { r.Requirements[0].Match = "git status " }},
		{name: "missing match", mutate: func(r *Request) { r.Requirements[0].Match = "" }},
		{name: "missing description", mutate: func(r *Request) { r.Requirements[0].Description = "" }},
		{name: "candidate match whitespace", mutate: func(r *Request) { r.Requirements[0].Candidates[0].Match = " Bash(git status)" }},
		{name: "control character", mutate: func(r *Request) { r.Requirements[0].Match = "git\x00status" }},
		{name: "invalid utf8", mutate: func(r *Request) { r.Requirements[0].Match = "git\xff" }},
		{name: "tool name whitespace", mutate: func(r *Request) { r.ToolName = " Bash" }},
		{name: "duplicate requirement", mutate: func(r *Request) { r.Requirements = append(r.Requirements, r.Requirements[0]) }},
		{name: "duplicate candidate", mutate: func(r *Request) {
			r.Requirements[0].Candidates = append(r.Requirements[0].Candidates, r.Requirements[0].Candidates[0])
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := validCommandRequest()
			tt.mutate(&request)
			if err := ValidateRequest(request); err == nil {
				t.Fatal("ValidateRequest() error = nil, want rejection")
			}
		})
	}
}

func TestValidateRequestRequiresExecutionBindingWhenAnyGrantIsRequested(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{name: "missing execution id", mutate: func(r *Request) { r.ExecutionID = "" }},
		{name: "missing command", mutate: func(r *Request) { r.Command = "" }},
		{name: "missing working directory", mutate: func(r *Request) { r.WorkingDirectory = "" }},
		{name: "missing expiry", mutate: func(r *Request) { r.ExpiresAtUnixMilli = 0 }},
		{name: "negative expiry", mutate: func(r *Request) { r.ExpiresAtUnixMilli = -1 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := validCommandRequest()
			tt.mutate(&request)
			// The command.execute invariant also depends on Command; use a
			// direct-tool style requirement so only the binding rule is exercised.
			if tt.name != "missing command" {
				if err := ValidateRequest(request); err == nil {
					t.Fatal("ValidateRequest() error = nil, want binding rejection")
				}
			}
			networkOnly := validNetworkOnlyRequest()
			tt.mutate(&networkOnly)
			if err := ValidateRequest(networkOnly); err == nil {
				t.Fatal("ValidateRequest(network-only) error = nil, want binding rejection")
			}
		})
	}
}

func validNetworkOnlyRequest() Request {
	return Request{
		ToolName:           "Bash",
		Summary:            "run git push",
		ExecutionID:        "exec-2",
		Command:            "git push",
		WorkingDirectory:   "/workspace",
		ExpiresAtUnixMilli: 1_800_000_000_000,
		Requirements: []Requirement{{
			Kind:        "network",
			Match:       "tcp:github.com:443",
			Description: "connect to github.com:443",
			GrantClass:  "network.proxy-target.v1",
			GrantTarget: "tcp:github.com:443",
		}},
	}
}

func TestRequestCloneIsIndependent(t *testing.T) {
	original := validCommandRequest()
	clone := original.Clone()
	clone.Requirements[0].Match = "mutated"
	clone.Requirements[0].Candidates[0].Match = "mutated"
	if original.Requirements[0].Match != "git status" {
		t.Fatal("Clone() shares requirement backing storage")
	}
	if original.Requirements[0].Candidates[0].Match != "Bash(git status)" {
		t.Fatal("Clone() shares candidate backing storage")
	}
}
