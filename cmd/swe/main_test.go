package main

import (
	"testing"

	"github.com/ciram-co/looprig/pkg/uuid"
)

// TestParseFlags covers the SWE CLI flag parser: --list, --resume <uuid>, and the boundary
// validation (an invalid/empty resume id fails at the boundary, not deep in the wiring;
// --list and --resume are mutually exclusive). The swarm has no positional agent name (it
// is a single swarm), so an unexpected positional arg is rejected.
func TestParseFlags(t *testing.T) {
	t.Parallel()

	validID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	tests := []struct {
		name              string
		args              []string
		wantList          bool
		wantResume        uuid.UUID
		wantRuntimeSkills bool
		wantGreeting      bool
		wantErr           bool
	}{
		{name: "no flags → new session", args: nil},
		{name: "list flag", args: []string{"-list"}, wantList: true},
		{name: "list flag double dash", args: []string{"--list"}, wantList: true},
		{name: "resume a session", args: []string{"-resume", validID.String()}, wantResume: validID},
		{name: "resume double dash", args: []string{"--resume", validID.String()}, wantResume: validID},
		{name: "runtime-skills off by default", args: nil, wantRuntimeSkills: false},
		{name: "runtime-skills flag", args: []string{"-runtime-skills"}, wantRuntimeSkills: true},
		{name: "runtime-skills flag double dash", args: []string{"--runtime-skills"}, wantRuntimeSkills: true},
		{name: "runtime-skills with resume", args: []string{"-runtime-skills", "-resume", validID.String()}, wantResume: validID, wantRuntimeSkills: true},
		{name: "greeting off by default", args: nil, wantGreeting: false},
		{name: "greeting flag", args: []string{"-greeting"}, wantGreeting: true},
		{name: "greeting flag double dash", args: []string{"--greeting"}, wantGreeting: true},
		{name: "greeting with resume", args: []string{"-greeting", "-resume", validID.String()}, wantResume: validID, wantGreeting: true},
		{name: "invalid resume id rejected", args: []string{"-resume", "not-a-uuid"}, wantErr: true},
		{name: "empty resume id rejected", args: []string{"-resume", ""}, wantErr: true},
		{name: "list and resume are mutually exclusive", args: []string{"-list", "-resume", validID.String()}, wantErr: true},
		{name: "unknown flag rejected", args: []string{"-nope"}, wantErr: true},
		{name: "unexpected positional rejected", args: []string{"extra"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseFlags(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseFlags(%v) err = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.list != tt.wantList {
				t.Errorf("list = %v, want %v", got.list, tt.wantList)
			}
			if got.resume != tt.wantResume {
				t.Errorf("resume = %v, want %v", got.resume, tt.wantResume)
			}
			if got.runtimeSkills != tt.wantRuntimeSkills {
				t.Errorf("runtimeSkills = %v, want %v", got.runtimeSkills, tt.wantRuntimeSkills)
			}
			if got.greeting != tt.wantGreeting {
				t.Errorf("greeting = %v, want %v", got.greeting, tt.wantGreeting)
			}
		})
	}
}

// TestFlagParseErrorIsTyped proves FlagParseError carries its reason and unwraps its cause,
// so the boundary failure is errors.As-recoverable rather than a bare string.
func TestFlagParseErrorIsTyped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       *FlagParseError
		wantMsg   string
		wantCause bool
	}{
		{name: "reason only", err: &FlagParseError{Reason: "boom"}, wantMsg: "swe: boom"},
		{
			name:      "reason with cause",
			err:       &FlagParseError{Reason: "bad id", Cause: errStub{}},
			wantMsg:   "swe: bad id: stub",
			wantCause: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.wantMsg {
				t.Errorf("Error() = %q, want %q", got, tt.wantMsg)
			}
			if (tt.err.Unwrap() != nil) != tt.wantCause {
				t.Errorf("Unwrap() non-nil = %v, want %v", tt.err.Unwrap() != nil, tt.wantCause)
			}
		})
	}
}

// errStub is a minimal error for the cause-chaining assertion.
type errStub struct{}

func (errStub) Error() string { return "stub" }
