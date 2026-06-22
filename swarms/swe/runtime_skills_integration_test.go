//go:build integration

package swe

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/agents/researcher"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/session"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// runtime_skills_integration_test.go is the P2b Phase 3c END-TO-END acceptance: with
// RuntimeSkills ON and a real on-disk <root>/.skills/<name>/SKILL.md, the orchestrator
// spawns the researcher, the researcher calls Skill{name:"<workspace-skill>"}, the
// workspace load surfaces a HUMAN-GATED SkillLoadRequest (ScopeOnce) attributed to the
// RESEARCHER's loop (not the orchestrator's), and after Approve the snapshot body is
// returned as the tool result. It crosses the filesystem boundary (a real os.Root
// snapshot of the workspace skill), so it is integration-tagged.
//
// It reuses the scripted fake-LLM idiom from acceptance_test.go and asserts on the
// whole-session event stream exactly like TestAcceptanceGateAttributedToLeaf — only the
// gated tool is the workspace Skill load rather than operator's WriteFile.

// workspaceSkillBody is the marker phrase planted in the on-disk workspace SKILL.md so
// the test can prove the APPROVED snapshot body (not an error) is what the Skill tool
// returned.
const workspaceSkillBody = "WORKSPACE-SKILL-MARKER: project-local checklist"

// writeWorkspaceSkill writes a valid <root>/.skills/<name>/SKILL.md (frontmatter + body)
// and returns name. The frontmatter mirrors the embedded catalogue's format so parseSkill
// accepts it; the body carries workspaceSkillBody for the result assertion.
func writeWorkspaceSkill(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, ".skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", dir, err)
	}
	doc := "---\nname: " + name + "\ndescription: A project-local workspace skill.\n---\n\n# Local\n\n" + workspaceSkillBody + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(doc), 0o600); err != nil {
		t.Fatalf("WriteFile(SKILL.md) error = %v", err)
	}
	return name
}

// newRuntimeSkillsSwarm assembles the swarm at a controlled root under
// Config{RuntimeSkills: true} (so the eligible read-only leaves get a workspace-enabled
// Skill tool) and binds the session, mirroring newCappedAcceptanceSwarm but with the
// runtime-skills mode on and a caller-chosen root (so the workspace .skills/ tree the
// test planted is the one the Skill tool reads).
func newRuntimeSkillsSwarm(t *testing.T, client *scriptedSwarmLLM, root string) *sessionAgent {
	t.Helper()
	wiring, err := buildOrchestratorWiring(client, newModelFactory("test-key"), root, Config{RuntimeSkills: true})
	if err != nil {
		t.Fatalf("buildOrchestratorWiring() error = %v", err)
	}
	agent, err := newSessionAgent(context.Background(), wiring.cfg,
		session.WithLimits(session.Limits{Depth: orchestratorSpawnDepth, Quota: orchestratorSpawnQuota}))
	if err != nil {
		t.Fatalf("newSessionAgent() error = %v", err)
	}
	wiring.spawner.bind(agent.session)
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	return agent
}

// TestRuntimeSkillWorkspaceLoadGatedEndToEnd drives the assembled swarm with RuntimeSkills
// ON: the orchestrator spawns the researcher, the researcher loads a WORKSPACE skill via
// Skill{name}, the load opens a SkillLoadRequest gate (ScopeOnce) on the RESEARCHER's loop,
// the test Approves it on that exact loop, and the researcher's Skill ToolCallCompleted
// carries the approved snapshot body — proving the §7a workspace path is enforced-gated,
// attributed to the right loop, and returns the snapshot after approval.
func TestRuntimeSkillWorkspaceLoadGatedEndToEnd(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	skillName := writeWorkspaceSkill(t, root, "project-local")

	client := newScriptedSwarmLLM()
	client.script(routeOrchestrator,
		subagentCallReply("call-1", researcher.Name, "load the project skill"),
		textReply("orchestrator: researcher loaded the workspace skill"),
	)
	// The researcher loads the workspace skill (Ask-gated), then — after approval — ends.
	client.script(route(researcher.Name),
		skillCallReply("res-skill-1", skillName),
		textReply("researcher: applied the workspace checklist"),
	)

	agent := newRuntimeSkillsSwarm(t, client, root)
	primary := agent.PrimaryLoopID()
	// All-scope recorder so the SPAWNED researcher's ToolCallCompleted (an Ephemeral,
	// loop-scoped event) is observable, like TestAcceptanceSkillLoadedEndToEnd.
	rec := newAllScopeRecorder(t, agent)

	// The workspace Skill load BLOCKS on its gate until approved, and the Subagent tool
	// blocks the orchestrator turn until the researcher completes — so the approval must
	// come from a separate goroutine while Submit's effects are in flight. The driver
	// waits for the LEAF's SkillLoadRequest gate, asserts it is a ScopeOnce SkillLoadRequest
	// attributed to the researcher (not the orchestrator), then Approves it on that loop.
	gateInfo := make(chan permGate, 1)
	go func() {
		ev, ok := rec.waitFor(func(ev event.Event) bool {
			pr, isPR := ev.(event.PermissionRequested)
			return isPR && pr.Coordinates.LoopID != primary
		})
		if !ok {
			gateInfo <- permGate{}
			return
		}
		pr := ev.(event.PermissionRequested)
		g := permGate{loop: pr.Coordinates.LoopID, req: pr.Request}
		if err := agent.Approve(context.Background(), pr.Coordinates.LoopID, pr.ToolExecutionID, tool.ScopeOnce); err != nil {
			t.Errorf("Approve(researcher loop %v) error = %v", pr.Coordinates.LoopID, err)
		}
		gateInfo <- g
	}()

	if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "use the workspace skill"}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	g := <-gateInfo
	if g.loop.IsZero() {
		t.Fatal("never observed a PermissionRequested attributed to the spawned researcher loop")
	}
	if g.loop == primary {
		t.Errorf("gate loop id = orchestrator primary %v, want the spawned researcher's own loop", primary)
	}

	// The gate is a SkillLoadRequest (the §7a workspace gate), naming the workspace path,
	// the researcher, and offering ScopeOnce only (a workspace load is never persisted).
	slr, ok := g.req.(tool.SkillLoadRequest)
	if !ok {
		t.Fatalf("gate Request type = %T, want tool.SkillLoadRequest", g.req)
	}
	if slr.Agent != researcher.Name {
		t.Errorf("SkillLoadRequest.Agent = %q, want %q", slr.Agent, researcher.Name)
	}
	if !strings.Contains(slr.RelPath, ".skills/"+skillName+"/SKILL.md") {
		t.Errorf("SkillLoadRequest.RelPath = %q, want it to name the workspace skill path", slr.RelPath)
	}
	scopes := slr.AllowedScopes()
	if len(scopes) != 1 || scopes[0] != tool.ScopeOnce {
		t.Errorf("SkillLoadRequest.AllowedScopes() = %v, want exactly [ScopeOnce]", scopes)
	}

	// After approval, the researcher's Skill ToolCallCompleted (on the LEAF loop) carries
	// the APPROVED snapshot body — proving the snapshot ran, not an error.
	ev, ok := rec.waitFor(func(ev event.Event) bool {
		tc, isTC := ev.(event.ToolCallCompleted)
		return isTC && tc.Coordinates.LoopID == g.loop && strings.Contains(tc.ResultPreview, workspaceSkillBody)
	})
	if !ok {
		t.Fatal("never observed the researcher's Skill ToolCallCompleted carrying the approved workspace body")
	}
	tc := ev.(event.ToolCallCompleted)
	if tc.IsError {
		t.Errorf("Skill ToolCallCompleted IsError = true, want false (the workspace skill loaded): %q", tc.ResultPreview)
	}
	if strings.Contains(tc.ResultPreview, "error:") {
		t.Errorf("Skill result carries an error string, want the workspace body: %q", tc.ResultPreview)
	}

	// The orchestrator's turn completes with its scripted final text.
	if _, ok := rec.waitFor(isPrimaryTurnDone(primary)); !ok {
		t.Fatal("never observed the orchestrator's terminal TurnDone")
	}
}

// permGate captures the loop id and sealed Request of a permission gate so the approving
// goroutine can hand them back to the main goroutine for assertion.
type permGate struct {
	loop uuid.UUID
	req  tool.PermissionRequest
}
