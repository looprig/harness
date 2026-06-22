package tui

import (
	"testing"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// TestFoldDisplay covers the exported displayed-projection fold: it folds a primary
// loop's Enduring events through the SAME reducers the live/restore paths use, and
// the resulting projection equals the one built by the internal foldBacklog helper
// (so the public seam and the production fold are the SAME fold). It also covers the
// headline-property comparators EqualTranscript / PendingPrompts that the persistence
// integration tests assert through.
func TestFoldDisplay(t *testing.T) {
	t.Parallel()

	primary := callID(0xAA)
	other := callID(0xBB)
	hdr := event.Header{Coordinates: identity.Coordinates{LoopID: primary}}
	user := func(text string) *content.UserMessage {
		return &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: text}}}}
	}

	cleanTurn := []event.Event{
		event.TurnStarted{Header: hdr, Message: user("first question")},
		event.StepDone{Header: hdr, Messages: content.AgenticMessages{aiMessage("", "first answer")}},
		event.TurnDone{Header: hdr, Message: aiMessage("", "first answer")},
	}

	tests := []struct {
		name             string
		events           []event.Event
		wantCommittedLen int
		wantPending      int
	}{
		{name: "empty fold has no committed entries", events: nil, wantCommittedLen: 0, wantPending: 0},
		{
			name:             "clean single turn commits user + assistant",
			events:           cleanTurn,
			wantCommittedLen: 2,
			wantPending:      0,
		},
		{
			name: "pending permission gate counts as a pending prompt",
			events: []event.Event{
				event.TurnStarted{Header: hdr, Message: user("q")},
				event.PermissionRequested{Header: hdr, ToolExecutionID: callID(7), Request: tool.BashRequest{Command: "ls"}},
			},
			wantCommittedLen: 1, // the user row; the gate surfaces on the (uncommitted) tool card, not a record
			wantPending:      1,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := FoldDisplay(tt.events, primary)
			if got.CommittedLen() != tt.wantCommittedLen {
				t.Errorf("CommittedLen() = %d, want %d", got.CommittedLen(), tt.wantCommittedLen)
			}
			if got.PendingPrompts() != tt.wantPending {
				t.Errorf("PendingPrompts() = %d, want %d", got.PendingPrompts(), tt.wantPending)
			}

			// The exported fold IS the production fold: it must equal the internal
			// foldBacklog helper's transcript exactly.
			wantTr, _ := foldBacklog(primary, tt.events)
			if !got.equalTranscriptModel(wantTr) {
				t.Errorf("FoldDisplay transcript != foldBacklog transcript")
			}
		})
	}

	// EqualTranscript is reflexive across two independent folds of the SAME events
	// from the SAME zero state — the deep-equality the displayed==restored property
	// relies on.
	a := FoldDisplay(cleanTurn, primary)
	b := FoldDisplay(cleanTurn, primary)
	if !a.EqualTranscript(b) {
		t.Error("EqualTranscript on two folds of identical events = false, want true")
	}

	// A different primary loop scope yields a different committed transcript (the
	// primary-loop user row is scoped), so EqualTranscript must be false.
	if a.EqualTranscript(FoldDisplay(cleanTurn, other)) {
		t.Error("EqualTranscript across different primaryLoopID = true, want false")
	}
}

// equalTranscriptModel is a same-package test bridge: it deep-compares a
// DisplayProjection's (unexported) transcript model against an internally-built
// transcriptModel, so TestFoldDisplay can assert the exported fold equals the
// internal foldBacklog fold without leaking the unexported type across packages.
func (p DisplayProjection) equalTranscriptModel(tr transcriptModel) bool {
	return p.EqualTranscript(DisplayProjection{transcript: tr})
}

var _ = uuid.UUID{}
