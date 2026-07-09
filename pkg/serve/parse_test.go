package serve

import (
	"errors"
	"net/url"
	"testing"

	"github.com/looprig/core/uuid"
)

// TestParseSessionID and TestParseGateID share the same UUID validation, so a
// single table drives both via the parser under test.
func TestParseIDs(t *testing.T) {
	t.Parallel()
	const canonical = "11111111-2222-3333-4444-555555555555"

	parsers := map[string]func(string) (uuid.UUID, error){
		"session": parseSessionID,
		"gate":    parseGateID,
	}
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "valid canonical", in: canonical, wantErr: false},
		{name: "empty", in: "", wantErr: true},
		{name: "wrong length", in: "11111111-2222-3333-4444-5555", wantErr: true},
		{name: "non-hex", in: "zzzzzzzz-2222-3333-4444-555555555555", wantErr: true},
		{name: "missing hyphens", in: "111111112222333344445555555555550", wantErr: true},
	}
	for pname, parse := range parsers {
		parse := parse
		for _, tt := range tests {
			tt := tt
			t.Run(pname+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				got, err := parse(tt.in)
				if (err != nil) != tt.wantErr {
					t.Fatalf("parse(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
				}
				if tt.wantErr {
					var pe InvalidParamError
					if !errors.As(err, &pe) {
						t.Errorf("err = %v, want InvalidParamError", err)
					}
					return
				}
				if got != uuid.MustParse(canonical) {
					t.Errorf("parse(%q) = %v, want %v", tt.in, got, canonical)
				}
			})
		}
	}
}

func TestParseLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     string // "" means absent
		absent  bool
		want    int
		wantErr bool
	}{
		{name: "absent yields default", absent: true, want: defaultLimit},
		{name: "empty yields default", raw: "", want: defaultLimit},
		{name: "explicit valid", raw: "50", want: 50},
		{name: "minimum one", raw: "1", want: minLimit},
		{name: "zero is below minimum", raw: "0", wantErr: true},
		{name: "exactly cap", raw: "1000", want: maxLimit},
		{name: "above cap", raw: "1001", wantErr: true},
		{name: "negative", raw: "-1", wantErr: true},
		{name: "non-numeric", raw: "abc", wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			values := url.Values{}
			if !tt.absent {
				values.Set("limit", tt.raw)
			}
			got, err := parseLimit(values)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseLimit(%q) err = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
			if tt.wantErr {
				var pe InvalidParamError
				if !errors.As(err, &pe) {
					t.Errorf("err = %v, want InvalidParamError", err)
				}
				return
			}
			if got != tt.want {
				t.Errorf("parseLimit(%q) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

func TestParseSkip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     string
		absent  bool
		want    int
		wantErr bool
	}{
		{name: "absent yields zero", absent: true, want: 0},
		{name: "explicit valid", raw: "25", want: 25},
		{name: "zero is valid", raw: "0", want: 0},
		{name: "negative", raw: "-1", wantErr: true},
		{name: "non-numeric", raw: "x", wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			values := url.Values{}
			if !tt.absent {
				values.Set("skip", tt.raw)
			}
			got, err := parseSkip(values)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseSkip(%q) err = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
			if tt.wantErr {
				var pe InvalidParamError
				if !errors.As(err, &pe) {
					t.Errorf("err = %v, want InvalidParamError", err)
				}
				return
			}
			if got != tt.want {
				t.Errorf("parseSkip(%q) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

func TestParseFromJournalSeq(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     string
		absent  bool
		want    uint64
		wantErr bool
	}{
		{name: "absent yields zero", absent: true, want: 0},
		{name: "empty yields zero", raw: "", want: 0},
		{name: "explicit valid", raw: "42", want: 42},
		{name: "zero is valid", raw: "0", want: 0},
		{name: "large valid", raw: "18446744073709551615", want: 18446744073709551615},
		{name: "negative", raw: "-1", wantErr: true},
		{name: "non-numeric", raw: "nope", wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			values := url.Values{}
			if !tt.absent {
				values.Set("from_journal_seq", tt.raw)
			}
			got, err := parseFromJournalSeq(values)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseFromJournalSeq(%q) err = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
			if tt.wantErr {
				var pe InvalidParamError
				if !errors.As(err, &pe) {
					t.Errorf("err = %v, want InvalidParamError", err)
				}
				return
			}
			if got != tt.want {
				t.Errorf("parseFromJournalSeq(%q) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}
