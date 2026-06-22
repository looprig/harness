package swe

import "embed"

// SkillsFS is the read-only, compiled-in tree of SKILL.md documents shipped with
// the SWE-Swarm. Each skill lives at skills/<name>/SKILL.md and is curated source
// — never user input — so embedding it keeps the binary self-contained and the
// catalogue tamper-proof at runtime.
//
// The embed lives here, in swarms/swe, rather than in tools, so the parser and
// loader (in tools) never import swarms/swe. A later phase's SkillLoader takes
// this as an fs.FS (embed.FS satisfies fs.FS) plus a per-agent allow-map: the
// dependency points swarms/swe -> tools, never the reverse, so there is no cycle.
//
//go:embed skills/*/SKILL.md
var SkillsFS embed.FS
