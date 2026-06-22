package tools

import "github.com/inventivepotter/urvi/internal/agent/loop/identity"

// UnknownSkillError is returned when a skill is requested by name that is not
// known to — or not authorized for — the requesting agent. It is fail-secure:
// the Skill tool denies the invocation rather than guessing. It is
// errors.As-recoverable so the caller can report which agent asked for which
// skill without leaking the (curated) skill catalogue.
type UnknownSkillError struct {
	Agent identity.AgentName // the agent that requested the skill
	Name  string             // the requested (unknown/unauthorized) skill name
}

func (e *UnknownSkillError) Error() string {
	return "tools: unknown or unauthorized skill " + e.Name + " for agent " + string(e.Agent)
}

// MalformedSkillError is returned when a SKILL.md document cannot be parsed:
// it is oversize, has no opening frontmatter fence, has an unterminated fence,
// or carries a duplicate frontmatter key. The parser is fail-secure and never
// returns a partial or ambiguous parse alongside this error. It is
// errors.As-recoverable so the caller can surface the offending skill and a
// non-secret reason. Name is the skill identifier when known (empty when the
// raw bytes are parsed before a name is established).
type MalformedSkillError struct {
	Name   string // the skill identifier, if known ("" when unknown)
	Reason string // non-secret, human-readable reason for the rejection
}

func (e *MalformedSkillError) Error() string {
	if e.Name == "" {
		return "tools: malformed skill: " + e.Reason
	}
	return "tools: malformed skill " + e.Name + ": " + e.Reason
}

// SkillNotFoundError is returned when a skill name has passed the per-agent
// authorization check (it is a member of the agent's closed allow-set) but its
// SKILL.md document is absent from the backing file system. This is distinct
// from UnknownSkillError — which denies an untrusted, unauthorized name — and
// signals a catalogue/embed integrity problem (an allowed skill whose file was
// not shipped) rather than a denied request. It is errors.As-recoverable so the
// caller can surface the missing skill name; the underlying fs error is wrapped
// and reachable via errors.Unwrap for diagnostics.
type SkillNotFoundError struct {
	Name string // the authorized skill whose SKILL.md is missing
	Err  error  // the wrapped fs error (e.g. fs.ErrNotExist)
}

func (e *SkillNotFoundError) Error() string {
	return "tools: skill " + e.Name + " not found in catalogue"
}

func (e *SkillNotFoundError) Unwrap() error { return e.Err }
