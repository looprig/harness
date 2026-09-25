package sessionruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/present"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
)

func prefeatureGolden(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "compat", "testdata", "pre_v0410", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(body)
}

func TestPreFeatureJournalRestoresIdenticallyWithAndWithoutAPresenter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := sessionstoreOverMemstore(t)
	definition := cfg(&stubLLM{chunks: []content.Chunk{textChunk("done")}})
	sid, lid, turnID, stepID, commandID := mustUUID(), mustUUID(), mustUUID(), mustUUID(), mustUUID()
	bound, err := definition.Bind(ctx, tool.Bindings{SessionID: sid, LoopID: lid})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := testFingerprintProvider(bound)
	lease := mustAcquireLease(t, store, sid)
	writer, err := store.OpenJournal(ctx, sid, lease)
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(sid, hub.WithAppender(journal.NewJournalEventAppender(writer)), hub.WithFactory(testFactory()))
	stamp := &eventStamper{}
	stamp.stamp(t, ctx, h, event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}}, Config: fingerprint})
	stamp.stamp(t, ctx, h, event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid}, AgentName: definition.Name()}, Runtime: runtimeFromFingerprint(fingerprint)})

	decodedCommand, err := command.UnmarshalCommand(prefeatureGolden(t, "user_input.json"))
	if err != nil {
		t.Fatal(err)
	}
	intent := decodedCommand.(command.UserInput)
	intent.CommandID = commandID
	if err := journal.NewJournalCommandAppender(writer).AppendCommand(ctx, journal.NewCommandRecord(sid, lid, intent)); err != nil {
		t.Fatal(err)
	}
	decodedStarted, err := event.UnmarshalEvent(prefeatureGolden(t, "turn_started.json"))
	if err != nil {
		t.Fatal(err)
	}
	started := decodedStarted.(event.TurnStarted)
	started.Header.Coordinates = identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: turnID}
	started.Cause.CommandID = commandID
	stamp.stamp(t, ctx, h, started)
	started.EventID = uuid.UUID{0xE0, stamp.n}
	started.CreatedAt = time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	decodedStep, err := event.UnmarshalEvent(prefeatureGolden(t, "step_done.json"))
	if err != nil {
		t.Fatal(err)
	}
	step := decodedStep.(event.StepDone)
	step.Header.Coordinates = identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: turnID, StepID: stepID}
	step.Cause.CommandID = commandID
	stamp.stamp(t, ctx, h, step)
	step.EventID = uuid.UUID{0xE0, stamp.n}
	step.CreatedAt = time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	handOver(t, lease)

	wantMessages, err := json.Marshal(foldLoop([]event.Event{started, step}).Msgs)
	if err != nil {
		t.Fatal(err)
	}
	var snapshots [][]byte
	presenter := &countingPresenter{frame: present.Frame{Prefix: []content.Block{&content.TextBlock{Text: "MUST NOT RENDER"}}}}
	for _, options := range [][]Option{nil, {WithMessagePresenter(presenter)}} {
		restored, err := restoreTestSession(ctx, definition, sid, store, options...)
		if err != nil {
			t.Fatalf("RestoreTopology: %v", err)
		}
		messages, _ := restoredSnapshot(t, restored)
		encoded, err := json.Marshal(messages)
		if err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, encoded)
		if err := restored.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(snapshots[0], snapshots[1]) || !bytes.Equal(snapshots[0], wantMessages) {
		t.Fatalf("restore drifted:\nfirst %s\nsecond %s\ngolden %s", snapshots[0], snapshots[1], wantMessages)
	}
	if presenter.count() != 0 {
		t.Fatalf("restore called presenter %d times", presenter.count())
	}

	// The store-level replayer must preserve the original command and message
	// bodies, not merely reconstruct an equivalent in-memory history.
	replayer, err := store.OpenInternalRecordReplayer(sid, sessionstore.ReplayRequest{})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := replayer.Open(ctx, journal.ReplayRequest{SessionID: sid, From: journal.Beginning()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cursor.Close() }()
	wantIntent, _ := command.MarshalCommand(intent)
	wantStarted, _ := event.MarshalEvent(started)
	wantStep, _ := event.MarshalEvent(step)
	seen := make(map[string]bool)
	for {
		record, _, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch typed := record.(type) {
		case journal.CommandRecord:
			if cmd, ok := typed.Command().(command.UserInput); ok && cmd.CommandID == commandID {
				body, _ := command.MarshalCommand(cmd)
				if !bytes.Equal(body, wantIntent) {
					t.Fatalf("intent drifted: %s != %s", body, wantIntent)
				}
				seen["intent"] = true
			}
		case journal.EventRecord:
			switch value := typed.Event().(type) {
			case event.TurnStarted:
				if value.Cause.CommandID == commandID {
					body, _ := event.MarshalEvent(value)
					if !bytes.Equal(body, wantStarted) {
						t.Fatalf("TurnStarted drifted: %s != %s", body, wantStarted)
					}
					seen["started"] = true
				}
			case event.StepDone:
				if value.Cause.CommandID == commandID {
					body, _ := event.MarshalEvent(value)
					if !bytes.Equal(body, wantStep) {
						t.Fatalf("StepDone drifted: %s != %s", body, wantStep)
					}
					seen["step"] = true
				}
			}
		}
	}
	for _, kind := range []string{"intent", "started", "step"} {
		if !seen[kind] {
			t.Fatalf("store replay omitted %s", kind)
		}
	}
}
