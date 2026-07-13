package event

import "testing"

func FuzzHustleEvent(f *testing.F) {
	f.Add([]byte(`{"type":"HustleStarted","v":1,"visibility":1}`))
	f.Add([]byte(`{"type":"HustleCompleted","v":1}`))
	f.Add([]byte(`{"type":"HustleFailed","v":1,"stage":255,"reason_code":255}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = UnmarshalEvent(data)
	})
}
