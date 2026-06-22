package tools

import (
	"context"
	"errors"
	"io/fs"
	"path"

	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
)

// SkillLoader resolves a named skill into the markdown body to inject into an
// agent's context. It is the narrow seam between the Skill tool and the on-disk
// (embedded) skill catalogue: the tool asks for a body by (agent, name) and the
// loader is responsible for authorizing the request and reading the document.
//
// The agent identity is a parameter — not loader state — so a single loader can
// serve every agent in a swarm while still scoping each call to that agent's own
// allowed-skill set. Implementations MUST be fail-secure: an unauthorized or
// unknown name is denied, never guessed.
type SkillLoader interface {
	Load(ctx context.Context, agent identity.AgentName, name string) (string, error)
}

// embeddedSkillLoader reads SKILL.md documents from a compiled-in fs.FS (the
// swarm injects swarms/swe.SkillsFS at the composition root) and authorizes each
// request against a static, per-agent allow-map.
//
// The allow-map is a closed set per agent: allow[agent] is the exact set of
// skill names that agent may load, and a name is authorized iff it is a member.
// The map is owned by the loader and treated as read-only after construction; it
// is never mutated by Load, so concurrent Load calls are safe (fs.FS reads are
// independent and the embed.FS the swarm injects is itself read-only).
type embeddedSkillLoader struct {
	fsys  fs.FS
	allow map[identity.AgentName]map[string]struct{}
}

// NewEmbeddedSkillLoader wires an embeddedSkillLoader from an injected file
// system and a per-agent allow-map. fsys is the catalogue root holding
// skills/<name>/SKILL.md (an embed.FS satisfies fs.FS); allow[agent] is that
// agent's closed set of permitted skill names. Dependencies are injected here at
// the composition root so the tools package never imports the embed package
// (swarms/swe), keeping the dependency arrow swarms/swe -> tools and cycle-free.
//
// A nil allow-map is treated as "no agent is authorized for anything" — the
// fail-secure default. The returned concrete type satisfies SkillLoader.
func NewEmbeddedSkillLoader(fsys fs.FS, allow map[identity.AgentName]map[string]struct{}) *embeddedSkillLoader {
	return &embeddedSkillLoader{fsys: fsys, allow: allow}
}

// Load authorizes (agent, name) against the closed allow-set, then reads and
// parses skills/<name>/SKILL.md, returning the markdown body.
//
// The order is deliberate and is the traversal-safety guarantee: authorization
// happens BEFORE any path is built. The model-supplied name is untrusted, so it
// must first be proven to be a member of the agent's closed allow-set; only a
// validated member is ever joined into a path. A traversal payload such as
// "../../etc/passwd" can never be a member of the curated set, so it is rejected
// at the authorization gate and no path is ever constructed from it. The
// subsequent path.Join + path.Clean is defense-in-depth, not the boundary.
func (l *embeddedSkillLoader) Load(ctx context.Context, agent identity.AgentName, name string) (string, error) {
	// Honor cancellation up front; reads below are from a compiled-in fs and do
	// not otherwise observe the context, so this is the one place it can matter.
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// 1. Authorize first (fail-secure). The name is untrusted until it is proven
	// to be a member of this agent's closed allow-set.
	if !l.authorized(agent, name) {
		return "", &UnknownSkillError{Agent: agent, Name: name}
	}

	// 2. Build the path only now that name is a validated closed-set member.
	// path.Join cleans the result; with an authorized (traversal-free) name this
	// is purely defensive.
	skillPath := path.Join("skills", name, "SKILL.md")

	raw, err := fs.ReadFile(l.fsys, skillPath)
	if err != nil {
		return "", &SkillNotFoundError{Name: name, Err: err}
	}

	// 3. Parse and return the body. Propagate the parser's *MalformedSkillError,
	// stamping the now-known skill name (the parser sets Name="" since it sees
	// only raw bytes).
	_, body, err := parseSkill(raw)
	if err != nil {
		var me *MalformedSkillError
		if errors.As(err, &me) && me.Name == "" {
			me.Name = name
		}
		return "", err
	}

	return body, nil
}

// authorized reports whether agent may load the named skill: true iff name is a
// member of the agent's closed allow-set. An agent absent from the map, a nil
// map, or a name absent from the agent's set all deny — the fail-secure default.
func (l *embeddedSkillLoader) authorized(agent identity.AgentName, name string) bool {
	set, ok := l.allow[agent]
	if !ok {
		return false
	}
	_, ok = set[name]
	return ok
}
