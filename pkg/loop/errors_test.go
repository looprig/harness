package loop

import (
	"errors"
	"testing"
)

func TestConfigError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"missing client", &ConfigError{Kind: ConfigMissingClient}, "loop: config error: Config.Client is required"},
		{"invalid model", &ConfigError{Kind: ConfigInvalidModel}, "loop: config error: Config.Model invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfigErrorUnwrap(t *testing.T) {
	t.Parallel()
	cause := errors.New("inner")
	err := &ConfigError{Kind: ConfigInvalidModel, Cause: cause}
	if !errors.Is(err, cause) {
		t.Error("ConfigError does not unwrap to its Cause")
	}
}
