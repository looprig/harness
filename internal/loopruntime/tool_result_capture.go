package loopruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// ToolResultObjectStat is what a SessionObjectStore reports back about an object
// the loop has just written: the stored byte count and the lowercase-hex SHA-256
// of the stored bytes. Both are compared against what the capture sink computed
// before the referencing StepDone is allowed to commit, so a store that silently
// truncated or rewrote the payload cannot be recorded as a successful retention.
type ToolResultObjectStat struct {
	SizeBytes uint64
	Digest    string
}

// ToolResultObjectStore is the narrow SessionObjectStore surface the loop runtime
// needs in order to retain a tool result that the committed model message cannot
// carry in full. It is deliberately two methods wide: the loop mints the opaque
// object identity itself (see captureObjectID) and never asks the store for a
// name, a URL, a credential or a backend path, so no such value can reach the
// public journal through this seam.
//
// A nil store means retention is not configured: the loop then commits exactly
// what it committed before this seam existed. Requiring durable retention is the
// composition root's decision, taken by wiring a store.
type ToolResultObjectStore interface {
	// PutToolResultObject stores content under the caller-minted opaque objectID.
	// The object is immutable: writing the same identity twice must either be a
	// no-op or store identical bytes.
	PutToolResultObject(ctx context.Context, objectID string, content []byte) error
	// StatToolResultObject reports the size and digest of a stored object.
	StatToolResultObject(ctx context.Context, objectID string) (ToolResultObjectStat, error)
}

// toolResultRetainedMarkerPrefix opens every model-visible retention marker. It
// is a distinct literal from toolResultTruncatedMarker so a reader (and a test)
// can tell "the preview was shaped and the rest is retained" from "the preview
// was shaped and nothing else exists".
const toolResultRetainedMarkerPrefix = "\n[tool output shaped"

// materializedCaptureCeiling is the byte bound the materialized fallback's sink
// applies: the smaller of the definition's retention ceiling and the runtime's
// declared materialized maximum. Both are Harness retention ceilings, so a
// truncation at either records ToolResultTruncatedCaptureCeiling; the codec's
// other reason, ToolResultTruncatedSourceLimit, describes a producer that
// bounded ITSELF, which a materialized ToolResult cannot report.
func materializedCaptureCeiling(ts ToolSet) int {
	return min(ts.MaxToolResultCaptureBytes, ts.MaxMaterializedToolResultBytes)
}

// captureObjectIDPrefix is the fixed version-and-algorithm prefix every capture
// object identity carries.
//
// sessionwire.ObjectReference.Validate accepts any bounded non-empty UTF-8
// string, so keeping object_id free of URLs, credentials and backend keys is
// this producer's obligation. Two mechanisms carry it, and neither is a claim
// about intent. First, captureObjectID's only argument is a digest and its only
// call site passes captureSink.digestHex(), whose value is
// hex.EncodeToString of a SHA-256 sum — so every identity this package mints
// matches captureObjectIDPrefix followed by exactly captureObjectIDHexLen
// lowercase hex characters, which
// TestCaptureObjectIDIsOpaqueForEveryResultContent asserts as a whole-string
// grammar over payloads that ARE signed URLs, credentials, bucket keys and
// filesystem paths. Second,
// TestToolResultCaptureObjectIDIsMintedOnlyByCaptureObjectID walks the module's
// own production files and fails on any other construction, so a second mint
// site added later cannot bypass the first mechanism silently.
//
// The second mechanism is syntactic, and its limit is worth stating because it
// is what the guard would have to be rewritten to exceed. It flags a keyed field,
// an unkeyed element and a post-construction assignment, and it resolves the
// type by bare name against ObjectReference plus every alias or defined type
// declared over it in the files it scans. It therefore does NOT see a value
// whose type reaches ObjectReference only through a declaration outside those
// files — an embedded field, a generic instantiation, or an alias published by a
// dependency — nor a reference this module never constructs at all, such as one
// received already built from another module. Closing those needs a full type
// resolution, which this module does nowhere today.
const captureObjectIDPrefix = "v1:sha256:"

// captureObjectIDHexLen is the exact number of lowercase hex characters a
// SHA-256 digest occupies.
const captureObjectIDHexLen = 64

// captureObjectID mints the opaque logical identity for one retained capture.
// Content addressing is deliberate: two identical results share an identity, and
// an identity can only ever be written with the bytes that produced it, so a Put
// is idempotent and an object can never be rewritten with different content.
func captureObjectID(digestHex string) string {
	return captureObjectIDPrefix + digestHex
}

// newCaptureReference builds the reference recorded on a capture. It takes a
// digest rather than an identity, so no caller-shaped string can be placed in
// object_id through this signature; see captureObjectIDPrefix for the guard that
// holds it to being the module's only mint site, and for what that guard does
// and does not reach.
func newCaptureReference(digestHex string) sessionwire.ObjectReference {
	return sessionwire.ObjectReference{ObjectID: captureObjectID(digestHex)}
}

// ToolResultRetentionStage names the step of the retention pipeline that failed.
// Each stage is reached by exactly one check, so the stage on a
// ToolResultRetentionError identifies the failure rather than merely grouping it.
type ToolResultRetentionStage string

const (
	// ToolResultRetentionStagePut means the object write itself failed.
	ToolResultRetentionStagePut ToolResultRetentionStage = "put"
	// ToolResultRetentionStageStat means the object was written but could not be
	// read back for verification.
	ToolResultRetentionStageStat ToolResultRetentionStage = "stat"
	// ToolResultRetentionStageSize means verification found a different stored
	// byte count than the sink captured.
	ToolResultRetentionStageSize ToolResultRetentionStage = "size_mismatch"
	// ToolResultRetentionStageDigest means verification found different stored
	// content than the sink captured.
	ToolResultRetentionStageDigest ToolResultRetentionStage = "digest_mismatch"
)

// ToolResultRetentionError is the typed terminal cause when a tool ran but its
// complete result could not be retained durably. The loop commits the step —
// including a model-visible notice in place of the result — and then ends the
// turn on it, rather than continuing to another inference with a shaped preview
// whose elided bytes no longer exist anywhere.
type ToolResultRetentionError struct {
	ToolExecutionID uuid.UUID
	ToolUseID       string
	Stage           ToolResultRetentionStage
	Cause           error
}

func (e *ToolResultRetentionError) Error() string {
	return "loop: tool result retention failed at stage " + string(e.Stage)
}

func (e *ToolResultRetentionError) Unwrap() error { return e.Cause }

// toolResultCommit is one step's tool-result outcome: the ToolResultMessages to
// commit, the captures to record on the same StepDone, and — when retention
// failed — the typed cause the turn ends on. captures is empty whenever
// retention is non-nil, because a StepDone's capture list is all-or-nothing per
// step: a partial list cannot be told apart from a truncated one.
type toolResultCommit struct {
	messages  []*content.ToolResultMessage
	captures  []event.ToolResultCapture
	retention *ToolResultRetentionError
}

// retainToolResults runs the durable retention pipeline for one completed step's
// results, in the order the contract requires: encode into the session spill,
// apply the ceiling/count/digest there, write and verify the object, build the
// capture metadata, and only then shape the separate model preview.
//
// A nil ToolResultObjectStore means retention is not configured; the returned
// messages are then exactly what the loop committed before this pipeline
// existed, and no capture is recorded.
//
// The returned error is non-nil only for cancellation, which discards the whole
// step: nothing is committed and no partial capture list is produced.
//
// A StepDone's capture list is all-or-nothing, so the only way to record fewer
// captures than results is to record none. The per-step capture COUNT ceiling
// cannot force that: pkg/event derives maxToolResultCapturesPerStep as
// maxMessagesPerStep-1, and this list holds exactly one entry per committed
// ToolResultMessage, so a step whose message list is itself acceptable always
// has an acceptable capture list. The BYTE ceiling cannot force it either, since
// it truncates one capture rather than removing it. Retention failure is the one
// path that drops the list, and it drops all of it.
func retainToolResults(ctx context.Context, cfg turnConfig, results []result) (toolResultCommit, error) {
	if cfg.toolResultObjects == nil {
		return toolResultCommit{messages: plainToolResultMessages(cfg, results)}, nil
	}
	messages := make([]*content.ToolResultMessage, 0, len(results))
	captures := make([]event.ToolResultCapture, 0, len(results))
	for _, r := range results {
		message, capture, err := retainOneToolResult(ctx, cfg, r)
		if err == nil {
			messages = append(messages, message)
			captures = append(captures, capture)
			continue
		}
		// Cancellation is decided here and nowhere else: a store call that
		// returns because its context died is a cancelled turn, not a storage
		// defect, and a cancelled turn commits nothing at all.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return toolResultCommit{}, ctxErr
		}
		// Every message is rebuilt without a retention marker. A marker on an
		// EARLIER result would otherwise promise a durable object that this
		// step's discarded capture list no longer references.
		return toolResultCommit{
			messages:  retentionFailureMessages(cfg, results, err),
			retention: err,
		}, nil
	}
	return toolResultCommit{messages: messages, captures: captures}, nil
}

// retainOneToolResult retains a single result. The capture it returns is
// reference-free when the committed preview already carries the complete
// retained bytes verbatim — the "below threshold" case, where a separate object
// would hold nothing the journal does not already hold.
func retainOneToolResult(ctx context.Context, cfg turnConfig, r result) (*content.ToolResultMessage, event.ToolResultCapture, *ToolResultRetentionError) {
	sink := newCaptureSink(materializedCaptureCeiling(cfg.tools))
	// A materialized producer hands back a complete ToolResult, so it is written
	// to the sink in one call; the sink, not this caller, decides what survives
	// the ceiling.
	_, _ = sink.Write(rawToolResultBytes(r.Content))
	original := sink.offeredBytes()
	capture := event.ToolResultCapture{
		ToolExecutionID: r.ToolExecutionID,
		ToolUseID:       r.ToolUseID,
		CapturedBytes:   sink.capturedBytes(),
		OriginalBytes:   &original,
		Encoding:        sink.encoding(),
	}
	if sink.truncated() {
		capture.Truncated = true
		capture.TruncationReason = event.ToolResultTruncatedCaptureCeiling
	}
	preview := shapeToolResultText(flattenToText(r.Content), cfg.tools.MaxToolResultBytes)
	if !capture.Truncated && preview == string(sink.bytes()) {
		return toolResultMessageWithText(r, preview), capture, nil
	}
	reference := newCaptureReference(sink.digestHex())
	objectID := reference.ObjectID
	if err := cfg.toolResultObjects.PutToolResultObject(ctx, objectID, sink.bytes()); err != nil {
		return nil, event.ToolResultCapture{}, retentionFailure(r, ToolResultRetentionStagePut, err)
	}
	stat, err := cfg.toolResultObjects.StatToolResultObject(ctx, objectID)
	if err != nil {
		return nil, event.ToolResultCapture{}, retentionFailure(r, ToolResultRetentionStageStat, err)
	}
	if stat.SizeBytes != sink.capturedBytes() {
		return nil, event.ToolResultCapture{}, retentionFailure(r, ToolResultRetentionStageSize, nil)
	}
	if stat.Digest != sink.digestHex() {
		return nil, event.ToolResultCapture{}, retentionFailure(r, ToolResultRetentionStageDigest, nil)
	}
	capture.Reference = &reference
	shaped := shapeCapturedToolResultText(flattenToText(r.Content), cfg.tools.MaxToolResultBytes, toolResultRetainedMarker(capture))
	return toolResultMessageWithText(r, shaped), capture, nil
}

func retentionFailure(r result, stage ToolResultRetentionStage, cause error) *ToolResultRetentionError {
	return &ToolResultRetentionError{
		ToolExecutionID: r.ToolExecutionID,
		ToolUseID:       r.ToolUseID,
		Stage:           stage,
		Cause:           cause,
	}
}

func plainToolResultMessages(cfg turnConfig, results []result) []*content.ToolResultMessage {
	messages := make([]*content.ToolResultMessage, 0, len(results))
	for _, r := range results {
		messages = append(messages, toolResultMessage(r, cfg.tools.MaxToolResultBytes))
	}
	return messages
}

// retentionFailureMessages rebuilds the step's committed tool results after a
// retention failure: the failing result becomes a model-visible error notice,
// and every other result keeps its ordinary shaped text with no retention
// marker.
func retentionFailureMessages(cfg turnConfig, results []result, failure *ToolResultRetentionError) []*content.ToolResultMessage {
	messages := make([]*content.ToolResultMessage, 0, len(results))
	for _, r := range results {
		if r.ToolExecutionID == failure.ToolExecutionID {
			messages = append(messages, toolResultRetentionNotice(r))
			continue
		}
		messages = append(messages, toolResultMessage(r, cfg.tools.MaxToolResultBytes))
	}
	return messages
}

// toolResultRetentionNoticeText is what the model sees in place of a result the
// loop could not retain. It names no object identity, stage or backend detail:
// the model can act on "this result is gone", and the diagnosis rides the typed
// ToolResultRetentionError on the turn terminal instead.
const toolResultRetentionNoticeText = "error: the tool ran, but its complete result could not be retained durably, so the result is unavailable and this turn ended"

func toolResultRetentionNotice(r result) *content.ToolResultMessage {
	return &content.ToolResultMessage{
		Message:   content.Message{Role: content.RoleTool, Blocks: []content.Block{&content.TextBlock{Text: toolResultRetentionNoticeText}}},
		ToolUseID: r.ToolUseID,
		IsError:   true,
	}
}

// toolResultRetainedMarker builds the model-visible retrieval marker for a
// capture that has an object behind it. It names the provider tool_use id, which
// the model already issued, and never the object identity: the model needs to
// know THAT the result is retrievable and by which call, and a retrieval tool
// resolves the identity from the journal rather than from the prompt.
//
// The size is read through ToolResultCapture.OriginalSize, so an inexact count
// is rendered as a lower bound rather than as a fact. The materialized path
// always knows the producer's exact length; the lower-bound rendering exists for
// a streaming producer stopped at the ceiling and is covered directly by
// TestToolResultRetainedMarkerRendersInexactSizeAsLowerBound.
func toolResultRetainedMarker(capture event.ToolResultCapture) string {
	original, exact := capture.OriginalSize()
	size := strconv.FormatUint(original, 10) + " bytes"
	if !exact {
		size = "at least " + size
	}
	// "all N bytes" and "M of N bytes" both name the ORIGINAL count, so a reader
	// never has to combine the marker with anything else to learn what was
	// elided; "at least" is the only difference an inexact count makes.
	retained := "all " + size
	if capture.Truncated {
		retained = strconv.FormatUint(capture.CapturedBytes, 10) + " of " + size
	}
	return fmt.Sprintf("%s; %s retained for tool_use_id %q]\n", toolResultRetainedMarkerPrefix, retained, capture.ToolUseID)
}

// shapeCapturedToolResultText shapes the model preview so the preview PLUS the
// retention marker fits the model budget, rather than shaping to the budget and
// then overrunning it. When limit is zero the model text is unbounded and the
// whole flattened result is previewed. Otherwise the result is at most limit
// bytes whenever limit exceeds len(marker); when it does not, the marker alone
// is returned and the message is len(marker) bytes, because dropping the marker
// would leave the model unable to tell that anything was elided.
func shapeCapturedToolResultText(text string, limit int, marker string) string {
	if limit <= 0 {
		return text + marker
	}
	budget := limit - len(marker)
	if budget <= 0 {
		return marker
	}
	return shapeToolResultText(text, budget) + marker
}

// rawToolResultBytes is the deterministic byte encoding of a materialized tool
// result — what the capture retains, and what a later reader gets back.
//
// It is NOT flattenToText: flattening is the MODEL projection and replaces a
// non-text block with an "[unsupported …]" placeholder, so retaining the
// flattened form would discard exactly the structured payload the capture exists
// to keep. Text blocks contribute their bytes verbatim so an all-text result's
// object is the result itself, and every other block contributes its canonical
// JSON encoding. A block that cannot be marshalled falls back to the same
// placeholder flattening would have produced, so the encoding is total.
func rawToolResultBytes(blocks []content.Block) []byte {
	var buf bytes.Buffer
	appendRawToolResultBytes(&buf, blocks)
	return buf.Bytes()
}

func appendRawToolResultBytes(buf *bytes.Buffer, blocks []content.Block) {
	for _, b := range blocks {
		switch v := b.(type) {
		case *content.TextBlock:
			buf.WriteString(v.Text)
		case *content.ToolResultBlock:
			appendRawToolResultBytes(buf, v.Content)
		default:
			encoded, err := json.Marshal(b)
			if err != nil {
				buf.WriteString("[unsupported " + string(blockTypeOf(b)) + "]")
				continue
			}
			buf.Write(encoded)
		}
	}
}
