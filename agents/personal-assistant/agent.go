// Package personalassistant is a conversational personal-assistant agent built
// on the session engine. It wraps a session.AgentSession with a fixed persona
// and a named model, exposing a small text-in / event-out surface.
package personalassistant

import (
	"context"
	"strings"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/session"
)

// Assistant is a persona-bearing wrapper over a session.AgentSession. The
// caller owns it and must call Close to release the underlying actor goroutine.
type Assistant struct {
	session *session.AgentSession
	cancel  context.CancelFunc // cancels the session's root context; called by Close
}

// newWithClient is the construction seam shared by New and tests; tests inject a
// fake llm.LLM here, avoiding real environment reads and network calls. It gives
// the session a root context derived from context.Background() — independent of
// the caller's ctx — so a request-scoped or timeout ctx passed to New cannot
// later tear the session down. ctx bounds only this construction call.
func newWithClient(ctx context.Context, client llm.LLM, spec llm.ModelSpec) (*Assistant, error) {
	if err := ctx.Err(); err != nil {
		return nil, &session.SessionError{Kind: session.SessionContextDone, Cause: err}
	}
	rootCtx, cancel := context.WithCancel(context.Background())
	sess, err := session.NewAgent(rootCtx, loop.Config{Client: client, Model: spec})
	if err != nil {
		cancel()
		return nil, err
	}
	return &Assistant{session: sess, cancel: cancel}, nil
}

// Send delivers one user message and blocks until the turn reaches a terminal
// event, returning it unchanged as one of the value types loop.TurnDone,
// loop.TurnFailed, or loop.TurnInterrupted. The Go error return is nil for all
// three terminal outcomes: a provider failure surfaces as a loop.TurnFailed
// whose Err carries the original provider/engine cause, not as a Go error. The
// Go error is non-nil only when no turn completed (transport failures: the loop
// exited, or ctx done), and the event is then nil. Cancel ctx to interrupt the
// in-flight turn; Send then returns loop.TurnInterrupted with a nil error.
func (a *Assistant) Send(ctx context.Context, text string) (loop.Event, error) {
	blocks, err := userBlocks(text)
	if err != nil {
		return nil, err
	}
	return a.session.Invoke(ctx, blocks)
}

// Stream delivers one user message and returns the session's event stream:
// TurnStarted, TokenDelta×N, then one terminal event, then EOF. Callers must
// read until EOF or call sr.Close(). sr.Close() abandons the stream and
// interrupts the turn asynchronously, so an immediately following Send may
// briefly observe *loop.TurnBusyError until the cancelled turn unwinds.
func (a *Assistant) Stream(ctx context.Context, text string) (*llm.StreamReader[loop.Event], error) {
	blocks, err := userBlocks(text)
	if err != nil {
		return nil, err
	}
	return a.session.Stream(ctx, blocks)
}

// Close gracefully shuts the session down and releases the session's root
// context. It blocks until the actor exits (or ctx is done), then cancels the
// root as a backstop so the actor goroutine cannot leak even if Shutdown timed
// out on ctx. Safe to call more than once.
func (a *Assistant) Close(ctx context.Context) error {
	err := a.session.Shutdown(ctx)
	a.cancel()
	return err
}

// userBlocks wraps user text into a single text content block. It rejects blank
// input before the session is touched.
func userBlocks(text string) ([]*content.Block, error) {
	if strings.TrimSpace(text) == "" {
		return nil, &EmptyInputError{}
	}
	return []*content.Block{{
		Type: content.TypeText,
		Text: &content.TextBlock{Text: text},
	}}, nil
}
