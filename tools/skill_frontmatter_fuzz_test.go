package tools

import "testing"

// FuzzSkillFrontmatter throws arbitrary bytes at parseSkill. The parser must
// never panic and must always honor the size cap: an input over maxSkillBytes
// must be rejected with an error, and a successful parse must never escape the
// fail-secure invariant (a non-nil error always yields zero meta and body, which
// the unit tests pin; here we assert the cap and no-panic properties).
func FuzzSkillFrontmatter(f *testing.F) {
	seeds := []string{
		// Well-formed.
		"---\nname: code-style\ndescription: A coding style checklist\n---\nUse tabs.\n",
		// Well-formed, CRLF.
		"---\r\nname: x\r\ndescription: y\r\n---\r\nbody\r\n",
		// Empty body.
		"---\nname: x\ndescription: y\n---\n",
		// No opening fence.
		"name: x\ndescription: y\nbody\n",
		// Unterminated fence.
		"---\nname: x\ndescription: y\nbody\n",
		// Duplicate key.
		"---\nname: a\nname: b\n---\n",
		// Empty.
		"",
		// Just a fence.
		"---",
		// Line without colon.
		"---\nnocolon\n---\n",
		// Embedded NUL and high bytes.
		"---\nname: \x00\xff\n---\n\x00body",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		meta, body, err := parseSkill(raw)

		// Size cap: anything over the ceiling must be rejected.
		if len(raw) > maxSkillBytes && err == nil {
			t.Fatalf("parseSkill accepted oversize input (%d bytes > %d)", len(raw), maxSkillBytes)
		}

		// Fail-secure: an error must never come with a partial parse.
		if err != nil && (meta.Name != "" || meta.Description != "" || body != "") {
			t.Fatalf("parseSkill returned error with non-zero result: meta=%+v body=%q err=%v", meta, body, err)
		}
	})
}
