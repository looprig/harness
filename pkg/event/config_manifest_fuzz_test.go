package event

import "testing"

func FuzzManifestCanonical(f *testing.F) {
	f.Add("kind", "model", "root", "tool", "rev", uint8(3))
	f.Fuzz(func(t *testing.T, kind, model, root, tool, rev string, level uint8) {
		m := ConfigManifest{
			SchemaVersion:        ManifestSchemaVersion,
			AgentKind:            kind,
			ModelID:              model,
			WorkspaceRoot:        root,
			Tools:                []ToolManifestEntry{{Name: tool, InputSchemaRev: rev}},
			PermissionStrictness: StrictnessLevel(level),
			AppFields:            map[string]string{kind: model},
		}
		first, second := m.Fingerprint(), m.Fingerprint()
		if first != second {
			t.Fatalf("non-deterministic fingerprint: %s != %s", first, second)
		}
		if len(first) != 64 {
			t.Fatalf("fingerprint length = %d, want 64", len(first))
		}
	})
}
