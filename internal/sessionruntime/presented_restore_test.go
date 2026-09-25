package sessionruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/present"
)

func TestReofferedPresentedInputIsNotPresentedAgain(t *testing.T) {
	t.Parallel()
	store := sessionstoreOverMemstore(t)
	first := &countingPresenter{frame: present.Frame{Prefix: []content.Block{&content.TextBlock{Text: "[from: Alex]"}}}}
	lifecycle, err := newTestLifecycle(cfg(&stubLLM{chunks: []content.Chunk{textChunk("done")}}), store, WithLifecycleMessagePresenter(first))
	if err != nil {
		t.Fatal(err)
	}
	s, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	sid := s.SessionID()
	release := holdTurnStart(s)
	defer release()
	runtimeID := mustUUID()
	admitted := admittedInputFor(s, "presented-crash", runtimeID, "hi", "attempt/presented-crash")
	admitted.Principal = &sessionwire.Principal{Tenant: "acme", Subject: "u1", Kind: sessionwire.PrincipalKindActor}
	admitted.Metadata = sessionwire.MessageMetadata{"source": "phone"}
	if _, err := s.ApplyRuntimeCommand(context.Background(), admitted); err != nil {
		t.Fatal(err)
	}
	if first.count() != 1 {
		t.Fatalf("first presenter calls = %d, want 1", first.count())
	}
	if got := countOpenings(t, causedEvents(t, store, sid, runtimeID)); got != 0 {
		t.Fatalf("pre-crash openings = %d, want 0", got)
	}
	crashWithoutTeardown(t, s)

	second := &countingPresenter{frame: present.Frame{Prefix: []content.Block{&content.TextBlock{Text: "CHANGED"}}}}
	successor, err := newTestLifecycle(cfg(&stubLLM{chunks: []content.Chunk{textChunk("done")}}), store, WithLifecycleMessagePresenter(second))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := successor.RestoreSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	waitForOpening(t, store, sid, runtimeID)
	if second.count() != 0 {
		t.Fatalf("restore called presenter %d times", second.count())
	}
	for _, item := range causedEvents(t, store, sid, runtimeID) {
		started, ok := item.(event.TurnStarted)
		if !ok {
			continue
		}
		if len(started.Message.Blocks) != 2 || started.Message.Blocks[0].(*content.TextBlock).Text != "[from: Alex]" {
			t.Fatalf("restored rendering = %#v", started.Message.Blocks)
		}
		if started.Input == nil || started.Input.Principal == nil || started.Input.Principal.Subject != "u1" || started.Input.Metadata["source"] != "phone" || started.Input.Prefix != 1 {
			t.Fatalf("restored attribution = %#v", started.Input)
		}
		got, err := content.MarshalBlocks(event.UserBlocks(started.Message, started.Input))
		if err != nil {
			t.Fatal(err)
		}
		want, err := content.MarshalBlocks(admitted.Blocks)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("source blocks drifted: %s != %s", got, want)
		}
		return
	}
	t.Fatal("restored turn opening not found")
}

func TestFoldReproducesPresentedMessagesVerbatim(t *testing.T) {
	t.Parallel()
	msg := &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{
		&content.TextBlock{Text: "[from: A]"}, &content.TextBlock{Text: "hi"},
	}}}
	started := event.TurnStarted{Message: msg, Input: &event.MessageInput{Prefix: 1}}
	got := foldLoop([]event.Event{started}).Msgs
	wantJSON, err := json.Marshal(content.AgenticMessages{msg})
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Fatalf("fold drifted:\n%s\n%s", wantJSON, gotJSON)
	}
}
