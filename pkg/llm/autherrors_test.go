package llm

import (
	"errors"
	"strings"
	"testing"
)

func TestAuthRequiredError(t *testing.T) {
	t.Parallel()
	err := error(&AuthRequiredError{Provider: ProviderPhala, Kind: AuthAPIKey})
	var are *AuthRequiredError
	if !errors.As(err, &are) {
		t.Fatalf("errors.As failed for *AuthRequiredError")
	}
	if are.Provider != ProviderPhala || are.Kind != AuthAPIKey {
		t.Errorf("fields not preserved: %+v", are)
	}
	if msg := err.Error(); !strings.Contains(msg, "phala") || !strings.Contains(msg, string(AuthAPIKey)) {
		t.Errorf("message missing provider/kind: %q", msg)
	}
}

func TestModelMismatchError(t *testing.T) {
	t.Parallel()
	err := error(&ModelMismatchError{
		BoundProvider: ProviderPhala, RequestProvider: ProviderChutes,
		BoundEndpoint: "https://a", RequestEndpoint: "https://b",
	})
	var mme *ModelMismatchError
	if !errors.As(err, &mme) {
		t.Fatalf("errors.As failed for *ModelMismatchError")
	}
	if mme.RequestProvider != ProviderChutes || mme.BoundProvider != ProviderPhala {
		t.Errorf("fields not preserved: %+v", mme)
	}
	msg := err.Error()
	for _, want := range []string{string(ProviderPhala), string(ProviderChutes), "https://a", "https://b"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}
