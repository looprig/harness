package sessionruntime

import (
	"bytes"
	"context"
	"testing"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/runtimecommand"
)

func TestAdmittedInterruptJournalsItsPrincipal(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	got := make(chan command.Interrupt, 1)
	go func() {
		in := (<-f.cmds).(command.Interrupt)
		got <- in
		in.Ack <- false
	}()
	admitted := f.admittedInterrupt("int-1", mustUUID())
	admitted.Principal = &sessionwire.Principal{Tenant: "acme", Subject: "parent", Kind: sessionwire.PrincipalKindActor}
	if _, err := f.session.ApplyRuntimeCommand(context.Background(), admitted); err != nil {
		t.Fatal(err)
	}
	if in := <-got; in.Principal == nil || in.Principal.Subject != "parent" {
		t.Fatalf("interrupt principal = %#v", in.Principal)
	}
}

func TestAdmittedGateResponseStampsGateResolvedPrincipal(t *testing.T) {
	t.Parallel()
	f := newGateResponseFixture(t)
	gateID := f.openGate(t, permissionGate(), bashPayload())
	admitted := f.admittedGateResponse(mustUUID(), userResponse(gateID, string(gate.ApprovalApprove)))
	admitted.Principal = &sessionwire.Principal{Tenant: "acme", Subject: "parent", Kind: sessionwire.PrincipalKindActor}
	if _, err := f.session.ApplyRuntimeCommand(context.Background(), admitted); err != nil {
		t.Fatal(err)
	}
	resolved := readGateResolutions(t, f.runtimeCommandFixture)
	if len(resolved) != 1 || resolved[0].Principal == nil || resolved[0].Principal.Subject != "parent" {
		t.Fatalf("gate resolutions = %#v", resolved)
	}
}

func TestRespondGateLeavesPrincipalNil(t *testing.T) {
	t.Parallel()
	f := newGateResponseFixture(t)
	gateID := f.openGate(t, permissionGate(), bashPayload())
	if err := f.session.RespondGate(context.Background(), userResponse(gateID, string(gate.ApprovalApprove))); err != nil {
		t.Fatal(err)
	}
	resolved := readGateResolutions(t, f.runtimeCommandFixture)
	if len(resolved) != 1 || resolved[0].Principal != nil {
		t.Fatalf("gate resolutions = %#v", resolved)
	}
	encoded, err := event.MarshalEvent(resolved[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"principal"`)) {
		t.Fatalf("unattributed gate encoded principal: %s", encoded)
	}
}

func TestRestoreAcceptsPrincipalAndJournalsNoEffect(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	admitted := runtimecommand.Admitted{
		CommandID: "restore-1", RuntimeCommandID: mustUUID(), Kind: runtimecommand.KindRestore,
		LeaseEpoch: f.lease.Epoch(), AttemptID: "attempt-restore",
		Principal: &sessionwire.Principal{Tenant: "acme", Subject: "parent", Kind: sessionwire.PrincipalKindActor},
	}
	if _, err := f.session.ApplyRuntimeCommand(context.Background(), admitted); err != nil {
		t.Fatal(err)
	}
	if got := onlyDisposition(t, f); got.Disposition != runtimecommand.DispositionApplied {
		t.Fatalf("restore disposition = %+v", got)
	}
	f.requireNoCommand(t, "restore has no loop command")
	requireNoIntentRecord(t, f, admitted.RuntimeCommandID)
}
