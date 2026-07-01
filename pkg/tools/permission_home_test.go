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

func TestDeniedRead_HomeResolvedAtConstruction(t *testing.T) {
	t.Parallel()
	c, err := NewPermissionChecker(
		PermissionPolicy{HardDeny: HardDenyRules{DeniedReadPaths: []string{"~/.ssh/**"}}},
		WithHomeDir(func() (string, error) { return "/home/tester", nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !c.DeniedRead("/home/tester/.ssh/id_rsa") {
		t.Errorf("~/.ssh/** must deny /home/tester/.ssh/id_rsa")
	}
	if c.DeniedRead("/home/tester/project/main.go") {
		t.Errorf("non-secret path must not be denied")
	}
}

func TestDeniedRead_DefensiveFailClosed_EmptyHome(t *testing.T) {
	t.Parallel()
	// Constructed with a NON-home policy so construction succeeds with home="",
	// then a ~/ read pattern is present -> defensive deny (backstop; should not
	// occur post-construction, but must fail closed if it ever does).
	c, err := NewPermissionChecker(
		PermissionPolicy{HardDeny: HardDenyRules{DeniedReadPaths: []string{"**/.env"}}},
		WithHomeDir(func() (string, error) { return "", errors.New("no home") }),
	)
	if err != nil {
		t.Fatal(err)
	}
	c.policy.HardDeny.DeniedReadPaths = append(c.policy.HardDeny.DeniedReadPaths, "~/.ssh/**")
	if !c.DeniedRead("/anything/.ssh/id_rsa") {
		t.Errorf("empty home + ~/ pattern must fail CLOSED (deny)")
	}
}
