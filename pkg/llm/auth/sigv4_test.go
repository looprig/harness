package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Known-good inputs from the AWS "aws-sig-v4-test-suite" reference vectors.
// Access key / secret / region / date are the canonical published example values.
const (
	vecAccessKey = "AKIDEXAMPLE"
	vecSecretKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	vecRegion    = "us-east-1"
	vecService   = "service"
	vecAmzDate   = "20150830T123600Z"
	// credPrefix is the Credential= portion shared by every vector below.
	credPrefix = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request"
)

// fixedClock pins sign-time to the vector timestamp so signatures are deterministic.
func fixedClock() time.Time {
	return time.Date(2015, time.August, 30, 12, 36, 0, 0, time.UTC)
}

func TestSigV4KnownVectors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		method       string
		url          string
		body         string
		sessionToken string
		wantAuthz    string
		wantTokenHdr string // "" means the header must be absent
	}{
		{
			name:   "get-vanilla no body no token",
			method: http.MethodGet,
			url:    "https://example.amazonaws.com/",
			// Published aws-sig-v4-test-suite get-vanilla signature.
			wantAuthz: credPrefix +
				", SignedHeaders=host;x-amz-date" +
				", Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
		},
		{
			name:   "post with body signs payload hash",
			method: http.MethodPost,
			url:    "https://example.amazonaws.com/",
			body:   `{"prompt":"hello"}`,
			wantAuthz: credPrefix +
				", SignedHeaders=host;x-amz-date" +
				", Signature=87119bee1be6ed605a413e4a1907542abd902528c939138099ebb0ddd69991e0",
		},
		{
			name:         "get with session token signs x-amz-security-token",
			method:       http.MethodGet,
			url:          "https://example.amazonaws.com/",
			sessionToken: "AQoEXAMPLEsessiontoken",
			wantAuthz: credPrefix +
				", SignedHeaders=host;x-amz-date;x-amz-security-token" +
				", Signature=19284a1703a89c65adc7a78c6c600567c304a1689f7e9ea7064384b1f1497c10",
			wantTokenHdr: "AQoEXAMPLEsessiontoken",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var body io.Reader
			if tt.body != "" {
				body = strings.NewReader(tt.body)
			}
			req, err := http.NewRequest(tt.method, tt.url, body)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			signer := newSigV4(SigV4Credentials{
				AccessKeyID:     vecAccessKey,
				SecretAccessKey: vecSecretKey,
				SessionToken:    tt.sessionToken,
			}, vecRegion, vecService, fixedClock)

			if err := signer.Authorize(context.Background(), req); err != nil {
				t.Fatalf("Authorize: %v", err)
			}

			if got := req.Header.Get("Authorization"); got != tt.wantAuthz {
				t.Errorf("Authorization =\n  %q\nwant\n  %q", got, tt.wantAuthz)
			}
			if got := req.Header.Get("X-Amz-Date"); got != vecAmzDate {
				t.Errorf("X-Amz-Date = %q, want %q", got, vecAmzDate)
			}
			if got := req.Header.Get("X-Amz-Security-Token"); got != tt.wantTokenHdr {
				t.Errorf("X-Amz-Security-Token = %q, want %q", got, tt.wantTokenHdr)
			}
		})
	}
}

// TestAWSEncode locks the byte-level RFC-3986 encoding, including the
// double-encoding property (an already-escaped "%20" becomes "%2520") and the
// keepSlash distinction between path (keep "/") and query ("/"->"%2F") encoding.
func TestAWSEncode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		in        string
		keepSlash bool
		want      string
	}{
		{name: "colon encodes to %3A (path)", in: ":", keepSlash: true, want: "%3A"},
		{name: "slash preserved when keepSlash", in: "/", keepSlash: true, want: "/"},
		{name: "slash encoded when not keepSlash (query)", in: "/", keepSlash: false, want: "%2F"},
		{name: "unreserved preserved", in: "aZ0-_.~", keepSlash: true, want: "aZ0-_.~"},
		{name: "space encodes to %20", in: " ", keepSlash: true, want: "%20"},
		{name: "already-escaped octet double-encodes", in: "%20", keepSlash: true, want: "%2520"},
		{name: "bedrock model segment colon", in: "anthropic.claude-3-5-sonnet-20241022-v2:0", keepSlash: true, want: "anthropic.claude-3-5-sonnet-20241022-v2%3A0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := awsEncode(tt.in, tt.keepSlash); got != tt.want {
				t.Errorf("awsEncode(%q, %v) = %q, want %q", tt.in, tt.keepSlash, got, tt.want)
			}
		})
	}
}

// TestCanonicalURI proves the non-s3 second URI-encode pass: a Bedrock-style
// colon path is double-encoded (":" -> "%3A") for any non-s3 service, while the
// same path signed for s3 keeps the colon verbatim (single-encoded). Root and
// unreserved paths are unchanged.
func TestCanonicalURI(t *testing.T) {
	t.Parallel()
	const colonPath = "/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke"
	tests := []struct {
		name    string
		service string
		path    string
		want    string
	}{
		{name: "bedrock colon path double-encoded", service: "bedrock", path: colonPath, want: "/model/anthropic.claude-3-5-sonnet-20241022-v2%3A0/invoke"},
		{name: "generic non-s3 service double-encoded", service: vecService, path: colonPath, want: "/model/anthropic.claude-3-5-sonnet-20241022-v2%3A0/invoke"},
		{name: "s3 colon path verbatim (single-encoded)", service: serviceS3, path: colonPath, want: colonPath},
		{name: "root path unchanged non-s3", service: "bedrock", path: "/", want: "/"},
		{name: "unreserved path unchanged non-s3", service: "bedrock", path: "/a-b_c.d~e/f", want: "/a-b_c.d~e/f"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(http.MethodPost, "https://example.amazonaws.com"+tt.path, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			if got := canonicalURI(req, tt.service); got != tt.want {
				t.Errorf("canonicalURI(%q, %q) = %q, want %q", tt.path, tt.service, got, tt.want)
			}
		})
	}
}

// TestSigV4ColonPathSignatureDiffersByService is the behavioral guard on the fix:
// signing an identical colon-bearing request as a non-s3 service (bedrock) versus
// s3 must yield different Authorization signatures, because their canonical URIs
// differ ("%3A" vs ":"). If the double-encode were dropped the two would collide.
func TestSigV4ColonPathSignatureDiffersByService(t *testing.T) {
	t.Parallel()
	const colonURL = "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke"
	creds := SigV4Credentials{AccessKeyID: vecAccessKey, SecretAccessKey: vecSecretKey}

	sign := func(service string) string {
		req, err := http.NewRequest(http.MethodPost, colonURL, strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if err := newSigV4(creds, "us-east-1", service, fixedClock).Authorize(context.Background(), req); err != nil {
			t.Fatalf("Authorize(%s): %v", service, err)
		}
		return req.Header.Get("Authorization")
	}

	bedrockAuthz := sign("bedrock")
	s3Authz := sign(serviceS3)
	if bedrockAuthz == "" || s3Authz == "" {
		t.Fatal("empty Authorization header")
	}
	if bedrockAuthz == s3Authz {
		t.Errorf("bedrock and s3 signatures collided; canonical URI was not double-encoded for non-s3:\n  %q", bedrockAuthz)
	}
}

func TestSigV4BodyReadableAfterSigning(t *testing.T) {
	t.Parallel()

	const payload = `{"prompt":"hello"}`
	req, err := http.NewRequest(http.MethodPost, "https://example.amazonaws.com/", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	signer := newSigV4(SigV4Credentials{
		AccessKeyID:     vecAccessKey,
		SecretAccessKey: vecSecretKey,
	}, vecRegion, vecService, fixedClock)
	if err := signer.Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	// The signed payload hash must correspond to the exact body (asserted via the
	// pinned signature in TestSigV4KnownVectors) AND the body must still be sendable.
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("ReadAll(body): %v", err)
	}
	if string(got) != payload {
		t.Errorf("body after signing = %q, want %q", got, payload)
	}
	// GetBody (used by the transport for retries/redirects) must also be restored.
	if req.GetBody == nil {
		t.Fatalf("GetBody was not restored")
	}
	rc, err := req.GetBody()
	if err != nil {
		t.Fatalf("GetBody(): %v", err)
	}
	got2, _ := io.ReadAll(rc)
	if string(got2) != payload {
		t.Errorf("GetBody body = %q, want %q", got2, payload)
	}
}

func TestSigV4FailsClosedOnMissingCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		creds   SigV4Credentials
		wantErr bool
	}{
		{
			name:    "both present",
			creds:   SigV4Credentials{AccessKeyID: vecAccessKey, SecretAccessKey: vecSecretKey},
			wantErr: false,
		},
		{
			name:    "empty access key",
			creds:   SigV4Credentials{SecretAccessKey: vecSecretKey},
			wantErr: true,
		},
		{
			name:    "empty secret key",
			creds:   SigV4Credentials{AccessKeyID: vecAccessKey},
			wantErr: true,
		},
		{
			name:    "both empty",
			creds:   SigV4Credentials{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req, _ := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
			err := SigV4(tt.creds, vecRegion, vecService).Authorize(context.Background(), req)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Authorize() err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			var missing *MissingSigV4CredentialsError
			if !errors.As(err, &missing) {
				t.Fatalf("error %v is not *MissingSigV4CredentialsError", err)
			}
			// Fail closed: no signature must be written on error.
			if req.Header.Get("Authorization") != "" {
				t.Errorf("Authorization header set despite failure: %q", req.Header.Get("Authorization"))
			}
		})
	}
}

func TestSigV4CredentialsRedaction(t *testing.T) {
	t.Parallel()

	const (
		secret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
		token  = "AQoEXAMPLEsessiontoken-SUPERSECRET"
	)
	creds := SigV4Credentials{
		AccessKeyID:     vecAccessKey,
		SecretAccessKey: secret,
		SessionToken:    token,
	}

	// fmt verbs (incl. the Go-syntax %#v, covered by GoString) and String() must
	// never leak the secret or session token.
	for _, s := range []string{
		fmt.Sprintf("%v", creds),
		fmt.Sprintf("%+v", creds),
		fmt.Sprintf("%v", &creds),
		fmt.Sprintf("%#v", creds),
		fmt.Sprintf("%#v", &creds),
		creds.String(),
	} {
		if strings.Contains(s, secret) || strings.Contains(s, token) {
			t.Errorf("formatted credentials leaked a secret: %q", s)
		}
	}

	// slog must never leak the secret or session token.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("creds", slog.Any("creds", creds))
	if out := buf.String(); strings.Contains(out, secret) || strings.Contains(out, token) {
		t.Errorf("slog output leaked a secret: %q", out)
	}
}
