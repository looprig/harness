package logging

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    slog.Level
		wantErr bool
	}{
		{name: "empty defaults to info", input: "", want: slog.LevelInfo},
		{name: "info", input: "info", want: slog.LevelInfo},
		{name: "debug", input: "debug", want: slog.LevelDebug},
		{name: "warn", input: "warn", want: slog.LevelWarn},
		{name: "warning alias", input: "warning", want: slog.LevelWarn},
		{name: "error", input: "error", want: slog.LevelError},
		{name: "uppercase is normalized", input: "DEBUG", want: slog.LevelDebug},
		{name: "surrounding whitespace trimmed", input: "  warn  ", want: slog.LevelWarn},
		{name: "unknown returns info and error", input: "verbose", want: slog.LevelInfo, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseLevel(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseLevel(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
			if tt.wantErr {
				var le *LevelError
				if !errors.As(err, &le) {
					t.Errorf("ParseLevel(%q) error = %v, want *LevelError", tt.input, err)
				} else if le.Value != tt.input {
					t.Errorf("LevelError.Value = %q, want %q", le.Value, tt.input)
				}
			}
		})
	}
}

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		level        slog.Level
		emit         func(*slog.Logger)
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:  "info record carries msg, level, source, and attrs",
			level: slog.LevelInfo,
			emit:  func(l *slog.Logger) { l.Info("turn started", "agent", "coding") },
			wantContains: []string{
				`"level":"INFO"`, `"msg":"turn started"`, `"agent":"coding"`, `"source"`,
			},
		},
		{
			name:       "debug is filtered out at info level",
			level:      slog.LevelInfo,
			emit:       func(l *slog.Logger) { l.Debug("noisy detail") },
			wantAbsent: []string{"noisy detail"},
		},
		{
			name:         "debug passes at debug level",
			level:        slog.LevelDebug,
			emit:         func(l *slog.Logger) { l.Debug("shown now") },
			wantContains: []string{`"level":"DEBUG"`, `"msg":"shown now"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			log := New(Config{Writer: &buf, Level: tt.level})
			tt.emit(log)

			got := buf.String()
			for _, w := range tt.wantContains {
				if !strings.Contains(got, w) {
					t.Errorf("log output = %q, want to contain %q", got, w)
				}
			}
			for _, a := range tt.wantAbsent {
				if strings.Contains(got, a) {
					t.Errorf("log output = %q, want to NOT contain %q", got, a)
				}
			}
		})
	}
}

// TestNewNilWriterDoesNotPanic verifies the io.Discard fallback: a nil Writer must
// not panic on use (fail safe — drop logs rather than crash).
func TestNewNilWriterDoesNotPanic(t *testing.T) {
	t.Parallel()

	log := New(Config{Writer: nil, Level: slog.LevelInfo})
	log.Info("should be silently discarded", "k", "v")
}
