package tee

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-tdx-guest/verify/trust"
)

// loadTestQuote decodes the QuoteV4 raw bytes from the aci/1 testdata report.
// It is a genuine Intel TDX QuoteV4 (version 0x0004), so abi.QuoteToProto and
// the parse/validate prologue of VerifyTDXQuote succeed; only the chain/
// collateral verification differs by option.
func loadTestQuote(t *testing.T) (raw []byte, reportData []byte) {
	t.Helper()
	path := filepath.Join("..", "aci", "testdata", "report_aci1.json")
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read testdata %s: %v", path, err)
	}
	var doc struct {
		Attestation struct {
			Evidence struct {
				Quote           string `json:"quote"`
				QuoteReportData string `json:"quote_report_data"`
			} `json:"evidence"`
		} `json:"attestation"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatalf("unmarshal testdata: %v", err)
	}
	raw, err = hex.DecodeString(doc.Attestation.Evidence.Quote)
	if err != nil {
		t.Fatalf("decode quote hex: %v", err)
	}
	reportData, err = hex.DecodeString(doc.Attestation.Evidence.QuoteReportData)
	if err != nil {
		t.Fatalf("decode report_data hex: %v", err)
	}
	return raw, reportData
}

// recordingGetter is a fake trust.HTTPSGetter that records invocation and the
// URLs it was asked to fetch, then returns a canned failure. Verification will
// fail (no real collateral), which is fine: the test asserts the getter was
// threaded through and invoked, not that verification succeeds.
type recordingGetter struct {
	called bool
	urls   []string
}

func (g *recordingGetter) Get(url string) (map[string][]string, []byte, error) {
	g.called = true
	g.urls = append(g.urls, url)
	return nil, nil, errors.New("recordingGetter: no collateral served")
}

func TestVerifyTDXQuoteWithOptions_GetterThreaded(t *testing.T) {
	t.Parallel()
	raw, _ := loadTestQuote(t)

	tests := []struct {
		name          string
		getCollateral bool
		wantCalled    bool
	}{
		{
			name:          "collateral on uses injected getter",
			getCollateral: true,
			wantCalled:    true,
		},
		{
			name:          "collateral off never touches getter",
			getCollateral: false,
			wantCalled:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &recordingGetter{}
			_, err := VerifyTDXQuoteWithOptions(raw, Options{
				GetCollateral: tt.getCollateral,
				Getter:        fake,
			})
			// With a fake getter that serves no collateral, verification
			// must fail when collateral is requested. When collateral is
			// off, no network is attempted so the local chain verification
			// runs to completion (its pass/fail is not what we assert).
			if tt.getCollateral && err == nil {
				t.Fatalf("expected verification error with no-collateral getter, got nil")
			}
			if fake.called != tt.wantCalled {
				t.Fatalf("getter called = %v, want %v (urls=%v)", fake.called, tt.wantCalled, fake.urls)
			}
		})
	}
}

func TestVerifyTDXQuote_BackCompat(t *testing.T) {
	t.Parallel()
	raw, wantReportData := loadTestQuote(t)

	// The wrapper does no network I/O (collateral + revocation off). On this
	// real-but-not-currently-chainable testdata quote, local chain
	// verification fails; the contract we lock in is: identical behavior to
	// calling VerifyTDXQuoteWithOptions with a zero Options.
	gotWrap, errWrap := VerifyTDXQuote(raw)
	gotOpts, errOpts := VerifyTDXQuoteWithOptions(raw, Options{})

	if (errWrap == nil) != (errOpts == nil) {
		t.Fatalf("wrapper err=%v, options err=%v: error presence differs", errWrap, errOpts)
	}
	if !reflect.DeepEqual(gotWrap, gotOpts) {
		t.Fatalf("wrapper report_data %x != options report_data %x", gotWrap, gotOpts)
	}

	// Both must classify identically via *tee.Error when they fail.
	if errWrap != nil {
		var teeErrWrap, teeErrOpts *Error
		if !errors.As(errWrap, &teeErrWrap) {
			t.Fatalf("wrapper error not *tee.Error: %T %v", errWrap, errWrap)
		}
		if !errors.As(errOpts, &teeErrOpts) {
			t.Fatalf("options error not *tee.Error: %T %v", errOpts, errOpts)
		}
		if teeErrWrap.Reason != teeErrOpts.Reason {
			t.Fatalf("reason mismatch: wrapper=%q options=%q", teeErrWrap.Reason, teeErrOpts.Reason)
		}
	} else {
		// Success path: report_data must be the quote's 64-byte field.
		if len(gotWrap) != 64 {
			t.Fatalf("report_data len = %d, want 64", len(gotWrap))
		}
		if !reflect.DeepEqual(gotWrap, wantReportData) {
			t.Fatalf("report_data %x != expected %x", gotWrap, wantReportData)
		}
	}
}

func TestVerifyTDXQuoteWithOptions_ParseErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		raw        []byte
		wantReason Reason
	}{
		{
			name:       "empty bytes are malformed",
			raw:        []byte{},
			wantReason: ReasonEvidenceMalformed,
		},
		{
			name:       "garbage bytes are malformed",
			raw:        []byte("not a tdx quote at all"),
			wantReason: ReasonEvidenceMalformed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := VerifyTDXQuoteWithOptions(tt.raw, Options{})
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			var teeErr *Error
			if !errors.As(err, &teeErr) {
				t.Fatalf("expected *tee.Error, got %T: %v", err, err)
			}
			if teeErr.Reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", teeErr.Reason, tt.wantReason)
			}
		})
	}
}

func TestResolveGetter_DefaultIsBounded(t *testing.T) {
	t.Parallel()

	// nil Getter must resolve to our bounded type, never the unbounded
	// library default (trust.RetryHTTPSGetter wrapping SimpleHTTPSGetter).
	got := resolveGetter(Options{})
	if _, ok := got.(*boundedGetter); !ok {
		t.Fatalf("default getter type = %T, want *boundedGetter", got)
	}

	defType := reflect.TypeOf(trust.DefaultHTTPSGetter())
	if reflect.TypeOf(got) == defType {
		t.Fatalf("default getter resolved to library default %v; must be bounded", defType)
	}

	// An injected getter is returned verbatim.
	fake := &recordingGetter{}
	if resolveGetter(Options{Getter: fake}) != fake {
		t.Fatalf("resolveGetter did not return the injected getter")
	}
}

func TestBoundedGetter_RejectsNonHTTPS(t *testing.T) {
	t.Parallel()
	g := newBoundedGetter()
	tests := []struct {
		name string
		url  string
	}{
		{name: "plain http", url: "http://example.com/cert"},
		{name: "ftp scheme", url: "ftp://example.com/cert"},
		{name: "no scheme", url: "example.com/cert"},
		{name: "garbage url", url: "://::not-a-url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := g.Get(tt.url)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tt.url)
			}
			var ge *getterError
			if !errors.As(err, &ge) {
				t.Fatalf("expected *getterError, got %T: %v", err, err)
			}
		})
	}
}

func TestBoundedGetter_HTTPSStatusAndBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("X-Test", "yes")
			_, _ = w.Write([]byte("collateral-body"))
		case "/notfound":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	// Build the bounded getter, then graft the test server's self-signed cert
	// pool onto its transport so TLS verification succeeds while preserving the
	// bounded TLS floor. The httptest client carries the server's root pool.
	g := newBoundedGetter()
	tr, ok := g.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("bounded getter transport is %T, want *http.Transport", g.client.Transport)
	}
	clientTransport, ok := srv.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httptest client transport is %T, want *http.Transport", srv.Client().Transport)
	}
	tr.TLSClientConfig.RootCAs = clientTransport.TLSClientConfig.RootCAs
	if tr.TLSClientConfig.MinVersion != boundedTLSMinVersion {
		t.Fatalf("bounded getter TLS min version = %x, want %x", tr.TLSClientConfig.MinVersion, boundedTLSMinVersion)
	}

	t.Run("2xx returns header and body", func(t *testing.T) {
		header, body, err := g.Get(srv.URL + "/ok")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(body) != "collateral-body" {
			t.Fatalf("body = %q, want %q", body, "collateral-body")
		}
		if got := header["X-Test"]; len(got) != 1 || got[0] != "yes" {
			t.Fatalf("missing X-Test header: %v", header)
		}
	})

	t.Run("status >= 300 is an error", func(t *testing.T) {
		_, _, err := g.Get(srv.URL + "/notfound")
		if err == nil {
			t.Fatalf("expected error for 404, got nil")
		}
		var ge *getterError
		if !errors.As(err, &ge) {
			t.Fatalf("expected *getterError, got %T: %v", err, err)
		}
	})
}

func TestBoundedGetter_EnforcesDeadline(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep well past the per-request deadline the test installs.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		_, _ = w.Write([]byte("late"))
	}))
	defer srv.Close()

	g := newBoundedGetter()
	g.client = srv.Client()                  // trust the self-signed cert
	g.client.Timeout = 50 * time.Millisecond // tighten for the test
	g.requestTimeout = 50 * time.Millisecond // tighten the ctx deadline too

	start := time.Now()
	_, _, err := g.Get(srv.URL + "/slow")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if elapsed > time.Second {
		t.Fatalf("getter did not honor deadline; took %v", elapsed)
	}
}

func TestVerifyTDXQuoteWithOptions_NowCalled(t *testing.T) {
	t.Parallel()
	raw, _ := loadTestQuote(t)

	var called bool
	sentinel := time.Now()
	now := func() time.Time {
		called = true
		return sentinel
	}

	// Now is only consulted when verification runs; with collateral off the
	// local chain check still consults options.Now for cert validity.
	_, _ = VerifyTDXQuoteWithOptions(raw, Options{Now: now})
	if !called {
		t.Fatalf("opts.Now was never called")
	}
}

// wantRTMR3Hex is the RTMR3 the fixture aci/1 quote carries (4th entry of the
// TDX quote body's RTMRs, 48 bytes). Independently confirmed against the
// IMR3 event-log replay (the aci package replays the same value).
const wantRTMR3Hex = "4861f99a6e910713667986c6ae6b4830c562eec3aad9e55d25a15bcf7c8dfc0a6b4fde2c326cdc6b3fcf708df20c10c9"

func TestTDXQuoteRTMR3_Fixture(t *testing.T) {
	t.Parallel()
	raw, _ := loadTestQuote(t)

	got, err := TDXQuoteRTMR3(raw)
	if err != nil {
		t.Fatalf("TDXQuoteRTMR3() unexpected error: %v", err)
	}
	if len(got) != 48 {
		t.Fatalf("RTMR3 len = %d, want 48", len(got))
	}
	if hex.EncodeToString(got) != wantRTMR3Hex {
		t.Fatalf("RTMR3 = %s, want %s", hex.EncodeToString(got), wantRTMR3Hex)
	}
}

func TestTDXQuoteRTMR3_ReturnsCopy(t *testing.T) {
	t.Parallel()
	raw, _ := loadTestQuote(t)

	first, err := TDXQuoteRTMR3(raw)
	if err != nil {
		t.Fatalf("first call error: %v", err)
	}
	// Mutating the returned slice must not poison a subsequent parse of the
	// same quote bytes: the accessor must hand back a copy, not an alias into
	// the parsed quote body.
	for i := range first {
		first[i] ^= 0xFF
	}
	second, err := TDXQuoteRTMR3(raw)
	if err != nil {
		t.Fatalf("second call error: %v", err)
	}
	if hex.EncodeToString(second) != wantRTMR3Hex {
		t.Fatalf("second RTMR3 = %s, want %s (returned slice aliased quote body)", hex.EncodeToString(second), wantRTMR3Hex)
	}
}

func TestTDXQuoteRTMR3_Malformed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "empty bytes", raw: []byte{}},
		{name: "garbage bytes", raw: []byte("not a tdx quote at all")},
		{name: "nil", raw: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := TDXQuoteRTMR3(tt.raw)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			var teeErr *Error
			if !errors.As(err, &teeErr) {
				t.Fatalf("expected *tee.Error, got %T: %v", err, err)
			}
			if teeErr.Reason != ReasonEvidenceMalformed {
				t.Fatalf("reason = %q, want %q", teeErr.Reason, ReasonEvidenceMalformed)
			}
		})
	}
}

// Compile-time assertion that boundedGetter satisfies the library interface.
var _ trust.HTTPSGetter = (*boundedGetter)(nil)
