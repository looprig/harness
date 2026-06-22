package tools

import (
	"testing"

	"github.com/ciram-co/looprig/pkg/loop"
)

// FuzzApprovalsParse asserts the approvals-file parser is TOTAL and FAIL-SECURE
// over arbitrary bytes (the file is external, user-editable input):
//
//  1. parseApprovalsFile never panics for any input.
//  2. parseApprovalsFile either errors (→ the Stage-5 stage behaves as empty) OR
//     yields an ApprovalsFile whose records are all individually valid (a record
//     with an unknown effect can never survive the parse, so a malformed record
//     can never carry EffectAutoApprove).
//  3. The Stage-5 evaluation over the parsed records never returns AutoApprove
//     unless at least one parsed record both matches AND carries EffectAutoApprove
//     — i.e. arbitrary bytes can never silently auto-approve.
func FuzzApprovalsParse(f *testing.F) {
	seeds := []string{
		``,
		`{}`,
		`{"version":1,"approvals":[]}`,
		`{"version":1,"approvals":[{"tool":"Bash","match":"go test ./...","effect":"allow"}]}`,
		`{"version":1,"approvals":[{"tool":"ReadFile","effect":"deny"}]}`,
		`{"version":1,"approvals":[{"tool":"X","effect":"frobnicate"}]}`, // bad effect
		`{"version":1,"approvals":[ this is not json `,                   // syntax error
		`{"version":1,"approvals":[{"tool":"X","effect":7}]}`,            // non-string effect
		`[]`,
		`null`,
		"\x00\x01\x02",
		`{"version":1,"approvals":[{"tool":"","match":"","effect":"allow"}]}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		af, err := parseApprovalsFile([]byte(raw))
		if err != nil {
			// Errored parse → the stage behaves as empty; nothing more to assert.
			return
		}
		// A successful parse must only contain individually-valid records: every
		// Effect must be one of the three known values. A malformed record can
		// never survive to carry an unknown (and silently-allowing) effect.
		for _, rec := range af.Approvals {
			switch rec.Effect {
			case loop.EffectAsk, loop.EffectAutoApprove, loop.EffectDeny:
				// ok
			default:
				t.Fatalf("parsed record has out-of-range effect %d: %+v", rec.Effect, rec)
			}
		}

		// The Stage-5 reducer over the parsed records: AutoApprove only when at
		// least one record carries EffectAutoApprove (deny-beats-allow handled by
		// the reducer). Use a matcher that matches everything so an allow record,
		// if present, is the deciding factor.
		matchAll := func(ApprovalRecord) bool { return true }
		eff, decided := reduceApprovalRecords(af.Approvals, matchAll)
		if decided && eff == loop.EffectAutoApprove {
			ok := false
			for _, rec := range af.Approvals {
				if rec.Effect == loop.EffectAutoApprove {
					ok = true
					break
				}
			}
			if !ok {
				t.Fatalf("reducer returned AutoApprove with no allow record: %+v", af.Approvals)
			}
		}
	})
}
