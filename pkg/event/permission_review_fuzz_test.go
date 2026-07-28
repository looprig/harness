package event

import (
	"bytes"
	"testing"
)

func FuzzPermissionReviewEvent(f *testing.F) {
	for _, ev := range []Event{
		validPermissionReviewStarted(),
		validPermissionReviewCompleted(),
	} {
		data, err := MarshalEvent(ev)
		if err != nil {
			f.Fatalf("MarshalEvent(%T) seed error = %v", ev, err)
		}
		f.Add(data)
	}
	f.Add([]byte(`{"type":"PermissionReviewStarted","v":1}`))
	f.Add([]byte(`{"type":"PermissionReviewCompleted","v":1}`))
	f.Add([]byte(`{"type":"PermissionReviewStarted","type":"PermissionReviewCompleted","v":1}`))
	f.Add([]byte(`{"type":"PermissionReviewCompleted","v":1,"status":"future"}`))
	f.Add([]byte(`{"type":"PermissionReviewCompleted","v":1,"categories":[`))
	f.Add(append([]byte(`{"type":"PermissionReviewStarted","v":1,"classifier":"`), []byte{0xff, '"', '}'}...))
	f.Add(append([]byte(`{"type":"PermissionReviewCompleted","v":1,"classifier":"`), []byte{0xfe, '"', '}'}...))

	f.Fuzz(func(t *testing.T, data []byte) {
		ev, err := UnmarshalEvent(data)
		if err != nil {
			return
		}
		if err := ValidateEvent(ev); err != nil {
			t.Fatalf("successful decode returned invalid %T: %v", ev, err)
		}
		first, err := MarshalEvent(ev)
		if err != nil {
			t.Fatalf("MarshalEvent(successfully decoded %T) error = %v", ev, err)
		}
		decoded, err := UnmarshalEvent(first)
		if err != nil {
			t.Fatalf("UnmarshalEvent(remarshal %T) error = %v", ev, err)
		}
		second, err := MarshalEvent(decoded)
		if err != nil {
			t.Fatalf("MarshalEvent(redecoded %T) error = %v", decoded, err)
		}
		if !bytes.Equal(second, first) {
			t.Fatalf("codec not a fixed point for %T:\nfirst:  %s\nsecond: %s", ev, first, second)
		}
	})
}
