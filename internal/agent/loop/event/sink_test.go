package event

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/uuid"
)

func TestEventEnvelopeFields(t *testing.T) {
	t.Parallel()
	sid, _ := uuid.New()
	tid, _ := uuid.New()
	eid, _ := uuid.New()
	cid, _ := uuid.New()
	callid, _ := uuid.New()
	env := EventEnvelope{
		SessionID: sid, TurnID: tid, TurnIndex: 1,
		EventID: eid, CausationID: cid, CallID: callid,
		Event: TurnStarted{TurnIndex: 1},
	}
	if env.TurnID != tid || env.EventID != eid || env.CausationID != cid || env.CallID != callid {
		t.Fatal("envelope correlation fields not preserved")
	}
}
