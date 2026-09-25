package compat_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
)

var updateGoldens = flag.Bool("update", false, "rewrite the v0.41.0 cross-release codec goldens")

func fixtureID(t *testing.T, raw string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestV0410Goldens(t *testing.T) {
	sessionID := fixtureID(t, "11111111-1111-4111-8111-111111111111")
	loopID := fixtureID(t, "22222222-2222-4222-8222-222222222222")
	turnID := fixtureID(t, "33333333-3333-4333-8333-333333333333")
	cmdID := fixtureID(t, "44444444-4444-4444-8444-444444444444")
	evID := fixtureID(t, "55555555-5555-4555-8555-555555555555")
	gateID := fixtureID(t, "66666666-6666-4666-8666-666666666666")
	principal := &sessionwire.Principal{Tenant: "acme", Subject: "u1", Kind: sessionwire.PrincipalKindActor}
	metadata := sessionwire.MessageMetadata{"space": "family"}
	header := event.Header{
		EventID:     evID,
		Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID, TurnID: turnID},
		Cause:       identity.Cause{CommandID: cmdID, Agency: identity.AgencyUser},
	}
	message := func(body string) *content.UserMessage {
		return &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{
			&content.TextBlock{Text: "[from: Alex]"}, &content.TextBlock{Text: body}, &content.TextBlock{Text: "(sent from phone)"},
		}}}
	}
	input := &event.MessageInput{Principal: principal, Metadata: metadata, Prefix: 1, Suffix: 1}
	events := map[string]event.Event{
		"turn_started.json":     event.TurnStarted{Header: header, TurnIndex: 1, Message: message("hello"), Input: input},
		"turn_folded_into.json": event.TurnFoldedInto{Header: header, TurnIndex: 1, Message: message("more"), Input: input},
		"input_cancelled.json":  event.InputCancelled{Header: header, TurnIndex: 1, Reason: event.CancelTurnInterrupted, Message: message("late"), Input: input},
		"turn_interrupted.json": event.TurnInterrupted{Header: header, TurnIndex: 1, Principal: principal},
		"gate_resolved.json": event.GateResolved{Header: header, GateID: gate.ID(gateID), Resolver: gate.ResolverSession,
			Reason: gate.CloseAnswered, Action: "approve", Principal: principal},
	}
	commands := map[string]command.Command{
		"user_input.json": command.UserInput{
			Header: command.Header{CommandID: cmdID, Agency: identity.AgencyUser},
			Blocks: []content.Block{&content.TextBlock{Text: "hello"}}, Principal: principal, Metadata: metadata,
			Presented: &command.Presented{Prefix: []content.Block{&content.TextBlock{Text: "[from: Alex]"}}, Suffix: []content.Block{&content.TextBlock{Text: "(sent from phone)"}}},
		},
		"interrupt.json": command.Interrupt{Header: command.Header{CommandID: cmdID, Agency: identity.AgencyUser}, Principal: principal},
	}
	for name, value := range events {
		body, err := event.MarshalEvent(value)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		checkGolden(t, name, body)
	}
	for name, value := range commands {
		body, err := command.MarshalCommand(value)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		checkGolden(t, name, body)
	}
}

func checkGolden(t *testing.T, name string, body []byte) {
	t.Helper()
	path := filepath.Join("testdata", "v0410", name)
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), body) {
		t.Fatalf("%s drifted:\nwant %s\n got %s", name, want, body)
	}
}
