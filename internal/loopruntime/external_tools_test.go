package loopruntime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

// stubTool is a minimal InvokableTool with a fixed name and schema.
type stubTool struct {
	name   string
	schema string
}

func (s *stubTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: s.name, Desc: "d", Schema: json.RawMessage(s.schema)}, nil
}

func (*stubTool) PrepareCall(context.Context, uuid.UUID, string) (tool.Request, tool.PreparedArtifact, error) {
	return tool.Request{}, nil, nil
}

func (s *stubTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return &tool.ToolResult{Content: []content.Block{&content.TextBlock{Text: "ok"}}}, nil
}

func externalTool(name string) tool.InvokableTool { return &stubTool{name: name, schema: `{"a":1}`} }

// countingTool records how many times it was actually INVOKED, so a test can assert on
// the execution registry (cfg.tools -> RunBatch) rather than only on the advertised
// toolset in the inference request. The two are separate halves of a turn's snapshot.
type countingTool struct {
	name string
	mu   sync.Mutex
	runs int
}

func (c *countingTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: c.name, Desc: "d", Schema: json.RawMessage(`{"a":1}`)}, nil
}

func (*countingTool) PrepareCall(context.Context, uuid.UUID, string) (tool.Request, tool.PreparedArtifact, error) {
	return tool.Request{}, nil, nil
}

func (c *countingTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	c.mu.Lock()
	c.runs++
	c.mu.Unlock()
	return &tool.ToolResult{Content: []content.Block{&content.TextBlock{Text: "ran"}}}, nil
}

func (c *countingTool) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs
}

func toolIdentity(name string) event.ExternalToolIdentity {
	return event.ExternalToolIdentity{Name: name, SchemaDigest: mustDigest(name)}
}

func mustDigest(string) string {
	d, err := tool.SchemaDigest(json.RawMessage(`{"a":1}`))
	if err != nil {
		panic(err)
	}
	return d
}

// declaredToolDefinition builds a loop tool definition producing one named tool, so a
// bound definition has a DECLARED tool the external slot must compose with (and must
// never shadow).
func declaredToolDefinition(name string) tool.Definition {
	return tool.NewDefinition(name, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{&stubTool{name: name, schema: `{"d":1}`}}, nil
	})
}

// toolBearingDefinition binds a definition with one declared tool in the base mode and a
// second mode ("other") declaring a different tool, so mode-change recomposition is
// observable.
func toolBearingDefinition(t *testing.T, client inference.Client) loop.BoundDefinition {
	t.Helper()
	// Auto-approve every call (WithAccessGate(autoApproveGate{}) below). Without a
	// gate that approves, EVERY tool result is "permission denied" and any
	// assertion about whether a tool actually RAN is vacuous — the registry is
	// never even consulted.
	// The SAME definition value must be reused across base and mode: loop.Bind compares
	// definitions by pointer identity, so two distinct definitions sharing a name are a
	// duplicate-definition error rather than a cache hit.
	declared := declaredToolDefinition("declared")
	d, err := loop.Define(
		loop.WithName("agent"),
		loop.WithInference(client, testModel()),
		loop.WithSystem("base"),
		loop.WithTools(declared),
		loop.WithModes(
			loop.Mode{Name: "main", Tools: []tool.Definition{declared}},
			loop.Mode{Name: "other", Tools: []tool.Definition{declaredToolDefinition("declared_other")}},
		),
		loop.WithInitialMode("main"),
		loop.WithAccessGate(autoApproveGate{}),
		loop.WithPolicyRevision("test"),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	bound, err := d.Bind(context.Background(), tool.Bindings{SessionID: mustID(t), LoopID: mustID(t)})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	return bound
}

func sendReplace(t *testing.T, l *Loop, c command.ReplaceLoopExternalTools) command.LoopToolsResult {
	t.Helper()
	c.Header = command.Header{CommandID: mustID(t)}
	ack := make(chan command.LoopToolsResult, 1)
	c.Ack = ack
	if !sendCmd(t, l, c) {
		t.Fatal("ReplaceLoopExternalTools send did not land (loop exited)")
	}
	select {
	case res := <-ack:
		return res
	case <-l.Done:
		t.Fatal("loop exited before ReplaceLoopExternalTools ack")
	case <-time.After(2 * time.Second):
		t.Fatal("ReplaceLoopExternalTools ack timeout")
	}
	return command.LoopToolsResult{}
}

// requestToolNames returns the model-facing tool names of the nth recorded request —
// the ONLY ground truth for "what toolset did this turn actually run under".
func requestToolNames(t *testing.T, rec *recordingLLM, n int) []string {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if n >= len(rec.reqs) {
		t.Fatalf("request %d not recorded (have %d)", n, len(rec.reqs))
	}
	names := make([]string, 0, len(rec.reqs[n].Tools))
	for _, tl := range rec.reqs[n].Tools {
		names = append(names, tl.Name)
	}
	return names
}

// TestReplaceExternalToolsAppliesAtNextTurn is the apply-at-boundary property: a
// replacement accepted while the loop is idle is visible to the NEXT turn's request.
//
// Mutation check: deleting the composeRegistry call in applyReplaceExternalTools (or
// having it return only declared tools) drops "ext_a" from turn 2 and fails here.
func TestReplaceExternalToolsAppliesAtNextTurn(t *testing.T) {
	t.Parallel()
	client := &recordingLLM{chunks: []content.Chunk{textChunk("hi")}}
	l, rec := newBoundLoop(t, client, toolBearingDefinition(t, client))

	runOneTurn(t, l, rec, "one")
	if got, want := requestToolNames(t, client, 0), []string{"declared"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("turn 1 tools = %v, want %v (external slot must start EMPTY)", got, want)
	}

	res := sendReplace(t, l, command.ReplaceLoopExternalTools{
		Source: "mcp", Generation: "g1",
		Tools:      []tool.InvokableTool{externalTool("ext_a")},
		Identities: []event.ExternalToolIdentity{toolIdentity("ext_a")},
	})
	if res.Err != nil {
		t.Fatalf("replace refused: %v", res.Err)
	}
	if res.Generation != "g1" || res.Installed != 1 {
		t.Fatalf("ack = %+v, want generation g1 / 1 installed", res)
	}

	runOneTurn(t, l, rec, "two")
	if got, want := requestToolNames(t, client, 1), []string{"declared", "ext_a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("turn 2 tools = %v, want %v", got, want)
	}
}

// TestReplaceExternalToolsMidTurnKeepsSnapshot is THE correctness property: a turn in
// flight keeps the toolset it started under.
//
// It uses a MULTI-STEP turn, which is the only way to observe this honestly. Asserting
// on the first request would be vacuous — that request is issued before the replacement
// is even sent, so it could not contain the new tools whatever the implementation did.
// Instead the replacement is injected from a hook that fires while step 1's stream is
// open: it is committed on the actor (its ack returns) BEFORE step 2 of the SAME turn
// issues its request. Step 2 must still see the old toolset, because the tools were
// snapshotted into turnConfig at turn start and toolDefs is computed once per turn.
//
// Mutation check: making buildTurnConfig/turn.go read the actor's live effective tools
// per STEP rather than snapshotting per TURN puts "ext_a" into request 2 and fails here.
func TestReplaceExternalToolsMidTurnKeepsSnapshot(t *testing.T) {
	t.Parallel()
	var l *Loop
	replaced := make(chan command.LoopToolsResult, 1)
	ext := &countingTool{name: "ext_a"}
	client := &scriptedLLM{
		scripts: [][]content.Chunk{
			// Step 1: call the declared tool, forcing the turn to take a second step.
			{&content.ToolUseChunk{Index: 0, ID: "id-1", Name: "declared", InputJSON: `{}`}},
			// Step 2 (SAME turn, after the replacement committed): try to EXECUTE the
			// newly installed external tool. The running turn's execution registry must
			// not know it.
			{&content.ToolUseChunk{Index: 0, ID: "id-2", Name: "ext_a", InputJSON: `{}`}},
			// Step 3 (same turn): plain text, ending the turn.
			{textChunk("done")},
		},
	}
	client.onStreamN = map[int]func(){
		// Fires while step 1's stream is open — the turn is provably in flight.
		0: func() {
			ack := make(chan command.LoopToolsResult, 1)
			l.Commands <- command.ReplaceLoopExternalTools{
				Header: command.Header{CommandID: mustID(t)}, Source: "mcp", Generation: "g1",
				Tools:      []tool.InvokableTool{ext},
				Identities: []event.ExternalToolIdentity{toolIdentity("ext_a")},
				Ack:        ack,
			}
			replaced <- <-ack
		},
	}
	l, rec := newBoundLoop(t, client, toolBearingDefinition(t, client))

	runOneTurn(t, l, rec, "one")

	select {
	case res := <-replaced:
		if res.Err != nil {
			t.Fatalf("mid-turn replace refused: %v", res.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mid-turn replace never acked")
	}

	client.mu.Lock()
	reqs := append([]inference.Request(nil), client.reqs...)
	client.mu.Unlock()
	if len(reqs) < 3 {
		t.Fatalf("want a 3-step turn, got %d requests", len(reqs))
	}
	// The ADVERTISED toolset: every step of the running turn still offers the old set.
	for i, req := range reqs[:3] {
		names := make([]string, 0, len(req.Tools))
		for _, tl := range req.Tools {
			names = append(names, tl.Name)
		}
		if want := []string{"declared"}; !reflect.DeepEqual(names, want) {
			t.Fatalf("turn 1 step %d tools = %v, want %v — a running turn must keep its snapshot across steps", i+1, names, want)
		}
	}

	// The EXECUTION registry (cfg.tools -> RunBatch) is the other half of the snapshot,
	// and the half a backing-array aliasing regression would corrupt invisibly to the
	// request assertions above. Step 2 of the running turn asked to invoke ext_a; the
	// turn's registry must not have resolved it.
	if n := ext.count(); n != 0 {
		t.Fatalf("external tool executed %d times DURING the turn it was installed in; the running turn's execution registry must not have it", n)
	}

	// The next turn crosses the boundary and picks the replacement up.
	runOneTurn(t, l, rec, "two")
	client.mu.Lock()
	last := client.reqs[len(client.reqs)-1]
	client.mu.Unlock()
	names := make([]string, 0, len(last.Tools))
	for _, tl := range last.Tools {
		names = append(names, tl.Name)
	}
	if want := []string{"declared", "ext_a"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("post-boundary turn tools = %v, want %v", names, want)
	}
}

// TestReplaceExternalToolsNextTurnCanExecuteExternalTool is the positive half of the
// execution-registry property: once the boundary is crossed the external tool is not
// merely ADVERTISED, it is actually dispatchable by RunBatch.
//
// Mutation check: composing only into the advertised list (leaving cfg.tools.Registry
// unchanged) would keep the request assertions green but leave this at 0 runs.
func TestReplaceExternalToolsNextTurnCanExecuteExternalTool(t *testing.T) {
	t.Parallel()
	ext := &countingTool{name: "ext_a"}
	client := &scriptedLLM{
		scripts: [][]content.Chunk{
			{&content.ToolUseChunk{Index: 0, ID: "id-1", Name: "ext_a", InputJSON: `{}`}},
			{textChunk("done")},
		},
	}
	l, rec := newBoundLoop(t, client, toolBearingDefinition(t, client))

	if res := sendReplace(t, l, command.ReplaceLoopExternalTools{
		Source: "mcp", Generation: "g1",
		Tools:      []tool.InvokableTool{ext},
		Identities: []event.ExternalToolIdentity{toolIdentity("ext_a")},
	}); res.Err != nil {
		t.Fatalf("replace refused: %v", res.Err)
	}
	runOneTurn(t, l, rec, "one")

	if n := ext.count(); n != 1 {
		t.Fatalf("external tool executed %d times, want 1 — an installed tool must be dispatchable, not just advertised", n)
	}
}

// TestReplaceExternalToolsSurvivesModeChange guards the subtle bug that a mode change
// re-resolves the mode's DECLARED tools and would otherwise silently uninstall every
// external tool.
//
// Mutation check: removing the composeRegistry call from applySetMode leaves turn 2 with
// only "declared_other" and fails here.
func TestReplaceExternalToolsSurvivesModeChange(t *testing.T) {
	t.Parallel()
	client := &recordingLLM{chunks: []content.Chunk{textChunk("hi")}}
	l, rec := newBoundLoop(t, client, toolBearingDefinition(t, client))

	if res := sendReplace(t, l, command.ReplaceLoopExternalTools{
		Source: "mcp", Generation: "g1",
		Tools:      []tool.InvokableTool{externalTool("ext_a")},
		Identities: []event.ExternalToolIdentity{toolIdentity("ext_a")},
	}); res.Err != nil {
		t.Fatalf("replace refused: %v", res.Err)
	}
	runOneTurn(t, l, rec, "one")
	if got, want := requestToolNames(t, client, 0), []string{"declared", "ext_a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("turn 1 tools = %v, want %v", got, want)
	}

	if res := sendSetMode(t, l, "other"); res.Err != nil {
		t.Fatalf("SetMode: %v", res.Err)
	}
	runOneTurn(t, l, rec, "two")
	if got, want := requestToolNames(t, client, 1), []string{"declared_other", "ext_a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after mode change tools = %v, want %v — a mode change must not uninstall external tools", got, want)
	}
}

// TestReplaceExternalToolsCrossSourceCollisionRefused is the actor-owned collision rule:
// a replacement may not install a name another SOURCE already installed. The refusal must
// leave the prior generation intact — no partial swap.
//
// Mutation check: deleting the state.external.collides guard installs "dup" twice and the
// duplicate-name assertion on the next turn's request fails.
func TestReplaceExternalToolsCrossSourceCollisionRefused(t *testing.T) {
	t.Parallel()
	client := &recordingLLM{chunks: []content.Chunk{textChunk("hi")}}
	l, rec := newBoundLoop(t, client, toolBearingDefinition(t, client))

	if res := sendReplace(t, l, command.ReplaceLoopExternalTools{
		Source: "mcp", Generation: "g1",
		Tools:      []tool.InvokableTool{externalTool("dup")},
		Identities: []event.ExternalToolIdentity{toolIdentity("dup")},
	}); res.Err != nil {
		t.Fatalf("first install refused: %v", res.Err)
	}

	res := sendReplace(t, l, command.ReplaceLoopExternalTools{
		Source: "other", Generation: "g2",
		Tools:      []tool.InvokableTool{externalTool("dup")},
		Identities: []event.ExternalToolIdentity{toolIdentity("dup")},
	})
	var changeErr *loop.ChangeError
	if !errors.As(res.Err, &changeErr) || changeErr.Kind != loop.ChangeExternalToolCollision {
		t.Fatalf("second install err = %v, want ChangeExternalToolCollision", res.Err)
	}
	if changeErr.Tool != "dup" {
		t.Errorf("ChangeError.Tool = %q, want dup", changeErr.Tool)
	}

	// Prior generation intact, nothing from the refused source installed.
	runOneTurn(t, l, rec, "one")
	if got, want := requestToolNames(t, client, 0), []string{"declared", "dup"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tools after refused replace = %v, want %v (prior generation must stay)", got, want)
	}
	// The refused replacement must not be journaled.
	if n := countExternalToolsetChanged(rec.events()); n != 1 {
		t.Errorf("LoopExternalToolsetChanged count = %d, want 1 (a refusal emits nothing)", n)
	}
}

// TestReplaceExternalToolsEmptyClearsSlot covers the boundary case: an empty toolset is a
// legal replacement that uninstalls the source, and it is durably recorded.
func TestReplaceExternalToolsEmptyClearsSlot(t *testing.T) {
	t.Parallel()
	client := &recordingLLM{chunks: []content.Chunk{textChunk("hi")}}
	l, rec := newBoundLoop(t, client, toolBearingDefinition(t, client))

	if res := sendReplace(t, l, command.ReplaceLoopExternalTools{
		Source: "mcp", Generation: "g1",
		Tools:      []tool.InvokableTool{externalTool("ext_a")},
		Identities: []event.ExternalToolIdentity{toolIdentity("ext_a")},
	}); res.Err != nil {
		t.Fatalf("install refused: %v", res.Err)
	}
	if res := sendReplace(t, l, command.ReplaceLoopExternalTools{Source: "mcp", Generation: "g2"}); res.Err != nil {
		t.Fatalf("clear refused: %v", res.Err)
	}
	runOneTurn(t, l, rec, "one")
	if got, want := requestToolNames(t, client, 0), []string{"declared"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tools after clear = %v, want %v", got, want)
	}
	if n := countExternalToolsetChanged(rec.events()); n != 2 {
		t.Errorf("LoopExternalToolsetChanged count = %d, want 2 (a clear is durably recorded)", n)
	}
}

// TestReplaceExternalToolsSameSourceReplacesGeneration proves a replacement REPLACES its
// own source rather than accumulating, and that reusing its own names is not a collision.
//
// Mutation check: if applyReplaceExternalTools appended instead of assigning next[Source],
// turn 1 would still carry "ext_old" and fail.
func TestReplaceExternalToolsSameSourceReplacesGeneration(t *testing.T) {
	t.Parallel()
	client := &recordingLLM{chunks: []content.Chunk{textChunk("hi")}}
	l, rec := newBoundLoop(t, client, toolBearingDefinition(t, client))

	if res := sendReplace(t, l, command.ReplaceLoopExternalTools{
		Source: "mcp", Generation: "g1",
		Tools:      []tool.InvokableTool{externalTool("ext_old"), externalTool("shared")},
		Identities: []event.ExternalToolIdentity{toolIdentity("ext_old"), toolIdentity("shared")},
	}); res.Err != nil {
		t.Fatalf("g1 refused: %v", res.Err)
	}
	// "shared" is reused by the SAME source — that must not read as a collision.
	if res := sendReplace(t, l, command.ReplaceLoopExternalTools{
		Source: "mcp", Generation: "g2",
		Tools:      []tool.InvokableTool{externalTool("shared"), externalTool("ext_new")},
		Identities: []event.ExternalToolIdentity{toolIdentity("shared"), toolIdentity("ext_new")},
	}); res.Err != nil {
		t.Fatalf("g2 refused: %v", res.Err)
	}
	runOneTurn(t, l, rec, "one")
	if got, want := requestToolNames(t, client, 0), []string{"declared", "shared", "ext_new"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tools = %v, want %v (g1 must be fully replaced)", got, want)
	}
}

func countExternalToolsetChanged(evs []event.Event) int {
	n := 0
	for _, e := range evs {
		if _, ok := e.(event.LoopExternalToolsetChanged); ok {
			n++
		}
	}
	return n
}

// TestReplaceExternalToolsEmitsIdentityOnly asserts the durable record carries identity
// and nothing else — the event must never grow a field holding a live factory or a raw
// schema. It also pins the loop-scoped header stamping.
func TestReplaceExternalToolsEmitsIdentityOnly(t *testing.T) {
	t.Parallel()
	client := &recordingLLM{chunks: []content.Chunk{textChunk("hi")}}
	l, rec := newBoundLoop(t, client, toolBearingDefinition(t, client))

	if res := sendReplace(t, l, command.ReplaceLoopExternalTools{
		Source: "mcp", Generation: "gen-7",
		Tools:      []tool.InvokableTool{externalTool("ext_a")},
		Identities: []event.ExternalToolIdentity{toolIdentity("ext_a")},
	}); res.Err != nil {
		t.Fatalf("replace refused: %v", res.Err)
	}
	blockUntilEvents(t, rec, func(evs []event.Event) bool { return countExternalToolsetChanged(evs) == 1 })
	for _, e := range rec.events() {
		ev, ok := e.(event.LoopExternalToolsetChanged)
		if !ok {
			continue
		}
		if ev.Source != "mcp" || ev.Generation != "gen-7" {
			t.Errorf("event = %+v, want source mcp / generation gen-7", ev)
		}
		if len(ev.Tools) != 1 || ev.Tools[0].Name != "ext_a" {
			t.Fatalf("event tools = %+v, want one ext_a identity", ev.Tools)
		}
		if ev.Tools[0].SchemaDigest != mustDigest("ext_a") {
			t.Errorf("digest = %q, want the compacted-schema digest", ev.Tools[0].SchemaDigest)
		}
		// Loop-scoped: SessionID+LoopID stamped; the turn id deliberately is not.
		if ev.LoopID.IsZero() || ev.SessionID.IsZero() {
			t.Errorf("header not loop-stamped: %+v", ev.Header)
		}
		if err := event.ValidateEvent(ev); err != nil {
			t.Errorf("emitted event fails its own validator: %v", err)
		}
	}
}

// TestComposeRegistry covers composition directly: order (declared first), the empty
// slot, and the no-aliasing guarantee.
func TestComposeRegistry(t *testing.T) {
	t.Parallel()
	declared := []tool.InvokableTool{externalTool("d1")}
	tests := []struct {
		name  string
		slots externalSlots
		want  []string
	}{
		{name: "empty slot yields declared only", slots: externalSlots{}, want: []string{"d1"}},
		{
			name:  "declared first then external",
			slots: externalSlots{"mcp": {tools: []tool.InvokableTool{externalTool("e1")}}},
			want:  []string{"d1", "e1"},
		},
		{
			name: "multiple sources compose in sorted order",
			slots: externalSlots{
				"zeta":  {tools: []tool.InvokableTool{externalTool("z1")}},
				"alpha": {tools: []tool.InvokableTool{externalTool("a1")}},
			},
			want: []string{"d1", "a1", "z1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := composeRegistry(declared, tt.slots)
			names := make([]string, 0, len(got))
			for _, tl := range got {
				info, err := tl.Info(context.Background())
				if err != nil {
					t.Fatalf("Info: %v", err)
				}
				names = append(names, info.Name)
			}
			if !reflect.DeepEqual(names, tt.want) {
				t.Fatalf("composeRegistry = %v, want %v", names, tt.want)
			}
			if len(got) > 0 && len(declared) > 0 && &got[0] == &declared[0] {
				t.Error("composeRegistry aliased the declared backing array")
			}
		})
	}
}

// TestExternalSlotsCollides pins the collision rule's edges, especially that a source
// never collides with ITSELF (the replace-my-own-generation case).
func TestExternalSlotsCollides(t *testing.T) {
	t.Parallel()
	slots := externalSlots{
		"mcp": {identities: []event.ExternalToolIdentity{toolIdentity("a"), toolIdentity("b")}},
	}
	tests := []struct {
		name        string
		replacing   string
		incoming    []event.ExternalToolIdentity
		wantName    string
		wantCollide bool
	}{
		{name: "same source reusing its names is not a collision", replacing: "mcp", incoming: []event.ExternalToolIdentity{toolIdentity("a")}},
		{name: "other source reusing a name collides", replacing: "other", incoming: []event.ExternalToolIdentity{toolIdentity("a")}, wantName: "a", wantCollide: true},
		{name: "other source with fresh names is fine", replacing: "other", incoming: []event.ExternalToolIdentity{toolIdentity("c")}},
		{name: "empty incoming never collides", replacing: "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			name, collides := slots.collides(tt.replacing, tt.incoming)
			if collides != tt.wantCollide || name != tt.wantName {
				t.Fatalf("collides() = (%q, %v), want (%q, %v)", name, collides, tt.wantName, tt.wantCollide)
			}
		})
	}
}
