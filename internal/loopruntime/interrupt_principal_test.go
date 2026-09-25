package loopruntime

import (
	"testing"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
)

func TestInterruptPrincipalLandsOnTurnInterrupted(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	startTurn(t, l, rec, nil)
	ack := make(chan bool, 1)
	principal := &sessionwire.Principal{Tenant: "acme", Subject: "parent", Kind: sessionwire.PrincipalKindActor}
	l.Commands <- command.Interrupt{Ack: ack, Principal: principal}
	if !<-ack {
		t.Fatal("interrupt ack false")
	}
	terminal, ok := drainToTerminal(t, rec).(event.TurnInterrupted)
	if !ok || terminal.Principal == nil || terminal.Principal.Subject != "parent" {
		t.Fatalf("terminal = %#v", terminal)
	}
}

func TestUnattributedInterruptDoesNotInheritPrincipal(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	startTurn(t, l, rec, nil)
	ack := make(chan bool, 1)
	l.Commands <- command.Interrupt{Ack: ack, Principal: &sessionwire.Principal{Tenant: "acme", Subject: "first", Kind: sessionwire.PrincipalKindActor}}
	if !<-ack {
		t.Fatal("first interrupt ack false")
	}
	drainToTerminal(t, rec)
	from := len(rec.events())
	startTurn(t, l, rec, nil)
	l.Commands <- command.Interrupt{Ack: ack}
	if !<-ack {
		t.Fatal("second interrupt ack false")
	}
	terminal, ok := awaitTerminalAfter(t, rec, from).(event.TurnInterrupted)
	if !ok || terminal.Principal != nil {
		t.Fatalf("second terminal = %#v", terminal)
	}
}
