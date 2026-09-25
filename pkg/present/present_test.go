package present_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/present"
)

func textBlock(s string) content.Block { return &content.TextBlock{Text: s} }

func TestFrameValidate(t *testing.T) {
	t.Parallel()
	many := make([]content.Block, present.MaxFrameBlocks+1)
	for i := range many {
		many[i] = textBlock("x")
	}
	tests := []struct {
		name    string
		frame   present.Frame
		wantErr bool
	}{
		{name: "empty frame", frame: present.Frame{}},
		{name: "prefix only", frame: present.Frame{Prefix: []content.Block{textBlock("[from: A]")}}},
		{name: "at block cap", frame: present.Frame{Prefix: many[:4], Suffix: many[:4]}},
		{name: "over block cap", frame: present.Frame{Prefix: many}, wantErr: true},
		{name: "at byte cap", frame: present.Frame{Prefix: []content.Block{textBlock(strings.Repeat("a", present.MaxFrameTextBytes))}}},
		{name: "over byte cap", frame: present.Frame{Prefix: []content.Block{textBlock(strings.Repeat("a", present.MaxFrameTextBytes)), textBlock("b")}}, wantErr: true},
		{name: "nil block", frame: present.Frame{Suffix: []content.Block{nil}}, wantErr: true},
		{name: "typed nil", frame: present.Frame{Prefix: []content.Block{(*content.TextBlock)(nil)}}, wantErr: true},
		{name: "non-text", frame: present.Frame{Prefix: []content.Block{&content.ThinkingBlock{Thinking: "x"}}}, wantErr: true},
		{name: "empty text", frame: present.Frame{Prefix: []content.Block{textBlock("")}}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.frame.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				var pe *present.Error
				if !errors.As(err, &pe) || pe.Kind != present.ErrorFrameInvalid {
					t.Fatalf("error = %v, want frame_invalid", err)
				}
			}
		})
	}
}

type presenterFunc func(context.Context, present.Input) (present.Frame, error)

func (f presenterFunc) Present(ctx context.Context, in present.Input) (present.Frame, error) {
	return f(ctx, in)
}

func TestRunClonesAndClassifies(t *testing.T) {
	t.Parallel()
	user := []content.Block{textBlock("Add milk")}
	principal := &sessionwire.Principal{Tenant: "acme", Subject: "user_01", Kind: sessionwire.PrincipalKindActor}
	metadata := sessionwire.MessageMetadata{"space": "family"}
	var returned content.Block
	frame, err := present.Run(context.Background(), presenterFunc(func(_ context.Context, in present.Input) (present.Frame, error) {
		in.Blocks[0].(*content.TextBlock).Text = "MUTATED"
		in.Principal.Subject = "mutated"
		in.Metadata["space"] = "mutated"
		returned = textBlock("[from: Alex]")
		return present.Frame{Prefix: []content.Block{returned}}, nil
	}), present.Input{Blocks: user, Principal: principal, Metadata: metadata})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if user[0].(*content.TextBlock).Text != "Add milk" || principal.Subject != "user_01" || metadata["space"] != "family" {
		t.Fatal("presenter mutated the caller's input")
	}
	returned.(*content.TextBlock).Text = "MUTATED"
	if got := frame.Prefix[0].(*content.TextBlock).Text; got != "[from: Alex]" {
		t.Fatalf("returned frame was not cloned: %q", got)
	}

	_, err = present.Run(context.Background(), presenterFunc(func(context.Context, present.Input) (present.Frame, error) {
		return present.Frame{}, errors.New("directory down")
	}), present.Input{Blocks: user})
	var pe *present.Error
	if !errors.As(err, &pe) || pe.Kind != present.ErrorPresenterFailed {
		t.Fatalf("presenter error = %v, want presenter_failed", err)
	}
	_, err = present.Run(context.Background(), presenterFunc(func(context.Context, present.Input) (present.Frame, error) {
		panic("boom")
	}), present.Input{Blocks: user})
	if !errors.As(err, &pe) || pe.Kind != present.ErrorPresenterFailed {
		t.Fatalf("panic = %v, want presenter_failed", err)
	}
	_, err = present.Run(context.Background(), presenterFunc(func(context.Context, present.Input) (present.Frame, error) {
		return present.Frame{Prefix: []content.Block{&content.ThinkingBlock{Thinking: "x"}}}, nil
	}), present.Input{Blocks: user})
	if !errors.As(err, &pe) || pe.Kind != present.ErrorFrameInvalid {
		t.Fatalf("invalid frame = %v, want frame_invalid", err)
	}
}
