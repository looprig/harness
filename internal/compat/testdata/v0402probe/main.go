// Command v0402probe runs the released harness v0.40.2 codecs. It freezes
// pre-feature goldens and measures how that release reads v0.41.0 records.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
)

func id(s string) uuid.UUID {
	u, err := uuid.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

var (
	session = id("11111111-1111-4111-8111-111111111111")
	loopID  = id("22222222-2222-4222-8222-222222222222")
	turnID  = id("33333333-3333-4333-8333-333333333333")
	cmdID   = id("44444444-4444-4444-8444-444444444444")
	evID    = id("55555555-5555-4555-8555-555555555555")
	gateID  = id("66666666-6666-4666-8666-666666666666")
	stepID  = id("77777777-7777-4777-8777-777777777777")
)

func header() event.Header {
	return event.Header{
		EventID:     evID,
		Coordinates: identity.Coordinates{SessionID: session, LoopID: loopID, TurnID: turnID},
		Cause:       identity.Cause{CommandID: cmdID, Agency: identity.AgencyUser},
	}
}

func user(body string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: body}}}}
}

func gen(dir string) error {
	stepHeader := header()
	stepHeader.StepID = stepID
	events := map[string]event.Event{
		"turn_started.json": event.TurnStarted{Header: header(), TurnIndex: 1, Message: user("hello")},
		"step_done.json": event.StepDone{Header: stepHeader, Messages: content.AgenticMessages{
			&content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: "answer"}}}},
		}},
		"turn_folded_into.json": event.TurnFoldedInto{Header: header(), TurnIndex: 1, Message: user("more")},
		"input_cancelled.json":  event.InputCancelled{Header: header(), TurnIndex: 1, Reason: event.CancelTurnInterrupted, Message: user("late")},
		"turn_interrupted.json": event.TurnInterrupted{Header: header(), TurnIndex: 1},
		"gate_resolved.json": event.GateResolved{
			Header: header(), GateID: gate.ID(gateID), Resolver: gate.ResolverSession,
			Reason: gate.CloseAnswered, Action: "approve",
		},
	}
	for name, ev := range events {
		body, err := event.MarshalEvent(ev)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), append(body, '\n'), 0o600); err != nil {
			return err
		}
	}
	commands := map[string]command.Command{
		"user_input.json": command.UserInput{
			Header: command.Header{CommandID: cmdID, Agency: identity.AgencyUser},
			Blocks: []content.Block{&content.TextBlock{Text: "hello"}},
		},
		"interrupt.json": command.Interrupt{Header: command.Header{CommandID: cmdID, Agency: identity.AgencyUser}},
	}
	for name, cmd := range commands {
		body, err := command.MarshalCommand(cmd)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), append(body, '\n'), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func decode(files []string) error {
	for _, file := range files {
		raw, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			return err
		}
		raw = bytes.TrimSpace(raw)
		var out []byte
		if strings.Contains(filepath.Base(file), "user_input") || filepath.Base(file) == "interrupt.json" {
			cmd, decodeErr := command.UnmarshalCommand(raw)
			if decodeErr != nil {
				fmt.Printf("%s\tREFUSED\t%v\n", filepath.Base(file), decodeErr)
				continue
			}
			out, err = command.MarshalCommand(cmd)
		} else {
			ev, decodeErr := event.UnmarshalEvent(raw)
			if decodeErr != nil {
				fmt.Printf("%s\tREFUSED\t%v\n", filepath.Base(file), decodeErr)
				continue
			}
			out, err = event.MarshalEvent(ev)
		}
		if err != nil {
			return err
		}
		verdict := "IDENTICAL"
		if !bytes.Equal(out, raw) {
			verdict = "LOSSY"
		}
		fmt.Printf("%s\t%s\t%s\n", filepath.Base(file), verdict, out)
	}
	return nil
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: v0402probe gen DIR | decode FILE...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = gen(os.Args[2])
	case "decode":
		err = decode(os.Args[2:])
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
