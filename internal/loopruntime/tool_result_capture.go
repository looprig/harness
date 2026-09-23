package loopruntime

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
)

// ToolResultObjectStat, ToolResultObjectStore and ToolResultObjectStreamStore are
// ALIASES of the public declarations in pkg/loop, not distinct types. The seam is
// public because a composition root outside this module has to name the store it
// wires, and internal/ types cannot be named from outside github.com/looprig/harness;
// it is aliased rather than re-declared so there is exactly one type in each case
// and no conversion — or divergence — at the boundary.
type (
	ToolResultObjectStat = loop.ToolResultObjectStat
	//lint:ignore SA1019 the deprecated legacy seam is still served until the next major version.
	ToolResultObjectStore       = loop.ToolResultObjectStore
	ToolResultObjectStreamStore = loop.ToolResultObjectStreamStore
)

// ToolResultPublisher is the SESSION-BOUND view of loop.ToolResultObjects the
// loop runtime publishes through. The session runtime binds the session id
// before a loop ever sees the seam, so no loop — and no tool — can name a
// session other than its own. The store, not the loop, mints the returned
// reference; see retainPublishedObject for what the loop verifies about it.
type ToolResultPublisher interface {
	PublishToolResultObject(ctx context.Context, content io.Reader, size uint64, sum [32]byte) (sessionwire.ObjectMetadata, error)
}

// objectDigestPrefix is the algorithm prefix a store-issued ObjectMetadata.Digest
// carries before the lowercase hex SHA-256.
const objectDigestPrefix = "sha256:"

// toolResultRetainedMarkerPrefix opens every model-visible retention marker. It
// is a distinct literal from toolResultTruncatedMarker so a reader (and a test)
// can tell "the preview was shaped and the rest is retained" from "the preview
// was shaped and nothing else exists".
// It is PROMPT TEXT, and no consumer may parse it. Its wording, its punctuation
// and its very presence are model-facing choices that will change; the
// machine-readable channel for a retained capture is StepDone.Captures, which
// carries the object reference, the byte counts and the truncation reason as
// typed fields. A downstream reader that matched this string would be reading a
// prompt.
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
	// ToolResultRetentionStageSpill means the local session spill could not be
	// established or could not hold what the producer supplied — it was never
	// opened, or its backing failed or short-wrote part way through. Nothing is
	// uploaded for such a capture: the counts and the digest the sink recorded no
	// longer describe anything that exists, so a Put would store bytes the
	// verification stages would then reject for the wrong reason.
	ToolResultRetentionStageSpill ToolResultRetentionStage = "spill"
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
	// ToolResultRetentionStageReference means a publishing store returned a
	// reference that does not validate, so there is nothing safe to record.
	ToolResultRetentionStageReference ToolResultRetentionStage = "invalid_reference"
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
	// Every streamed capture's local spill is discarded when this returns,
	// whatever it returns: after a verified upload it has served its purpose,
	// after a failure the turn ends, and after cancellation nothing is committed.
	// retainOneToolResult releases its own sink too; release is idempotent, so
	// the two owners need not coordinate.
	defer releaseCaptures(results)
	if !retentionConfigured(cfg) {
		return toolResultCommit{messages: plainToolResultMessages(cfg, results)}, nil
	}
	readable := cfg.toolResultPublisher != nil && toolSetHasReader(ctx, cfg.tools)
	messages := make([]*content.ToolResultMessage, 0, len(results))
	captures := make([]event.ToolResultCapture, 0, len(results))
	for _, r := range results {
		message, capture, err := retainOneToolResult(ctx, cfg, r, readable)
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
//
// The sink is EITHER the one a capturing tool already streamed into while it ran
// (r.capture, opened by the runner before the call) or one built here from the
// materialized ToolResult. Both are captureSinks over the same session spill, so
// the ceiling, the counts, the digest and the encoding are decided in one place
// whichever producer the tool was. The local spill is released on every path:
// after a verified upload it has served its purpose, and after a failed one the
// turn ends, so it has no reader either.
func retainOneToolResult(ctx context.Context, cfg turnConfig, r result, readable bool) (*content.ToolResultMessage, event.ToolResultCapture, *ToolResultRetentionError) {
	sink := r.capture
	if sink == nil {
		sink = materializedCaptureSink(cfg, r)
	}
	defer func() { _ = sink.release() }()
	if err := sink.spillErr(); err != nil {
		return nil, event.ToolResultCapture{}, retentionFailure(r, ToolResultRetentionStageSpill, err)
	}
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
	// The retained prefix is only read back when it could possibly EQUAL the
	// preview, which requires the two to be the same length. That keeps the
	// below-threshold check from materializing a large spill merely to discover
	// it is large.
	if !capture.Truncated && sink.capturedBytes() == uint64(len(preview)) {
		retained, err := sink.materialize()
		if err != nil {
			return nil, event.ToolResultCapture{}, retentionFailure(r, ToolResultRetentionStageSpill, err)
		}
		if preview == string(retained) {
			return toolResultMessageWithText(r, preview), capture, nil
		}
	}
	var (
		reference sessionwire.ObjectReference
		failure   *ToolResultRetentionError
	)
	if cfg.toolResultPublisher != nil {
		reference, failure = retainPublishedObject(ctx, cfg.toolResultPublisher, r, sink)
	} else {
		reference, failure = retainMintedObject(ctx, cfg.toolResultObjects, r, sink)
	}
	if failure != nil {
		return nil, event.ToolResultCapture{}, failure
	}
	capture.Reference = &reference
	shaped := shapeCapturedToolResultText(flattenToText(r.Content), cfg.tools.MaxToolResultBytes, toolResultRetainedMarker(capture, readable))
	return toolResultMessageWithText(r, shaped), capture, nil
}

// retentionConfigured reports whether this turn retains tool results at all:
// either seam turns retention on, and the publisher wins when both are set.
func retentionConfigured(cfg turnConfig) bool {
	return cfg.toolResultPublisher != nil || cfg.toolResultObjects != nil
}

// retainPublishedObject is the READABLE retention path: the store mints the
// reference and returns it with the metadata it recorded. Nothing is taken on
// trust. The returned size and digest must be exactly what the sink captured —
// a store that reported another object's metadata, or rewrote the bytes, must
// not be recorded as having retained this one — and the reference must
// validate, since it is written verbatim into the public journal.
//
// Stat is not called on this path: the publisher's own contract is to verify the
// persisted bytes before returning (SessionStore re-reads them), and the
// returned metadata is what the loop checks.
func retainPublishedObject(ctx context.Context, publisher ToolResultPublisher, r result, sink *captureSink) (sessionwire.ObjectReference, *ToolResultRetentionError) {
	digestHex := sink.digestHex()
	var sum [32]byte
	if _, err := hex.Decode(sum[:], []byte(digestHex)); err != nil {
		return sessionwire.ObjectReference{}, retentionFailure(r, ToolResultRetentionStageSpill, err)
	}
	reader, err := sink.reader()
	if err != nil {
		return sessionwire.ObjectReference{}, retentionFailure(r, ToolResultRetentionStageSpill, err)
	}
	defer func() { _ = reader.Close() }()
	metadata, err := publisher.PublishToolResultObject(ctx, reader, sink.capturedBytes(), sum)
	if err != nil {
		return sessionwire.ObjectReference{}, retentionFailure(r, ToolResultRetentionStagePut, err)
	}
	if metadata.SizeBytes != sink.capturedBytes() {
		return sessionwire.ObjectReference{}, retentionFailure(r, ToolResultRetentionStageSize, nil)
	}
	if metadata.Digest != objectDigestPrefix+digestHex {
		return sessionwire.ObjectReference{}, retentionFailure(r, ToolResultRetentionStageDigest, nil)
	}
	if err := metadata.Reference.Validate(); err != nil {
		return sessionwire.ObjectReference{}, retentionFailure(r, ToolResultRetentionStageReference, err)
	}
	return metadata.Reference, nil
}

// retainMintedObject is the LEGACY retention path over the deprecated
// ToolResultObjectStore: the loop mints a content-addressed identity no session
// object store can resolve, writes under it, and verifies by Stat. It is kept so
// an existing composition keeps committing what it committed before; its
// captures are never readable.
func retainMintedObject(ctx context.Context, store ToolResultObjectStore, r result, sink *captureSink) (sessionwire.ObjectReference, *ToolResultRetentionError) {
	reference := newCaptureReference(sink.digestHex())
	objectID := reference.ObjectID
	if stage, err := putCapturedObject(ctx, store, objectID, sink); err != nil {
		return sessionwire.ObjectReference{}, retentionFailure(r, stage, err)
	}
	stat, err := store.StatToolResultObject(ctx, objectID)
	if err != nil {
		return sessionwire.ObjectReference{}, retentionFailure(r, ToolResultRetentionStageStat, err)
	}
	if stat.SizeBytes != sink.capturedBytes() {
		return sessionwire.ObjectReference{}, retentionFailure(r, ToolResultRetentionStageSize, nil)
	}
	if stat.Digest != sink.digestHex() {
		return sessionwire.ObjectReference{}, retentionFailure(r, ToolResultRetentionStageDigest, nil)
	}
	return reference, nil
}

// toolSetHasReader reports whether the loop's current tool set carries the
// read_tool_result tool, which is what licenses the marker to tell the model to
// call it. A tool whose Info fails is treated as absent: the marker must never
// instruct a call the model cannot make.
func toolSetHasReader(ctx context.Context, ts ToolSet) bool {
	for _, t := range ts.Registry {
		if t == nil {
			continue
		}
		info, err := t.Info(ctx)
		if err == nil && info != nil && info.Name == loop.ReadToolResultToolName {
			return true
		}
	}
	return false
}

// materializedCaptureSink is the fallback producer path: a materialized tool
// hands back a complete ToolResult, so the whole encoding is written to the sink
// in one call and the sink, not this caller, decides what survives the ceiling.
//
// With a session spill directory configured the retained prefix goes to disk like
// a streamed one; without one it stays in memory, bounded by the same ceiling.
// The memory backing is not a second policy — it is what a composition that never
// wired a spill base gets, and pkg/rig's public option refuses to build one.
// A spill that cannot be opened produces a sink that still accepts and counts
// every byte, so the tool's own outcome is unchanged and the failure surfaces as
// a retention failure at the spill stage rather than as a tool error.
func materializedCaptureSink(cfg turnConfig, r result) *captureSink {
	ceiling := materializedCaptureCeiling(cfg.tools)
	sink := newCaptureSink(ceiling)
	if cfg.toolResultSpills != nil {
		spilled, err := cfg.toolResultSpills.openSink(r.ToolExecutionID, ceiling)
		if err != nil {
			sink = newFailedCaptureSink(ceiling, err)
		} else {
			sink = spilled
		}
	}
	_, _ = sink.Write(rawToolResultBytes(r.Content))
	return sink
}

// putCapturedObject writes the retained prefix to the object store, preferring
// the optional streaming capability. The returned stage distinguishes a failure
// reading the LOCAL spill from a failure in the STORE, because they are different
// faults with different operator responses and an identical error code is the
// most common mask for a survivor.
//
// The streaming path hands the store a rewound reader over the spill and never
// materializes the capture, which is what keeps a large result out of host
// memory. A store without the capability gets the bounded materialized Put; the
// bound is the capture ceiling, so the fallback is resident but never unbounded.
func putCapturedObject(ctx context.Context, store ToolResultObjectStore, objectID string, sink *captureSink) (ToolResultRetentionStage, error) {
	if streamer, ok := store.(ToolResultObjectStreamStore); ok {
		reader, err := sink.reader()
		if err != nil {
			return ToolResultRetentionStageSpill, err
		}
		defer func() { _ = reader.Close() }()
		if err := streamer.PutToolResultObjectStream(ctx, objectID, reader, sink.capturedBytes()); err != nil {
			return ToolResultRetentionStagePut, err
		}
		return "", nil
	}
	retained, err := sink.materialize()
	if err != nil {
		return ToolResultRetentionStageSpill, err
	}
	if err := store.PutToolResultObject(ctx, objectID, retained); err != nil {
		return ToolResultRetentionStagePut, err
	}
	return "", nil
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

// toolResultRetainedMarker builds the model-visible retention marker for a
// capture that has an object behind it.
//
// It says three things, in order. How much of the producer's output was
// retained ("all N bytes" or "M of N bytes"). When the capture is truncated,
// that the elided tail is UNAVAILABLE and why — without this a model reads "M of
// N retained" as "the rest is somewhere" and goes looking. And, only when
// readable is true, how to page through what was retained: the reader tool's
// name and the capture id, which is the capture's ToolExecutionID.
//
// readable is true only when the capture's reference was issued by a store
// that can resolve it AND the calling loop has the reader tool bound. The
// marker must never instruct a call the model cannot make. It names neither the
// object identity nor the provider tool_use_id: the first is opaque and useless
// to the model, and the second is not unique within a session.
//
// The size is read through ToolResultCapture.OriginalSize, so an inexact count
// is rendered as a lower bound rather than as a fact. The materialized path
// always knows the producer's exact length; the lower-bound rendering exists for
// a streaming producer stopped at the ceiling and is covered directly by
// TestToolResultRetainedMarkerRendersInexactSizeAsLowerBound.
func toolResultRetainedMarker(capture event.ToolResultCapture, readable bool) string {
	original, exact := capture.OriginalSize()
	lowerBound := ""
	if !exact {
		lowerBound = "at least "
	}
	var b strings.Builder
	b.WriteString(toolResultRetainedMarkerPrefix)
	b.WriteString("; ")
	if capture.Truncated {
		b.WriteString(strconv.FormatUint(capture.CapturedBytes, 10) + " of ")
	} else {
		b.WriteString("all ")
	}
	b.WriteString(lowerBound + strconv.FormatUint(original, 10) + " bytes retained")
	if capture.Truncated && original > capture.CapturedBytes {
		b.WriteString("; " + lowerBound + "the last " + strconv.FormatUint(original-capture.CapturedBytes, 10) + " bytes ")
		if capture.TruncationReason == event.ToolResultTruncatedSourceLimit {
			b.WriteString("were not supplied by the tool")
		} else {
			b.WriteString("exceeded the capture ceiling")
		}
		b.WriteString(" and are unavailable")
	}
	if readable {
		if capture.Truncated {
			b.WriteString("; read the retained bytes with ")
		} else {
			b.WriteString("; read the rest with ")
		}
		b.WriteString(loop.ReadToolResultToolName + " capture_id=" + strconv.Quote(capture.ToolExecutionID.String()))
	}
	b.WriteString("]\n")
	return b.String()
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

// turnCaptureSinks builds the per-call capture sink factory for one turn, or nil
// when this loop has no retention configured — the runner then keeps every tool
// on the ordinary InvokableRun path, so a composition without a store behaves
// exactly as it did before the streaming capability existed.
//
// Without a spill directory the sink is memory-backed: still bounded by the same
// ceiling, but resident. That is the honest cost of wiring a store without a
// spill base, which pkg/rig's public option does not allow.
func turnCaptureSinks(cfg turnConfig) func(uuid.UUID) *captureSink {
	if !retentionConfigured(cfg) {
		return nil
	}
	ceiling := materializedCaptureCeiling(cfg.tools)
	if cfg.toolResultSpills == nil {
		return func(uuid.UUID) *captureSink { return newCaptureSink(ceiling) }
	}
	return spillCaptureSinks(cfg.toolResultSpills, ceiling)
}
