package sessionruntime

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/present"
	"github.com/looprig/harness/pkg/runtimecommand"
)

type countingPresenter struct {
	mu    sync.Mutex
	calls []present.Input
	frame present.Frame
	err   error
}

func (p *countingPresenter) Present(_ context.Context, in present.Input) (present.Frame, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, in)
	return p.frame, p.err
}

func (p *countingPresenter) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *countingPresenter) first(t *testing.T) present.Input {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) == 0 {
		t.Fatal("presenter was never called")
	}
	return p.calls[0]
}

func TestAdmittedInputIsPresentedOnceAndCarriesAttribution(t *testing.T) {
	t.Parallel()
	fixture := newRuntimeCommandFixture(t)
	p := &countingPresenter{frame: present.Frame{Prefix: []content.Block{&content.TextBlock{Text: "[from: Alex]"}}}}
	WithMessagePresenter(p)(fixture.session)
	admitted := fixture.admittedInput("cmd-1", mustUUID(), "Add milk")
	admitted.AttemptID = "attempt-1"
	admitted.Principal = &sessionwire.Principal{Tenant: "acme", Subject: "user_01", Kind: sessionwire.PrincipalKindActor}
	admitted.Metadata = sessionwire.MessageMetadata{"space": "family"}
	if _, err := fixture.session.ApplyRuntimeCommand(context.Background(), admitted); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	input := fixture.drainOne(t).(command.UserInput)
	if got := p.count(); got != 1 {
		t.Fatalf("presenter calls = %d, want 1", got)
	}
	call := p.first(t)
	if call.Kind != runtimecommand.KindInput || call.Principal.Subject != "user_01" || call.Metadata["space"] != "family" {
		t.Fatalf("presenter input = %#v", call)
	}
	if input.Presented == nil || len(input.Presented.Prefix) != 1 || input.Principal == nil || input.Metadata["space"] != "family" {
		t.Fatalf("dispatched input lacks attribution: %#v", input)
	}
	if got := input.Blocks[0].(*content.TextBlock).Text; got != "Add milk" {
		t.Fatalf("user blocks modified: %q", got)
	}
	if disposition, err := fixture.session.ApplyRuntimeCommand(context.Background(), admitted); err != nil || !disposition.Duplicate {
		t.Fatalf("redelivery = %+v, %v", disposition, err)
	}
	if got := p.count(); got != 1 {
		t.Fatalf("redelivery called presenter: %d calls", got)
	}
}

func TestAdmittedInputWithoutPresenterKeepsAttribution(t *testing.T) {
	t.Parallel()
	fixture := newRuntimeCommandFixture(t)
	admitted := fixture.admittedInput("cmd-1", mustUUID(), "hi")
	admitted.Principal = &sessionwire.Principal{Tenant: "acme", Subject: "u1", Kind: sessionwire.PrincipalKindActor}
	admitted.Metadata = sessionwire.MessageMetadata{"space": "family"}
	if _, err := fixture.session.ApplyRuntimeCommand(context.Background(), admitted); err != nil {
		t.Fatal(err)
	}
	input := fixture.drainOne(t).(command.UserInput)
	if input.Presented != nil || input.Principal == nil || input.Principal.Subject != "u1" || input.Metadata["space"] != "family" {
		t.Fatalf("attribution lost or unexpectedly presented: %#v", input)
	}
}

func TestMachineInputIsNeverPresented(t *testing.T) {
	t.Parallel()
	fixture := newRuntimeCommandFixture(t)
	p := &countingPresenter{frame: present.Frame{Prefix: []content.Block{&content.TextBlock{Text: "x"}}}}
	WithMessagePresenter(p)(fixture.session)
	if _, err := fixture.session.submitToLoop(context.Background(), fixture.session.activeLoopID,
		[]content.Block{&content.TextBlock{Text: "hand-back"}}, identity.AgencyMachine, false); err != nil {
		t.Fatal(err)
	}
	input := fixture.drainOne(t).(command.UserInput)
	if p.count() != 0 || input.Presented != nil || input.Principal != nil || input.Metadata != nil {
		t.Fatalf("machine input was presented or attributed: calls=%d input=%#v", p.count(), input)
	}
	for _, name := range []string{"Presented", "Principal"} {
		if _, ok := reflect.TypeOf(command.SubagentResult{}).FieldByName(name); ok {
			t.Fatalf("SubagentResult acquired %s attribution", name)
		}
	}
}

func TestBareCreateIsNotPresented(t *testing.T) {
	t.Parallel()
	fixture := newRuntimeCommandFixture(t)
	p := &countingPresenter{}
	WithMessagePresenter(p)(fixture.session)
	admitted := fixture.admittedInput("create-1", mustUUID(), "unused")
	admitted.Kind = runtimecommand.KindCreate
	admitted.Blocks = nil
	admitted.AttemptID = "attempt-1"
	admitted.Principal = &sessionwire.Principal{Tenant: "acme", Subject: "u1", Kind: sessionwire.PrincipalKindActor}
	if _, err := fixture.session.ApplyRuntimeCommand(context.Background(), admitted); err != nil {
		t.Fatal(err)
	}
	fixture.requireNoCommand(t, "bare create")
	if got := p.count(); got != 0 {
		t.Fatalf("bare create called presenter %d times", got)
	}
}
