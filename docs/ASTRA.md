# GPT-6 Astra verification

Verified on 2026-09-12 with Go 1.27.1, macOS ARM64, and Claude Code 2.1.269.

## Why both version pins change

Authenticated requests to the Codex models endpoint with client version 0.144.6
returned seven models without `gpt-6-astra`. The same account and endpoint with
0.154.0 returned eight models including Astra. Updating discovery alone was not
enough: a Responses request still advertising 0.144.6 returned HTTP 400 saying
that Astra requires a newer Codex version. Both discovery and transport now
advertise 0.154.0, the current Codex CLI release used for this check.

The existing Responses translation and the historical 0.144.6 fixtures remain
unchanged. This change does not claim to implement every new Codex feature.

## Fallback provenance

The Astra entry is a projection of the authenticated response from
`https://chatgpt.com/backend-api/codex/models?client_version=0.154.0` onto the
existing fallback schema. No credentials, account metadata, or model instructions
are included. Existing model entries are unchanged.

The observed Codex entry advertises medium by default; low, medium, high, xhigh,
max, and ultra reasoning; a fast/priority tier; text and image inputs; parallel
tool calls; original image detail; and Responses Lite. Its default service tier
is null. Its context window is 272,000 and maximum context window is 872,000.
The omitted effective context percentage uses the existing 95 percent default.
These are Codex subscription capabilities, not public API model settings.

## Regression and live checks

The updated catalog, discovery-header, model-selection, and transport-header
checks failed before their corresponding implementation changes and passed
afterward. Selection covers all advertised efforts, fast variants, Claude model
wrappers, defaults, and rejection of unsupported `none` reasoning.

Local validation passed:

- `go vet ./...`
- `go test -race -count=1 ./...` (32 packages; live suites skipped by default)
- `go mod verify`
- Static builds for darwin, linux, and windows, on amd64 and arm64
- Actual Claude Code Astra medium Read-tool round trip: two turns, correct marker,
  no permission denials, and nonzero usage
- Actual Claude Code `gpt-6-astra:high:fast` text request: correct marker and exit 0

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

All effort variants are covered by offline selection tests; live generation was
checked at medium and high/fast. Native Windows/Linux execution and long-context
or image behavior specific to Astra were not tested locally.
