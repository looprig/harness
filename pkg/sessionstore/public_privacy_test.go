package sessionstore

import (
	"bytes"
	"context"
	"testing"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	model "github.com/looprig/inference/model"
	"github.com/looprig/storage/memstore"
)

// TestJournalPublicSlotRedactsEndpointAndWorkspaceWhileReplayKeepsThem drives the
// real frame path: the PUBLIC body a viewer is served names neither the model
// endpoint nor the Host's workspace path, while the native body restore replays
// still carries both verbatim — so restore, fingerprinting and drift are unchanged.
func TestJournalPublicSlotRedactsEndpointAndWorkspaceWhileReplayKeepsThem(t *testing.T) {
	t.Parallel()
	const baseURL = "https://user:secret@gw.example/v1?key=k3y-zz"
	const physicalRoot = "exclusive:/private/var/host-7/ws"
	st, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id, loopID := newTestUUID(t), newTestUUID(t)
	lease, err := st.AcquireLease(context.Background(), id)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	j, err := st.OpenJournal(context.Background(), id, lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	committed, ok := j.(journal.CommittedPublicJournal)
	if !ok {
		t.Fatalf("journal %T does not report committed public bodies", j)
	}
	started := event.SessionStarted{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: id}, EventID: newTestUUID(t)},
		Config: event.ConfigFingerprint{ModelID: "m", WorkspaceRoot: physicalRoot},
	}
	loop := event.LoopStarted{
		Header:  event.Header{Coordinates: identity.Coordinates{SessionID: id, LoopID: loopID}, EventID: newTestUUID(t)},
		Runtime: event.ModelRuntime{Key: model.ModelKey{Provider: "p", Model: "m"}, BaseURL: baseURL},
	}
	for _, ev := range []event.Event{started, loop} {
		result, err := committed.AppendCommitted(context.Background(), journal.NewEventRecord(ev))
		if err != nil {
			t.Fatalf("AppendCommitted(%T): %v", ev, err)
		}
		for _, marker := range []string{"secret", "user", "k3y-zz", "gw.example", "/private/var", "host-7"} {
			if bytes.Contains(result.Public.Body, []byte(marker)) {
				t.Errorf("stored public body for %T leaked %q: %s", ev, marker, result.Public.Body)
			}
		}
	}

	er, err := st.OpenInternalEventReplayer(id, ReplayRequest{FromSeq: 1})
	if err != nil {
		t.Fatalf("OpenInternalEventReplayer: %v", err)
	}
	events, _ := drainEvents(t, er, journal.ReplayRequest{})
	var sawRoot, sawURL bool
	for _, ev := range events {
		switch value := ev.(type) {
		case event.SessionStarted:
			sawRoot = value.Config.WorkspaceRoot == physicalRoot
		case event.LoopStarted:
			sawURL = value.Runtime.BaseURL == baseURL
		}
	}
	if !sawRoot || !sawURL {
		t.Errorf("replay lost native configuration: workspace root kept=%v, base url kept=%v", sawRoot, sawURL)
	}
}
