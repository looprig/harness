package loopruntime

import (
	"context"

	"github.com/looprig/core/content"
)

// TruncatedResponseNotice is the marker text block appended, as the LAST block, to
// an assistant message stored after a stream failed part-way through the model's
// reply. It is the transcript's own record that the turn was cut off: a reader who
// only ever sees committed history — a human scrolling back, an export, the model
// on the next request — must not mistake a partial reply for a complete one.
//
// It is deliberately in-band rather than a side-channel flag. The alternatives all
// lose the signal at some boundary: a field on content.AIMessage would have to be
// carried by every codec, and an event-only marker is invisible to anything that
// reads the stored message graph (which is exactly what the next request is built
// from). A block travels with the content it qualifies.
// The wording claims only what the runtime can actually prove. A stream can fail
// after every content chunk arrived (a malformed terminal-metadata frame does
// exactly that), so the notice says the reply MAY be incomplete rather than
// asserting it was cut mid-word. What is always true is that the stream failed and
// the turn did not finish.
const TruncatedResponseNotice = "[truncated: the stream failed before this reply completed; the content above may be incomplete]"

// InterruptedResponseNotice is TruncatedResponseNotice's sibling for the OTHER
// way a reply is cut short: someone stopped it on purpose. A cancelled turn is
// not a failed turn, and telling a reader — a human scrolling back, or the model
// on the next request — that "the stream failed" would be a false report of a
// fault. It matters most to the model: "you were interrupted" is a resumable
// state it can act on, while "the connection broke" invites it to apologize for
// an outage that never happened.
//
// It deliberately does not name WHO stopped the reply. From inside the turn a
// user's Ctrl-C, a parent agent's StopAgent, and a graceful shutdown are the same
// cancelled context; a notice that guessed would be wrong a third of the time.
// What is always true is that the reply was stopped and the text above is what
// had arrived by then — and unlike a stream failure, that text is exactly what
// the user saw, with nothing else in flight.
const InterruptedResponseNotice = "[interrupted: this reply was stopped before it completed; the content above is what had arrived]"

// truncationNotice picks the transcript's account of WHY a reply is only a
// prefix. It reads the same context predicate streamFailure uses to pick the
// terminal, so the notice and the turn's terminal can never disagree: a turn
// that ends TurnInterrupted always says interrupted, and one that ends
// TurnFailed always says the stream failed. A provider error that arrives in the
// same instant as a cancellation is reported as a cancellation by both, which is
// the honest reading — the caller had already withdrawn the request.
func truncationNotice(ctx context.Context) string {
	if ctx.Err() != nil {
		return InterruptedResponseNotice
	}
	return TruncatedResponseNotice
}

// truncatedAssistantMessage materializes the storable remains of a step whose
// stream was cut short, or nil when nothing survives. notice is the marker block
// appended last (see truncationNotice).
//
// The content already reached the user: chunkProcessor emits every chunk as a live
// TokenDelta BEFORE folding it, so by the time the stream errors the model's words
// are on screen. Discarding them is silent data loss — the text vanishes from the
// transcript, from committed history, and from the next request, after the user
// watched it arrive and the provider (usually) billed for it.
//
// Persisting the whole accumulator is the opposite defect. Committed history is
// replayed on the next request AS IF COMPLETE, so anything structurally unfinished
// becomes a real request: see replayableTruncatedBlocks for exactly what that rules
// out. What survives is the safe prefix, plus the notice that says it is a prefix.
//
// Returning nil (rather than a message holding only the notice) preserves the
// historical outcome for a stream that failed before delivering anything usable:
// no message, no commit, no StepDone.
func truncatedAssistantMessage(blocks *blockState, notice string) *content.AIMessage {
	msg := blocks.AIMessage()
	kept := replayableTruncatedBlocks(msg.Blocks)
	if len(kept) == 0 {
		return nil
	}
	msg.Blocks = append(kept, &content.TextBlock{Text: notice})
	return msg
}

// replayableTruncatedBlocks keeps only the blocks of an interrupted response that
// are both meaningful on their own and safe to send back to a provider. It is the
// truncation-time counterpart of sanitizeAssistantBlocks, and strictly narrower:
// sanitizeAssistantBlocks repairs a COMPLETE response for storage, while a
// truncated response has no completion guarantee at all, so anything whose
// integrity cannot be established from the accumulated state is dropped rather
// than repaired.
//
// KEPT:
//   - Text, when non-empty. Text accumulates by pure concatenation, so a truncated
//     text block is a shorter true text block — never a malformed one. This is the
//     content the user actually watched arrive.
//   - A refusal, whatever its text, for the same reason sanitizeAssistantBlocks
//     keeps one: the refusal IS the answer, and providers routinely decline with no
//     explanation.
//   - A thinking block that is SEALED (see sealedReasoning) — its terminal
//     signature or opaque provider state arrived, so the block is whole and the
//     next request can replay it verbatim.
//
// DROPPED:
//   - Every tool-use block, unconditionally. Two independent reasons, either one
//     sufficient. (1) Input is concatenated from InputJSON fragments, so a
//     truncated call carries incomplete arguments; a half-decoded call must never
//     reach a provider or a tool runner, and rewriting it to "{}" the way
//     sanitizeAssistantBlocks does would be worse still — a syntactically valid
//     call the model never made. (2) Even byte-complete arguments are unusable
//     here: the step died before RunBatch, so no tool_result follows. Anthropic and
//     Bedrock reject a tool_use with no matching tool_result, and
//     validateCompactionTailTranscript already rejects it as "orphan_tool_call".
//   - An unsealed thinking block. Its signature never arrived, so it is mid-block;
//     Anthropic and Bedrock reject an unsigned thinking block outright. Reasoning
//     is also provider-private scratch work rather than the answer the user is
//     owed, so dropping it costs the transcript far less than dropping the text.
//   - Images. streamaccumulator.Images concatenates raw bytes, and its own contract
//     says a mis-assembled image "produces a corrupt file that nothing downstream
//     can detect or recover". A truncated payload is exactly that.
//   - Anything else, including a block variant core adds later. Fail closed: a new
//     variant is unproven under truncation until someone reasons about it here.
func replayableTruncatedBlocks(blocks []content.Block) []content.Block {
	out := make([]content.Block, 0, len(blocks))
	for _, b := range blocks {
		switch v := b.(type) {
		case *content.TextBlock:
			if v.Text != "" {
				out = append(out, v)
			}
		case *content.RefusalBlock:
			out = append(out, v)
		case *content.ThinkingBlock:
			if sealedReasoning(v) {
				out = append(out, v)
			}
		}
	}
	return out
}

// sealedReasoning reports whether a thinking block is COMPLETE, not merely
// non-empty. streamaccumulator.Thinking documents that "the signature arrives as a
// terminal delta for its own Index", and a redacted block arrives whole inside
// ProviderState, so either one present means the provider finished this block.
//
// It composes hasReasoning rather than restating it, so the three predicates that
// decide a thinking block's fate (isEmptyAssistantMessage, sanitizeAssistantBlocks,
// and this) can never disagree about which blocks carry content — this one only
// adds the completeness requirement that truncation makes necessary.
//
// A signature counts as a seal only when it carries the label of the dialect
// that minted it. An UNLABELLED signature is not replayable by anyone: every
// codec refuses one, because a signature is verified by its issuer and there is
// no way to tell whose it is. Keeping such a block would convert this function's
// job — "keep only what can be sent back" — into a guaranteed encode failure on
// the very next request, which is strictly worse than the drop this predicate
// already prescribes for a mid-block thinking delta.
func sealedReasoning(b *content.ThinkingBlock) bool {
	return hasReasoning(b) && ((b.Signature != "" && b.SignatureFormat != "") || len(b.ProviderState) > 0)
}
