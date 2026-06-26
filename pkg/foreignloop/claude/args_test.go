package claude

import (
	"testing"

	"github.com/ciram-co/looprig/pkg/foreignloop"
)

// indexOf returns the first index of v in argv, or -1 when absent.
func indexOf(argv []string, v string) int {
	for i, a := range argv {
		if a == v {
			return i
		}
	}
	return -1
}

// next returns argv[i+1] for the flag v (or "" when the flag is absent or last).
func next(argv []string, v string) string {
	i := indexOf(argv, v)
	if i < 0 || i+1 >= len(argv) {
		return ""
	}
	return argv[i+1]
}

func TestBuildArgsSessionSelector(t *testing.T) {
	t.Parallel()
	const sid = "11111111-2222-3333-4444-555555555555"
	tests := []struct {
		name       string
		startNew   bool
		wantFlag   string
		absentFlag string
	}{
		{name: "start new selects --session-id", startNew: true, wantFlag: "--session-id", absentFlag: "--resume"},
		{name: "resume selects --resume", startNew: false, wantFlag: "--resume", absentFlag: "--session-id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			turn := foreignloop.ForeignTurn{ForeignSID: sid, StartNew: tt.startNew}
			argv := buildArgs(turn, "small")
			if got := next(argv, tt.wantFlag); got != sid {
				t.Fatalf("flag %s: next = %q, want sid %q", tt.wantFlag, got, sid)
			}
			if indexOf(argv, tt.absentFlag) >= 0 {
				t.Fatalf("flag %s must be absent, argv = %v", tt.absentFlag, argv)
			}
		})
	}
}

func TestBuildArgsPosture(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		posture foreignloop.PermissionPosture
		want    string
	}{
		{name: "default posture", posture: foreignloop.PostureDefault, want: "default"},
		{name: "accept edits posture", posture: foreignloop.PostureAcceptEdits, want: "acceptEdits"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			turn := foreignloop.ForeignTurn{ForeignSID: "sid", Posture: tt.posture}
			argv := buildArgs(turn, "small")
			if got := next(argv, "--permission-mode"); got != tt.want {
				t.Fatalf("--permission-mode next = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildArgsValueFlagsSeparate(t *testing.T) {
	t.Parallel()
	turn := foreignloop.ForeignTurn{
		SystemPrompt: "SYS PROMPT",
		ForeignSID:   "sid",
		StartNew:     true,
		Cwd:          "/work/dir",
		Posture:      foreignloop.PostureDefault,
	}
	argv := buildArgs(turn, "the-model")
	tests := []struct {
		name string
		flag string
		want string
	}{
		{name: "append-system-prompt value is a separate element", flag: "--append-system-prompt", want: "SYS PROMPT"},
		{name: "model value is a separate element", flag: "--model", want: "the-model"},
		{name: "add-dir value is a separate element", flag: "--add-dir", want: "/work/dir"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := next(argv, tt.flag); got != tt.want {
				t.Fatalf("%s next = %q, want %q", tt.flag, got, tt.want)
			}
		})
	}
	// No "--flag=value" concatenation must ever appear.
	for _, a := range argv {
		if len(a) > 2 && a[:2] == "--" {
			for _, c := range a {
				if c == '=' {
					t.Fatalf("argv element %q uses --flag=value form, want separate elements", a)
				}
			}
		}
	}
}

func TestBuildArgsAlwaysPresent(t *testing.T) {
	t.Parallel()
	turn := foreignloop.ForeignTurn{ForeignSID: "sid", StartNew: true}
	argv := buildArgs(turn, "small")
	tests := []struct {
		name string
		flag string
	}{
		{name: "print mode", flag: "-p"},
		{name: "verbose always present", flag: "--verbose"},
		{name: "include partial messages always present", flag: "--include-partial-messages"},
		{name: "output-format flag present", flag: "--output-format"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if indexOf(argv, tt.flag) < 0 {
				t.Fatalf("flag %s missing from argv %v", tt.flag, argv)
			}
		})
	}
	if got := next(argv, "--output-format"); got != "stream-json" {
		t.Fatalf("--output-format next = %q, want stream-json", got)
	}
}
