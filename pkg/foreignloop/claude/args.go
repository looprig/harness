package claude

import "github.com/looprig/harness/pkg/foreignloop"

// Fixed claude CLI flag tokens. Each is a SEPARATE argv element; a flag and its
// value are never concatenated ("--model x", never "--model=x").
const (
	flagPrint           = "-p"
	flagOutputFormat    = "--output-format"
	valStreamJSON       = "stream-json"
	flagPartialMessages = "--include-partial-messages"
	flagVerbose         = "--verbose"
	flagSystemPrompt    = "--append-system-prompt"
	flagModel           = "--model"
	flagPermissionMode  = "--permission-mode"
	flagAddDir          = "--add-dir"
	flagSessionID       = "--session-id"
	flagResume          = "--resume"
)

// Permission-mode strings claude --permission-mode understands. Pinned by the E4
// integration test against the installed CLI.
const (
	permModeDefault     = "default"
	permModeAcceptEdits = "acceptEdits"
)

// postureString maps the typed posture enum to claude's --permission-mode value via
// a typed switch, so no raw mode string leaks from a caller. Fail-secure: an
// unknown posture maps to the least-privileged "default" mode.
func postureString(p foreignloop.PermissionPosture) string {
	switch p {
	case foreignloop.PostureAcceptEdits:
		return permModeAcceptEdits
	default:
		return permModeDefault
	}
}

// buildArgs returns the claude argv (NOT a shell string) for one foreign turn. Every
// token is a separate slice element; value-taking flags emit two elements. Exactly
// one session selector is appended: --session-id for a fresh session (StartNew),
// otherwise --resume to continue the existing foreign session.
func buildArgs(t foreignloop.ForeignTurn, model string) []string {
	args := []string{
		flagPrint,
		flagOutputFormat, valStreamJSON,
		flagPartialMessages,
		flagVerbose,
		flagSystemPrompt, t.SystemPrompt,
		flagModel, model,
		flagPermissionMode, postureString(t.Posture),
		flagAddDir, t.Cwd,
	}
	if t.StartNew {
		return append(args, flagSessionID, t.ForeignSID)
	}
	return append(args, flagResume, t.ForeignSID)
}
