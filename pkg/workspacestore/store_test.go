package workspacestore

import (
	"testing"

	"github.com/ciram-co/storekit/memstore"
)

// TestOpenResolvesLimitDefaults guards the wiring the plan called out explicitly:
// Open must resolve the extraction bounds to their defaults when the caller
// leaves them unset (or passes a non-positive value), and must honor an explicit
// override. A regression here would leave the decompression-bomb guards at zero
// — i.e. rejecting every archive — so the defaults must actually be applied.
func TestOpenResolvesLimitDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		opts           []Option
		wantMaxEntries int64
		wantMaxBytes   int64
	}{
		{
			name:           "no options resolves both defaults",
			wantMaxEntries: defaultMaxEntries,
			wantMaxBytes:   defaultMaxBytes,
		},
		{
			name:           "explicit positive values are kept",
			opts:           []Option{WithMaxEntries(7), WithMaxBytes(4096)},
			wantMaxEntries: 7,
			wantMaxBytes:   4096,
		},
		{
			name:           "zero values resolve to defaults",
			opts:           []Option{WithMaxEntries(0), WithMaxBytes(0)},
			wantMaxEntries: defaultMaxEntries,
			wantMaxBytes:   defaultMaxBytes,
		},
		{
			name:           "negative values resolve to defaults",
			opts:           []Option{WithMaxEntries(-1), WithMaxBytes(-1)},
			wantMaxEntries: defaultMaxEntries,
			wantMaxBytes:   defaultMaxBytes,
		},
		{
			name:           "only entries overridden keeps byte default",
			opts:           []Option{WithMaxEntries(3)},
			wantMaxEntries: 3,
			wantMaxBytes:   defaultMaxBytes,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, err := Open(memstore.New().Blobs, tt.opts...)
			if err != nil {
				t.Fatalf("Open(): %v", err)
			}
			if s.opts.MaxEntries != tt.wantMaxEntries {
				t.Errorf("opts.MaxEntries = %d, want %d", s.opts.MaxEntries, tt.wantMaxEntries)
			}
			if s.opts.MaxBytes != tt.wantMaxBytes {
				t.Errorf("opts.MaxBytes = %d, want %d", s.opts.MaxBytes, tt.wantMaxBytes)
			}
		})
	}
}
