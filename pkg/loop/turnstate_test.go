package loop

import (
	"testing"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// testUserMsg builds a UserMessage carrying a single text block.
func testUserMsg(text string) *content.UserMessage {
	return &content.UserMessage{
		Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: text}}},
	}
}

func TestNewTurnState(t *testing.T) {
	t.Parallel()
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	turnID, _ := uuid.New()
	causationID, _ := uuid.New()

	tests := []struct {
		name        string
		sessionID   uuid.UUID
		loopID      uuid.UUID
		turnID      uuid.UUID
		causationID uuid.UUID
		user        *content.UserMessage
	}{
		{
			name:        "happy path stamps identity and seeds the initial UserMessage",
			sessionID:   sessionID,
			loopID:      loopID,
			turnID:      turnID,
			causationID: causationID,
			user:        testUserMsg("hello"),
		},
		{
			name:        "zero ids still seed exactly the initial UserMessage",
			sessionID:   uuid.UUID{},
			loopID:      uuid.UUID{},
			turnID:      uuid.UUID{},
			causationID: uuid.UUID{},
			user:        testUserMsg("zero"),
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := newTurnState(tt.sessionID, tt.loopID, tt.turnID, 7, tt.causationID, tt.user)
			if ts.sessionID != tt.sessionID {
				t.Errorf("sessionID = %v, want %v", ts.sessionID, tt.sessionID)
			}
			if ts.loopID != tt.loopID {
				t.Errorf("loopID = %v, want %v", ts.loopID, tt.loopID)
			}
			if ts.id != tt.turnID {
				t.Errorf("id = %v, want %v", ts.id, tt.turnID)
			}
			if ts.index != 7 {
				t.Errorf("index = %v, want 7", ts.index)
			}
			if ts.causationID != tt.causationID {
				t.Errorf("causationID = %v, want %v", ts.causationID, tt.causationID)
			}
			// turnState.msgs starts with EXACTLY one UserMessage: the initial one.
			if len(ts.msgs) != 1 {
				t.Fatalf("msgs len = %d, want 1 (the initial UserMessage)", len(ts.msgs))
			}
			if ts.msgs[0] != tt.user {
				t.Errorf("msgs[0] is not the initial UserMessage")
			}
			if _, ok := ts.msgs[0].(*content.UserMessage); !ok {
				t.Errorf("msgs[0] = %T, want *content.UserMessage", ts.msgs[0])
			}
			if ts.toolIterations != 0 || ts.toolCalls != 0 {
				t.Errorf("counters = (%d,%d), want (0,0)", ts.toolIterations, ts.toolCalls)
			}
		})
	}
}

// TestCloneMessages proves the base clone has its own backing array: mutating
// (appending to) the SOURCE after cloning does not change the clone. This is the
// safety property turnConfig.base relies on — runLoop keeps appending committed
// step groups to loopState.msgs while runTurn reads base concurrently.
func TestCloneMessages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  content.AgenticMessages
	}{
		{name: "nil source clones to empty, append to src does not grow clone", src: nil},
		{name: "single element source", src: content.AgenticMessages{testUserMsg("a")}},
		{
			name: "multi element source",
			src:  content.AgenticMessages{testUserMsg("a"), testUserMsg("b"), testUserMsg("c")},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clone := cloneMessages(tt.src)
			if len(clone) != len(tt.src) {
				t.Fatalf("clone len = %d, want %d", len(clone), len(tt.src))
			}
			for i := range tt.src {
				if clone[i] != tt.src[i] {
					t.Errorf("clone[%d] != src[%d] (element pointers must match)", i, i)
				}
			}
			// Mutate the SOURCE: append many to force a possible cap-preserving write,
			// then assert the clone is unchanged in both length and contents.
			grown := tt.src
			for i := 0; i < 8; i++ {
				grown = append(grown, testUserMsg("extra"))
			}
			if len(clone) != len(tt.src) {
				t.Errorf("after appending to source, clone len = %d, want %d (own backing array)", len(clone), len(tt.src))
			}
			for i := range tt.src {
				if clone[i] != tt.src[i] {
					t.Errorf("after appending to source, clone[%d] changed", i)
				}
			}
		})
	}
}
