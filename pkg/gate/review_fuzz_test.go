package gate

import (
	"bytes"
	"reflect"
	"testing"
)

func FuzzPermissionReviewSubjectWire(f *testing.F) {
	subject := validPermissionReviewSubjectForFuzz(f)
	valid, err := marshalPermissionReviewSubject(subject)
	if err != nil {
		f.Fatalf("marshalPermissionReviewSubject(seed) error = %v", err)
	}
	f.Add(valid)
	f.Add([]byte("null"))
	f.Add([]byte(`{"version":"permission_review_subject.v1","version":"duplicate"}`))
	f.Add([]byte(`{"attacker_secret_key":"attacker_secret_value"}`))
	f.Add(bytes.Replace(valid, []byte(`"summary":"run git status"`), []byte(`"summary":null`), 1))
	f.Add(bytes.Replace(valid, []byte(`"truncated":false`), []byte(`"truncated":null`), 1))
	f.Add(bytes.Replace(valid, []byte(`"omitted_entries":0`), []byte(`"omitted_entries":null`), 1))

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := unmarshalPermissionReviewSubject(data)
		if err != nil {
			if !reflect.DeepEqual(got, PermissionReviewSubject{}) {
				t.Fatalf("error returned nonzero subject: %#v", got)
			}
			if len(err.Error()) > 128 {
				t.Fatalf("error length = %d, want bounded", len(err.Error()))
			}
			return
		}

		first, err := SubjectDigest(got)
		if err != nil {
			t.Fatalf("SubjectDigest(decoded) error = %v", err)
		}
		second, err := SubjectDigest(got.Clone())
		if err != nil || second != first || got.Basis.SubjectDigest != first {
			t.Fatalf("digest = (%x, %x, stored %x, %v), want equal", first, second, got.Basis.SubjectDigest, err)
		}
		wire, err := marshalPermissionReviewSubject(got)
		if err != nil {
			t.Fatalf("marshalPermissionReviewSubject(decoded) error = %v", err)
		}
		roundTrip, err := unmarshalPermissionReviewSubject(wire)
		if err != nil {
			t.Fatalf("unmarshalPermissionReviewSubject(round trip) error = %v", err)
		}
		clone := roundTrip.Clone()
		if len(clone.Context.Entries) > 0 {
			clone.Context.Entries[0].Content += " mutation"
			if clone.Context.Entries[0].Content == roundTrip.Context.Entries[0].Content {
				t.Fatal("round-trip clone aliases context entries")
			}
		}
		if len(clone.Request.Requirements) > 0 {
			clone.Request.Requirements[0].Description += " mutation"
			if clone.Request.Requirements[0].Description == roundTrip.Request.Requirements[0].Description {
				t.Fatal("round-trip clone aliases requirements")
			}
		}
	})
}

func FuzzPermissionReviewSubjectDigest(f *testing.F) {
	f.Add([]byte("inspect the repository"))
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xfe})

	f.Fuzz(func(t *testing.T, content []byte) {
		subject := validPermissionReviewSubjectForFuzz(t)
		subject.Context.Entries[0].Content = string(content)
		first, err := SubjectDigest(subject)
		if err != nil {
			if first != ([32]byte{}) {
				t.Fatalf("SubjectDigest(error) = %x, want zero", first)
			}
			return
		}
		second, err := SubjectDigest(subject.Clone())
		if err != nil || second != first {
			t.Fatalf("SubjectDigest() = (%x, %x, %v), want deterministic", first, second, err)
		}
	})
}

func validPermissionReviewSubjectForFuzz(t testing.TB) PermissionReviewSubject {
	t.Helper()
	return validPermissionReviewSubject(t)
}
