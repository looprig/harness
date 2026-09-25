package command_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/identity"
)

func attributionID() uuid.UUID {
	return uuid.UUID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
}
func attributionText(s string) content.Block { return &content.TextBlock{Text: s} }

func TestUserInputAttributionRoundTrips(t *testing.T) {
	t.Parallel()
	in := command.UserInput{
		Header:    command.Header{CommandID: attributionID(), Agency: identity.AgencyUser},
		Blocks:    []content.Block{attributionText("Add milk")},
		Principal: &sessionwire.Principal{Tenant: "acme", Subject: "user_01", Kind: sessionwire.PrincipalKindActor},
		Metadata:  sessionwire.MessageMetadata{"space": "family"},
		Presented: &command.Presented{Prefix: []content.Block{attributionText("[from: Alex]")}},
	}
	body, err := command.MarshalCommand(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := command.UnmarshalCommand(body)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	again, err := command.MarshalCommand(got)
	if err != nil || !bytes.Equal(again, body) {
		t.Fatalf("not a fixed point:\n%s\n%s (%v)", body, again, err)
	}
	input := got.(command.UserInput)
	if blocks := input.ModelBlocks(); len(blocks) != 2 || blocks[0].(*content.TextBlock).Text != "[from: Alex]" {
		t.Fatalf("ModelBlocks = %#v", blocks)
	}
	meta := input.MessageInput()
	if meta == nil || meta.Prefix != 1 || meta.Suffix != 0 || meta.Principal.Subject != "user_01" || meta.Metadata["space"] != "family" {
		t.Fatalf("MessageInput = %#v", meta)
	}
}

func TestUserInputWithoutAttributionIsByteIdenticalToV0402(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"user_input.json", "interrupt.json"} {
		raw, err := os.ReadFile("../../internal/compat/testdata/pre_v0410/" + name)
		if err != nil {
			t.Fatal(err)
		}
		raw = bytes.TrimSpace(raw)
		got, err := command.UnmarshalCommand(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out, err := command.MarshalCommand(got)
		if err != nil || !bytes.Equal(out, raw) {
			t.Fatalf("%s drifted from v0.40.2:\n%s\n%s", name, raw, out)
		}
		if input, ok := got.(command.UserInput); ok && (input.Presented != nil || input.Principal != nil || input.Metadata != nil || input.MessageInput() != nil) {
			t.Fatalf("%s: old record decoded with attribution", name)
		}
	}
}

func TestUserInputAttributionValidation(t *testing.T) {
	t.Parallel()
	principal := &sessionwire.Principal{Tenant: "acme", Subject: "u", Kind: sessionwire.PrincipalKindActor}
	base := func(agency identity.Agency) command.UserInput {
		return command.UserInput{Header: command.Header{CommandID: attributionID(), Agency: agency}, Blocks: []content.Block{attributionText("x")}}
	}
	tests := []struct {
		name    string
		agency  identity.Agency
		mutate  func(*command.UserInput)
		wantErr bool
	}{
		{name: "user principal", agency: identity.AgencyUser, mutate: func(c *command.UserInput) { c.Principal = principal }},
		{name: "machine principal", agency: identity.AgencyMachine, mutate: func(c *command.UserInput) { c.Principal = principal }, wantErr: true},
		{name: "machine metadata", agency: identity.AgencyMachine, mutate: func(c *command.UserInput) { c.Metadata = sessionwire.MessageMetadata{"a": "b"} }, wantErr: true},
		{name: "machine presented", agency: identity.AgencyMachine, mutate: func(c *command.UserInput) {
			c.Presented = &command.Presented{Prefix: []content.Block{attributionText("x")}}
		}, wantErr: true},
		{name: "empty presented", agency: identity.AgencyUser, mutate: func(c *command.UserInput) { c.Presented = &command.Presented{} }, wantErr: true},
		{name: "invalid principal", agency: identity.AgencyUser, mutate: func(c *command.UserInput) { c.Principal = &sessionwire.Principal{} }, wantErr: true},
		{name: "invalid metadata", agency: identity.AgencyUser, mutate: func(c *command.UserInput) { c.Metadata = sessionwire.MessageMetadata{"looprig_x": "v"} }, wantErr: true},
		{name: "over frame cap", agency: identity.AgencyUser, mutate: func(c *command.UserInput) { c.Presented = &command.Presented{Prefix: make([]content.Block, 9)} }, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			input := base(tc.agency)
			tc.mutate(&input)
			if err := command.ValidateCommand(input); (err != nil) != tc.wantErr {
				t.Fatalf("ValidateCommand() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestInterruptCarriesPrincipal(t *testing.T) {
	t.Parallel()
	principal := &sessionwire.Principal{Tenant: "acme", Subject: "u", Kind: sessionwire.PrincipalKindActor}
	body, err := command.MarshalCommand(command.Interrupt{Header: command.Header{CommandID: attributionID(), Agency: identity.AgencyUser}, Principal: principal})
	if err != nil {
		t.Fatal(err)
	}
	got, err := command.UnmarshalCommand(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.(command.Interrupt).Principal.Subject != "u" {
		t.Fatalf("principal lost: %s", body)
	}
}
