# GPT-6 Astra verification

Verified on 2026-09-12 with Go 1.27.1, macOS ARM64, and Claude Code 2.1.269.
Re-verified on 2026-09-13 against the live Codex catalog and backend.

## Why both version pins change

Authenticated requests to the Codex models endpoint with client version 0.144.6
returned six models without `gpt-6-astra`. The same account and endpoint with
0.154.0 returned seven models including Astra, which advertises
`minimal_client_version: 0.153.0`. Updating discovery alone was not enough: a
Responses request still advertising 0.144.6 returned HTTP 400 saying that Astra
requires a newer Codex version. Both discovery and transport now advertise
0.154.0, the current Codex CLI release used for this check.

The existing Responses translation and the historical 0.144.6 fixtures remain
unchanged. This change does not claim to implement every new Codex feature.

## Fallback provenance

The Astra entry is a projection of the authenticated response from
`https://chatgpt.com/backend-api/codex/models?client_version=0.154.0` onto the
existing fallback schema. No credentials, account metadata, or model instructions
are included — `base_instructions` is rejected outright by the fallback safety
scan. Existing model entries are unchanged.

The observed Codex entry advertises `low` by default; low, medium, high, xhigh,
max, and ultra reasoning; a fast/priority tier described as "2x speed, increased
usage" with `default_service_tier: "priority"`; text and image inputs; parallel
tool calls; original image detail; and Responses Lite. Its context window is
272,000 and its maximum context window is 872,000. Upstream omits
`effective_context_window_percent` for every model, so the entry records the
schema's 95 percent default explicitly, as the existing entries do. These are
Codex subscription capabilities, not public API model settings.

## Capabilities Clodex does not carry over

Astra advertises Codex-CLI harness settings that Clodex deliberately ignores,
because Claude Code supplies its own system prompt, tools, and truncation:
`base_instructions`, `tool_mode: code_mode_only`, `apply_patch_tool_type`,
`shell_type`, `web_search_tool_type`, `truncation_policy`,
`experimental_supported_tools`, and `prefer_websockets`. Plain Responses function
tools were exercised live against Astra and work, so `code_mode_only` is a CLI
harness choice rather than a model requirement.

Three limits are real and shared with the other models:

- `ultra` is client-side. The backend answers
  `Invalid value: 'ultra'. Supported values are: 'none', 'minimal', 'low',
  'medium', 'high', 'xhigh', and 'max'.`, so Clodex sends `max` and does not
  reproduce the CLI's multi-agent delegation (`multi_agent_version: v2`).
- `max_context_window: 872000` is recorded but unused; the active window stays
  `context_window` (272,000) at the schema's 95 percent, as with every model.
  The same refresh corrected the sibling entries, which still carried the
  0.144.6-era `max_context_window: 272000` and a stale `gpt-5.6-sol` priority.
- `support_verbosity`/`default_verbosity: low` are not sent; requests use the
  backend default rather than pinning Codex's verbosity.

Responses Lite applies to Astra exactly as it does to the other Lite models: the
backend refuses Lite with `parallel_tool_calls: true`, so a request that offers
tools and allows parallel calls takes the full Responses path and keeps parallel
tool calls; Lite is used when parallelism is moot.

## Regression and live checks

The updated catalog, discovery-header, model-selection, and transport-header
checks failed before their corresponding implementation changes and passed
afterward. Selection covers all advertised efforts, fast variants, Claude model
wrappers, defaults, and rejection of unsupported `none` reasoning.

Local validation passed:

- `gofmt -l`, `go vet ./...`, `go build ./...`
- `go test -race -count=1 ./...` (31 packages; live suites skipped by default)
- `go mod verify`
- Static builds for darwin, linux, and windows, on amd64 and arm64
- The same vet/build/race gate under the CI-pinned `GOTOOLCHAIN=go1.26.5`
- `govulncheck` clean on the current toolchain. Under the 1.26.5 pin it reports
  five standard-library findings fixed in Go 1.26.6; they are identical on `main`
  and are a property of the pin, not of this change.

Live checks through a running proxy on the 0.154.0 pin:

- Astra non-stream text, streaming text, `:fast`, and every effort including
  `ultra` (sent as `max`)
- Astra single tool call, two parallel tool calls in one response (with thinking
  enabled), and a base64 PNG image input read and described
- Astra extended thinking: `thinking` block plus final text, signature attached
- Claude Code Astra medium Read-tool round trip: two turns, correct marker, no
  permission denials, nonzero usage (`TestClaudeCodeAstraE2E`)
- No regression for the older pins: `gpt-5.6-luna:low` and `gpt-5.5:xhigh`
  streamed correctly, and `internal/livesmoke` passed its catalog, stream,
  tool round-trip, and parallel-tool subtests against the same proxy

Repeat the opt-in tool test with an unused local port and an authenticated Codex
account that has Astra access:

```sh
go build -o /tmp/clodex-astra ./cmd/clodex
CLODEX_E2E_TESTS=1 CLODEX_E2E_BINARY=/tmp/clodex-astra CLODEX_E2E_PORT=18485 \
  go test ./internal/claudee2e -run '^TestClaudeCodeAstraE2E$' -v -count=1
```

The launcher leaves its proxy running. Use a fresh port after rebuilding to avoid
reusing an older binary. The test uses a temporary marker file and only the Read
tool. It requires no private image fixture. Account availability can change;
Astra-specific live checks deliberately remain opt-in.

Native Windows and Linux execution and Astra long-context behavior above
272,000 tokens were not tested locally; the workflow covers the native runners.
