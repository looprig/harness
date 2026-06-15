package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/tool"
)

// askuser.go implements the AskUser tool (design §4b, row AskUser; §2e
// loop.RequestUserInput; impl plan row 6.9). AskUser lets the model pause a turn
// to ask the human a question — optionally constrained to a fixed choice list —
// and resume with the answer.
//
// LEAST PRIVILEGE: AskUser takes NO dependencies (no filesystem, no network). It
// reaches the user solely through loop.RequestUserInput, which reads the per-call
// emit/CallID/gateReg the runner injected into ctx (gate.go). The tool never
// touches the gate plumbing directly.
//
// AUTO-APPROVE: AskUser is AutoApprove — it deliberately does NOT implement
// tool.PermissionPrompter (asking the user is itself the interaction; gating it
// behind a second approval prompt would be absurd). It DOES implement
// tool.Auditable: the question is shown to the user anyway, so it is not a secret
// and is the right one-line summary.
//
// FAILURE MODEL: every failure — unparsable args, a missing question, an answer
// that violates the choice list, or a provider/gate error — is a tool-result
// error STRING. InvokableRun never returns a Go error (CLAUDE.md: tool failures →
// tool-result strings).
//
// TEST SEAM (documented): loop.RequestUserInput reads emit/CallID/gateReg from ctx
// via unexported injectors in package loop, which package tools cannot call. So
// AskUser holds an indirect requestUserInput func field defaulting to
// loop.RequestUserInput in NewAskUser; a unit test overrides it to exercise the
// answer-validation logic without standing up a real loop gate. The real loop
// wiring is exercised by the loop package's gate tests + integration later.

// askUserToolName is the EXACT tool name. It is an UNKNOWN class to classifyTool
// (no path/command boundary), so check.go skips Stages 1–2 and the call reaches
// AutoApprove only via the manifest's HardApprove list (which names "AskUser").
const askUserToolName = "AskUser"

// otherChoice is the reserved escape-hatch answer always accepted alongside an
// explicit choice list, so the user can decline the menu and give free text. It
// is matched case-sensitively, like every choice.
const otherChoice = "other"

const askUserSchema = `{
  "type": "object",
  "properties": {
    "question": {"type": "string", "description": "The question to ask the user."},
    "choices": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Optional fixed answer choices. When present, the answer must be exactly one of these or the literal \"other\" (an escape hatch for free text)."
    }
  },
  "required": ["question"]
}`

const askUserDesc = "Pause and ask the user a question, optionally constrained to a fixed list of choices, then resume with their answer. Has no filesystem or network access."

// requestUserInputFunc is the seam type matching loop.RequestUserInput's
// signature. NewAskUser defaults a field of this type to loop.RequestUserInput;
// tests override it (see the package doc on the test seam).
type requestUserInputFunc func(ctx context.Context, question string, choices []string) (string, error)

// askUserArgs is the typed decode of AskUser's untrusted argsJSON. The JSON field
// contract is {question string, choices []string (optional)}.
type askUserArgs struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices"`
}

// AskUser asks the human a question via the loop's user-input gate. It holds no
// dependencies beyond the (overridable) requestUserInput seam.
type AskUser struct {
	requestUserInput requestUserInputFunc
}

// NewAskUser constructs an AskUser whose seam is wired to the real
// loop.RequestUserInput (which reads the runner-injected ctx values per call).
func NewAskUser() *AskUser {
	return &AskUser{requestUserInput: loop.RequestUserInput}
}

// Info returns AskUser's self-description. Name MUST equal "AskUser".
func (a *AskUser) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   askUserToolName,
		Desc:   askUserDesc,
		Schema: json.RawMessage(askUserSchema),
	}, nil
}

// AuditSummary returns "AskUser: <question>". The question is shown to the user
// at the gate anyway, so it is not a secret. Unparsable args / an empty question
// yield a generic summary.
func (a *AskUser) AuditSummary(argsJSON string) string {
	var args askUserArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || strings.TrimSpace(args.Question) == "" {
		return "AskUser (unparsable args)"
	}
	return "AskUser: " + args.Question
}

// InvokableRun parses the args, requests user input via the seam, validates the
// answer against the choice list (if any), and returns the validated answer as
// the tool-result text. Every failure is a tool-result error STRING; it never
// returns a Go error.
func (a *AskUser) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	args, err := parseAskUserArgs(argsJSON)
	if err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}

	answer, err := a.requestUserInput(ctx, args.Question, args.Choices)
	if err != nil {
		// A cancelled turn, departed actor, or missing gate plumbing — surface a
		// generic failure (the underlying error may reference internal gate state).
		return tool.TextResult("error: could not get user input: " + err.Error()), nil
	}

	if err := validateAnswer(answer, args.Choices); err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	return tool.TextResult(answer), nil
}

// parseAskUserArgs decodes + validates the args. A non-object document or an
// empty question is a typed *askUserError.
func parseAskUserArgs(argsJSON string) (askUserArgs, error) {
	var args askUserArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return askUserArgs{}, &askUserError{reason: "invalid arguments: not a JSON object", cause: err}
	}
	if strings.TrimSpace(args.Question) == "" {
		return askUserArgs{}, &askUserError{reason: "a non-empty 'question' is required"}
	}
	return args, nil
}

// validateAnswer enforces the choice contract: when choices is non-empty the
// answer must be EXACTLY one of the choices (case-sensitive) or the literal
// "other" escape hatch; when choices is empty any free-text answer (including the
// empty string) is accepted. An invalid answer is a typed *askUserError listing
// the allowed values.
func validateAnswer(answer string, choices []string) error {
	if len(choices) == 0 {
		return nil // free-text mode: anything is accepted.
	}
	if answer == otherChoice {
		return nil // escape hatch.
	}
	for _, c := range choices {
		if answer == c {
			return nil
		}
	}
	return &askUserError{reason: "answer must be one of: " + strings.Join(choices, ", ") + ", or \"" + otherChoice + "\""}
}

// askUserError is the typed failure for AskUser arg parsing/validation and answer
// validation. It carries a non-secret reason; InvokableRun maps every failure to
// a tool-result string.
type askUserError struct {
	reason string
	cause  error
}

func (e *askUserError) Error() string { return e.reason }

func (e *askUserError) Unwrap() error { return e.cause }

// compile-time assertions: AskUser is an InvokableTool and Auditable. It is
// deliberately NOT a PermissionPrompter (AutoApprove) and NOT a WriteTarget.
var (
	_ tool.InvokableTool = (*AskUser)(nil)
	_ tool.Auditable     = (*AskUser)(nil)
)
