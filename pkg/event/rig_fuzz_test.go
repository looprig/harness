package event_test

import (
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

func FuzzRigEvent(f *testing.F) {
	h := event.Header{Coordinates: identity.Coordinates{SessionID: vIDForFuzz(1)}, EventID: vIDForFuzz(2)}
	seeds := []event.Event{
		event.ActiveLoopChanged{Header: h, ActiveLoopID: vIDForFuzz(3)},
		event.WorkspaceCheckpointed{Header: h, Ref: "v1:sha256:x", Consistency: event.SnapshotQuiescent, Trigger: event.SnapshotTriggerManual},
		event.WorkspaceRestored{Header: h, Ref: "v1:sha256:x"},
	}
	for _, ev := range seeds {
		if data, err := event.MarshalEvent(ev); err == nil {
			f.Add(data)
		}
	}
	f.Add([]byte(`{"type":"WorkspaceCheckpointed"}`))
	f.Add([]byte(`{"type":"WorkspaceCheckpointed","Consistency":0,"Trigger":0}`))
	f.Add([]byte(`{"type":"WorkspaceCheckpointed","consistency":1,"Consistency":2,"trigger":1}`))
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = event.UnmarshalEvent(data) })
}

func vIDForFuzz(seed byte) (id uuid.UUID) {
	for i := range id {
		id[i] = seed
	}
	return
}
