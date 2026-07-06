package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"

	"github.com/looprig/harness/pkg/loop"
)

// policySchemaVersion is bumped whenever the fingerprint input shape changes, so
// digests computed by different schema versions never compare equal by accident.
// v2 added the per-policy GrantDeltas section (Task 17c).
const policySchemaVersion = 2

// FingerprintMode carries the headless mode bits that affect the effective
// permission decision but are not on PermissionPolicy: whether the gate is
// NonInteractiveGate-wrapped and whether the checker was built WithUnattended.
type FingerprintMode struct {
	Wrapped    bool
	Unattended bool
}

// PolicyFingerprint returns a canonical, deterministic hex-sha256 digest over the
// effective native permission configuration, for the
// event.ConfigFingerprint.NativePermissionPolicyRev field (so a durable session
// cannot silently restore under a changed allowlist or posture). Inputs are
// sorted/canonicalized so semantically identical configs digest equally regardless
// of source ordering, and every variable-length value is length-prefixed so no
// operator glob/command containing a separator can alias another config.
func PolicyFingerprint(policy PermissionPolicy, mode FingerprintMode) string {
	var b strings.Builder
	b.WriteString("v")
	b.WriteString(strconv.Itoa(policySchemaVersion))
	b.WriteString("\nwrapped=")
	b.WriteString(strconv.FormatBool(mode.Wrapped))
	b.WriteString("\nunattended=")
	b.WriteString(strconv.FormatBool(mode.Unattended))
	writeSorted(&b, "hardApprove", policy.HardApprove.Tools)
	writePolicies(&b, policy.Policies)
	writeSorted(&b, "denyRead", policy.HardDeny.DeniedReadPaths)
	writeSorted(&b, "denyWrite", policy.HardDeny.DeniedWritePaths)
	writeSorted(&b, "denyBash", policy.HardDeny.DeniedBashPrefixes)
	b.WriteString("\nmaxRead=")
	b.WriteString(strconv.FormatInt(policy.HardDeny.MaxReadBytes, 10))
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// writeLen writes a length-prefixed string ("<len>:<bytes>") so no value can
// alias a separator: the digest cannot collide on operator globs/commands that
// contain ',', ';', '|', or newlines. Every variable-length value in the digest
// goes through writeLen so the byte stream is unambiguous by construction.
func writeLen(b *strings.Builder, s string) {
	b.WriteString(strconv.Itoa(len(s)))
	b.WriteByte(':')
	b.WriteString(s)
}

func writeSorted(b *strings.Builder, label string, in []string) {
	s := slices.Clone(in)
	slices.Sort(s)
	b.WriteByte('\n')
	b.WriteString(label)
	b.WriteByte('=')
	b.WriteString(strconv.Itoa(len(s)))
	for _, e := range s {
		b.WriteByte(':')
		writeLen(b, e)
	}
}

func writePolicies(b *strings.Builder, in []loop.ToolPolicy) {
	lines := make([]string, 0, len(in))
	for _, p := range in {
		m := slices.Clone(p.Match)
		slices.Sort(m)
		var lb strings.Builder
		writeLen(&lb, p.Tool)
		lb.WriteByte('#')
		lb.WriteString(strconv.Itoa(int(p.Effect)))
		lb.WriteByte('#')
		lb.WriteString(strconv.Itoa(len(m)))
		for _, e := range m {
			lb.WriteByte(':')
			writeLen(&lb, e)
		}
		// GrantDeltas are enforcement-affecting (a grant restores only under a
		// matching delta set) and are canonicalized (sorted + length-prefixed) exactly
		// like Match, so delta reordering is digest-stable and any content change is
		// digest-sensitive. Always present (even count 0) so the section is unambiguous.
		d := slices.Clone(p.GrantDeltas)
		slices.Sort(d)
		lb.WriteByte('#')
		lb.WriteString(strconv.Itoa(len(d)))
		for _, e := range d {
			lb.WriteByte(':')
			writeLen(&lb, e)
		}
		lines = append(lines, lb.String())
	}
	slices.Sort(lines)
	b.WriteString("\npolicies=")
	b.WriteString(strconv.Itoa(len(lines)))
	for _, l := range lines {
		b.WriteByte(':')
		writeLen(b, l)
	}
}
