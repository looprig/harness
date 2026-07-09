package serve_test

// This file guards the SPEC §7b CLIENT-SIDE exact-join contract.
//
// serve deliberately implements NO server-side replay-then-follow fusion. A client
// that wants the complete Enduring history + a live tail must join the two primitives
// serve exposes:
//
//  1. GET /v1/sessions/{sid}/events  — an SSE live stream; each Enduring frame carries
//     `id: <journal_seq>` (event.Delivery.JournalSeq). The client subscribes FIRST and
//     buffers live Enduring frames by their journal_seq.
//  2. GET /v1/sessions/{sid}/journal — a paged durable read returning
//     serve.EventJournalPage{Events []StatusEvent, NextJournalSeq, Done}, where each
//     StatusEvent pairs a journal_seq with its event.
//
// The exact join the CLIENT performs (modeled here, NOT in serve):
//   - subscribe to /events and buffer live Enduring frames;
//   - drain /journal from the beginning to the tip T (last durable seq at read time);
//   - emit journal events 1..T in order, then emit buffered live frames with seq > T,
//     DROPPING any buffered frame with seq <= T (already covered by the journal read);
//   - continue following the live stream.
//
// Guarantee proven: every Enduring event is delivered EXACTLY ONCE — no loss, no
// duplication — INCLUDING an event appended DURING the join window (after subscribe,
// around the journal tip read). Ephemeral frames (journal_seq == 0) are never part of
// the enduring join set.
//
// This test drives the join with fakes so it is deterministic (no correctness-sleeps)
// and lives in the default `go test` run. It uses the REAL serve DTOs
// (EventJournalPage / StatusEvent) so a change to the journal primitive shape breaks
// it. If this test ever requires a change to serve *production* code to pass, that is a
// signal that either someone added server-side join fusion (out of scope for §7b) or
// broke the `id:` / journal primitives the client join depends on.

import (
	"sort"
	"testing"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/serve"
)

// opKind is one step in a deterministic session timeline. The timeline lets a table
// row script the exact interleaving of durable appends, live-observed appends, and the
// journal tip snapshot that reproduces the join-window boundary.
type opKind int

const (
	// opDurable appends an Enduring event that is durable BEFORE the client subscribed:
	// it lands in the journal but is NOT observed on the live stream (pre-subscribe
	// history).
	opDurable opKind = iota
	// opLive appends an Enduring event AND observes it on the live stream: it is both
	// durable (journal) and buffered (live). Whether it is dropped from the live side
	// depends on its position relative to the snapshot.
	opLive
	// opEphemeralLive pushes an Ephemeral delivery (journal_seq == 0) onto the live
	// stream only. It is never durable and must never enter the enduring join set.
	opEphemeralLive
	// opSnapshot marks the moment the client drains the journal: it freezes the tip T
	// at the current durable length. Enduring events appended after this op are seq > T
	// and must be delivered from the live buffer.
	opSnapshot
)

// op is one scripted timeline step.
type op struct {
	kind opKind
}

// journalPageLimit is the per-page cap the modeled client passes to the journal read,
// small enough that multi-event scenarios exercise the NextJournalSeq/Done cursor loop.
const journalPageLimit = 2

// mkEnduring builds an Enduring event tagged with its sequence so a joined record can be
// identified back to the seq it should carry. TurnDone is terminal (=> Enduring).
func mkEnduring(seq uint64) event.Event {
	return event.TurnDone{TurnIndex: event.TurnIndex(seq)}
}

// mkEphemeral builds an Ephemeral delivery-class event (journal_seq is 0). TokenDelta is
// ephemeral — it never reaches the journal and must be excluded from the enduring join.
func mkEphemeral() event.Event {
	return event.TokenDelta{}
}

// timeline is the result of running a scripted op sequence: the frozen journal snapshot
// the client will page, the live buffer it accumulated, and totalEnduring — the count of
// distinct Enduring events, which is exactly the set the join must reproduce (seq 1..M).
type timeline struct {
	frozen        []event.Event    // durable snapshot at opSnapshot; index i => seq i+1
	liveBuf       []event.Delivery // frames observed on the live stream, in arrival order
	totalEnduring uint64           // M: every Enduring event ever appended
}

// runTimeline executes the ops, building the journal snapshot and the live buffer the
// modeled client join consumes. It performs no join itself — it only models the two
// primitives producing their inputs.
func runTimeline(ops []op) timeline {
	var (
		log     []event.Event // durable, ordered; seq = index+1
		frozen  []event.Event
		liveBuf []event.Delivery
		seq     uint64
	)
	for _, o := range ops {
		switch o.kind {
		case opDurable:
			seq++
			log = append(log, mkEnduring(seq))
		case opLive:
			seq++
			e := mkEnduring(seq)
			log = append(log, e)
			liveBuf = append(liveBuf, event.Delivery{Event: e, JournalSeq: seq})
		case opEphemeralLive:
			liveBuf = append(liveBuf, event.Delivery{Event: mkEphemeral(), JournalSeq: 0})
		case opSnapshot:
			frozen = append([]event.Event(nil), log...)
		}
	}
	return timeline{frozen: frozen, liveBuf: liveBuf, totalEnduring: seq}
}

// readJournalPage is the fake /journal primitive: given a frozen snapshot and a paging
// window it returns the durable Enduring events with seq in [from, tip], capped at limit,
// plus the NextJournalSeq/Done cursor — mirroring serve's EventJournalPage contract.
func readJournalPage(frozen []event.Event, from uint64, limit int) serve.EventJournalPage {
	tip := uint64(len(frozen))
	seq := from
	if seq == 0 {
		seq = 1 // from_journal_seq 0/absent == from the beginning (seq starts at 1)
	}
	var evs []serve.StatusEvent
	for seq <= tip && len(evs) < limit {
		evs = append(evs, serve.StatusEvent{JournalSeq: seq, Event: frozen[seq-1]})
		seq++
	}
	done := seq > tip
	var next uint64
	if !done {
		next = seq
	}
	return serve.EventJournalPage{Events: evs, NextJournalSeq: next, Done: done}
}

// drainJournal pages the frozen snapshot from the beginning to the tip, returning the
// ordered journal events and the tip T (the last durable seq at read time).
func drainJournal(frozen []event.Event, limit int) (events []serve.StatusEvent, tip uint64) {
	from := uint64(1)
	for {
		page := readJournalPage(frozen, from, limit)
		events = append(events, page.Events...)
		if n := len(page.Events); n > 0 {
			tip = page.Events[n-1].JournalSeq
		}
		if page.Done {
			return events, tip
		}
		from = page.NextJournalSeq
	}
}

// clientJoin is the modeled CLIENT-SIDE exact join over the two primitives. It emits the
// drained journal (seq 1..T) in order, then the buffered live Enduring frames with seq >
// T in increasing order, dropping any live frame with seq <= T (covered by the journal)
// and any Ephemeral frame (seq 0, never enduring). serve does none of this.
func clientJoin(t timeline, limit int) []serve.StatusEvent {
	journal, tip := drainJournal(t.frozen, limit)
	out := append([]serve.StatusEvent(nil), journal...)

	// Buffered live Enduring frames above the journal tip, in seq order. Dropping seq
	// <= tip is the dedup against the journal; filtering on Class()==Enduring is what
	// excludes Ephemeral (seq 0) frames from the enduring join set.
	var tail []event.Delivery
	for _, d := range t.liveBuf {
		if d.Event.Class() != event.Enduring {
			continue
		}
		if d.JournalSeq <= tip {
			continue // already delivered by the journal read
		}
		tail = append(tail, d)
	}
	sort.Slice(tail, func(i, j int) bool { return tail[i].JournalSeq < tail[j].JournalSeq })
	for _, d := range tail {
		out = append(out, serve.StatusEvent{JournalSeq: d.JournalSeq, Event: d.Event})
	}
	return out
}

func TestClientSideExactJoin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ops  []op
	}{
		{
			// Base case: three durable events, no live activity during the window. The
			// join is exactly the journal read: 1..3, once each.
			name: "journal only, no live activity",
			ops: []op{
				{opDurable}, {opDurable}, {opDurable},
				{opSnapshot},
			},
		},
		{
			// A pure live tail: nothing durable pre-subscribe; every event is appended
			// during the window and observed live above an empty-ish journal tip.
			name: "live tail above empty pre-subscribe journal",
			ops: []op{
				{opSnapshot}, // tip T = 0
				{opLive}, {opLive}, {opLive},
			},
		},
		{
			// THE boundary case, both sub-cases in one timeline:
			//   seq 1,2   durable pre-subscribe -> journal only.
			//   seq 3     appended DURING the window, observed live, THEN the snapshot
			//             includes it -> seq 3 <= T, so it is in BOTH the journal and the
			//             live buffer and MUST be dropped from the live side (exactly once).
			//   seq 4     appended DURING the window AFTER the snapshot -> seq 4 > T, only
			//             in the live buffer, MUST be kept (exactly once, not lost).
			name: "event appended during window at and above the tip",
			ops: []op{
				{opDurable}, {opDurable}, // seq 1,2 pre-subscribe
				{opLive},     // seq 3 observed live...
				{opSnapshot}, // ...and captured by the tip (T=3): drop from live
				{opLive},     // seq 4 above the tip: keep from live
			},
		},
		{
			// Ephemeral frames on the live stream must never enter the enduring join set,
			// even interleaved around the boundary. Enduring set is still 1..3.
			name: "ephemeral live frames excluded from enduring join",
			ops: []op{
				{opDurable},       // seq 1
				{opEphemeralLive}, // seq 0 — not enduring
				{opLive},          // seq 2 observed live, at/below tip
				{opEphemeralLive}, // seq 0
				{opSnapshot},      // T=2
				{opEphemeralLive}, // seq 0 after snapshot
				{opLive},          // seq 3 above tip
			},
		},
		{
			// Many events straddling the tip with a small page limit to exercise the
			// NextJournalSeq/Done cursor loop across several pages.
			name: "multi-page journal with live straddling the tip",
			ops: []op{
				{opDurable}, {opDurable}, {opDurable}, {opDurable}, // seq 1..4
				{opLive},                     // seq 5 observed live, below tip
				{opSnapshot},                 // T=5
				{opLive}, {opLive}, {opLive}, // seq 6..8 above tip
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tl := runTimeline(tt.ops)
			joined := clientJoin(tl, journalPageLimit)

			// Collect the joined seqs in emission order.
			seqs := make([]uint64, len(joined))
			for i, ev := range joined {
				seqs[i] = ev.JournalSeq
			}

			// Ordering: strictly increasing journal_seq (no equal, no descending).
			for i := 1; i < len(seqs); i++ {
				if seqs[i] <= seqs[i-1] {
					t.Fatalf("join out of order: seqs[%d]=%d not > seqs[%d]=%d (full=%v)",
						i, seqs[i], i-1, seqs[i-1], seqs)
				}
			}

			// Duplication: no journal_seq appears twice.
			seen := make(map[uint64]int, len(seqs))
			for _, s := range seqs {
				seen[s]++
				if seen[s] > 1 {
					t.Fatalf("duplicate seq %d in join (count=%d, full=%v)", s, seen[s], seqs)
				}
			}

			// Loss + completeness: the enduring join is EXACTLY the full set 1..M once.
			if got, want := uint64(len(seqs)), tl.totalEnduring; got != want {
				t.Fatalf("join delivered %d enduring events, want %d (full=%v)", got, want, seqs)
			}
			for want := uint64(1); want <= tl.totalEnduring; want++ {
				if seen[want] != 1 {
					t.Fatalf("seq %d delivered %d times, want exactly 1 (full=%v)", want, seen[want], seqs)
				}
			}

			// The join carries no Ephemeral (seq 0) frame.
			if seen[0] != 0 {
				t.Fatalf("ephemeral frame (seq 0) leaked into the enduring join (full=%v)", seqs)
			}

			// Each joined record carries the event whose seq it claims (the id: line and
			// the journal record agree on the same durable event).
			for _, ev := range joined {
				td, ok := ev.Event.(event.TurnDone)
				if !ok {
					t.Fatalf("joined seq %d is not the expected Enduring TurnDone: %T", ev.JournalSeq, ev.Event)
				}
				if uint64(td.TurnIndex) != ev.JournalSeq {
					t.Fatalf("joined seq %d carries event tagged %d (misjoined event/seq)", ev.JournalSeq, td.TurnIndex)
				}
			}
		})
	}
}
