package registry

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// constructorFor returns a constructor that builds a deterministic value for a name.
func constructorFor(name string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		return "value-for-" + name, nil
	}
}

func TestRegistryOpen(t *testing.T) {
	t.Parallel()

	errConstruct := errors.New("construct failed")

	tests := []struct {
		name string
		// register is applied to a fresh Registry before opening.
		register func(t *testing.T, r *Registry[string])
		open     string
		want     string
		wantErr  error // sentinel to match via errors.Is, or nil
		// wantDuplicate asserts the result of the second Register call (not Open).
		wantDuplicateOnSecondRegister bool
	}{
		{
			name: "happy path register then open",
			register: func(t *testing.T, r *Registry[string]) {
				if err := r.Register("alpha", constructorFor("alpha")); err != nil {
					t.Fatalf("Register() unexpected error = %v", err)
				}
			},
			open: "alpha",
			want: "value-for-alpha",
		},
		{
			name: "open unknown name",
			register: func(t *testing.T, r *Registry[string]) {
				if err := r.Register("alpha", constructorFor("alpha")); err != nil {
					t.Fatalf("Register() unexpected error = %v", err)
				}
			},
			open:    "missing",
			wantErr: errUnknownSentinel,
		},
		{
			name: "constructor error propagated unchanged",
			register: func(t *testing.T, r *Registry[string]) {
				if err := r.Register("boom", func(context.Context) (string, error) {
					return "", errConstruct
				}); err != nil {
					t.Fatalf("Register() unexpected error = %v", err)
				}
			},
			open:    "boom",
			wantErr: errConstruct,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := New[string]()
			tt.register(t, r)

			got, err := r.Open(context.Background(), tt.open)

			switch {
			case tt.wantErr == errUnknownSentinel:
				var unknown *UnknownNameError
				if !errors.As(err, &unknown) {
					t.Fatalf("Open() error = %v, want *UnknownNameError", err)
				}
				if unknown.Name != tt.open {
					t.Errorf("UnknownNameError.Name = %q, want %q", unknown.Name, tt.open)
				}
				if got != "" {
					t.Errorf("Open() value = %q, want zero value", got)
				}
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Open() error = %v, want %v", err, tt.wantErr)
				}
				if got != "" {
					t.Errorf("Open() value = %q, want zero value", got)
				}
			default:
				if err != nil {
					t.Fatalf("Open() unexpected error = %v", err)
				}
				if got != tt.want {
					t.Errorf("Open() = %q, want %q", got, tt.want)
				}
			}
		})
	}
}

// errUnknownSentinel is a marker used by the table to request UnknownNameError assertion.
var errUnknownSentinel = errors.New("expect-unknown-name-error")

func TestRegistryRegisterDuplicate(t *testing.T) {
	t.Parallel()

	r := New[string]()
	if err := r.Register("dup", constructorFor("dup")); err != nil {
		t.Fatalf("first Register() unexpected error = %v", err)
	}

	err := r.Register("dup", constructorFor("dup"))
	var duplicate *DuplicateNameError
	if !errors.As(err, &duplicate) {
		t.Fatalf("second Register() error = %v, want *DuplicateNameError", err)
	}
	if duplicate.Name != "dup" {
		t.Errorf("DuplicateNameError.Name = %q, want %q", duplicate.Name, "dup")
	}
}

func TestRegistryUnknownKnownNames(t *testing.T) {
	t.Parallel()

	r := New[string]()
	for _, name := range []string{"gamma", "alpha", "beta"} {
		if err := r.Register(name, constructorFor(name)); err != nil {
			t.Fatalf("Register(%q) unexpected error = %v", name, err)
		}
	}

	_, err := r.Open(context.Background(), "delta")
	var unknown *UnknownNameError
	if !errors.As(err, &unknown) {
		t.Fatalf("Open() error = %v, want *UnknownNameError", err)
	}
	want := []string{"alpha", "beta", "gamma"}
	if !reflect.DeepEqual(unknown.Known, want) {
		t.Errorf("UnknownNameError.Known = %v, want %v", unknown.Known, want)
	}
}

func TestRegistryNamesSorted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		register []string
		want     []string
	}{
		{
			name:     "empty registry",
			register: nil,
			want:     []string{},
		},
		{
			name:     "single name",
			register: []string{"solo"},
			want:     []string{"solo"},
		},
		{
			name:     "out of order returns sorted",
			register: []string{"zeta", "alpha", "mu", "beta"},
			want:     []string{"alpha", "beta", "mu", "zeta"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := New[string]()
			for _, name := range tt.register {
				if err := r.Register(name, constructorFor(name)); err != nil {
					t.Fatalf("Register(%q) unexpected error = %v", name, err)
				}
			}

			got := r.Names()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Names() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRegistryNamesNotAliased(t *testing.T) {
	t.Parallel()

	r := New[string]()
	if err := r.Register("alpha", constructorFor("alpha")); err != nil {
		t.Fatalf("Register() unexpected error = %v", err)
	}
	if err := r.Register("beta", constructorFor("beta")); err != nil {
		t.Fatalf("Register() unexpected error = %v", err)
	}

	first := r.Names()
	first[0] = "MUTATED"

	second := r.Names()
	if second[0] == "MUTATED" {
		t.Errorf("Names() returns aliased internal state; mutation leaked: %v", second)
	}
}
