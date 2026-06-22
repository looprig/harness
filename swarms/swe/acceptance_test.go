package swe

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/agents/operator"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/agent/session"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/inventivepotter/urvi/tui"
)

// acceptance_test.go is the swarm's CROSS-CUTTING acceptance suite: it drives the
// ASSEMBLED swarm (orchestrator-as-primary + the agent-aware Subagent tool + the
// swarmSpawner + the real leaf registry + the session under the spawn caps) end-to-end
// through the same surface a TUI uses — Submit a user turn, observe the session event
// stream, route gate decisions — proving the wired behaviour, not a unit in isolation.
//
// It uses the fake llm.LLM (no network, no key): a SCRIPTED fake that routes its reply
// by the requesting agent's system prompt (orchestrator vs a named leaf) so a single
// shared client drives BOTH the orchestrator turn (which emits a Subagent tool call)
// and the spawned leaf turn (which completes) — the production wiring shares one client
// across the whole tree. Routing by system prompt + a per-route call counter is robust
// to interleaving (it never depends on a fragile global call order).
//
// The unit-level roster/cfg assertions live in swarm_test.go and spawner_test.go; this
// file EXTENDS them to the end-to-end plane and does not duplicate them.

// scriptedReply is one fake-LLM turn's worth of chunks for a given route, consumed in
// order across successive Stream() calls on that route (so the orchestrator can emit a
// tool call on its first turn and a final text on its second).
type scriptedReply struct {
	chunks []content.Chunk
}

// route classifies a request by the agent driving it, read from the system prompt the
// swarm bakes into each loop's ModelSpec (Identity + the agent's Role). It is the
// stable join key between a Stream() call and the script the test wants that agent to
// follow — far more robust than a global call-ordinal, since the orchestrator and a
// spawned leaf interleave their Stream() calls.
type route string

// routeOrchestrator names the orchestrator (primary) loop's route; a leaf route is the
// leaf's agent name (e.g. route(operator.Name)). routeUnknown is the fail-safe sentinel
// classify returns when no role marker matches, so an unrouted request streams a visible
// fail-loud text rather than silently following the wrong script.
const (
	routeOrchestrator route = "orchestrator"
	routeUnknown      route = ""
)

// scriptedSwarmLLM is a controllable llm.LLM that drives the whole swarm tree from one
// client. Each Stream() call is classified to a route by the request's system prompt;
// the route's next scripted reply (by per-route call index) is streamed. An unrouted
// request, or a route that runs out of scripts, streams a single fail-loud text so a
// mis-wired test surfaces as a visible assertion failure rather than a hang.
type scriptedSwarmLLM struct {
	mu sync.Mutex

	// scripts maps a route to its ordered replies. The orchestrator route typically has
	// two (tool-call turn, then final-text turn); a leaf route has one (its final text).
	scripts map[route][]scriptedReply
	// calls counts Stream() invocations per route, so the i-th call on a route gets that
	// route's i-th scripted reply.
	calls map[route]int
}

func newScriptedSwarmLLM() *scriptedSwarmLLM {
	return &scriptedSwarmLLM{scripts: map[route][]scriptedReply{}, calls: map[route]int{}}
}

// script registers the ordered replies for a route. Successive Stream() calls on that
// route consume them in order.
func (c *scriptedSwarmLLM) script(r route, replies ...scriptedReply) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scripts[r] = replies
}

// errInvokeUnused is returned by the fake's Invoke: the loop only ever calls Stream, so
// Invoke is a stub (mirrors fakeLLM.Invoke).
var errInvokeUnused = errors.New("scriptedSwarmLLM.Invoke not used")

func (c *scriptedSwarmLLM) Invoke(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errInvokeUnused
}

// classify reads a request's system prompt and returns its route. The orchestrator's
// role marker wins (it is the only loop carrying it); otherwise the first registered
// leaf route whose name appears as a role marker matches.
func (c *scriptedSwarmLLM) classify(system string) route {
	if strings.Contains(system, `<role name="orchestrator">`) {
		return routeOrchestrator
	}
	for r := range c.scripts {
		if r == routeOrchestrator {
			continue
		}
		if strings.Contains(system, `<role name="`+string(r)+`">`) {
			return r
		}
	}
	return routeUnknown
}

func (c *scriptedSwarmLLM) Stream(ctx context.Context, req llm.Request) (*llm.StreamReader[content.Chunk], error) {
	c.mu.Lock()
	r := c.classify(req.Model.System)
	i := c.calls[r]
	c.calls[r]++
	var chunks []content.Chunk
	if replies, ok := c.scripts[r]; ok && i < len(replies) {
		chunks = replies[i].chunks
	} else {
		// Unrouted or out-of-script: stream a fail-loud text so a mis-scripted test
		// produces a visible wrong-answer assertion, never a silent hang.
		chunks = []content.Chunk{&content.TextChunk{Text: "FAKE-UNROUTED:" + string(r)}}
	}
	c.mu.Unlock()

	j := 0
	next := func() (content.Chunk, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if j < len(chunks) {
			ch := chunks[j]
			j++
			return ch, nil
		}
		return nil, io.EOF
	}
	return llm.NewStreamReader(next, nil), nil
}

// textReply scripts a single final-text turn (no tool call).
func textReply(text string) scriptedReply {
	return scriptedReply{chunks: []content.Chunk{&content.TextChunk{Text: text}}}
}

// subagentCallReply scripts a turn that emits ONE Subagent tool call naming agent with
// message, so the loop invokes the wired Subagent tool. id is the tool_use id.
func subagentCallReply(id string, agent identity.AgentName, message string) scriptedReply {
	args := `{"agent":` + jsonString(string(agent)) + `,"message":` + jsonString(message) + `}`
	return scriptedReply{chunks: []content.Chunk{
		&content.ToolUseChunk{Index: 0, ID: id, Name: "Subagent", InputJSON: args},
	}}
}

// toolCallReply scripts a turn that emits ONE tool call (name/args), so the loop invokes
// that tool (used to drive a leaf into a permission gate, e.g. operator's WriteFile).
func toolCallReply(id, name, argsJSON string) scriptedReply {
	return scriptedReply{chunks: []content.Chunk{
		&content.ToolUseChunk{Index: 0, ID: id, Name: name, InputJSON: argsJSON},
	}}
}

// jsonString quotes s as a JSON string literal (the test args are simple ASCII; this
// keeps the scripted args readable without pulling in encoding/json for one field).
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, ch := range s {
		switch ch {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(ch)
		default:
			b.WriteRune(ch)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// observerFilter delivers the primary loop's EPHEMERAL stream (so the orchestrator's
// ToolCallStarted/Completed are observable — those are Ephemeral) plus ENDURING events
// from EVERY loop (so a spawned leaf's LoopStarted / PermissionRequested / terminals all
// arrive). Leaf ephemerals (the token firehose) are deliberately excluded — the swarm
// tests assert on enduring leaf events only.
func observerFilter(primary uuid.UUID) event.EventFilter {
	return event.EventFilter{
		Ephemeral: event.LoopScope{Loops: map[uuid.UUID]struct{}{primary: {}}},
		Enduring:  event.LoopScope{All: true},
	}
}

// acceptanceDeadline bounds every wait so a wiring regression fails as a timeout
// assertion rather than hanging the suite.
const acceptanceDeadline = 5 * time.Second

// newAcceptanceSwarm builds the assembled swarm over a scripted fake client (no env, no
// network). The agent is Closed on cleanup.
func newAcceptanceSwarm(t *testing.T, client *scriptedSwarmLLM) *sessionAgent {
	t.Helper()
	agent, err := newWithClient(context.Background(), client, newModelFactory("test-key"))
	if err != nil {
		t.Fatalf("newWithClient() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	return agent
}

// recorder drains a whole-session subscription in a background goroutine into a
// mutex-guarded slice, so EVERY event the session fan-in delivered is available to assert
// on regardless of arrival order. It is the no-replay observer pattern the session's own
// tests use: a single subscription feeds the recorder, and assertions read the full
// recorded history (never a second consumer that could race the first for an event).
type recorder struct {
	mu     sync.Mutex
	events []event.Event
}

// newRecorder subscribes the agent's whole-session stream (scoped via observerFilter) and
// starts draining it. The subscription is Closed on cleanup.
func newRecorder(t *testing.T, agent *sessionAgent) *recorder {
	t.Helper()
	sub, err := agent.Subscribe(observerFilter(agent.PrimaryLoopID()))
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	rec := &recorder{}
	go func() {
		for ev := range sub.Events() {
			rec.mu.Lock()
			rec.events = append(rec.events, ev)
			rec.mu.Unlock()
		}
	}()
	return rec
}

// find returns the first recorded event matching pred, or nil/false.
func (r *recorder) find(pred func(event.Event) bool) (event.Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.events {
		if pred(ev) {
			return ev, true
		}
	}
	return nil, false
}

// count returns how many recorded events match pred.
func (r *recorder) count(pred func(event.Event) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, ev := range r.events {
		if pred(ev) {
			n++
		}
	}
	return n
}

// waitFor polls the recorded slice until an event matches pred or the deadline elapses,
// returning the first match. The drain runs in a goroutine, so an event published by the
// time a producing call returns may not yet be recorded; polling bridges that gap
// deterministically without a fixed sleep.
func (r *recorder) waitFor(pred func(event.Event) bool) (event.Event, bool) {
	deadline := time.Now().Add(acceptanceDeadline)
	for {
		if ev, ok := r.find(pred); ok {
			return ev, true
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		time.Sleep(time.Millisecond)
	}
}

// isPrimaryTurnDone matches the orchestrator primary's terminal TurnDone.
func isPrimaryTurnDone(primary uuid.UUID) func(event.Event) bool {
	return func(ev event.Event) bool {
		td, ok := ev.(event.TurnDone)
		return ok && td.Coordinates.LoopID == primary
	}
}

// isNonPrimaryLoopStarted matches a LoopStarted for any loop other than primary (a
// spawned sub-loop).
func isNonPrimaryLoopStarted(primary uuid.UUID) func(event.Event) bool {
	return func(ev event.Event) bool {
		ls, ok := ev.(event.LoopStarted)
		return ok && ls.Coordinates.LoopID != primary
	}
}

// TestAcceptanceLeavesCannotSpawn proves the depth-1-by-construction invariant on the
// ASSEMBLED swarm: the orchestrator (primary) carries Subagent (auto-approved) and ONLY
// the read/Todo/AskUser set, while EVERY leaf the registry can spawn carries its own
// allowlist with NO Subagent — so a leaf can never spawn a grandchild. This consolidates
// the roster assertion across the whole registry end-to-end (the per-agent toolset units
// live in swarm_test.go / spawner_test.go).
func TestAcceptanceLeavesCannotSpawn(t *testing.T) {
	t.Parallel()

	deps := LeafToolDeps{Root: t.TempDir(), HTTPCl: newHTTPClient()}
	reg, err := leafRegistry(deps)
	if err != nil {
		t.Fatalf("leafRegistry() error = %v", err)
	}

	// Orchestrator: Subagent present + auto-approved; the set is exactly the six.
	orchSpawner := newSwarmSpawner(reg, deps, newScriptedSwarmLLM(), newModelFactory("k"))
	orchTS := orchestratorToolSet(deps.Root, orchSpawner, toolCatalog(reg))
	orchNames := toolNames(t, orchTS)
	if !containsName(orchNames, "Subagent") {
		t.Errorf("orchestrator toolset = %v, want it to contain Subagent (only the primary may spawn)", orchNames)
	}
	if !containsName(orchAutoApproved(), "Subagent") {
		t.Error("Subagent is not in the orchestrator's auto-approve set")
	}

	// Every leaf: NO Subagent (cannot spawn — depth 1 by construction).
	for _, entry := range reg.Catalog() {
		entry := entry
		t.Run(string(entry.Name), func(t *testing.T) {
			t.Parallel()
			a, ok := reg.Lookup(entry.Name)
			if !ok {
				t.Fatalf("registry lost agent %q", entry.Name)
			}
			names := toolNames(t, a.BuildTools(deps))
			if containsName(names, "Subagent") {
				t.Errorf("leaf %q toolset = %v, must NOT contain Subagent (a leaf cannot spawn)", entry.Name, names)
			}
		})
	}
}

// orchAutoApproved returns a copy of the orchestrator's auto-approve set for assertion
// without mutating the package var.
func orchAutoApproved() []string { return append([]string(nil), orchestratorAutoApprovedTools...) }

// TestAcceptanceEndToEndSpawn drives the assembled swarm: the orchestrator (scripted to
// emit a Subagent{operator,...} call then a final text) Submits a turn; the test asserts
// on the whole-session stream that a sub-loop was created and attributed to the leaf —
// a LoopStarted whose Header.AgentName == "operator" carrying a non-zero Header.Cause
// LoopID (a real child of the orchestrator's turn) — and that the orchestrator's turn
// completes with the final text. The leaf is scripted to return its own final text,
// which the Subagent tool returns to the orchestrator as the tool result.
func TestAcceptanceEndToEndSpawn(t *testing.T) {
	t.Parallel()

	client := newScriptedSwarmLLM()
	client.script(routeOrchestrator,
		subagentCallReply("call-1", operator.Name, "implement the fix"),
		textReply("orchestrator: done, the operator handled it"),
	)
	client.script(route(operator.Name), textReply("operator: fix applied"))

	agent := newAcceptanceSwarm(t, client)
	primary := agent.PrimaryLoopID()
	rec := newRecorder(t, agent)

	if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "fix the bug"}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	// A sub-loop is created and ATTRIBUTED to the operator leaf, as a real child of the
	// orchestrator's turn (non-zero Cause.LoopID = the spawning loop).
	ev, ok := rec.waitFor(isNonPrimaryLoopStarted(primary))
	if !ok {
		t.Fatal("never observed a LoopStarted for a spawned (non-primary) sub-loop")
	}
	ls := ev.(event.LoopStarted)
	if ls.Header.AgentName != operator.Name {
		t.Errorf("spawned LoopStarted AgentName = %q, want %q (attributed to the operator leaf)", ls.Header.AgentName, operator.Name)
	}
	if ls.Header.Cause.LoopID.IsZero() {
		t.Error("spawned LoopStarted Cause.LoopID is zero, want the orchestrator's loop (a real child of the spawning turn)")
	}
	if ls.Header.Cause.LoopID != primary {
		t.Errorf("spawned LoopStarted Cause.LoopID = %v, want the orchestrator primary %v", ls.Header.Cause.LoopID, primary)
	}

	// The orchestrator's turn completes with its scripted final text — the Subagent tool
	// returned the leaf's final text, the orchestrator ran a second turn, and it ended
	// successfully on the primary loop.
	done, ok := rec.waitFor(isPrimaryTurnDone(primary))
	if !ok {
		t.Fatal("never observed the orchestrator's terminal TurnDone")
	}
	if got := aiMessageText(done.(event.TurnDone).Message); !strings.Contains(got, "the operator handled it") {
		t.Errorf("orchestrator final text = %q, want it to contain the scripted final answer", got)
	}
}

// TestAcceptanceUnknownAgentFailsSecure proves the fail-secure boundary end-to-end: an
// orchestrator turn that calls Subagent with an UNREGISTERED agent name yields a
// tool-result ERROR (carrying the UnknownAgentError text) and spawns NO sub-loop — no
// LoopStarted is published for the unknown name. The orchestrator then completes its turn
// (a real model would recover; the script just ends the turn).
func TestAcceptanceUnknownAgentFailsSecure(t *testing.T) {
	t.Parallel()

	client := newScriptedSwarmLLM()
	client.script(routeOrchestrator,
		subagentCallReply("call-x", "nope", "do something"),
		textReply("orchestrator: recovered from the unknown agent"),
	)

	agent := newAcceptanceSwarm(t, client)
	primary := agent.PrimaryLoopID()
	rec := newRecorder(t, agent)

	if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "use a bad agent"}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	// The Subagent tool call completes carrying the UnknownAgentError text. NOTE: the
	// Subagent tool's failure model renders every failure as a tool-result STRING (per its
	// doc + CLAUDE.md), NOT as an IsError-flagged ToolResultBlock — so ToolCallCompleted.
	// IsError stays false and the failure is conveyed in the result text. The model sees
	// the "error: subagent failed: ... unknown subagent \"nope\"" string and recovers.
	ev, ok := rec.waitFor(func(ev event.Event) bool {
		tc, isTC := ev.(event.ToolCallCompleted)
		return isTC && tc.Coordinates.LoopID == primary
	})
	if !ok {
		t.Fatal("never observed the orchestrator's Subagent ToolCallCompleted")
	}
	tc := ev.(event.ToolCallCompleted)
	if !strings.Contains(tc.ResultPreview, "unknown subagent") || !strings.Contains(tc.ResultPreview, `"nope"`) {
		t.Errorf("Subagent result preview = %q, want it to carry the unknown-agent error naming %q", tc.ResultPreview, "nope")
	}

	// The orchestrator's turn still completes; once it has, the whole turn's events are
	// recorded — so a zero count of non-primary LoopStarted proves the unknown agent
	// spawned nothing (no sub-loop was ever announced).
	if _, ok := rec.waitFor(isPrimaryTurnDone(primary)); !ok {
		t.Fatal("never observed the orchestrator's terminal TurnDone")
	}
	if n := rec.count(isNonPrimaryLoopStarted(primary)); n != 0 {
		t.Errorf("observed %d non-primary LoopStarted, want 0 (an unknown agent spawns nothing)", n)
	}
}

// TestAcceptanceQuotaCapRejectsSpawn proves the spawn quota is enforced through the SWARM
// path: with the orchestrator's session built under a Quota of 1, the orchestrator's
// SECOND Subagent call (the quota+1-th spawn) comes back as a tool-result ERROR and
// publishes NO second sub-loop. The first spawn succeeds (one leaf LoopStarted); the
// second is refused by the session's NewLoop cap (the typed *SessionError, rendered into
// the tool-result string by the Subagent tool). The session-level typed-error assertions
// live in the session package's quota_cap_test; this proves the cap is wired live on the
// orchestrator's session and surfaces to the spawning model.
func TestAcceptanceQuotaCapRejectsSpawn(t *testing.T) {
	t.Parallel()

	client := newScriptedSwarmLLM()
	// Two Subagent calls then a final text. The leaf returns quickly so the first spawn
	// completes before the second is attempted.
	client.script(routeOrchestrator,
		subagentCallReply("call-a", operator.Name, "task one"),
		subagentCallReply("call-b", operator.Name, "task two"),
		textReply("orchestrator: done"),
	)
	client.script(route(operator.Name), textReply("operator: ok"))

	// Build the swarm wiring by hand so the session can be capped at Quota=1 (newWithClient
	// uses the production caps). This exercises the SAME wiring (orchestratorConfig +
	// spawner.bind) under a tighter quota.
	agent := newCappedAcceptanceSwarm(t, client, session.Limits{Depth: orchestratorSpawnDepth, Quota: 1})
	primary := agent.PrimaryLoopID()
	rec := newRecorder(t, agent)

	if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "spawn twice"}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	// The orchestrator's turn must end (it eventually streams the final text).
	if _, ok := rec.waitFor(isPrimaryTurnDone(primary)); !ok {
		t.Fatal("never observed the orchestrator's terminal TurnDone")
	}

	// Exactly ONE leaf sub-loop was spawned: the second Subagent call was refused by the
	// quota cap before any LoopStarted. Once TurnDone is recorded, every spawn the turn
	// produced has already been announced — so a count of 1 proves the 2nd was refused.
	if n := rec.count(isNonPrimaryLoopStarted(primary)); n != 1 {
		t.Errorf("observed %d non-primary LoopStarted, want exactly 1 (Quota=1: the 2nd spawn is refused before any LoopStarted)", n)
	}
	// The 2nd Subagent call surfaced to the model as a tool-result carrying the quota-cap
	// message — the typed *SessionError{SessionLoopQuotaExceeded} ("session: loop spawn
	// quota exceeded") that NewLoop returned, rendered into the tool-result string by the
	// Subagent tool's failure model (a string, not an IsError-flagged block).
	if _, ok := rec.find(func(ev event.Event) bool {
		tc, isTC := ev.(event.ToolCallCompleted)
		return isTC && tc.Coordinates.LoopID == primary && strings.Contains(tc.ResultPreview, "quota exceeded")
	}); !ok {
		t.Error("never observed a quota-cap tool-result for the refused 2nd Subagent call")
	}
}

// newCappedAcceptanceSwarm assembles the orchestrator wiring (the SAME buildOrchestratorWiring
// the production New path uses) but starts the session under explicit limits, then binds the
// live session onto the spawner exactly as newWithClient does. It lets an acceptance test
// drive the assembled swarm under a tighter cap than the production defaults.
func newCappedAcceptanceSwarm(t *testing.T, client *scriptedSwarmLLM, limits session.Limits) *sessionAgent {
	t.Helper()
	root := t.TempDir()
	wiring, err := buildOrchestratorWiring(client, newModelFactory("test-key"), root)
	if err != nil {
		t.Fatalf("buildOrchestratorWiring() error = %v", err)
	}
	agent, err := newSessionAgent(context.Background(), wiring.cfg, session.WithLimits(limits))
	if err != nil {
		t.Fatalf("newSessionAgent() error = %v", err)
	}
	wiring.spawner.bind(agent.session)
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	return agent
}

// TestAcceptanceGateAttributedToLeaf proves a SPAWNED leaf's permission gate is attributed
// to the LEAF's loop and is routable via the session's Approve(loopID, callID, scope): the
// orchestrator spawns the operator, the operator is scripted to call WriteFile (which the
// operator's policy gates as Ask), and the test — watching the whole-session stream from a
// goroutine — observes a PermissionRequested whose Header.LoopID is the LEAF's loop (not the
// orchestrator's), then Approves it on that exact loop id. The whole flow then drains to the
// orchestrator's TurnDone, proving the gate decision reached the right loop and unblocked it.
func TestAcceptanceGateAttributedToLeaf(t *testing.T) {
	t.Parallel()

	client := newScriptedSwarmLLM()
	client.script(routeOrchestrator,
		subagentCallReply("call-1", operator.Name, "write a file"),
		textReply("orchestrator: file written"),
	)
	// The operator first calls WriteFile (gated Ask), then — after approval — ends its turn.
	client.script(route(operator.Name),
		toolCallReply("op-write-1", "WriteFile", `{"path":"out.txt","content":"hello"}`),
		textReply("operator: wrote out.txt"),
	)

	// Assemble via the capped seam (default caps) which roots the workspace at t.TempDir(),
	// so the operator's WriteFile target is a real, writable, in-root path.
	agent := newCappedAcceptanceSwarm(t, client, session.Limits{Depth: orchestratorSpawnDepth, Quota: orchestratorSpawnQuota})
	primary := agent.PrimaryLoopID()
	rec := newRecorder(t, agent)

	// The Subagent tool call blocks the orchestrator turn until the spawned operator
	// completes, and the operator's WriteFile blocks on its gate until approved — so the
	// approval MUST come from a separate goroutine while Submit's effects are in flight.
	// The driver polls the recorder for the LEAF's gate (a PermissionRequested on a
	// non-primary loop) and Approves it on that exact loop id, proving Approve(loopID, ...)
	// routes to the right loop in a multi-loop session. It records the gate's loop id so the
	// main goroutine can assert the attribution.
	approveDone := make(chan uuid.UUID, 1)
	go func() {
		ev, ok := rec.waitFor(func(ev event.Event) bool {
			pr, isPR := ev.(event.PermissionRequested)
			return isPR && pr.Coordinates.LoopID != primary
		})
		if !ok {
			approveDone <- uuid.UUID{}
			return
		}
		pr := ev.(event.PermissionRequested)
		if err := agent.Approve(context.Background(), pr.Coordinates.LoopID, pr.ToolExecutionID, tool.ScopeOnce); err != nil {
			t.Errorf("Approve(leaf loop %v) error = %v", pr.Coordinates.LoopID, err)
		}
		approveDone <- pr.Coordinates.LoopID
	}()

	if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "have the operator write a file"}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	gateLoop := <-approveDone
	if gateLoop.IsZero() {
		t.Fatal("never observed a PermissionRequested attributed to the spawned leaf loop")
	}
	if gateLoop == primary {
		t.Errorf("gate loop id = orchestrator primary %v, want the spawned leaf's own loop", primary)
	}

	// After the approval reaches the leaf, the spawn completes and the orchestrator's turn
	// ends — proving the gate decision unblocked the right loop.
	if _, ok := rec.waitFor(isPrimaryTurnDone(primary)); !ok {
		t.Fatal("orchestrator turn never completed after the leaf gate was approved")
	}
}

// Compile-time check: the assembled agent is a tui.Agent (the surface every acceptance
// test drives it through). Mirrors agent_test.go's assertion at the swarm-assembly level.
var _ tui.Agent = (*sessionAgent)(nil)
