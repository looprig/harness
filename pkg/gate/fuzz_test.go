package gate

import (
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

func FuzzDecodeRequest(f *testing.F) {
	f.Add([]byte(`{"tool_name":"Bash","summary":"run git status","execution_id":"exec-1","command":"git status","working_directory":"/workspace","expires_at_unix_milli":1800000000000,"requirements":[{"kind":"command.execute","scope":"","match":"git status","description":"run command: git status","grant_class":"command.start.v1","grant_target":"git status","candidates":[{"kind":"command.execute","match":"Bash(git status)","description":"Bash(git status)","grant_class":"command.start.v1","grant_target":"git status"}]}]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"requirements":[{"kind":"network","scope":"","match":"tcp:github.com:443","description":"d"}]}`))
	f.Add([]byte(`{"tool_name":"Bash","tool_name":"Other"}`))
	f.Add([]byte(strings.Repeat("[", 20000)))
	f.Fuzz(func(t *testing.T, data []byte) {
		request, err := DecodeRequest(data)
		if err != nil {
			if request.ToolName != "" || request.Requirements != nil {
				t.Fatalf("DecodeRequest error path returned non-zero request %#v", request)
			}
			return
		}
		// Anything the decoder accepts must satisfy every prepared-request
		// invariant: the decoder is the untrusted boundary.
		if err := tool.ValidateRequest(request); err != nil {
			t.Fatalf("DecodeRequest accepted invariant-violating request: %v", err)
		}
	})
}

func FuzzDecodeApprovalAction(f *testing.F) {
	f.Add([]byte(`{"action":"Approve"}`))
	f.Add([]byte(`{"action":"Approve always for this workspace"}`))
	f.Add([]byte(`{"action":"Deny"}`))
	f.Add([]byte(`{"action":"approve"}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"action":"Approve","scope":"session"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		action, err := DecodeApprovalAction(data)
		if err != nil {
			if action != "" {
				t.Fatalf("DecodeApprovalAction error path returned action %q", action)
			}
			return
		}
		switch action {
		case ApprovalApprove, ApprovalApproveAlwaysWorkspace, ApprovalDeny:
		default:
			t.Fatalf("DecodeApprovalAction accepted non-exact action %q", action)
		}
	})
}
