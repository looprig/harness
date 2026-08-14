package loopruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
)

// midStreamBoom is the provider failure every truncation test injects after the
// chunks the user already watched arrive.
func midStreamBoom() error {
	return &model.ValidationError{Field: "stream", Reason: "connection reset"}
}

// truncatedStepMessage runs one step whose stream fails after the given chunks
// and returns the assistant message the step stored (nil when it stored none).
func truncatedStepMessage(t *testing.T, chunks []content.Chunk) (*content.AIMessage, stepResult) {
	t.Helper()
	client := &fakeLLM{chunks: chunks, nextErr: midStreamBoom()}
	cfg := stepConfig{req: inference.Request{Model: testModel()}, client: client, emit: func(event.Event) {}}
	res := runStep(context.Background(), cfg, 5, newTestStep(t, 0))
	if _, ok := res.terminal.(event.TurnFailed); !ok {
		t.Fatalf("terminal = %T, want event.TurnFailed", res.terminal)
	}
	if len(res.state.msgs) == 0 {
		return nil, res
	}
	msg, ok := res.state.msgs[0].(*content.AIMessage)
	if !ok {
		t.Fatalf("msgs[0] = %T, want *content.AIMessage", res.state.msgs[0])
	}
	return msg, res
}

// blockKinds names each block's concrete variant, so an assertion reports what a
// truncated message actually holds rather than a bare length mismatch.
func blockKinds(blocks []content.Block) []string {
	kinds := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch b.(type) {
		case *content.TextBlock:
			kinds = append(kinds, "text")
		case *content.ThinkingBlock:
			kinds = append(kinds, "thinking")
		case *content.ToolUseBlock:
			kinds = append(kinds, "tool_use")
		case *content.RefusalBlock:
			kinds = append(kinds, "refusal")
		case *content.ImageBlock:
			kinds = append(kinds, "image")
		default:
			kinds = append(kinds, "other")
		}
	}
	return kinds
}

func countToolUseBlocks(msgs content.AgenticMessages) int {
	calls, _ := countToolUseInHistory(msgs)
	return calls
}

// assertTruncationNotice fails unless the message's LAST block is the truncation
// notice: a reader of the stored transcript must be able to tell the turn was cut
// off, and it must be the final thing they read.
func assertTruncationNotice(t *testing.T, msg *content.AIMessage) {
	t.Helper()
	if msg == nil || len(msg.Blocks) == 0 {
		t.Fatalf("message = %v, want a stored message ending in the truncation notice", msg)
	}
	last, ok := msg.Blocks[len(msg.Blocks)-1].(*content.TextBlock)
	if !ok {
		t.Fatalf("last block = %T, want *content.TextBlock carrying the truncation notice", msg.Blocks[len(msg.Blocks)-1])
	}
	if last.Text != TruncatedResponseNotice {
		t.Fatalf("last block text = %q, want the truncation notice %q", last.Text, TruncatedResponseNotice)
	}
}

// TestRunStepTruncation pins what a step does with the content it ALREADY emitted
// as live TokenDeltas when the stream then fails: the safe prefix is stored, the
// structurally incomplete remainder is dropped, and the stored message says so.
func TestRunStepTruncation(t *testing.T) {
	t.Parallel()

	t.Run("text-only truncation stores the text the user already watched arrive", func(t *testing.T) {
		t.Parallel()
		msg, res := truncatedStepMessage(t, []content.Chunk{textChunk("the answer is "), textChunk("forty-t")})
		if msg == nil {
			t.Fatal("no assistant message stored; the streamed text was discarded")
		}
		if res.state.status != stepTruncated {
			t.Errorf("status = %v, want stepTruncated", res.state.status)
		}
		if got := blockKinds(msg.Blocks); len(got) != 2 || got[0] != "text" {
			t.Fatalf("blocks = %v, want [text text(notice)]", got)
		}
		text := msg.Blocks[0].(*content.TextBlock).Text
		if text != "the answer is forty-t" {
			t.Errorf("stored text = %q, want the full streamed prefix", text)
		}
		assertTruncationNotice(t, msg)
	})

	t.Run("truncation mid tool_use drops the partial call and keeps the text", func(t *testing.T) {
		t.Parallel()
		msg, res := truncatedStepMessage(t, []content.Chunk{
			textChunk("looking that up"),
			toolUseChunk(0, "call-1", "Echo", `{"path":"/etc/pas`),
		})
		if msg == nil {
			t.Fatal("no assistant message stored; the streamed text was discarded")
		}
		if res.state.status != stepTruncated {
			t.Errorf("status = %v, want stepTruncated", res.state.status)
		}
		for _, kind := range blockKinds(msg.Blocks) {
			if kind == "tool_use" {
				t.Fatalf("blocks = %v, want no tool_use: a half-decoded call must never be stored", blockKinds(msg.Blocks))
			}
		}
		if got := blockKinds(msg.Blocks); len(got) != 2 || got[0] != "text" {
			t.Fatalf("blocks = %v, want [text text(notice)]", got)
		}
		assertTruncationNotice(t, msg)
	})

	t.Run("truncation on a tool_use whose JSON happens to be complete still drops it", func(t *testing.T) {
		t.Parallel()
		// Byte-complete arguments are NOT enough: the batch never ran, so the call
		// has no tool_result and would be an orphan in committed history.
		msg, _ := truncatedStepMessage(t, []content.Chunk{
			textChunk("running"),
			toolUseChunk(0, "call-1", "Echo", `{"path":"/tmp"}`),
		})
		if msg == nil {
			t.Fatal("no assistant message stored")
		}
		for _, kind := range blockKinds(msg.Blocks) {
			if kind == "tool_use" {
				t.Fatalf("blocks = %v, want no tool_use (orphaned call)", blockKinds(msg.Blocks))
			}
		}
	})

	t.Run("truncation mid thinking with no signature drops the reasoning block", func(t *testing.T) {
		t.Parallel()
		msg, _ := truncatedStepMessage(t, []content.Chunk{
			&content.ThinkingChunk{Thinking: "let me work through th"},
			textChunk("so far"),
		})
		if msg == nil {
			t.Fatal("no assistant message stored; the streamed text was discarded")
		}
		for _, kind := range blockKinds(msg.Blocks) {
			if kind == "thinking" {
				t.Fatalf("blocks = %v, want no unsigned thinking block", blockKinds(msg.Blocks))
			}
		}
		if got := blockKinds(msg.Blocks); len(got) != 2 || got[0] != "text" {
			t.Fatalf("blocks = %v, want [text text(notice)]", got)
		}
	})

	t.Run("truncation after a signed thinking block keeps the sealed reasoning", func(t *testing.T) {
		t.Parallel()
		msg, _ := truncatedStepMessage(t, []content.Chunk{
			// The label is what makes the signature a seal: an unlabelled
			// signature is replayable by nobody, so sealedReasoning drops it.
			&content.ThinkingChunk{Index: 0, Thinking: "complete reasoning", Signature: "sig-0", SignatureFormat: "anthropic"},
			&content.ThinkingChunk{Index: 1, Thinking: "cut off he"},
			textChunk("partial answer"),
		})
		if msg == nil {
			t.Fatal("no assistant message stored")
		}
		got := blockKinds(msg.Blocks)
		if len(got) != 3 || got[0] != "thinking" || got[1] != "text" {
			t.Fatalf("blocks = %v, want [thinking text text(notice)]", got)
		}
		think := msg.Blocks[0].(*content.ThinkingBlock)
		if think.Signature != "sig-0" || think.Thinking != "complete reasoning" {
			t.Errorf("kept thinking = %+v, want the sealed index-0 block", think)
		}
	})

	t.Run("truncation drops a thinking block whose signature has no minting dialect", func(t *testing.T) {
		t.Parallel()
		// A signature is verified by the endpoint that minted it, so an
		// unlabelled one is replayable by nobody: every codec refuses it. Keeping
		// such a block in the truncated tail would turn this function's job —
		// "keep only what can be sent back" — into a guaranteed encode failure on
		// the next request, which is worse than the drop an unsigned block gets.
		msg, _ := truncatedStepMessage(t, []content.Chunk{
			&content.ThinkingChunk{Index: 0, Thinking: "complete reasoning", Signature: "sig-0"},
			textChunk("partial answer"),
		})
		if msg == nil {
			t.Fatal("no assistant message stored")
		}
		for _, kind := range blockKinds(msg.Blocks) {
			if kind == "thinking" {
				t.Fatalf("blocks = %v, want no thinking block: its signature carries no dialect label",
					blockKinds(msg.Blocks))
			}
		}
	})

	t.Run("truncation after a redacted thinking block keeps its provider state", func(t *testing.T) {
		t.Parallel()
		msg, _ := truncatedStepMessage(t, []content.Chunk{
			&content.ThinkingChunk{
				ProviderState:       json.RawMessage(`{"redacted":"opaque"}`),
				ProviderStateFormat: "anthropic",
			},
			textChunk("partial"),
		})
		if msg == nil {
			t.Fatal("no assistant message stored")
		}
		got := blockKinds(msg.Blocks)
		if len(got) != 3 || got[0] != "thinking" {
			t.Fatalf("blocks = %v, want [thinking text text(notice)]", got)
		}
		think := msg.Blocks[0].(*content.ThinkingBlock)
		if string(think.ProviderState) != `{"redacted":"opaque"}` || think.ProviderStateFormat != "anthropic" {
			t.Errorf("kept thinking = %+v, want the redacted block's provider state preserved", think)
		}
	})

	t.Run("failure with nothing decoded stores no message at all", func(t *testing.T) {
		t.Parallel()
		msg, res := truncatedStepMessage(t, nil)
		if msg != nil {
			t.Fatalf("stored message = %+v, want none (nothing was decoded)", msg)
		}
		if res.state.status != stepFailed {
			t.Errorf("status = %v, want stepFailed", res.state.status)
		}
	})

	t.Run("failure after empty-text chunks only stores no message", func(t *testing.T) {
		t.Parallel()
		msg, res := truncatedStepMessage(t, []content.Chunk{textChunk(""), textChunk("")})
		if msg != nil {
			t.Fatalf("stored message = %+v, want none (no content was decoded)", msg)
		}
		if res.state.status != stepFailed {
			t.Errorf("status = %v, want stepFailed", res.state.status)
		}
	})

	t.Run("failure after a partial tool call only stores no message", func(t *testing.T) {
		t.Parallel()
		// Nothing but an unusable tool call arrived: there is no safe prefix, so the
		// turn keeps the historical "store nothing" outcome rather than committing a
		// message whose only block is the notice.
		msg, res := truncatedStepMessage(t, []content.Chunk{toolUseChunk(0, "call-1", "Echo", `{"pa`)})
		if msg != nil {
			t.Fatalf("stored message = %+v, want none (only an unusable tool call arrived)", msg)
		}
		if res.state.status != stepFailed {
			t.Errorf("status = %v, want stepFailed", res.state.status)
		}
	})
}

// TestRunTurnTruncationCommits is the end-to-end guarantee: the text the user
// watched stream in reaches COMMITTED history, the turn still fails, and nothing
// structurally incomplete escapes toward a provider or a tool runner.
func TestRunTurnTruncationCommits(t *testing.T) {
	t.Parallel()

	input := []content.Block{&content.TextBlock{Text: "hi"}}
	emptyTS := func() ToolSet { return resolveToolSetCaps(ToolSet{Access: autoApproveGate{}}) }

	t.Run("streamed text survives in committed history and the turn still fails", func(t *testing.T) {
		t.Parallel()
		client := &fakeLLM{chunks: []content.Chunk{textChunk("half an ans")}, nextErr: midStreamBoom()}
		cfg, st, rec := newTurnFixture(input, nil, emptyTS(), client, noGateReg())

		terminal := runTurn(context.Background(), cfg, st)

		if _, ok := terminal.(event.TurnFailed); !ok {
			t.Fatalf("terminal = %T, want event.TurnFailed", terminal)
		}
		committed := rec.committedMsgs()
		if len(committed) != 1 {
			t.Fatalf("committed messages = %d, want 1 (the truncated assistant message)", len(committed))
		}
		msg, ok := committed[0].(*content.AIMessage)
		if !ok {
			t.Fatalf("committed[0] = %T, want *content.AIMessage", committed[0])
		}
		if txt := msg.Blocks[0].(*content.TextBlock).Text; txt != "half an ans" {
			t.Errorf("committed text = %q, want the streamed prefix", txt)
		}
		assertTruncationNotice(t, msg)
		if got := len(stepDones(rec.events())); got != 1 {
			t.Errorf("StepDone count = %d, want 1 (the truncated group was committed)", got)
		}
	})

	t.Run("a truncated tool call reaches neither the tool runner nor the next request", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "ran"}
		client := &fakeLLM{
			chunks: []content.Chunk{
				textChunk("I'll read that file"),
				toolUseChunk(0, "call-1", "Echo", `{"path":"/etc/sha`),
			},
			nextErr: midStreamBoom(),
		}
		ts := agenticToolSet([]tool.InvokableTool{echo}, 25, 100)
		cfg, st, rec := newTurnFixture(input, nil, ts, client, noGateReg())

		terminal := runTurn(context.Background(), cfg, st)

		if _, ok := terminal.(event.TurnFailed); !ok {
			t.Fatalf("terminal = %T, want event.TurnFailed", terminal)
		}
		// (1) The runner never saw the half-decoded call.
		if runs := echo.runCount(); runs != 0 {
			t.Errorf("tool runs = %d, want 0 (a truncated call must never execute)", runs)
		}
		for _, ev := range rec.events() {
			if _, ok := ev.(event.ToolCallStarted); ok {
				t.Error("ToolCallStarted emitted for a truncated tool call")
			}
		}
		// (2) Committed history holds the text but no tool_use block.
		committed := rec.committedMsgs()
		if calls := countToolUseBlocks(committed); calls != 0 {
			t.Errorf("committed tool_use blocks = %d, want 0", calls)
		}
		history := committedHistory(nil, initialUser(st), rec)
		if _, err := validateCompactionTailTranscript(history, 0, make([]content.TokenCount, len(history))); err != nil {
			t.Errorf("committed history rejected by the compaction tail validator: %v", err)
		}
		// (3) Replaying that history to a provider carries no tool_use either.
		next := &recordingLLM{chunks: []content.Chunk{textChunk("recovered")}}
		nextCfg, nextST, _ := newTurnFixture(input, history, emptyTS(), next, noGateReg())
		if _, ok := runTurn(context.Background(), nextCfg, nextST).(event.TurnDone); !ok {
			t.Fatal("follow-up turn did not complete")
		}
		sent := next.lastReq().Messages
		if calls := countToolUseBlocks(sent); calls != 0 {
			t.Fatalf("tool_use blocks sent to the provider = %d, want 0", calls)
		}
		if !strings.Contains(requestText(sent), TruncatedResponseNotice) {
			t.Error("the replayed request does not carry the truncation notice")
		}
	})

	t.Run("a structured-output turn commits nothing, even with content decoded", func(t *testing.T) {
		t.Parallel()
		// A structured answer only becomes an answer once the output validator
		// canonicalizes it. Half a JSON document is not conversational text the user
		// is owed; publishing it would break the atomicity rule that an unvalidated
		// final is wholly unobservable.
		client := &fakeLLM{chunks: []content.Chunk{textChunk(`{"answer":"sec`)}, nextErr: midStreamBoom()}
		cfg, st, rec := nativeTurnFixture(t, client, ToolSet{})

		terminal := runTurn(context.Background(), cfg, st)

		if _, ok := terminal.(event.TurnFailed); !ok {
			t.Fatalf("terminal = %T, want event.TurnFailed", terminal)
		}
		if len(rec.commits) != 0 {
			t.Errorf("commit count = %d, want 0 (unvalidated structured output stays unobservable)", len(rec.commits))
		}
	})

	t.Run("a failure with nothing decoded still commits nothing", func(t *testing.T) {
		t.Parallel()
		client := &fakeLLM{streamErr: midStreamBoom()}
		cfg, st, rec := newTurnFixture(input, nil, emptyTS(), client, noGateReg())

		terminal := runTurn(context.Background(), cfg, st)

		if _, ok := terminal.(event.TurnFailed); !ok {
			t.Fatalf("terminal = %T, want event.TurnFailed", terminal)
		}
		if len(rec.commits) != 0 {
			t.Errorf("commit count = %d, want 0 (nothing was decoded)", len(rec.commits))
		}
	})
}

// ---------------------------------------------------------------------------
// Cancellation: the OTHER way a reply is cut short. A user hitting Ctrl-C
// mid-stream is routine, and the text on their screen must survive it.
// ---------------------------------------------------------------------------

// durableCommitGrace is the budget the tests below hand to a commit they expect
// to SUCCEED. It is deliberately unreachable: those tests ask whether a cancelled
// turn's partial reply is committed at all, and a 250ms production budget is not
// a reproducible deadline inside a race-instrumented suite that saturates every
// core. The budget's own behavior is asserted separately, with a grace chosen to
// expire.
const durableCommitGrace = 30 * time.Second

// cancellingLLM streams its chunks and then cancels the turn exactly the way a
// user's Ctrl-C does — after the content is already on screen — reporting the
// cancellation as the stream error a real provider surfaces once its response
// body is torn down. cancelFirst cancels BEFORE the stream opens, modelling an
// interrupt that beat the provider.
type cancellingLLM struct {
	chunks      []content.Chunk
	cancel      context.CancelFunc
	cancelFirst bool
}

func (c *cancellingLLM) Invoke(ctx context.Context, req inference.Request) (*inference.Response, error) {
	return nil, errors.New("cancellingLLM.Invoke not used")
}

func (c *cancellingLLM) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	if c.cancelFirst {
		c.cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	i := 0
	next := func() (content.Chunk, error) {
		if i < len(c.chunks) {
			chunk := c.chunks[i]
			i++
			return chunk, nil
		}
		c.cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return stream.NewStreamReader(next, nil), nil
}

// cancelledStepMessage runs one step that is cancelled after the given chunks
// have streamed, and returns the assistant message the step stored.
func cancelledStepMessage(t *testing.T, chunks []content.Chunk) (*content.AIMessage, stepResult) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &cancellingLLM{chunks: chunks, cancel: cancel}
	cfg := stepConfig{req: inference.Request{Model: testModel()}, client: client, emit: func(event.Event) {}}
	res := runStep(ctx, cfg, 5, newTestStep(t, 0))
	if _, ok := res.terminal.(event.TurnInterrupted); !ok {
		t.Fatalf("terminal = %T, want event.TurnInterrupted (a cancelled turn is not a failed turn)", res.terminal)
	}
	if len(res.state.msgs) == 0 {
		return nil, res
	}
	msg, ok := res.state.msgs[0].(*content.AIMessage)
	if !ok {
		t.Fatalf("msgs[0] = %T, want *content.AIMessage", res.state.msgs[0])
	}
	return msg, res
}

// assertNotice fails unless the message's LAST block is exactly the given notice.
func assertNotice(t *testing.T, msg *content.AIMessage, want string) {
	t.Helper()
	if msg == nil || len(msg.Blocks) == 0 {
		t.Fatalf("message = %v, want a stored message ending in a notice", msg)
	}
	last, ok := msg.Blocks[len(msg.Blocks)-1].(*content.TextBlock)
	if !ok {
		t.Fatalf("last block = %T, want *content.TextBlock carrying the notice", msg.Blocks[len(msg.Blocks)-1])
	}
	if last.Text != want {
		t.Fatalf("last block text = %q, want %q", last.Text, want)
	}
}

// TestRunStepCancellation pins the step-level half: a turn cancelled mid-stream
// keeps the same safe prefix a provider failure keeps, ends on the CANCELLATION
// terminal, and says it was interrupted rather than that the stream failed.
func TestRunStepCancellation(t *testing.T) {
	t.Parallel()

	t.Run("cancelled mid-stream keeps the text the user already watched arrive", func(t *testing.T) {
		t.Parallel()
		msg, res := cancelledStepMessage(t, []content.Chunk{textChunk("the answer is "), textChunk("forty-t")})
		if msg == nil {
			t.Fatal("no assistant message stored; the streamed text was discarded on cancel")
		}
		if res.state.status != stepTruncated {
			t.Errorf("status = %v, want stepTruncated", res.state.status)
		}
		if got := blockKinds(msg.Blocks); len(got) != 2 || got[0] != "text" {
			t.Fatalf("blocks = %v, want [text text(notice)]", got)
		}
		if text := msg.Blocks[0].(*content.TextBlock).Text; text != "the answer is forty-t" {
			t.Errorf("stored text = %q, want the full streamed prefix", text)
		}
	})

	t.Run("the notice says interrupted, not failed", func(t *testing.T) {
		t.Parallel()
		msg, _ := cancelledStepMessage(t, []content.Chunk{textChunk("half an ans")})
		assertNotice(t, msg, InterruptedResponseNotice)
		if InterruptedResponseNotice == TruncatedResponseNotice {
			t.Fatal("the cancelled notice must differ from the stream-failure notice")
		}
		if strings.Contains(InterruptedResponseNotice, "failed") {
			t.Errorf("interrupted notice %q tells the reader the stream failed", InterruptedResponseNotice)
		}
	})

	t.Run("a provider failure still says the stream failed", func(t *testing.T) {
		t.Parallel()
		msg, _ := truncatedStepMessage(t, []content.Chunk{textChunk("half an ans")})
		assertNotice(t, msg, TruncatedResponseNotice)
	})

	t.Run("cancelled before anything decoded stores no message", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := &cancellingLLM{cancel: cancel, cancelFirst: true}
		cfg := stepConfig{req: inference.Request{Model: testModel()}, client: client, emit: func(event.Event) {}}
		res := runStep(ctx, cfg, 5, newTestStep(t, 0))
		if _, ok := res.terminal.(event.TurnInterrupted); !ok {
			t.Fatalf("terminal = %T, want event.TurnInterrupted", res.terminal)
		}
		if len(res.state.msgs) != 0 {
			t.Fatalf("stored messages = %v, want none (nothing was decoded)", res.state.msgs)
		}
		if res.state.status != stepFailed {
			t.Errorf("status = %v, want stepFailed", res.state.status)
		}
	})

	t.Run("cancelled mid tool_use drops the partial call and keeps the text", func(t *testing.T) {
		t.Parallel()
		msg, _ := cancelledStepMessage(t, []content.Chunk{
			textChunk("looking that up"),
			toolUseChunk(0, "call-1", "Echo", `{"path":"/etc/pas`),
		})
		if msg == nil {
			t.Fatal("no assistant message stored")
		}
		for _, kind := range blockKinds(msg.Blocks) {
			if kind == "tool_use" {
				t.Fatalf("blocks = %v, want no tool_use: a half-decoded call must never be stored", blockKinds(msg.Blocks))
			}
		}
		assertNotice(t, msg, InterruptedResponseNotice)
	})
}

// TestRunTurnCancellationCommits is the end-to-end guarantee for the cancelled
// half: the commit handshake is deliberately ctx-cancellable, so the turn must
// re-run it DETACHED from the cancelled turn ctx — under a bound — or the text
// the user watched arrive is lost at exactly the moment they interrupt.
func TestRunTurnCancellationCommits(t *testing.T) {
	t.Parallel()

	input := []content.Block{&content.TextBlock{Text: "hi"}}
	emptyTS := func() ToolSet { return resolveToolSetCaps(ToolSet{Access: autoApproveGate{}}) }

	t.Run("streamed text survives the interrupt and the turn still interrupts", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := &cancellingLLM{chunks: []content.Chunk{textChunk("half an ans")}, cancel: cancel}
		cfg, st, rec := newTurnFixture(input, nil, emptyTS(), client, noGateReg())
		cfg.lifetime = context.Background()
		cfg.commitGrace = durableCommitGrace

		terminal := runTurn(ctx, cfg, st)

		if _, ok := terminal.(event.TurnInterrupted); !ok {
			t.Fatalf("terminal = %T, want event.TurnInterrupted", terminal)
		}
		committed := rec.committedMsgs()
		if len(committed) != 1 {
			t.Fatalf("committed messages = %d, want 1 (the interrupted assistant message)", len(committed))
		}
		msg, ok := committed[0].(*content.AIMessage)
		if !ok {
			t.Fatalf("committed[0] = %T, want *content.AIMessage", committed[0])
		}
		if txt := msg.Blocks[0].(*content.TextBlock).Text; txt != "half an ans" {
			t.Errorf("committed text = %q, want the streamed prefix", txt)
		}
		assertNotice(t, msg, InterruptedResponseNotice)
		// Exactly one commit and one StepDone: the detached retry must not race the
		// ordinary path into a double commit.
		if got := len(rec.commits); got != 1 {
			t.Errorf("commit count = %d, want exactly 1", got)
		}
		if got := len(stepDones(rec.events())); got != 1 {
			t.Errorf("StepDone count = %d, want exactly 1", got)
		}
	})

	t.Run("an interrupted tool call reaches neither the runner nor committed history", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		echo := &echoTool{name: "Echo", output: "ran"}
		client := &cancellingLLM{
			chunks: []content.Chunk{
				textChunk("I'll read that file"),
				toolUseChunk(0, "call-1", "Echo", `{"path":"/etc/sha`),
			},
			cancel: cancel,
		}
		ts := agenticToolSet([]tool.InvokableTool{echo}, 25, 100)
		cfg, st, rec := newTurnFixture(input, nil, ts, client, noGateReg())
		cfg.lifetime = context.Background()
		cfg.commitGrace = durableCommitGrace

		terminal := runTurn(ctx, cfg, st)

		if _, ok := terminal.(event.TurnInterrupted); !ok {
			t.Fatalf("terminal = %T, want event.TurnInterrupted", terminal)
		}
		if runs := echo.runCount(); runs != 0 {
			t.Errorf("tool runs = %d, want 0 (an interrupted call must never execute)", runs)
		}
		committed := rec.committedMsgs()
		if calls := countToolUseBlocks(committed); calls != 0 {
			t.Errorf("committed tool_use blocks = %d, want 0", calls)
		}
		history := committedHistory(nil, initialUser(st), rec)
		if _, err := validateCompactionTailTranscript(history, 0, make([]content.TokenCount, len(history))); err != nil {
			t.Errorf("committed history rejected by the compaction tail validator: %v", err)
		}
	})

	t.Run("cancelled before anything decoded commits nothing", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := &cancellingLLM{cancel: cancel, cancelFirst: true}
		cfg, st, rec := newTurnFixture(input, nil, emptyTS(), client, noGateReg())
		cfg.lifetime = context.Background()

		terminal := runTurn(ctx, cfg, st)

		if _, ok := terminal.(event.TurnInterrupted); !ok {
			t.Fatalf("terminal = %T, want event.TurnInterrupted", terminal)
		}
		if len(rec.commits) != 0 {
			t.Errorf("commit count = %d, want 0 (nothing was decoded, so there is no empty message to store)", len(rec.commits))
		}
	})

	t.Run("a store that rejects the write loses the content but does not hang", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := &cancellingLLM{chunks: []content.Chunk{textChunk("half an ans")}, cancel: cancel}
		cfg, st, rec := newTurnFixture(input, nil, emptyTS(), client, noGateReg())
		cfg.lifetime = context.Background()
		cfg.commitGrace = durableCommitGrace
		rec.commitErr = errors.New("session store closed")

		done := make(chan event.Event, 1)
		go func() { done <- runTurn(ctx, cfg, st) }()
		select {
		case terminal := <-done:
			if _, ok := terminal.(event.TurnInterrupted); !ok {
				t.Fatalf("terminal = %T, want event.TurnInterrupted", terminal)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("runTurn did not return after a rejected commit")
		}
		if len(rec.committedMsgs()) != 0 {
			t.Errorf("committed messages = %d, want 0 (the store refused the write)", len(rec.committedMsgs()))
		}
	})

	t.Run("the detached commit is bounded when the actor never answers", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := &cancellingLLM{chunks: []content.Chunk{textChunk("half an ans")}, cancel: cancel}
		cfg, st, _ := newTurnFixture(input, nil, emptyTS(), client, noGateReg())
		cfg.lifetime = context.Background()
		// A grace chosen to expire: the question here is whether the wait ENDS, not
		// how long the production budget is, so it is stated rather than raced.
		grace := 100 * time.Millisecond
		cfg.commitGrace = grace
		// An actor that is not reading the commit channel: the ONLY thing that frees
		// the turn goroutine is the grace bound.
		cfg.commit = func(cctx context.Context, tc turnCommit) error {
			<-cctx.Done()
			return cctx.Err()
		}

		start := time.Now()
		done := make(chan event.Event, 1)
		go func() { done <- runTurn(ctx, cfg, st) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("runTurn wedged: the detached commit is not bounded")
		}
		elapsed := time.Since(start)
		if elapsed < grace {
			t.Errorf("elapsed = %v, want at least the grace %v (the wait must actually be attempted)", elapsed, grace)
		}
		if elapsed > 5*time.Second {
			t.Errorf("elapsed = %v, want the wait to end at the grace %v", elapsed, grace)
		}
	})

	t.Run("an unset grace falls back to the production budget", func(t *testing.T) {
		t.Parallel()
		// Every turnConfig that omits a grace — production and fixtures alike — must
		// still be bounded; an unbounded fallback would turn Ctrl-C into a hang.
		for _, unset := range []time.Duration{0, -time.Second} {
			if got := resolveTruncatedCommitGrace(unset); got != defaultTruncatedCommitGrace {
				t.Errorf("resolveTruncatedCommitGrace(%v) = %v, want %v", unset, got, defaultTruncatedCommitGrace)
			}
		}
		if got := resolveTruncatedCommitGrace(5 * time.Millisecond); got != 5*time.Millisecond {
			t.Errorf("resolveTruncatedCommitGrace(5ms) = %v, want it honored", got)
		}
		// The budget is a latency allowance on an interrupt, so it stays sub-second:
		// a Ctrl-C that visibly hangs is a worse defect than a lost prefix.
		if defaultTruncatedCommitGrace > 500*time.Millisecond {
			t.Errorf("default grace = %v; an interrupt must not pause that long", defaultTruncatedCommitGrace)
		}
	})

	t.Run("a dead loop skips the detached commit instead of paying the bound", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		deadLoop, killLoop := context.WithCancel(context.Background())
		killLoop()
		client := &cancellingLLM{chunks: []content.Chunk{textChunk("half an ans")}, cancel: cancel}
		cfg, st, rec := newTurnFixture(input, nil, emptyTS(), client, noGateReg())
		cfg.lifetime = deadLoop
		// A grace long enough that PAYING it would be unmistakable: the assertion is
		// that a dead loop is not waited for at all.
		cfg.commitGrace = durableCommitGrace
		cfg.commit = func(cctx context.Context, tc turnCommit) error {
			<-cctx.Done()
			return cctx.Err()
		}

		start := time.Now()
		terminal := runTurn(ctx, cfg, st)
		elapsed := time.Since(start)

		if _, ok := terminal.(event.TurnInterrupted); !ok {
			t.Fatalf("terminal = %T, want event.TurnInterrupted", terminal)
		}
		if elapsed >= 5*time.Second {
			t.Errorf("elapsed = %v, want an immediate return: a dead loop can never accept the commit", elapsed)
		}
		if len(rec.committedMsgs()) != 0 {
			t.Errorf("committed messages = %d, want 0", len(rec.committedMsgs()))
		}
	})
}

// streamedLLM streams its chunks and then reports, by closing `folded`, that the
// runtime has asked for the NEXT chunk — which it can only do after folding the
// previous one into the step's accumulator. After that it blocks until the turn
// is cancelled and surfaces the cancellation as its stream error, the way a real
// provider does once its response body is torn down.
//
// The distinction is the whole point of these tests. chunkProcessor emits the
// live TokenDelta BEFORE folding, so a test that interrupts as soon as a
// TokenDelta appears on the fan-in can land in the gap between "the user saw the
// text" and "the runtime is holding it" — and would then be asserting that an
// EMPTY accumulator commits nothing, while claiming to prove the opposite.
type streamedLLM struct {
	chunks []content.Chunk
	folded chan struct{}
	once   sync.Once
}

func newStreamedLLM(chunks ...content.Chunk) *streamedLLM {
	return &streamedLLM{chunks: chunks, folded: make(chan struct{})}
}

func (s *streamedLLM) Invoke(ctx context.Context, req inference.Request) (*inference.Response, error) {
	return nil, errors.New("streamedLLM.Invoke not used")
}

func (s *streamedLLM) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	i := 0
	next := func() (content.Chunk, error) {
		if i < len(s.chunks) {
			chunk := s.chunks[i]
			i++
			return chunk, nil
		}
		s.once.Do(func() { close(s.folded) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return stream.NewStreamReader(next, nil), nil
}

// assertTokenDeltaSeen pins the premise of every test below: the text was on the
// user's screen before the turn was stopped. Without it these tests could pass
// while committing content the user never actually saw stream in.
func assertTokenDeltaSeen(t *testing.T, rec *recordingPublisher) {
	t.Helper()
	for _, ev := range rec.events() {
		if _, ok := ev.(event.TokenDelta); ok {
			return
		}
	}
	t.Fatal("no TokenDelta was published; the user never saw the text")
}

// awaitFolded blocks until every streamed chunk is in the step's accumulator.
func awaitFolded(t *testing.T, client *streamedLLM) {
	t.Helper()
	select {
	case <-client.folded:
	case <-time.After(2 * time.Second):
		t.Fatal("the streamed chunks were never folded into the step")
	}
}

// newDurableCommitLoop starts a loop whose detached-commit grace is far too long
// to expire. The loop-level tests below ask whether a cancelled turn's partial
// reply reaches committed history AT ALL; making the budget unreachable keeps
// that question independent of how loaded the machine is when a race-instrumented
// suite runs them. The budget itself is asserted at the turn level, with a grace
// chosen to expire.
func newDurableCommitLoop(t *testing.T, client inference.Client) (*Loop, *recordingPublisher) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rec := &recordingPublisher{}
	l, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, rec, runtimeConfig{
		Client:               client,
		Model:                testModel(),
		DrainTimeout:         200 * time.Millisecond,
		truncatedCommitGrace: durableCommitGrace,
	})
	if err != nil {
		t.Fatalf("newWithConfig: %v", err)
	}
	return l, rec
}

// TestLoopInterruptCommitsPartialReply drives the REAL actor: a user interrupts a
// turn after the model's words are already on the fan-in, and the committed
// history must still hold them. This is the case the previous fix documented but
// could not close, so it is asserted against the production handshake rather than
// a fixture.
func TestLoopInterruptCommitsPartialReply(t *testing.T) {
	t.Parallel()

	client := newStreamedLLM(textChunk("half an ans"))
	l, rec := newDurableCommitLoop(t, client)
	startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "hi"}})
	awaitFolded(t, client)
	assertTokenDeltaSeen(t, rec)

	ack := make(chan bool, 1)
	l.Commands <- command.Interrupt{Header: command.Header{CommandID: mustID(t)}, Ack: ack}
	if !<-ack {
		t.Fatal("Interrupt did not cancel the running turn")
	}

	terminal := drainToTerminal(t, rec)
	if _, ok := terminal.(event.TurnInterrupted); !ok {
		t.Fatalf("terminal = %T, want event.TurnInterrupted", terminal)
	}

	msgs, _, err := l.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var stored *content.AIMessage
	for _, m := range msgs {
		if ai, ok := m.(*content.AIMessage); ok {
			stored = ai
		}
	}
	if stored == nil {
		t.Fatalf("committed history = %d messages with no assistant message; the interrupted text was lost", len(msgs))
	}
	if txt := stored.Blocks[0].(*content.TextBlock).Text; txt != "half an ans" {
		t.Errorf("committed text = %q, want the streamed prefix", txt)
	}
	assertNotice(t, stored, InterruptedResponseNotice)
	// Exactly one StepDone, and it must PRECEDE the terminal: a detached commit
	// that landed after the turn parked would grow committed history behind the
	// back of a turn that already reported it was over.
	stepDoneAt, terminalAt := -1, -1
	for i, ev := range rec.events() {
		switch ev.(type) {
		case event.StepDone:
			if stepDoneAt >= 0 {
				t.Fatalf("StepDone emitted twice: the detached commit double-committed")
			}
			stepDoneAt = i
		case event.TurnInterrupted:
			if terminalAt < 0 {
				terminalAt = i
			}
		}
	}
	if stepDoneAt < 0 {
		t.Fatal("no StepDone: the partial reply never reached committed history")
	}
	if terminalAt < 0 || stepDoneAt > terminalAt {
		t.Errorf("StepDone at %d, TurnInterrupted at %d; the commit must land before the turn parks", stepDoneAt, terminalAt)
	}
}

// TestLoopShutdownCommitsPartialReply is the shutdown peer: a graceful stop is
// still a live actor holding an open store, so the content the user watched
// arrive must survive it too.
func TestLoopShutdownCommitsPartialReply(t *testing.T) {
	t.Parallel()

	client := newStreamedLLM(textChunk("half an ans"))
	l, rec := newDurableCommitLoop(t, client)
	startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "hi"}})
	awaitFolded(t, client)
	assertTokenDeltaSeen(t, rec)

	ack := make(chan error, 1)
	l.Commands <- command.Shutdown{Header: command.Header{CommandID: mustID(t)}, Ack: ack}
	select {
	case err := <-ack:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not ack: the detached commit is holding the actor")
	}

	var stored *content.AIMessage
	for _, ev := range rec.events() {
		if sd, ok := ev.(event.StepDone); ok {
			for _, m := range sd.Messages {
				if ai, ok := m.(*content.AIMessage); ok {
					stored = ai
				}
			}
		}
	}
	if stored == nil {
		t.Fatal("no StepDone carried the partial reply; a graceful shutdown dropped what the user saw")
	}
	assertNotice(t, stored, InterruptedResponseNotice)
}

// TestLoopShutdownWithRejectingStore covers the sub-case where the durable store
// is already refusing writes as the loop stops: the content is genuinely lost (it
// cannot be persisted), but the shutdown must still ack promptly rather than
// block on a store that will never accept the write.
func TestLoopShutdownWithRejectingStore(t *testing.T) {
	t.Parallel()

	client := newStreamedLLM(textChunk("half an ans"))
	l, rec := newDurableCommitLoop(t, client)
	startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "hi"}})
	awaitFolded(t, client)
	assertTokenDeltaSeen(t, rec)
	rec.setCheckedError(errors.New("session store closed"))

	ack := make(chan error, 1)
	start := time.Now()
	l.Commands <- command.Shutdown{Header: command.Header{CommandID: mustID(t)}, Ack: ack}
	select {
	case <-ack:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not ack against a rejecting store")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("shutdown took %v; the rejected commit must not extend it", elapsed)
	}
	// The write was refused, so the content is genuinely lost — but cleanly: no
	// StepDone announcing a step that was never durably recorded.
	if got := len(stepDones(rec.events())); got != 0 {
		t.Errorf("StepDone count = %d, want 0: a refused write must not be announced", got)
	}
}

// requestText flattens every text block in a message thread, so a test can assert
// what the provider actually receives.
func requestText(msgs content.AgenticMessages) string {
	var sb strings.Builder
	for _, m := range msgs {
		blocks, ok := compactionTailMessageBlocks(m)
		if !ok {
			continue
		}
		for _, b := range blocks {
			if tb, ok := b.(*content.TextBlock); ok {
				sb.WriteString(tb.Text)
				sb.WriteString("\n")
			}
		}
	}
	return sb.String()
}
