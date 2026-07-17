package gate

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// secretURL is a realistic authorization URL: every component after the origin
// is a secret that must never be journaled.
const secretURL = "https://github.com/login/oauth/authorize?client_id=abc&state=S3CR3T-STATE&code_challenge=PKCE-CHALLENGE&redirect_uri=http%3A%2F%2F127.0.0.1%3A9999%2Fcb"

func TestOpenURLPayloadRoundTripDropsURL(t *testing.T) {
	t.Parallel()

	payload := OpenURLPayload{
		DisplayOrigin:      "https://github.com",
		URL:                secretURL,
		RequiresCompletion: true,
	}

	data, err := MarshalPayload(payload)
	if err != nil {
		t.Fatalf("MarshalPayload() error = %v", err)
	}
	got, err := UnmarshalPayload(data)
	if err != nil {
		t.Fatalf("UnmarshalPayload() error = %v", err)
	}

	// The round-trip is deliberately LOSSY: URL is ephemeral and is never
	// journaled, so a decoded payload always has an empty URL.
	want := OpenURLPayload{DisplayOrigin: "https://github.com", RequiresCompletion: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip = %#v, want %#v", got, want)
	}
	decoded, ok := got.(OpenURLPayload)
	if !ok {
		t.Fatalf("payload type = %T, want OpenURLPayload", got)
	}
	if decoded.URL != "" {
		t.Fatalf("decoded URL = %q, want empty", decoded.URL)
	}
}

// The core guarantee: no fragment of the action target survives marshaling.
func TestOpenURLPayloadMarshalExcludesURL(t *testing.T) {
	t.Parallel()

	payload := OpenURLPayload{
		DisplayOrigin:      "https://github.com",
		URL:                secretURL,
		RequiresCompletion: true,
	}
	data, err := MarshalPayload(payload)
	if err != nil {
		t.Fatalf("MarshalPayload() error = %v", err)
	}
	assertNoSecrets(t, data)

	// ...including when nested in the OpenPayload the journal actually writes.
	nested, err := MarshalPayload(OpenPayload{GateID: ID{}, Payload: payload})
	if err != nil {
		t.Fatalf("MarshalPayload(OpenPayload) error = %v", err)
	}
	assertNoSecrets(t, nested)

	// ...and when a caller marshals the struct directly, bypassing the codec.
	direct, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	assertNoSecrets(t, direct)
}

func assertNoSecrets(t *testing.T, data []byte) {
	t.Helper()

	got := string(data)
	for _, secret := range []string{
		secretURL,
		"S3CR3T-STATE",
		"PKCE-CHALLENGE",
		"code_challenge",
		"client_id",
		"redirect_uri",
		"/login/oauth/authorize",
	} {
		if strings.Contains(got, secret) {
			t.Fatalf("marshaled payload leaks %q: %s", secret, got)
		}
	}
	if strings.Contains(got, `"url"`) {
		t.Fatalf("marshaled payload carries a url key: %s", got)
	}
}

// The durable data type has no URL field, so a record carrying one is rejected
// rather than silently accepted and dropped.
func TestOpenURLPayloadRejectsURLBearingRecord(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"kind":"open_url","data":{"display_origin":"https://github.com","url":"` + secretURL + `"}}`)
	_, err := UnmarshalPayload(raw)
	var decodeErr *PayloadDecodeError
	if !errors.As(err, &decodeErr) {
		t.Fatalf("UnmarshalPayload() error = %v, want *PayloadDecodeError", err)
	}
}

func TestOpenURLPayloadUsesOpenURLKindTag(t *testing.T) {
	t.Parallel()

	data, err := MarshalPayload(OpenURLPayload{DisplayOrigin: "https://github.com"})
	if err != nil {
		t.Fatalf("MarshalPayload() error = %v", err)
	}
	var wrapper struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if wrapper.Kind != string(payloadKindOpenURL) {
		t.Fatalf("kind = %q, want %q", wrapper.Kind, payloadKindOpenURL)
	}
}

func TestOpenURLPayloadPointerFormMarshals(t *testing.T) {
	t.Parallel()

	payload := &OpenURLPayload{DisplayOrigin: "https://example.com", URL: secretURL}
	data, err := MarshalPayload(payload)
	if err != nil {
		t.Fatalf("MarshalPayload() error = %v", err)
	}
	got, err := UnmarshalPayload(data)
	if err != nil {
		t.Fatalf("UnmarshalPayload() error = %v", err)
	}
	want := OpenURLPayload{DisplayOrigin: "https://example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip = %#v, want %#v", got, want)
	}
}

func TestOpenURLPayloadNilPointerFailsClosed(t *testing.T) {
	t.Parallel()

	var payload *OpenURLPayload
	_, err := MarshalPayload(payload)
	var nilErr *NilPayloadError
	if !errors.As(err, &nilErr) {
		t.Fatalf("MarshalPayload() error = %v, want *NilPayloadError", err)
	}
}

// Dropping the URL field is worthless if the full URL can be smuggled through
// DisplayOrigin, so a non-bare origin is rejected at both boundaries.
func TestDisplayOriginMustBeBare(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		origin  string
		wantErr bool
	}{
		{name: "https origin", origin: "https://github.com"},
		{name: "http origin", origin: "http://localhost"},
		{name: "origin with port", origin: "https://example.com:8443"},
		{name: "origin with trailing slash", origin: "https://github.com/"},
		{name: "empty", origin: "", wantErr: true},
		{name: "full action url", origin: secretURL, wantErr: true},
		{name: "origin with path", origin: "https://github.com/login/oauth", wantErr: true},
		{name: "origin with query", origin: "https://github.com?state=SECRET", wantErr: true},
		{name: "origin with fragment", origin: "https://github.com#tok", wantErr: true},
		{name: "origin with userinfo", origin: "https://user:pass@github.com", wantErr: true},
		{name: "no scheme", origin: "github.com", wantErr: true},
		{name: "no host", origin: "https://", wantErr: true},
		{name: "javascript scheme", origin: "javascript:alert(1)", wantErr: true},
		{name: "file scheme", origin: "file:///etc/passwd", wantErr: true},
		{name: "data scheme", origin: "data:text/html,<h1>hi</h1>", wantErr: true},
		{name: "malformed", origin: "https://exa mple.com", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := OpenURLPayload{DisplayOrigin: tt.origin, URL: secretURL}
			data, err := MarshalPayload(payload)
			if (err != nil) != tt.wantErr {
				t.Fatalf("MarshalPayload(%q) error = %v, wantErr %v", tt.origin, err, tt.wantErr)
			}
			if tt.wantErr {
				var originErr *DisplayOriginError
				if !errors.As(err, &originErr) {
					t.Fatalf("MarshalPayload(%q) error = %v, want *DisplayOriginError", tt.origin, err)
				}
				return
			}
			if _, err := UnmarshalPayload(data); err != nil {
				t.Fatalf("UnmarshalPayload(%q) error = %v", tt.origin, err)
			}
		})
	}
}

// A hand-written record must not be able to reintroduce a non-bare origin.
func TestDisplayOriginValidatedOnDecode(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"kind":"open_url","data":{"display_origin":"https://github.com/login/oauth/authorize"}}`)
	_, err := UnmarshalPayload(raw)
	var originErr *DisplayOriginError
	if !errors.As(err, &originErr) {
		t.Fatalf("UnmarshalPayload() error = %v, want *DisplayOriginError", err)
	}
	var decodeErr *PayloadDecodeError
	if !errors.As(err, &decodeErr) {
		t.Fatalf("UnmarshalPayload() error = %v, want *PayloadDecodeError", err)
	}
}

func TestValidateGate(t *testing.T) {
	t.Parallel()

	bare := Prompt{Origin: "https://github.com"}

	tests := []struct {
		name     string
		gate     Gate
		wantErr  bool
		wantKind GateValidationErrorKind
	}{
		{
			name: "open-url gate not restorable",
			gate: Gate{Kind: KindOpenURL, Restorable: false, Prompt: bare},
		},
		{
			name:     "open-url gate restorable is rejected",
			gate:     Gate{Kind: KindOpenURL, Restorable: true, Prompt: bare},
			wantErr:  true,
			wantKind: GateRestorableNotAllowed,
		},
		{
			// The envelope is all a renderer sees. An open-url gate with no origin
			// asks a human to authorize an unnamed party.
			name:     "open-url gate with no origin is rejected",
			gate:     Gate{Kind: KindOpenURL},
			wantErr:  true,
			wantKind: GateOriginInvalid,
		},
		{
			// The exact attack the payload-side check exists to stop, now closed on
			// the envelope too: an action URL is not an origin.
			name:     "open-url gate whose origin is an action URL is rejected",
			gate:     Gate{Kind: KindOpenURL, Prompt: Prompt{Origin: "https://idp.example/authorize?state=SECRET"}},
			wantErr:  true,
			wantKind: GateOriginInvalid,
		},
		{
			name:     "open-url gate with a non-http origin is rejected",
			gate:     Gate{Kind: KindOpenURL, Prompt: Prompt{Origin: "javascript:alert(1)"}},
			wantErr:  true,
			wantKind: GateOriginInvalid,
		},
		// Every pre-existing kind must validate clean: this hook is additive and
		// must not retroactively reject envelopes consumers already open.
		{name: "permission gate not restorable", gate: Gate{Kind: KindPermission}},
		{name: "permission gate restorable", gate: Gate{Kind: KindPermission, Restorable: true}},
		{name: "ask-user gate not restorable", gate: Gate{Kind: KindAskUser}},
		{name: "ask-user gate restorable", gate: Gate{Kind: KindAskUser, Restorable: true}},
		{name: "form gate restorable", gate: Gate{Kind: KindForm, Restorable: true}},
		// Origin is an open-url concept. This hook is additive and must not start
		// policing an envelope field on kinds that never carried one.
		{name: "form gate with an origin", gate: Gate{Kind: KindForm, Prompt: bare}},
		{name: "permission gate with a junk origin", gate: Gate{Kind: KindPermission, Prompt: Prompt{Origin: "not a url"}}},
		{name: "zero gate", gate: Gate{}},
		{name: "unknown kind restorable", gate: Gate{Kind: Kind("whatever"), Restorable: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateGate(tt.gate)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateGate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			var validationErr *GateValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("ValidateGate() error = %v, want *GateValidationError", err)
			}
			if validationErr.Kind != tt.wantKind {
				t.Fatalf("kind = %q, want %q", validationErr.Kind, tt.wantKind)
			}
		})
	}
}
