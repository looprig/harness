package loop

import (
	"time"

	"github.com/inventivepotter/urvi/internal/llm"
)

type Config struct {
	Client       llm.LLM       // required — caller constructs via auto.New at composition root
	Model        llm.ModelSpec // model name, system prompt, sampling params — sent every turn
	DrainTimeout time.Duration // optional — bounds the hard-kill wait for a cancelled turn to drain; New defaults it to 5s

	// Tools is the runner's view of the tool subsystem (the consumer surface in
	// deps.go). Its runaway-guard caps are defaulted by New when zero;
	// Permission/Registry/Middlewares are left as the composition root set them
	// (nil is valid).
	Tools ToolSet

	// idGen mints the loop's correlation IDs: the per-turn TurnID, each StepID,
	// and each tool-call ToolExecutionID. It is unexported, so the composition root cannot
	// set it: New defaults it to uuid.New. It exists only as a test seam for
	// exercising the crypto/rand failure branches.
	idGen idGenerator

	// afterDrain is a test-only synchronization seam invoked by foldPending in the
	// turn goroutine AFTER cfg.drainPending has moved the inbox into the actor's
	// draining buffer but BEFORE the first TurnFoldedInto commit. It is unexported,
	// so the composition root cannot set it; New never assigns it, so it is nil in
	// production and foldPending's nil check skips it entirely. It exists only so a
	// test can cancel the loop deterministically in the post-drain/pre-commit window
	// to exercise the draining-buffer abnormal-return sweep.
	afterDrain func()
}
