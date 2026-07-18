package sessionruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
)

// extStubTool is a minimal external tool with a fixed name and schema.
type extStubTool struct{ name string }

func (s *extStubTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: s.name, Desc: "d", Schema: json.RawMessage(`{"a":1}`)}, nil
}

func (s *extStubTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return &tool.ToolResult{Content: []content.Block{&content.TextBlock{Text: "ok"}}}, nil
}

// extDefinition builds an external tool definition producing one named tool.
func extDefinition(name string) tool.Definition {
	return tool.NewDefinition(name, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{&extStubTool{name: name}}, nil
	})
}

// failingDefinition builds a definition whose factory always fails — the atomicity probe.
func failingDefinition(name string, err error) tool.Definition {
	return tool.NewDefinition(name, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return nil, err
	})
}

// toolCfg builds a definition declaring one tool named "declared", so the collision rule
// has something to protect.
func toolCfg(client *stubLLM) loop.Definition {
	return mustDefine(
		loop.WithName("agent"),
		loop.WithInference(client, validModel("base")),
		loop.WithSystem("base"),
		loop.WithTools(extDefinition("declared")),
		loop.WithDrainTimeout(100*time.Millisecond),
	)
}

func installerFor(t *testing.T, s *Session) loop.ExternalToolInstaller {
	t.Helper()
	ctrl, ok := s.LoopController(s.ActiveLoopID())
	if !ok {
		t.Fatal("LoopController not found")
	}
	// The capability is discovered by type assertion (the optional-interface pattern),
	// exactly as a composing application discovers it.
	installer, ok := ctrl.(loop.ExternalToolInstaller)
	if !ok {
		t.Fatal("loop controller does not implement loop.ExternalToolInstaller")
	}
	return installer
}

func newToolSession(t *testing.T) *Session {
	t.Helper()
	s, err := newTestSession(context.Background(), toolCfg(&stubLLM{chunks: []content.Chunk{textChunk("ok")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s
}

// countingExtTool is an external tool that records how often it actually ran.
type countingExtTool struct {
	name string
	mu   sync.Mutex
	n    int
}

func (c *countingExtTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: c.name, Desc: "d", Schema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (c *countingExtTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return &tool.ToolResult{Content: []content.Block{&content.TextBlock{Text: "ok"}}}, nil
}

func (c *countingExtTool) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// scriptedToolLLM streams a call to toolName on its FIRST request and plain text on every
// one after, so a single Submit drives exactly one external-tool execution and then
// terminates. stubLLM cannot do this: it replays the same chunks on every stream, so a
// tool-use chunk would re-issue forever.
type scriptedToolLLM struct {
	toolName string
	mu       sync.Mutex
	calls    int
}

func (s *scriptedToolLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("scriptedToolLLM.Invoke not used")
}

func (s *scriptedToolLLM) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	s.mu.Lock()
	first := s.calls == 0
	s.calls++
	s.mu.Unlock()

	chunks := []content.Chunk{textChunk("done")}
	if first {
		chunks = []content.Chunk{&content.ToolUseChunk{Index: 0, ID: "call-1", Name: s.toolName, InputJSON: `{}`}}
	}
	i := 0
	next := func() (content.Chunk, error) {
		if i < len(chunks) {
			c := chunks[i]
			i++
			return c, nil
		}
		return nil, io.EOF
	}
	return stream.NewStreamReader(next, nil), nil
}

// capturingDefinition records the tool.Bindings its factory was handed, and declares
// `requires` so tool.Definition.Build enforces them against those bindings. It is the
// probe for what a loop was actually provisioned with, as an external definition sees it.
type capturingDefinition struct {
	mu   sync.Mutex
	seen tool.Bindings
	got  bool
}

func (c *capturingDefinition) definition(t tool.InvokableTool, name string, requires tool.Requirements) tool.Definition {
	return tool.NewDefinition(name, requires, func(_ context.Context, b tool.Bindings) ([]tool.InvokableTool, error) {
		c.mu.Lock()
		c.seen, c.got = b, true
		c.mu.Unlock()
		return []tool.InvokableTool{t}, nil
	})
}

func (c *capturingDefinition) bindings(t *testing.T) tool.Bindings {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.got {
		t.Fatal("the factory was never called: nothing was built")
	}
	return c.seen
}

// TestRestoredRootLoopBindsExternalTools is the restored root's provisioning: a root loop
// brought up by Restore must carry the SAME tool.Bindings its declared tools were bound
// with in planLoops, so a later external-toolset replacement builds against the
// capabilities the loop actually has.
//
// This is not a cosmetic field. loopHandle.buildExternalTools calls def.Build(ctx,
// h.bindings), and Build validates a definition's Requirements against those bindings. A
// restored root with zero bindings therefore cannot build ANY external definition that
// declares a requirement — it fails closed with a *tool.MissingBindingError, and MCP tools
// (which is what the external-toolset seam exists for) can never be installed on a
// restored session at all. It also silently defeats loop_tools.go's stated invariant that
// "production builds full tool.Bindings for every engine".
//
// RequiresDelegateController is the probe: planLoops binds every loop with a non-nil
// scoped controller regardless of whether a workspace is configured, so this asserts the
// binding is threaded without needing a materialized workspace.
//
// Mutation check: drop `bindings: bindings` from the &loopHandle{} literal in
// buildRestoredSession (restore_constructor.go) and the install fails with
// ChangeExternalBuildFailed wrapping MissingBindingError; the identity assertions below
// then read a zero SessionID/LoopID.
func TestRestoredRootLoopBindsExternalTools(t *testing.T) {
	store := newRestoreStore(t)
	// restoreCfg plus an auto-approving permission gate: without one the loop's
	// fail-secure default denies the call, and an installed tool that is never dispatched
	// would make the execution assertion below vacuous. It still fingerprints identically
	// to restoreCfg (testFingerprintProvider reads model + system + tool names, none of
	// which this changes), so the restore takes the matching-config path and is exercising
	// the bindings rather than a mismatch.
	definition := func(client inference.Client) loop.Definition {
		return mustDefine(
			loop.WithName("agent"),
			loop.WithInference(client, validModel("model-x")),
			loop.WithSystem("be helpful"),
			loop.WithDrainTimeout(200*time.Millisecond),
			loop.WithPolicyRevision("test"),
			loop.WithPermissionFactory(func(context.Context, tool.Bindings) (loop.PermissionGate, error) {
				return newLivePermissionGate(loop.EffectAutoApprove), nil
			}),
		)
	}

	fp := fingerprintFromDefinition(restoreCfg(&stubLLM{}, "model-x", "be helpful"))
	orig := buildOriginalRun(t, store, fp, restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful"), 1)
	handOver(t, orig.lease)

	// The restored session's client calls the external tool on its first turn, so the
	// install is proven by execution rather than by the registry's own bookkeeping.
	ext := &countingExtTool{name: "mcp_tool"}
	s, err := restoreTestSession(context.Background(), definition(&scriptedToolLLM{toolName: "mcp_tool"}),
		orig.sessionID, store)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// The restored ROOT loop is the subject: Restore brings it up under its original id.
	if s.ActiveLoopID() != orig.rootLoopID {
		t.Fatalf("restored active loop = %v, want the root %v", s.ActiveLoopID(), orig.rootLoopID)
	}
	installer := installerFor(t, s)

	// A definition that DECLARES a requirement — the case a zero-bindings root cannot
	// serve. This is the shape a real MCP toolset has.
	capture := &capturingDefinition{}
	if err := installer.ReplaceExternalTools(context.Background(), loop.ExternalToolset{
		Source: "mcp", Generation: "g1",
		Definitions: []tool.Definition{capture.definition(ext, "mcp_tool", tool.RequiresDelegateController)},
	}); err != nil {
		t.Fatalf("a restored root could not install an external toolset: %v", err)
	}

	// The bindings the external factory was handed are the restored loop's own identity —
	// not a zero value, and not some other loop's.
	got := capture.bindings(t)
	if got.SessionID != orig.sessionID {
		t.Errorf("external Bindings.SessionID = %v, want the restored session %v", got.SessionID, orig.sessionID)
	}
	if got.LoopID != orig.rootLoopID {
		t.Errorf("external Bindings.LoopID = %v, want the restored root %v", got.LoopID, orig.rootLoopID)
	}
	// Delegate is the requirement this definition declared, so Build's attenuation carries
	// it through. (SecurityLimit/Workspace are deliberately NOT asserted: attenuateBindings
	// passes a factory only SessionID, LoopID, and what it declared.)
	if got.Delegate == nil {
		t.Error("external Bindings.Delegate is nil: the restored root was bound without its scoped controller")
	}

	// Installed AND runnable: the next turn on the restored loop dispatches a model tool
	// call to the instance the factory built. An install that cannot be executed would be
	// bookkeeping, not a tool.
	submitAndDrain(t, s, []content.Block{&content.TextBlock{Text: "use the tool"}})
	if n := ext.count(); n != 1 {
		t.Errorf("the installed external tool ran %d times, want 1 — a restored loop's external tool must be dispatchable, not just advertised", n)
	}
}

// TestReplaceExternalToolsBuildFailureIsAtomic is the no-partial-swap property: when ANY
// definition's Build fails, nothing is installed — not even the definitions that built
// fine before it — and the prior generation stays.
//
// Mutation check: building incrementally into the live slot (installing each definition
// as it builds, rather than building the whole batch first) leaves "ok_one" installed and
// the follow-up assertion that the good generation is untouched fails.
func TestReplaceExternalToolsBuildFailureIsAtomic(t *testing.T) {
	t.Parallel()
	s := newToolSession(t)
	installer := installerFor(t, s)

	// Install a good generation first, so a partial swap would be observable as damage.
	if err := installer.ReplaceExternalTools(context.Background(), loop.ExternalToolset{
		Source: "mcp", Generation: "g1", Definitions: []tool.Definition{extDefinition("keep_me")},
	}); err != nil {
		t.Fatalf("first install: %v", err)
	}

	boom := errors.New("factory exploded")
	err := installer.ReplaceExternalTools(context.Background(), loop.ExternalToolset{
		Source: "mcp", Generation: "g2",
		Definitions: []tool.Definition{extDefinition("ok_one"), failingDefinition("bad", boom)},
	})
	var changeErr *loop.ChangeError
	if !errors.As(err, &changeErr) || changeErr.Kind != loop.ChangeExternalBuildFailed {
		t.Fatalf("err = %v, want ChangeExternalBuildFailed", err)
	}
	if changeErr.Tool != "bad" {
		t.Errorf("ChangeError.Tool = %q, want bad", changeErr.Tool)
	}
	if !errors.Is(err, boom) {
		t.Errorf("typed error must wrap the factory cause; got %v", err)
	}

	// The prior generation must still be installed and replaceable — proof the failed
	// batch touched nothing.
	if err := installer.ReplaceExternalTools(context.Background(), loop.ExternalToolset{
		Source: "mcp", Generation: "g3", Definitions: []tool.Definition{extDefinition("keep_me")},
	}); err != nil {
		t.Fatalf("slot damaged by the failed replacement: %v", err)
	}
}

// TestReplaceExternalToolsRejectsDeclaredCollision is the shadowing rule: an external
// tool must never take the name of a DECLARED tool. Fail closed — refuse the whole
// replacement rather than namespace it or let it win.
//
// Mutation check: deleting checkDeclaredCollision lets "declared" install and this fails.
func TestReplaceExternalToolsRejectsDeclaredCollision(t *testing.T) {
	t.Parallel()
	s := newToolSession(t)
	installer := installerFor(t, s)

	err := installer.ReplaceExternalTools(context.Background(), loop.ExternalToolset{
		Source: "mcp", Generation: "g1",
		Definitions: []tool.Definition{extDefinition("safe"), extDefinition("declared")},
	})
	var changeErr *loop.ChangeError
	if !errors.As(err, &changeErr) || changeErr.Kind != loop.ChangeExternalToolCollision {
		t.Fatalf("err = %v, want ChangeExternalToolCollision", err)
	}
	if changeErr.Tool != "declared" {
		t.Errorf("ChangeError.Tool = %q, want declared", changeErr.Tool)
	}
}

// TestReplaceExternalToolsRejectsDuplicateWithinBatch covers the within-replacement
// collision: two external definitions producing the same model-facing name.
//
// Mutation check: dropping the `seen` map in buildExternalTools admits the duplicate,
// producing a registry with two identically named tools and a durable record the
// event validator would then reject.
func TestReplaceExternalToolsRejectsDuplicateWithinBatch(t *testing.T) {
	t.Parallel()
	s := newToolSession(t)
	installer := installerFor(t, s)

	err := installer.ReplaceExternalTools(context.Background(), loop.ExternalToolset{
		Source: "mcp", Generation: "g1",
		Definitions: []tool.Definition{extDefinition("dup"), extDefinition("dup")},
	})
	var changeErr *loop.ChangeError
	if !errors.As(err, &changeErr) || changeErr.Kind != loop.ChangeExternalToolCollision {
		t.Fatalf("err = %v, want ChangeExternalToolCollision", err)
	}
}

func TestReplaceExternalToolsRejectsReservedNameBeforeBuildingBatch(t *testing.T) {
	t.Parallel()
	s := newToolSession(t)
	installer := installerFor(t, s)

	var builds int
	definition := func(name string) tool.Definition {
		return tool.NewDefinition(name, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			builds++
			return []tool.InvokableTool{&extStubTool{name: name}}, nil
		})
	}
	err := installer.ReplaceExternalTools(context.Background(), loop.ExternalToolset{
		Source: "mcp", Generation: "g1",
		Definitions: []tool.Definition{definition("safe"), definition(inference.StructuredOutputToolName)},
	})
	var changeErr *loop.ChangeError
	if !errors.As(err, &changeErr) || changeErr.Kind != loop.ChangeExternalToolCollision {
		t.Fatalf("err = %T %v, want ChangeExternalToolCollision", err, err)
	}
	if changeErr.Tool != inference.StructuredOutputToolName {
		t.Fatalf("ChangeError.Tool = %q, want reserved name", changeErr.Tool)
	}
	if builds != 0 {
		t.Fatalf("external factories ran %d times, want preflight rejection before any build", builds)
	}

	// A failed reserved-name generation cannot partially occupy the source slot.
	if err := installer.ReplaceExternalTools(context.Background(), loop.ExternalToolset{
		Source: "mcp", Generation: "g2", Definitions: []tool.Definition{extDefinition("safe")},
	}); err != nil {
		t.Fatalf("valid generation after reserved collision: %v", err)
	}
}

// TestReplaceExternalToolsValidatesRequest covers the boundary validation: an unnamed
// source or generation is refused before anything is built.
func TestReplaceExternalToolsValidatesRequest(t *testing.T) {
	t.Parallel()
	s := newToolSession(t)
	installer := installerFor(t, s)

	tests := []struct {
		name string
		set  loop.ExternalToolset
		want loop.ChangeErrorKind
	}{
		{name: "empty source", set: loop.ExternalToolset{Generation: "g1"}, want: loop.ChangeInvalidExternalSource},
		{name: "over-long source", set: loop.ExternalToolset{Source: longString(65), Generation: "g1"}, want: loop.ChangeInvalidExternalSource},
		{name: "empty generation", set: loop.ExternalToolset{Source: "mcp"}, want: loop.ChangeInvalidExternalGeneration},
		{name: "over-long generation", set: loop.ExternalToolset{Source: "mcp", Generation: longString(129)}, want: loop.ChangeInvalidExternalGeneration},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := installer.ReplaceExternalTools(context.Background(), tt.set)
			var changeErr *loop.ChangeError
			if !errors.As(err, &changeErr) || changeErr.Kind != tt.want {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestReplaceExternalToolsNilDefinitionRefused is the defensive edge: a nil element in a
// sealed-interface slice would panic on projection, so it must be refused, not dereferenced.
func TestReplaceExternalToolsNilDefinitionRefused(t *testing.T) {
	t.Parallel()
	s := newToolSession(t)
	installer := installerFor(t, s)

	err := installer.ReplaceExternalTools(context.Background(), loop.ExternalToolset{
		Source: "mcp", Generation: "g1", Definitions: []tool.Definition{nil},
	})
	var changeErr *loop.ChangeError
	if !errors.As(err, &changeErr) || changeErr.Kind != loop.ChangeExternalBuildFailed {
		t.Fatalf("err = %v, want ChangeExternalBuildFailed", err)
	}
}

// TestReplaceExternalToolsRequiresProvisionedCapability is the privilege rule: an external
// definition demanding a capability this loop was never provisioned with (here a
// workspace) fails closed at Build rather than binding a nil capability. External tools
// are offered exactly the bindings the declared tools got — they can never escalate.
//
// Mutation check: passing a fabricated tool.Bindings (or skipping Build's validation)
// would let a workspace-requiring external tool install into a workspace-less loop.
func TestReplaceExternalToolsRequiresProvisionedCapability(t *testing.T) {
	t.Parallel()
	s := newToolSession(t) // no workspace configured
	installer := installerFor(t, s)

	workspaceTool := tool.NewDefinition("needs_ws", tool.RequiresWorkspace, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{&extStubTool{name: "needs_ws"}}, nil
	})
	err := installer.ReplaceExternalTools(context.Background(), loop.ExternalToolset{
		Source: "mcp", Generation: "g1", Definitions: []tool.Definition{workspaceTool},
	})
	var changeErr *loop.ChangeError
	if !errors.As(err, &changeErr) || changeErr.Kind != loop.ChangeExternalBuildFailed {
		t.Fatalf("err = %v, want ChangeExternalBuildFailed (a missing binding must fail closed)", err)
	}
	var missing *tool.MissingBindingError
	if !errors.As(err, &missing) {
		t.Errorf("cause = %v, want *tool.MissingBindingError", err)
	}
}

// TestReplaceExternalToolsHappyPath is the happy path through the public seam.
func TestReplaceExternalToolsHappyPath(t *testing.T) {
	t.Parallel()
	s := newToolSession(t)
	installer := installerFor(t, s)

	if err := installer.ReplaceExternalTools(context.Background(), loop.ExternalToolset{
		Source: "mcp", Generation: "g1", Definitions: []tool.Definition{extDefinition("search"), extDefinition("fetch")},
	}); err != nil {
		t.Fatalf("ReplaceExternalTools: %v", err)
	}
	// Clearing the slot is legal.
	if err := installer.ReplaceExternalTools(context.Background(), loop.ExternalToolset{
		Source: "mcp", Generation: "g2",
	}); err != nil {
		t.Fatalf("clear: %v", err)
	}
}

func longString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

// TestFoldLoopExternalToolsetInvalidatesContext pins the restore fold against the LIVE
// commit path. The actor emits LoopExternalToolsetChanged through the same
// context-mutation path as a mode change (the toolset rides in the inference request, so
// a cached token measurement is stale). If the fold did not mirror that, a restored loop
// would come up believing a stale measurement, and its context basis would disagree with
// the one the live loop had.
//
// Mutation check: deleting the event.LoopExternalToolsetChanged arm from foldLoop's
// switch leaves HasContext true and fails here.
func TestFoldLoopExternalToolsetInvalidatesContext(t *testing.T) {
	t.Parallel()
	measured := foldContextMeasurement(1)
	got := foldLoop([]event.Event{
		event.ContextMeasured{Measurement: measured},
		event.LoopExternalToolsetChanged{Source: "mcp", Generation: "g1"},
	})
	if got.Err != nil {
		t.Fatalf("fold error: %v", got.Err)
	}
	if got.HasContext {
		t.Fatalf("context = %#v, want invalidated by an external toolset replacement", got.Context)
	}
}

// TestFoldLoopInferenceIgnoresExternalToolset is the restore-with-an-empty-slot contract:
// external tools are LIVE resources (an MCP connection cannot be rebuilt from journal
// bytes), so the durable record must not restore a toolset, nor disturb the restored
// mode/runtime. The composing application re-installs after restore.
//
// Mutation check: folding the record back into a mode/runtime selection (or into a
// reconstructed slot) changes this result.
func TestFoldLoopInferenceIgnoresExternalToolset(t *testing.T) {
	t.Parallel()
	events := []event.Event{
		event.LoopModeChanged{Mode: "build", Runtime: event.ModelRuntime{Key: model.ModelKey{Provider: "provider", Model: "model"}}},
		event.LoopExternalToolsetChanged{Source: "mcp", Generation: "g1", Tools: []event.ExternalToolIdentity{{Name: "search"}}},
	}
	got := foldLoopInference(events)
	if !got.HasMode || got.Mode != "build" {
		t.Fatalf("mode = %q (has=%v), want build — an external replacement must not disturb the mode", got.Mode, got.HasMode)
	}
}

// TestRestoredLoopComesUpWithEmptySlot is the end-to-end restore contract: a journal
// carrying LoopExternalToolsetChanged restores WITHOUT error, and the restored loop's
// external slot is empty — proven by re-installing the same source/name that the journal
// records as previously installed. If the slot had been reconstructed from the journal,
// that name would already be present and the cross-source/duplicate rules would make this
// observable; more importantly a restore that tried to rebuild live tools would fail.
func TestRestoredLoopComesUpWithEmptySlot(t *testing.T) {
	t.Parallel()
	ev := event.LoopExternalToolsetChanged{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: uuid.UUID{1}, LoopID: uuid.UUID{2}}},
		Source: "mcp", Generation: "g1",
		Tools: []event.ExternalToolIdentity{{Name: "search", SchemaDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}},
	}
	// The fold tolerates the record (no error, nothing reconstructed) — restore never
	// treats it as a source of tools.
	if got := foldLoop([]event.Event{ev}); got.Err != nil {
		t.Fatalf("restore fold rejected a durable external-toolset record: %v", got.Err)
	}
	if got := foldLoopInference([]event.Event{ev}); got.HasMode || got.HasRuntime {
		t.Fatalf("external toolset record leaked into the restored inference view: %+v", got)
	}
}

// TestReplaceExternalToolsRefusedOnForeignLoop drives a foreign-engine loop through the
// public builder seam, not a hand-built handle. A foreign loop's toolset belongs to the
// foreign agent and its backend
// has no ReplaceLoopExternalTools arm, so the command would be dropped and the caller would
// block on the ack forever. The refusal must therefore be structural, not incidental.
//
// The bounded context is a HANG DETECTOR, not the mechanism under test: a correct
// implementation refuses immediately without ever consulting it. Against the previous
// h.bindings.LoopID.IsZero() guard this test fails — production builds full bindings for
// EVERY engine, so that guard never fired and this returned ChangeContextDone (an
// unbounded hang with a context.Background() caller) instead of the typed refusal.
func TestReplaceExternalToolsRefusedOnForeignLoop(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	builder := &fakeForeignBuilder{sid: fixedForeignSID, backend: newFakeBackend()}
	definition := engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("unused")}}, loop.EngineForeignClaude, "sys")
	s, err := newTestSession(ctx, definition,
		WithForeignBuilders(builder.build, builder.buildRestored))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	ctrl, ok := s.LoopController(s.ActiveLoopID())
	if !ok {
		t.Fatal("LoopController not found")
	}
	installer, ok := ctrl.(loop.ExternalToolInstaller)
	if !ok {
		t.Fatal("loop controller does not implement loop.ExternalToolInstaller")
	}

	// Pin the precondition the old guard got wrong: this loop is foreign AND production
	// gave its handle real, non-zero bindings.
	h := s.loops[s.ActiveLoopID()]
	if h.bound.Engine() == loop.EngineNative {
		t.Fatal("test setup: want a foreign-engine loop")
	}
	if h.bindings.LoopID.IsZero() {
		t.Fatal("test setup: production is expected to build full bindings for a foreign loop too")
	}

	bounded, cancelBounded := context.WithTimeout(ctx, 2*time.Second)
	defer cancelBounded()
	err = installer.ReplaceExternalTools(bounded, loop.ExternalToolset{
		Source: "mcp", Generation: "g1", Definitions: []tool.Definition{extDefinition("search")},
	})
	var changeErr *loop.ChangeError
	if !errors.As(err, &changeErr) {
		t.Fatalf("err = %v, want *loop.ChangeError", err)
	}
	if changeErr.Kind == loop.ChangeContextDone {
		t.Fatal("ReplaceExternalTools blocked on a foreign loop's ack until the deadline — it must refuse structurally, not hang")
	}
	if changeErr.Kind != loop.ChangeExternalToolsUnsupported {
		t.Fatalf("Kind = %v, want ChangeExternalToolsUnsupported", changeErr.Kind)
	}
}
