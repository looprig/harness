package llm

import (
	"context"
	"net/http"
	"testing"

	"github.com/ciram-co/looprig/pkg/content"
)

type fakeCodec struct{}

func (fakeCodec) EncodeRequest(Request, RequestMode) ([]byte, error) { return []byte("{}"), nil }
func (fakeCodec) DecodeResponse([]byte) (*Response, error)           { return &Response{}, nil }
func (fakeCodec) DecodeEvent([]byte) ([]content.Chunk, error)        { return nil, nil }

type fakeAuth struct{ called bool }

func (f *fakeAuth) Authorize(_ context.Context, r *http.Request) error {
	f.called = true
	r.Header.Set("X-Test", "1")
	return nil
}

func TestCodecSatisfiable(t *testing.T) {
	t.Parallel()
	var _ Codec = fakeCodec{} // compile-time assertion
}

func TestAuthenticatorSatisfiable(t *testing.T) {
	t.Parallel()
	var a Authenticator = &fakeAuth{}
	req, _ := http.NewRequest(http.MethodGet, "https://example.test", nil)
	if err := a.Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize error: %v", err)
	}
	if req.Header.Get("X-Test") != "1" {
		t.Errorf("Authorize did not mutate request header")
	}
}
