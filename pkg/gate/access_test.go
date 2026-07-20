package gate

import (
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

type stubAccessSource struct {
	version uint16
	access  uint8
	err     error
	calls   []accessCall
}

type accessCall struct {
	kind  string
	scope string
}

func (s *stubAccessSource) AccessVersion() uint16 { return s.version }

func (s *stubAccessSource) AccessFor(kind, scope string) (uint8, error) {
	s.calls = append(s.calls, accessCall{kind: kind, scope: scope})
	return s.access, s.err
}

func TestAccessABIValues(t *testing.T) {
	if CurrentAccessVersion != 1 {
		t.Fatalf("CurrentAccessVersion = %d, want 1", CurrentAccessVersion)
	}
	if AccessDeny != 0 || AccessGated != 1 || AccessAllow != 2 {
		t.Fatalf("access values = (%d,%d,%d), want (0,1,2)", AccessDeny, AccessGated, AccessAllow)
	}
}

func TestAccessBindingsRouteEachRequirementKindExactlyOnce(t *testing.T) {
	command := &stubAccessSource{version: CurrentAccessVersion, access: AccessAllow}
	filesystem := &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}
	bindings, err := NewAccessBindings([]AccessBinding{
		{Kind: "command.execute", Source: command},
		{Kind: "filesystem.read", Source: filesystem},
	})
	if err != nil {
		t.Fatalf("NewAccessBindings() error = %v", err)
	}

	if got, err := bindings.AccessFor(tool.Requirement{Kind: "command.execute", Scope: ""}); err != nil || got != AccessAllow {
		t.Fatalf("command AccessFor() = %d, %v, want %d, nil", got, err, AccessAllow)
	}
	if got, err := bindings.AccessFor(tool.Requirement{Kind: "filesystem.read", Scope: "/workspace/file"}); err != nil || got != AccessGated {
		t.Fatalf("filesystem AccessFor() = %d, %v, want %d, nil", got, err, AccessGated)
	}
	if len(command.calls) != 1 || command.calls[0] != (accessCall{kind: "command.execute"}) {
		t.Fatalf("command calls = %#v, want exact command route", command.calls)
	}
	if len(filesystem.calls) != 1 || filesystem.calls[0] != (accessCall{kind: "filesystem.read", scope: "/workspace/file"}) {
		t.Fatalf("filesystem calls = %#v, want exact filesystem route", filesystem.calls)
	}
}

func TestAccessBindingsFailClosed(t *testing.T) {
	sourceErr := errors.New("source failed")
	tests := []struct {
		name     string
		bindings []AccessBinding
		request  tool.Requirement
	}{
		{name: "missing source", request: tool.Requirement{Kind: "command.execute"}},
		{name: "duplicate source", bindings: []AccessBinding{{Kind: "command.execute", Source: &stubAccessSource{version: 1}}, {Kind: "command.execute", Source: &stubAccessSource{version: 1}}}, request: tool.Requirement{Kind: "command.execute"}},
		{name: "nil source", bindings: []AccessBinding{{Kind: "command.execute"}}, request: tool.Requirement{Kind: "command.execute"}},
		{name: "unsupported version", bindings: []AccessBinding{{Kind: "command.execute", Source: &stubAccessSource{version: 2}}}, request: tool.Requirement{Kind: "command.execute"}},
		{name: "unknown value", bindings: []AccessBinding{{Kind: "command.execute", Source: &stubAccessSource{version: 1, access: 3}}}, request: tool.Requirement{Kind: "command.execute"}},
		{name: "source error", bindings: []AccessBinding{{Kind: "command.execute", Source: &stubAccessSource{version: 1, err: sourceErr}}}, request: tool.Requirement{Kind: "command.execute"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bindings, err := NewAccessBindings(tt.bindings)
			if err == nil {
				_, err = bindings.AccessFor(tt.request)
			}
			if err == nil {
				t.Fatal("error = nil, want fail-closed error")
			}
		})
	}
}
