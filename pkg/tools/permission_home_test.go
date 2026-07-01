package tools

import (
	"errors"
	"testing"
)

func TestHomeUnresolvableError(t *testing.T) {
	t.Parallel()
	err := error(&HomeUnresolvableError{Cause: errors.New("no $HOME")})
	var target *HomeUnresolvableError
	if !errors.As(err, &target) {
		t.Fatalf("errors.As failed for *HomeUnresolvableError")
	}
	if target.Cause == nil {
		t.Errorf("Cause not preserved")
	}
	if got := err.Error(); got == "" {
		t.Errorf("Error() is empty")
	}
}

func TestNewPermissionChecker_HomeUnresolvable(t *testing.T) {
	t.Parallel()
	boom := func() (string, error) { return "", errors.New("no home") }
	tests := []struct {
		name    string
		deny    HardDenyRules
		wantErr bool
	}{
		{
			name:    "read-deny ~/ pattern + unresolvable home -> construction error",
			deny:    HardDenyRules{DeniedReadPaths: []string{"~/.ssh/**"}},
			wantErr: true,
		},
		{
			name:    "write-deny ~/ pattern + unresolvable home -> construction error",
			deny:    HardDenyRules{DeniedWritePaths: []string{"~/.looprig/**"}},
			wantErr: true,
		},
		{
			name:    "no ~/ pattern + unresolvable home -> ok",
			deny:    HardDenyRules{DeniedReadPaths: []string{"**/.env"}},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, err := NewPermissionChecker(PermissionPolicy{HardDeny: tt.deny}, WithHomeDir(boom))
			var hue *HomeUnresolvableError
			if tt.wantErr {
				if !errors.As(err, &hue) {
					t.Fatalf("want *HomeUnresolvableError, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c == nil {
				t.Fatalf("nil checker on success")
			}
		})
	}
}

func TestNewPermissionChecker_HomeResolvedOnce(t *testing.T) {
	t.Parallel()
	c, err := NewPermissionChecker(
		PermissionPolicy{HardDeny: DefaultHardDeny()},
		WithHomeDir(func() (string, error) { return "/home/tester", nil }),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.home != "/home/tester" {
		t.Errorf("home = %q, want /home/tester", c.home)
	}
}
