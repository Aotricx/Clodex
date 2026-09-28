# GPT-6 verification

Astra verified on 2026-09-12 and re-verified on 2026-09-13 on the 0.154.0 pin.
Sol and Luna added, and every catalog model re-verified, on 2026-09-28 with
Go 1.27.0 and 1.26.5, macOS ARM64, Codex CLI 0.156.1, and Claude Code 2.1.281.

## Why the version pins change

The backend gates each model on the client version the request advertises
(`minimal_client_version` in the catalog). Authenticated requests to the Codex
models endpoint returned:

| Advertised version | Models | GPT-6 |
| --- | --- | --- |
| 0.144.6 | 6 | none |
| 0.154.0 | 7 | `gpt-6-astra` (minimum 0.153.0) |
| 0.155.0, 0.156.0, 0.156.1 | 9 | `gpt-6-sol`, `gpt-6-astra`, `gpt-6-luna` (Sol and Luna minimum 0.155.0) |

On the 0.154.0 pin a request for `gpt-6-sol` failed with `requested model
"gpt-6-sol" is not present in catalog`. Updating discovery alone is not enough:
a Responses request advertising an older version than a model's minimum is
rejected with HTTP 400 (observed for Astra at 0.144.6). Both discovery and
transport therefore advertise 0.156.1, the current Codex CLI release. The 0.155.0
and 0.156.1 catalogs are identical apart from their envelope.

The existing Responses translation and the historical 0.144.6 fixtures remain
unchanged. This change does not claim to implement every new Codex feature.

## Fallback provenance

The fallback is a projection of the authenticated response from
`https://chatgpt.com/backend-api/codex/models?client_version=0.156.1` onto the
existing fallback schema, in upstream order. No credentials, account metadata, or
model instructions are included — `base_instructions` is rejected outright by the
fallback safety scan.

| Slug | Default | Efforts | Fast tier | Priority |
| --- | --- | --- | --- | --- |
| `gpt-6-sol` | medium | low–max, ultra | "1.5x speed" | 0 |
| `gpt-6-astra` | low | low–max, ultra | "2x speed, increased usage" | 1 |
| `gpt-6-luna` | medium | low–max | "1.5x speed" | 3 |

All three advertise text and image inputs, parallel tool calls, original image
detail, Responses Lite, `default_service_tier: "priority"`, a 272,000 context
window, and an 872,000 maximum context window. Upstream omits
`effective_context_window_percent` for every model, so each entry records the
schema's 95 percent default explicitly. The same refresh picked up upstream's new
descriptions for Astra and the older models (the 5.x models are now described as
"Older" or "Legacy") and their new priorities (`gpt-5.6-terra` 5,
`gpt-5.6-luna` 6, `gpt-5.5` 7). These are Codex subscription capabilities, not
public API model settings.

Clodex's defaults follow upstream's lead: the main model is now
`gpt-6-sol:medium` (upstream priority 0, the Codex CLI's own default) and the
small/fast model is `gpt-6-luna:low`, replacing `gpt-5.6-sol:medium` and
`gpt-5.6-luna:low`. `CLODEX_MODEL` and `CLODEX_SMALL_FAST_MODEL` still override
both.

## Capabilities Clodex does not carry over

The GPT-6 models advertise Codex-CLI harness settings that Clodex deliberately
ignores, because Claude Code supplies its own system prompt, tools, and
truncation: `base_instructions`, `tool_mode: code_mode_only`,
`apply_patch_tool_type`, `shell_type`, `web_search_tool_type`,
`truncation_policy`, `experimental_supported_tools`, and `prefer_websockets`.
Plain Responses function tools were exercised live against all three and work,
so `code_mode_only` is a CLI harness choice rather than a model requirement.

Three limits are real and shared with the other models:

- `ultra` is client-side. The backend answers
  `Invalid value: 'ultra'. Supported values are: 'none', 'minimal', 'low',
  'medium', 'high', 'xhigh', and 'max'.`, so Clodex sends `max` and does not
  reproduce the CLI's multi-agent delegation (`multi_agent_version: v2`).
  Luna does not advertise `ultra`, and Clodex rejects it.
- `max_context_window: 872000` is recorded but unused; the active window stays
  `context_window` (272,000) at the schema's 95 percent, as with every model.
- `support_verbosity`/`default_verbosity: low` are not sent; requests use the
  backend default rather than pinning Codex's verbosity.

Responses Lite applies to the GPT-6 models exactly as it does to the other Lite
models: the backend refuses Lite with `parallel_tool_calls: true`, so a request
that offers tools and allows parallel calls takes the full Responses path and
keeps parallel tool calls; Lite is used when parallelism is moot.

Extended thinking is the model's choice. With thinking enabled Clodex always
asks for `reasoning.summary: auto`, but a model may answer an easy prompt without
reasoning (upstream reports `reasoning_tokens: 0`), in which case no `thinking`
block is emitted. The old and new pins behaved the same way on repeated runs.

## Regression and live checks

The updated catalog, discovery-header, model-selection, and transport-header
checks failed before their corresponding implementation changes and passed
afterward. Selection covers every advertised effort, fast variants, Claude model
wrappers, defaults, and rejection of unsupported efforts (`none` for all three,
`ultra` for Luna).

Local validation passed:

- `gofmt -l`, `go vet ./...`
- `go test -race -count=1 ./...` (31 packages; live suites skipped by default)
- The same vet and race gate under the CI-pinned `GOTOOLCHAIN=go1.26.5`
- Static builds for darwin, linux, and windows, on amd64 and arm64

Live checks through a running proxy on the 0.156.1 pin, against every model the
live catalog serves (`gpt-6-sol`, `gpt-6-astra`, `gpt-6-luna`, `gpt-reserve`,
`gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna`, `gpt-5.5`, `codex-auto-review`):

- Non-stream text for every advertised model id: the bare slug and each effort,
  each with and without `:fast` (114 ids, `ultra` sent as `max`)
- Per model: streaming text, forced tool call plus tool-result round trip, two
  parallel tool calls in one response, thinking request, and a base64 PNG image
  input read and described (159 of 159 checks passed)
- Per model at `medium` on a reasoning prompt: a signed `thinking` block for
  every model except `gpt-6-astra` and `gpt-5.6-terra`, which answered without
  reasoning upstream (`reasoning_tokens: 0`)
- Claude Code Read-tool round trip on `gpt-6-sol:medium`, `gpt-6-astra:medium`,
  and `gpt-6-luna:medium`: two turns, correct marker, no permission denials,
  nonzero usage (`TestClaudeCodeGPT6E2E`)
- `internal/livesmoke` catalog, stream, tool round-trip, and parallel-tool
  subtests; `TestLiveModelDiscovery` (live source, nine models); and
  `TestLiveAuthStatus`

Repeat the opt-in Claude Code test with an unused local port and an
authenticated Codex account that has GPT-6 access:

```sh
go build -o /tmp/clodex-gpt6 ./cmd/clodex
CLODEX_E2E_TESTS=1 CLODEX_E2E_BINARY=/tmp/clodex-gpt6 CLODEX_E2E_PORT=18485 \
  go test ./internal/claudee2e -run '^TestClaudeCodeGPT6E2E$' -v -count=1
```

The launcher leaves its proxy running. Use a fresh port after rebuilding to avoid
reusing an older binary. The test uses a temporary marker file and only the Read
tool. It requires no private image fixture. Account availability can change;
GPT-6-specific live checks deliberately remain opt-in.

Not run here: `TestClaudeCodeE2E`, which needs a private Great Dane image
fixture; native Windows and Linux execution (the workflow covers the native
runners); `govulncheck`; and long-context behavior above 272,000 tokens.
