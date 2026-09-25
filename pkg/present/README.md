# Message Presenter

`present.Presenter` may add bounded text before and after a user message. Harness
gives it cloned input blocks, validates and clones the returned frame, and
journals that frame once so replay and restore show the model the same message.
Presenters must handle a nil principal and should be deterministic in their input.
Machine-originated messages bypass the presenter.

Register one with `rig.WithMessagePresenter`. `Present(ctx, in)` receives the
session and loop IDs, agent name, command kind, optional principal and metadata,
and cloned original blocks. Return `present.Frame{Prefix, Suffix}` containing at
most eight nonempty text blocks and 8192 UTF-8 bytes in total. The user blocks
remain between the two frame parts. A presenter error, panic, or invalid frame
returns a typed `*present.Error` and sends no message. The 5s context bounds
cooperative implementations only; Harness cannot stop a presenter that ignores
context.
