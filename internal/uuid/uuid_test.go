package uuid

import (
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"
)

var rfc4122v4 = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNew(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		reader  io.Reader
		wantErr bool
		wantStr string // golden String() form, checked when non-empty
	}{
		// All-0x01 input: u[6]=(0x01&0x0f)|0x40=0x41, u[8]=(0x01&0x3f)|0x80=0x81.
		{name: "happy path", reader: strings.NewReader(strings.Repeat("\x01", 16)),
			wantStr: "01010101-0101-4101-8101-010101010101"},
		{name: "short read returns error", reader: strings.NewReader("too short"), wantErr: true},
		{name: "reader failure returns error", reader: iotest.ErrReader(errors.New("boom")), wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u, err := generate(tt.reader)
			if (err != nil) != tt.wantErr {
				t.Fatalf("generate() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var ge *GenerateError
				if !errors.As(err, &ge) {
					t.Fatalf("err = %T, want *GenerateError", err)
				}
				return
			}
			if u == (UUID{}) {
				t.Fatal("generate() returned zero UUID")
			}
			if got := u[6] & 0xf0; got != 0x40 {
				t.Errorf("version nibble = %#x, want 0x40", got)
			}
			if got := u[8] & 0xc0; got != 0x80 {
				t.Errorf("variant bits = %#x, want 0x80", got)
			}
			if tt.wantStr != "" && u.String() != tt.wantStr {
				t.Errorf("String() = %q, want %q", u.String(), tt.wantStr)
			}
		})
	}
}

func TestUUIDString(t *testing.T) {
	t.Parallel()
	u, err := New()
	if err != nil {
		t.Fatalf("New() err = %v", err)
	}
	if !rfc4122v4.MatchString(u.String()) {
		t.Errorf("String() = %q, not RFC 4122 v4", u.String())
	}
}

func TestNewUnique(t *testing.T) {
	t.Parallel()
	a, _ := New()
	b, _ := New()
	if a == b {
		t.Error("two New() calls returned equal UUIDs")
	}
}

func TestUUIDTextRoundTrip(t *testing.T) {
	t.Parallel()
	// deterministic non-zero uuid (bytes 1..16)
	var nonZero UUID
	for i := range nonZero {
		nonZero[i] = byte(i + 1)
	}
	tests := []struct {
		name    string
		in      UUID
		wantStr string
	}{
		{name: "zero", in: UUID{}, wantStr: "00000000-0000-0000-0000-000000000000"},
		{name: "non-zero", in: nonZero, wantStr: "01020304-0506-0708-090a-0b0c0d0e0f10"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			text, err := tt.in.MarshalText()
			if err != nil {
				t.Fatalf("MarshalText() err = %v", err)
			}
			if string(text) != tt.wantStr {
				t.Errorf("MarshalText() = %q, want %q", text, tt.wantStr)
			}
			if string(text) != tt.in.String() {
				t.Errorf("MarshalText() = %q, want String() = %q", text, tt.in.String())
			}
			var got UUID
			if err := got.UnmarshalText(text); err != nil {
				t.Fatalf("UnmarshalText(%q) err = %v", text, err)
			}
			if got != tt.in {
				t.Errorf("round-trip = %v, want %v", got, tt.in)
			}
		})
	}
}

func TestUUIDJSONRoundTrip(t *testing.T) {
	t.Parallel()
	var nonZero UUID
	for i := range nonZero {
		nonZero[i] = byte(i + 1)
	}
	data, err := json.Marshal(nonZero)
	if err != nil {
		t.Fatalf("json.Marshal err = %v", err)
	}
	if want := `"01020304-0506-0708-090a-0b0c0d0e0f10"`; string(data) != want {
		t.Errorf("json.Marshal = %s, want %s", data, want)
	}
	var got UUID
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal err = %v", err)
	}
	if got != nonZero {
		t.Errorf("json round-trip = %v, want %v", got, nonZero)
	}
}

func TestUUIDUnmarshalTextErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
	}{
		{name: "empty", text: ""},
		{name: "too short", text: "01010101-0101-4101-8101-0101010101"},
		{name: "missing hyphens", text: "010101010101410181010101010101010101"},
		{name: "hyphen in wrong place", text: "0101010-10101-4101-8101-010101010101"},
		{name: "non-hex digit", text: "0101010g-0101-4101-8101-010101010101"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var u UUID
			err := u.UnmarshalText([]byte(tt.text))
			if err == nil {
				t.Fatalf("UnmarshalText(%q) err = nil, want error", tt.text)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %T, want *ParseError", err)
			}
		})
	}
}

func TestUUIDIsZero(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		u    UUID
		want bool
	}{
		{name: "zero value", u: UUID{}, want: true},
		{name: "non-zero", u: UUID{1}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.u.IsZero(); got != tt.want {
				t.Errorf("IsZero() = %v, want %v", got, tt.want)
			}
		})
	}
}
