package loop

import (
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/llm"
)

type Config struct {
	Client       llm.LLM           // required — caller constructs via auto.New at composition root
	Model        llm.ModelSpec     // model name, system prompt, sampling params — sent every turn
	Sinks        []event.EventSink // optional side-effect sinks for observability/journaling
	DrainTimeout time.Duration     // optional — bounds the hard-kill wait for a cancelled turn to drain; New defaults it to 5s

	// idGen mints correlation IDs (TurnID, EventID). It is unexported, so the
	// composition root cannot set it: New defaults it to uuid.New. It exists
	// only as a test seam for exercising the crypto/rand failure branches.
	idGen idGenerator
}
