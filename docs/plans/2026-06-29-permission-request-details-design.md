# Permission Request Details Display Design

Date: 2026-06-29

## Goal

Show the concrete action behind tool activity in the TUI. A user should see what is being approved in active permission prompts and what each rendered tool call did in live/committed transcript cards, for example `ReadFile(pkg/tui/render.go)`, `WriteFile(README.md)`, `Fetch(GET https://google.com)`, or `Bash(curl -p ...)`.

## Approach

Use the existing sealed `tool.PermissionRequest.Description()` contract for permission prompts and gated committed cards. `BashRequest.Description()` already returns the exact command, and `FetchRequest.Description()` already returns `METHOD URL`. The TUI already copies this value into `prompt.Description`; the active prompt currently just does not render it.

For tool cards, use a shared display-detail path:

- live cards normalize `ToolCallStarted.Summary` so `Bash: make test` renders as `Bash(make test)`, not `Bash(Bash: make test)`
- committed parent cards refresh the detail from the finalized `content.ToolUseBlock.Input`
- subagent nested cards reconstruct their detail from the durable `ToolUseBlock.Input`, because child `ToolCallStarted` events are ephemeral and filtered out

The active permission box will render:

- `Approve <ToolName>?`
- the wrapped permission description, when non-empty
- approval scope keys and deny key

The committed transcript will keep enough permission context to render the same description alongside the resolved decision. Permission gates already attach their approved/denied state to the matching tool card; this change extends that gate metadata with the description captured from `PermissionRequested`.

## Security

The permission display uses only `PermissionRequest.Description()`, which is the tool-owned approval prompt body. Tool-card reconstruction uses typed JSON decoders per known tool and emits only safe target fields: paths, commands, URL/method, patterns, query strings, counts, and names. It does not render file contents, edit old/new substrings, HTTP headers/bodies, or subagent task text from raw input.

## Testing

Add table-driven tests for:

- active permission prompt rendering for `Bash` and `Fetch`
- empty descriptions not creating noisy blank rows
- transcript gate flow rendering the description after approval/denial
- live summary normalization
- stored parent-card reconstruction from `ToolUseBlock.Input`
- subagent nested-card reconstruction from `ToolUseBlock.Input`
