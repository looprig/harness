//go:build integration

package coding

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/eval"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/auto"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// errTurnInterrupted is the eval-harness sentinel for a turn whose context was
// cancelled (event.TurnInterrupted carries no typed cause to forward).
var errTurnInterrupted = errors.New("turn interrupted")

// togoRunner adapts the live Togo agent to eval.Runner: it runs one turn for the
// input prompt over the session subscription transport and projects the terminal
// TurnDone.Message to text (reusing the aiMessageText projection from text_test.go
// — this test is in package coding, so the unexported helper is in scope).
type togoRunner struct{ agent *Coding }

// Run subscribes to the session fan-in, submits a single turn fire-and-forget, and
// drains the subscription to that turn's terminal, returning the terminal assistant
// text. It subscribes BEFORE submitting so no event is missed, correlates by the
// submit command id (TurnStarted.Cause.CommandID == id) to capture the turn id, then
// returns the latest TurnDone.Message for that turn; TurnFailed/TurnInterrupted map
// to an error. Enduring/all-loop scope is enough — every terminal is Enduring — and
// avoids importing the tui package for its DefaultEventFilter.
func (r togoRunner) Run(ctx context.Context, input string) (string, error) {
	sub, err := r.agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		return "", err
	}
	defer func() { _ = sub.Close() }()

	id, err := r.agent.Submit(ctx, []content.Block{&content.TextBlock{Text: input}})
	if err != nil {
		return "", err
	}

	var turnID uuid.UUID // captured from this submit's TurnStarted; zero until then
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case ev, ok := <-sub.Events():
			if !ok {
				return "", sub.Err() // hub-forced loss (or nil on intentional close)
			}
			switch e := ev.(type) {
			case event.TurnStarted:
				if e.Cause.CommandID == id {
					turnID = e.TurnID
				}
			case event.TurnDone:
				if !turnID.IsZero() && e.TurnID == turnID {
					return aiMessageText(e.Message), nil
				}
			case event.TurnFailed:
				if !turnID.IsZero() && e.TurnID == turnID {
					return "", e.Err
				}
			case event.TurnInterrupted:
				if !turnID.IsZero() && e.TurnID == turnID {
					return "", errTurnInterrupted
				}
			}
		}
	}
}

// modelCompleter adapts an llm.LLM to eval.Completer for the Judge metric. It
// holds the provider client and the model spec built from auto.New(judgeSpec).
type modelCompleter struct {
	client llm.LLM
	spec   llm.ModelSpec
}

// Complete builds a single user-message request and projects the response to
// text. The AgenticMessages construction mirrors the production turn builder in
// internal/agent/loop/turn.go:42-45 (a *content.UserMessage wrapping a
// content.Message with Role: content.RoleUser and the prompt as a TextBlock),
// and llm.Response.Message is a *content.AIMessage, so aiMessageText projects it.
func (m modelCompleter) Complete(ctx context.Context, prompt string) (string, error) {
	msgs := content.AgenticMessages{
		&content.UserMessage{Message: content.Message{
			Role:   content.RoleUser,
			Blocks: []content.Block{&content.TextBlock{Text: prompt}},
		}},
	}
	resp, err := m.client.Invoke(ctx, llm.Request{Model: m.spec, Messages: msgs})
	if err != nil {
		return "", err
	}
	return aiMessageText(resp.Message), nil
}

// TestTogoEvalIntegration runs the live Togo agent through the golden-set with
// the deterministic Contains metric and a model-backed Judge. It skips cleanly
// when LLM_API_KEY is unset, so the default (untagged) suite and a tagged build
// without a key never attempt a network call.
func TestTogoEvalIntegration(t *testing.T) {
	if os.Getenv("LLM_API_KEY") == "" {
		t.Skip("LLM_API_KEY not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	agent, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })

	cases, err := eval.LoadCases("golden-set/cases")
	if err != nil {
		t.Fatalf("LoadCases: %v", err)
	}

	run, err := eval.RunCases(ctx, togoRunner{agent: agent}, cases)
	if err != nil {
		t.Fatalf("RunCases: %v", err)
	}

	// The judge reuses the same production model + key (package-level model var),
	// with a strict-evaluator system prompt.
	judgeSpec := model.Spec(os.Getenv("LLM_API_KEY"), "You are a strict, impartial evaluator.")
	judgeClient, err := auto.New(judgeSpec)
	if err != nil {
		t.Fatalf("auto.New: %v", err)
	}

	results, err := eval.Evaluate(ctx, run, []eval.Metric{
		eval.Contains{},
		eval.Judge{
			Criteria:  "The response directly and correctly answers the input.",
			Threshold: 0.6,
			Model:     modelCompleter{client: judgeClient, spec: judgeSpec},
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	for _, r := range results {
		if r.Passed {
			continue
		}
		var b strings.Builder
		for _, s := range r.Scores {
			b.WriteString(s.Metric + "=" + s.Reason + "; ")
		}
		t.Errorf("case %q failed: %s", r.Case.Name, b.String())
	}
}
