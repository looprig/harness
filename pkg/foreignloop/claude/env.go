package claude

import (
	"sort"
	"strings"
)

// whitelistEnv is the child-environment gate: it returns ONLY the allow-listed
// KEY=VALUE entries from parent, with every extra entry appended as KEY=VALUE. It is
// the boundary that keeps the parent's secrets (anything not allow-listed) out of the
// child — never return os.Environ() wholesale, always run it through here.
//
// An extra key overrides a same-named parent key: the shadowed parent entry is
// dropped so the result holds exactly one entry per key. Order is deterministic:
// allow-listed parent entries in parent order, then extra keys sorted, so tests and
// audits are stable.
func whitelistEnv(parent []string, allow []string, extra map[string]string) []string {
	allowed := make(map[string]struct{}, len(allow))
	for _, k := range allow {
		allowed[k] = struct{}{}
	}
	out := make([]string, 0, len(parent)+len(extra))
	for _, e := range parent {
		i := strings.IndexByte(e, '=')
		if i < 0 {
			continue // not a KEY=VALUE element.
		}
		key := e[:i]
		if _, ok := allowed[key]; !ok {
			continue
		}
		if _, shadowed := extra[key]; shadowed {
			continue // extra wins; skip the parent entry to avoid duplicates.
		}
		out = append(out, e)
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+extra[k])
	}
	return out
}
