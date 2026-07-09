package codex

import (
	"sort"
	"strings"
)

func whitelistEnv(parent []string, allow []string, credential map[string]string) []string {
	allowed := make(map[string]struct{}, len(allow))
	for _, key := range allow {
		allowed[key] = struct{}{}
	}

	envByKey := make(map[string]string, len(parent)+len(credential))
	for _, entry := range parent {
		i := strings.IndexByte(entry, '=')
		if i < 0 {
			continue
		}
		key := entry[:i]
		if _, ok := allowed[key]; !ok {
			continue
		}
		envByKey[key] = entry[i+1:]
	}
	for key, value := range credential {
		envByKey[key] = value
	}

	keys := make([]string, 0, len(envByKey))
	for key := range envByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+envByKey[key])
	}
	return out
}
