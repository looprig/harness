package loopruntime

import "testing"

func FuzzCompactionSummaryXML(f *testing.F) {
	f.Add([]byte(`<conversation_summary><goal>g</goal><constraints></constraints><decisions></decisions><state>s</state><open_items></open_items></conversation_summary>`))
	f.Add([]byte(`<conversation_summary/>`))
	f.Add([]byte(`<wrong/>`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		summary, err := ParseCompactionSummaryXML(raw)
		if err != nil {
			return
		}
		if summary == nil || len(summary.Blocks) != 1 {
			t.Fatal("accepted XML did not produce one summary block")
		}
		if _, err := ParseCompactionSummaryXML(raw); err != nil {
			t.Fatalf("accepted XML was not stable: %v", err)
		}
	})
}
