package sessionruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/present"
	sessionapi "github.com/looprig/harness/pkg/session"
)

func TestSubmitInputPresentsAndAttributes(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	presenter := &countingPresenter{frame: present.Frame{Prefix: []content.Block{&content.TextBlock{Text: "[from: Alex]"}}}}
	WithMessagePresenter(presenter)(f.session)
	var submitter sessionapi.InputSubmitter = f.session
	principal := &sessionwire.Principal{Tenant: "acme", Subject: "u1", Kind: sessionwire.PrincipalKindActor}
	id, err := submitter.SubmitInput(context.Background(), sessionapi.Input{
		Blocks: []content.Block{&content.TextBlock{Text: "hi"}}, Principal: principal,
		Metadata: sessionwire.MessageMetadata{"space": "family"},
	})
	if err != nil || id.IsZero() {
		t.Fatalf("SubmitInput = %v, %v", id, err)
	}
	input := f.drainOne(t).(command.UserInput)
	if input.Presented == nil || input.Principal == nil || input.Principal.Subject != "u1" || presenter.first(t).Principal.Subject != "u1" {
		t.Fatalf("submitted input = %#v", input)
	}
}

func TestSubmitInputRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		input              sessionapi.Input
		presenter          present.Presenter
		wantPresenterError bool
	}{
		{name: "invalid metadata", input: sessionapi.Input{Blocks: []content.Block{&content.TextBlock{Text: "x"}}, Metadata: sessionwire.MessageMetadata{"looprig_a": "b"}}},
		{name: "invalid principal", input: sessionapi.Input{Blocks: []content.Block{&content.TextBlock{Text: "x"}}, Principal: &sessionwire.Principal{}}},
		{name: "no blocks"},
		{name: "presenter failure", input: sessionapi.Input{Blocks: []content.Block{&content.TextBlock{Text: "x"}}}, presenter: &countingPresenter{err: errors.New("x")}, wantPresenterError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRuntimeCommandFixture(t)
			if tc.presenter != nil {
				WithMessagePresenter(tc.presenter)(f.session)
			}
			id, err := f.session.SubmitInput(context.Background(), tc.input)
			if err == nil || !id.IsZero() {
				t.Fatalf("SubmitInput = %v, %v", id, err)
			}
			var presenterError *present.Error
			if tc.wantPresenterError && !errors.As(err, &presenterError) {
				t.Fatalf("error = %v, want *present.Error", err)
			}
			f.requireNoCommand(t, "refused submit")
		})
	}
}

func TestPlainSubmitIsPresentedWithNilPrincipal(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	presenter := &countingPresenter{}
	WithMessagePresenter(presenter)(f.session)
	if _, err := f.session.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	f.drainOne(t)
	if presenter.count() != 1 || presenter.first(t).Principal != nil {
		t.Fatalf("plain submit presenter calls = %d", presenter.count())
	}
}
