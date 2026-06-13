package uuid

import (
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
