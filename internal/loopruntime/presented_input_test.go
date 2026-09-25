package loopruntime

import (
	"context"
	"testing"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
)

func TestPresentedInputComposesTheModelMessage(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	l, rec, _ := newLoop(t, llm)
	id := mustID(t)
	principal := &sessionwire.Principal{Tenant: "acme", Subject: "u1", Kind: sessionwire.PrincipalKindActor}
	l.Commands <- command.UserInput{
		Header:    command.Header{CommandID: id, Agency: identity.AgencyUser},
		Blocks:    textBlocks("Add milk"),
		Principal: principal,
		Metadata:  sessionwire.MessageMetadata{"space": "family"},
		Presented: &command.Presented{Prefix: textBlocks("[from: Alex]"), Suffix: textBlocks("(sent from phone)")},
	}
	started, ok := awaitReply(t, rec, id).(event.TurnStarted)
	if !ok {
		t.Fatal("input did not start a turn")
	}
	blocks := started.Message.Blocks
	if len(blocks) != 3 || blocks[0].(*content.TextBlock).Text != "[from: Alex]" || blocks[2].(*content.TextBlock).Text != "(sent from phone)" {
		t.Fatalf("assembled message = %#v", blocks)
	}
	if started.Input == nil || started.Input.Prefix != 1 || started.Input.Suffix != 1 || started.Input.Principal.Subject != "u1" || started.Input.Metadata["space"] != "family" {
		t.Fatalf("Input = %#v", started.Input)
	}
	if got := event.UserBlocks(started.Message, started.Input); len(got) != 1 || got[0].(*content.TextBlock).Text != "Add milk" {
		t.Fatalf("user blocks = %#v", got)
	}
	if _, ok := drainToTerminal(t, rec).(event.TurnDone); !ok {
		t.Fatal("turn did not complete")
	}
	req := llm.lastReq()
	last := req.Messages[len(req.Messages)-1].(*content.UserMessage)
	if len(last.Blocks) != 3 {
		t.Fatalf("model saw %d blocks, want 3", len(last.Blocks))
	}
}

func TestPresentedInputOwnsCommandAndEventGraphs(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("ok")}})
	id := mustID(t)
	cmd := command.UserInput{
		Header:    command.Header{CommandID: id, Agency: identity.AgencyUser},
		Blocks:    textBlocks("source"),
		Principal: &sessionwire.Principal{Tenant: "acme", Subject: "u1", Kind: sessionwire.PrincipalKindActor},
		Metadata:  sessionwire.MessageMetadata{"source": "test"},
		Presented: &command.Presented{Prefix: textBlocks("prefix")},
	}
	l.Commands <- cmd
	started, ok := awaitReply(t, rec, id).(event.TurnStarted)
	if !ok {
		t.Fatal("input did not start a turn")
	}
	drainToTerminal(t, rec)
	cmd.Presented.Prefix[0].(*content.TextBlock).Text = "changed prefix"
	cmd.Principal.Subject = "changed subject"
	cmd.Metadata["source"] = "changed metadata"
	if started.Message.Blocks[0].(*content.TextBlock).Text != "prefix" || started.Input.Principal.Subject != "u1" || started.Input.Metadata["source"] != "test" {
		t.Fatalf("published event changed with command: %#v", started)
	}
	started.Message.Blocks[0].(*content.TextBlock).Text = "changed event"
	snapshot, _, err := l.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot[0].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text; got != "prefix" {
		t.Fatalf("history prefix = %q, want prefix", got)
	}
}

func TestPresentedInputSurvivesFold(t *testing.T) {
	t.Parallel()
	blocking := newBlockingTool()
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{toolUseChunk(0, "id-1", "Block", `{}`)},
		{textChunk("final")},
	}}
	l, rec := newFoldLoop(t, client, agenticToolSet([]tool.InvokableTool{blocking}, 25, 100))
	startTurn(t, l, rec, textBlocks("first"))
	<-blocking.started
	id := mustID(t)
	l.Commands <- command.UserInput{
		Header:    command.Header{CommandID: id, Agency: identity.AgencyUser},
		Blocks:    textBlocks("source"),
		Metadata:  sessionwire.MessageMetadata{"source": "test"},
		Presented: &command.Presented{Prefix: textBlocks("prefix")},
	}
	if _, ok := awaitReply(t, rec, id).(event.InputQueued); !ok {
		t.Fatal("presented input was not queued")
	}
	close(blocking.release)
	blockUntilEvents(t, rec, func(events []event.Event) bool {
		for _, item := range events {
			fold, ok := item.(event.TurnFoldedInto)
			if !ok || fold.Cause.CommandID != id {
				continue
			}
			if fold.Input == nil || fold.Input.Prefix != 1 || fold.Input.Metadata["source"] != "test" {
				t.Errorf("fold input = %#v", fold.Input)
			}
			if len(fold.Message.Blocks) != 2 || fold.Message.Blocks[0].(*content.TextBlock).Text != "prefix" {
				t.Errorf("fold message = %#v", fold.Message.Blocks)
			}
			return true
		}
		return false
	})
}

func TestPresentedInputSurvivesCancellation(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	startTurn(t, l, rec, textBlocks("first"))
	id := mustID(t)
	l.Commands <- command.UserInput{
		Header:    command.Header{CommandID: id, Agency: identity.AgencyUser},
		Blocks:    textBlocks("source"),
		Metadata:  sessionwire.MessageMetadata{"source": "test"},
		Presented: &command.Presented{Suffix: textBlocks("suffix")},
	}
	if _, ok := awaitReply(t, rec, id).(event.InputQueued); !ok {
		t.Fatal("presented input was not queued")
	}
	cancelQueuedInput(t, l, id)
	blockUntilEvents(t, rec, func(events []event.Event) bool {
		for _, item := range events {
			cancelled, ok := item.(event.InputCancelled)
			if !ok || cancelled.Cause.CommandID != id {
				continue
			}
			if cancelled.Input == nil || cancelled.Input.Suffix != 1 || cancelled.Input.Metadata["source"] != "test" {
				t.Errorf("cancelled input = %#v", cancelled.Input)
			}
			if len(cancelled.Message.Blocks) != 2 || cancelled.Message.Blocks[1].(*content.TextBlock).Text != "suffix" {
				t.Errorf("cancelled message = %#v", cancelled.Message.Blocks)
			}
			return true
		}
		return false
	})
}

func TestMachineInputHasNoMessageAttribution(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("ok")}})
	id := mustID(t)
	l.Commands <- command.SubagentResult{
		Coordinates: identity.Coordinates{LoopID: mustID(t)},
		Header:      command.Header{CommandID: id, Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: mustID(t)}}},
		Blocks:      textBlocks("machine"),
	}
	started, ok := awaitReply(t, rec, id).(event.TurnStarted)
	if !ok || started.Input != nil {
		t.Fatalf("machine outcome = %#v", started)
	}
}
