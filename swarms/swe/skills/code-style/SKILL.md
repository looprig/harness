---
name: code-style
description: A short coding-style checklist for Go changes in this repository.
---

# Code Style Checklist

Apply this checklist to every Go change before proposing it:

- Keep functions small and single-purpose; if one grows past ~30 lines, ask whether it violates the Single Responsibility Principle.
- Use strict typing. Avoid `any`/`interface{}` outside explicit serialization boundaries; narrow to a concrete type immediately.
- Return typed errors (a concrete error struct per failure mode), not bare `errors.New`/`fmt.Errorf`, from package-level APIs.
- Depend on interfaces, not concrete types; wire dependencies at the composition root.
- Validate all external input at the boundary before it enters business logic.
- Run `gofmt`, `go test -race ./...`, and `make secure` before committing.
