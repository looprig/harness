package tui

import "testing"

// TestScrollbackFlushPrintsEachEntryOnce exercises the print-once contract and the
// one-blank-line entry-spacing rule of scrollbackModel.Flush across the cases that
// matter: a basic two-entry flush, an empty committed slice, a growing committed
// slice (only the new tail flushes), and a multi-line entry render (still exactly
// one trailing blank line).
func TestScrollbackFlushPrintsEachEntryOnce(t *testing.T) {
	t.Parallel()

	oneLine := func(e entry) []string { return []string{"line-" + e.ID.String()} }
	multiLine := func(e entry) []string {
		return []string{"a-" + e.ID.String(), "b-" + e.ID.String()}
	}

	tests := []struct {
		name        string
		render      func(entry) []string
		committed   []entry
		wantActions int
		// wantLines is the expected Lines slice per action, in order.
		wantLines [][]string
	}{
		{
			name:        "two entries each flushed once with trailing blank",
			render:      oneLine,
			committed:   []entry{{ID: 1}, {ID: 2}},
			wantActions: 2,
			wantLines:   [][]string{{"line-1", ""}, {"line-2", ""}},
		},
		{
			name:        "empty committed yields no actions",
			render:      oneLine,
			committed:   nil,
			wantActions: 0,
			wantLines:   nil,
		},
		{
			name:        "multi-line render still gets exactly one trailing blank",
			render:      multiLine,
			committed:   []entry{{ID: 7}},
			wantActions: 1,
			wantLines:   [][]string{{"a-7", "b-7", ""}},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newScrollbackModel(80)
			s, actions := s.Flush(tt.committed, tt.render)
			if len(actions) != tt.wantActions {
				t.Fatalf("first flush actions = %d, want %d", len(actions), tt.wantActions)
			}
			for i, want := range tt.wantLines {
				got := actions[i].Lines
				if len(got) != len(want) {
					t.Fatalf("action[%d].Lines = %v, want %v", i, got, want)
				}
				for j := range want {
					if got[j] != want[j] {
						t.Errorf("action[%d].Lines[%d] = %q, want %q", i, j, got[j], want[j])
					}
				}
				// Each entry ends with exactly one trailing blank line (§Spacing):
				// the last line is "" and the second-to-last (if any) is not.
				if last := got[len(got)-1]; last != "" {
					t.Errorf("action[%d] not blank-line terminated: %q", i, last)
				}
				if len(got) >= 2 && got[len(got)-2] == "" {
					t.Errorf("action[%d] has more than one trailing blank line: %v", i, got)
				}
				if actions[i].EntryID != tt.committed[i].ID {
					t.Errorf("action[%d].EntryID = %d, want %d", i, actions[i].EntryID, tt.committed[i].ID)
				}
			}
			// Re-flush the same entries → nothing reprinted (print-once). The
			// returned model is discarded: this is the last flush in the test.
			_, again := s.Flush(tt.committed, tt.render)
			if len(again) != 0 {
				t.Fatalf("second flush actions = %d, want 0 (print-once)", len(again))
			}
		})
	}
}

// TestScrollbackFlushTightAttach verifies the step-group spacing exception: a
// non-promoted tool card attaches FLUSH (no trailing blank line) to the entry above
// it, so an assistant bullet and its "⎿ …" cards read as one message. The trailing
// blank line falls only on the LAST entry of a group (the one whose successor starts
// a new group), so groups stay separated by a single blank line. A promoted tool
// card renders as its own bullet, so it starts a new group and keeps its blank line.
func TestScrollbackFlushTightAttach(t *testing.T) {
	t.Parallel()

	oneLine := func(e entry) []string { return []string{"line-" + e.ID.String()} }

	tests := []struct {
		name      string
		committed []entry
		// wantBlank is the expected per-entry trailing-blank state, in committed order:
		// true = the entry's Lines end with a "" separator, false = it attaches tight.
		wantBlank []bool
	}{
		{
			name:      "assistant then one tool card → assistant tight, card blank",
			committed: []entry{{ID: 1, Kind: kindAssistant}, {ID: 2, Kind: kindTool}},
			wantBlank: []bool{false, true},
		},
		{
			name: "assistant then multiple cards → group tight, only last card blank",
			committed: []entry{
				{ID: 1, Kind: kindAssistant}, {ID: 2, Kind: kindTool},
				{ID: 3, Kind: kindTool}, {ID: 4, Kind: kindTool},
			},
			wantBlank: []bool{false, false, false, true},
		},
		{
			name: "promoted card starts a new group → preceding user keeps its blank",
			committed: []entry{
				{ID: 1, Kind: kindUser},
				{ID: 2, Kind: kindTool, promoted: true},
			},
			wantBlank: []bool{true, true},
		},
		{
			name: "assistant then a non-tool entry → both keep their blank",
			committed: []entry{
				{ID: 1, Kind: kindAssistant},
				{ID: 2, Kind: kindNotice},
			},
			wantBlank: []bool{true, true},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newScrollbackModel(80)
			_, actions := s.Flush(tt.committed, oneLine)
			if len(actions) != len(tt.wantBlank) {
				t.Fatalf("actions = %d, want %d", len(actions), len(tt.wantBlank))
			}
			for i, want := range tt.wantBlank {
				got := actions[i].Lines
				gotBlank := len(got) > 0 && got[len(got)-1] == ""
				if gotBlank != want {
					t.Errorf("entry %d (id %d) trailing-blank = %v, want %v (Lines %q)",
						i, tt.committed[i].ID, gotBlank, want, got)
				}
			}
		})
	}
}

// TestScrollbackFlushGrowingTail verifies that when committed grows, only the new
// tail entries are flushed — previously printed entries are skipped.
func TestScrollbackFlushGrowingTail(t *testing.T) {
	t.Parallel()
	render := func(e entry) []string { return []string{"line-" + e.ID.String()} }

	s := newScrollbackModel(80)
	s, first := s.Flush([]entry{{ID: 1}, {ID: 2}}, render)
	if len(first) != 2 {
		t.Fatalf("first flush actions = %d, want 2", len(first))
	}

	// Grow the committed slice by appending two new entries; only the tail flushes.
	// The returned model is discarded: this is the last flush in the test.
	_, second := s.Flush([]entry{{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}}, render)
	if len(second) != 2 {
		t.Fatalf("growing-tail flush actions = %d, want 2", len(second))
	}
	if second[0].EntryID != 3 || second[1].EntryID != 4 {
		t.Errorf("tail actions = [%d %d], want [3 4]", second[0].EntryID, second[1].EntryID)
	}
}

// TestScrollbackFlushPureReducer verifies Flush does not mutate the input model's
// printed map nor the input committed slice (pure-reducer style).
func TestScrollbackFlushPureReducer(t *testing.T) {
	t.Parallel()
	render := func(e entry) []string { return []string{"line-" + e.ID.String()} }

	original := newScrollbackModel(80)
	committed := []entry{{ID: 1}, {ID: 2}}
	_, _ = original.Flush(committed, render)

	// The input model's map must be unchanged: a fresh flush from `original`
	// must still produce both actions.
	_, again := original.Flush(committed, render)
	if len(again) != 2 {
		t.Fatalf("input model was mutated: re-flush from original = %d actions, want 2", len(again))
	}
}

// TestDisplayIDString verifies the stable string form used by renderers.
func TestDisplayIDString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		id   displayID
		want string
	}{
		{name: "zero", id: 0, want: "0"},
		{name: "one", id: 1, want: "1"},
		{name: "large", id: 18446744073709551615, want: "18446744073709551615"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.id.String(); got != tt.want {
				t.Errorf("displayID(%d).String() = %q, want %q", uint64(tt.id), got, tt.want)
			}
		})
	}
}
