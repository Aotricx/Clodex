# Architecture

Clodex is one static Go binary and one concrete backend pipeline:

```
Claude Code --Anthropic HTTP/SSE--> Clodex --Responses HTTP/SSE--> ChatGPT Codex
            <--ordered reducer-----        <--real usage/errors----
```

## Components

Grouped by where each package sits in a request's path, entry to exit.

**Entry, composition, and CLI**
- `cmd/clodex`: command parsing, signal handling, version injection.
- `internal/app`: concrete OAuth/catalog/tokenizer/retry/server composition.
- `internal/config`: loads and validates runtime configuration (`CLODEX_*`).
- `internal/launcher`: healthy-proxy reuse/spawn and Claude Code environment.
- `internal/commandauth`: the user-facing `clodex auth` command surface.
- `internal/server`: loopback listener and Anthropic routes.

**The wire pipeline** (translate in → transport → reduce out)
- `internal/anthropic`: request unions and schema errors.
- `internal/engine`: orchestrates one translated Anthropic Messages turn against
  the concrete Codex transport — the retry/breaker seam sits here.
- `internal/requestprep`, `internal/translate`, `internal/codexwire`: catalog
  resolution and complete stateless request translation.
- `internal/upstream`, `internal/codexstream`: authenticated backend HTTP/SSE.
- `internal/reducer`, `internal/anthropicstream`, `internal/stopscan`: one ordered
  semantic reducer used by streaming and buffered responses.

**Auth and model catalog**
- `internal/auth`, `internal/oauth`: cooperative Codex-compatible OAuth state.
- `internal/catalog`, `internal/model`: live/fallback catalog, effort/tier IDs,
  and reversible Claude discovery carriers.

**Reliability, telemetry, and safety**
- `internal/retry`, `internal/failure`, `internal/ratelimit`, `internal/status`:
  honest failures and bounded process telemetry (including per-call latency —
  see `internal/status`'s timing ring).
- `internal/redact`: strips credentials and account identifiers from wire data
  before anything reaches logs, captures, or diagnostic dumps.
- `internal/tokenizer`: embedded offline o200k counting and image estimation.

**Supporting and test-only**
- `internal/notices`: embedded third-party notices served by `clodex licenses`.
- `internal/fixtures`: loads and validates the protocol fixtures tests replay.
- `internal/livesmoke` (opt-in live backend smoke), `internal/claudee2e` (real
  Claude Code end-to-end), and `internal/workflowpolicy` (asserts `.gitattributes`
  LF pinning, commit-pinned first-party actions, and the six-target
  test/release workflow gates) — test-only, never imported by runtime code.

## Where state lives
Every backend request replays full history with `store:false`. Persistent state is
limited to shared `~/.codex/auth.json`, `~/.codex/models_cache.json`, and redacted
wire diagnostics under `~/.clodex/wire`. Process state is bounded telemetry,
catalog coalescing, retry budget/breaker state, and active sessions.

## To change X, edit Y
- Request mapping: `internal/translate/request.go`.
- SSE/terminal mapping: `internal/reducer`, `internal/anthropicstream`.
- Retry honesty/regressions: `internal/engine`, `internal/retry`, `internal/failure`.
- OAuth compatibility: `internal/auth`, `internal/oauth`, `internal/commandauth`.
- Routes/lifecycle: `internal/server`, composition in `internal/app/serve.go`.
- CLI/launcher: `cmd/clodex/main.go`, `internal/launcher`.
