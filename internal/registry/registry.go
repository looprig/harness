// Package registry provides a generic, domain-agnostic name→constructor
// registry. It lets a low-level package expose a lookup-by-name facility
// without depending on the high-level types being constructed: concrete
// constructors are bound at the composition root. The type parameter T is a
// generic constraint, not a dynamically-typed value, mirroring the existing
// llm.StreamReader[T any] precedent.
package registry

import (
	"context"
	"slices"
)

// Registry maps a name to a constructor that builds a value of type T. The
// zero Registry is not usable; construct one with New so the backing map is
// initialized.
type Registry[T any] struct {
	m map[string]func(context.Context) (T, error)
}

// New returns an empty Registry ready for Register and Open.
func New[T any]() *Registry[T] {
	return &Registry[T]{m: make(map[string]func(context.Context) (T, error))}
}

// Register binds name to a constructor. If name is already registered, the
// existing binding is preserved and a *DuplicateNameError is returned (fail
// secure — no overwrite).
func (r *Registry[T]) Register(name string, f func(context.Context) (T, error)) error {
	if _, exists := r.m[name]; exists {
		return &DuplicateNameError{Name: name}
	}
	r.m[name] = f
	return nil
}

// Open constructs the value bound to name. If name was never registered it
// returns the zero T and a *UnknownNameError whose Known field lists the
// registered names in ascending order. If the constructor itself fails, Open
// returns the zero T and the constructor's error unchanged.
func (r *Registry[T]) Open(ctx context.Context, name string) (T, error) {
	f, exists := r.m[name]
	if !exists {
		var zero T
		return zero, &UnknownNameError{Name: name, Known: r.Names()}
	}
	return f(ctx)
}

// Names returns the registered names sorted ascending. The result is a fresh
// slice; mutating it does not affect the registry.
func (r *Registry[T]) Names() []string {
	names := make([]string, 0, len(r.m))
	for name := range r.m {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
