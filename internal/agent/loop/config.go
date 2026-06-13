package loop

import (
	"time"

	"github.com/inventivepotter/urvi/internal/llm"
)

type Config struct {
	Client       llm.LLM       // required — caller constructs via auto.New at composition root
	Model        llm.ModelSpec // model name, system prompt, sampling params — sent every turn
	Sinks        []EventSink   // optional side-effect sinks for observability/journaling
	DrainTimeout time.Duration // optional — bounds the hard-kill wait for a cancelled turn to drain; New defaults it to 5s
}
