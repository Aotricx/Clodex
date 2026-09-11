# Clodex

<p align="center">
  <img src="docs/assets/blue-claude.png" alt="Blue Claude logo" width="240">
</p>

Clodex lets you run **GPT/Codex models inside Claude Code** — same terminal harness, same tool
use, same agentic loop, different model underneath. It's a local proxy: Claude Code speaks the
Anthropic Messages API on one side, the ChatGPT Codex backend
(`https://chatgpt.com/backend-api/codex`) speaks its own Responses-style protocol on the other, and
Clodex translates faithfully between them, advertising Codex CLI `0.154.0` for model discovery
and requests, and authenticating with your ChatGPT subscription OAuth credentials in the Codex CLI's shared
`~/.codex/auth.json`. No OpenAI API-key path, no second provider, one backend. One static Go
binary, no runtime dependencies.

Anthropic-to-Codex translation, streaming, and failure-handling behavior was adapted from
[raine/claude-code-proxy](https://github.com/raine/claude-code-proxy) (MIT, © Raine Virta).

**Contents:** [Why Clodex](#why-clodex-over-other-solutions) · [Before you use this](#before-you-use-this) ·
[How it works](#how-it-works) · [Quick start](#quick-start) · [Commands](#commands) ·
[Models](#models) · [Configuration](#configuration) · [Endpoints](#endpoints) ·
[Troubleshooting](#troubleshooting) · [Limitations](#limitations) · [Security](#security)

## Why Clodex over other solutions?

Most Claude Code proxies are multi-provider: one binary that claims to route to OpenAI, Gemini,
DeepSeek, local models, and everything else. Breadth like that has a cost. Each backend gets a
thin adapter, edge cases get handled for whichever provider the author uses most, and the rest
"mostly works" — tool calls that drop arguments mid-stream, usage counts that are guesses, cache
and context windows that are wrong, errors that get swallowed into a generic 500. You find out
which parts are broken by hitting them in the middle of a real session.

Clodex does one thing: run Codex models inside Claude Code, correctly. One backend, one protocol
(the pinned Codex CLI wire format), one auth path. Every Claude Code behavior that matters —
streaming, tool-call and tool-result round-trips, thinking/reasoning, stop sequences,
auto-compaction sizing, real token usage, real rate-limit and error propagation — is translated
end to end and checked against the real backend, not approximated. When Claude Code does
something, the Codex model sees it; when the backend answers, Claude Code gets the exact
Anthropic-shaped equivalent, on both the streaming and buffered paths.

The trade-off is deliberate. Clodex is not a jack of all trades — it will never add a second
provider, an API-key mode, or a model it can't fully verify. It is meant to be the one adapter you
don't have to think about. If you want many models with rough edges, another proxy will serve you
better; if you want Codex in Claude Code to behave like it belongs there, this is the tool.

See [`docs/EVIDENCE.md`](docs/EVIDENCE.md) for what has been demonstrated end to end, and
[Limitations](#limitations) for what has not.

## Before you use this

**Not affiliated.** Clodex is not affiliated with or endorsed by OpenAI or Anthropic. "Claude",
"Claude Code", "ChatGPT", and "Codex" are their respective owners' marks.

**Your ChatGPT account carries the risk.** Clodex drives the Codex backend from a client other than
the Codex CLI those credentials were issued for, identifying itself with the Codex CLI's public
OAuth client id and pinned protocol headers. Whatever follows from that — rate limiting, request
rejection, or action against the account — lands on your ChatGPT account. OpenAI's terms, not this
project, govern what is permitted.

**The proxy has no authentication.** No route checks an `Authorization` header, an `x-api-key`, a
bearer token, or the request's `Host`/`Origin`. While the proxy runs, any process on your machine
that can reach `127.0.0.1:<port>` can spend your ChatGPT subscription through it, and `GET /status`
discloses your plan tier and rate-limit state. Read [Security](#security) before you leave it up.

## How it works

```
Claude Code --Anthropic HTTP/SSE--> Clodex --Responses HTTP/SSE--> ChatGPT Codex backend
            <--ordered reducer-----        <--real usage/errors----
```

Claude Code talks to Clodex exactly as it would talk to `api.anthropic.com` — same request/response
shape, same SSE event stream. Clodex decodes that request, translates it into the Codex backend's
wire format (full conversation history each turn; the backend is stateless, `store:false`), and
replays the exact HTTP/SSE protocol the pinned Codex CLI would send, using your ChatGPT OAuth
credentials. Whatever comes back — text, tool calls, reasoning, streamed deltas, real usage counts,
real errors — gets folded back into Anthropic's wire shape by one ordered reducer shared by both the
streaming and buffered paths, so a client sees the same content either way.

`clodex claude` (the launcher) automates the tedious part of that: it checks whether a Clodex proxy
is already listening on `127.0.0.1:8484` and answering as *Clodex* — not just any process holding
that port — reuses it if so, otherwise starts one; then it launches `claude` with `ANTHROPIC_BASE_URL`
pointed at it, a model id Claude Code's picker will accept, and its auto-compaction window set from
the resolved model's real context size. `clodex serve` is the same proxy without any of that —
useful when you'd rather manage the Claude Code side yourself (see step 4 below).

## Quick start

You need Claude Code installed with `claude` on your `PATH`, an active ChatGPT subscription with
Codex access, and credentials at `~/.codex/auth.json` — step 2 creates them if you have none.

**1. Install.** Each [release](https://github.com/Aotricx/Clodex/releases/latest) publishes six
static binaries, each with a `.sha256` beside it: `clodex-darwin-amd64`, `clodex-darwin-arm64`,
`clodex-linux-amd64`, `clodex-linux-arm64`, `clodex-windows-amd64.exe`,
`clodex-windows-arm64.exe`. Download the pair for your platform, then:

```sh
shasum -a 256 -c clodex-darwin-arm64.sha256   # or: sha256sum --check
chmod +x clodex-darwin-arm64
mv clodex-darwin-arm64 /usr/local/bin/clodex
```

From a clone, build it yourself instead — the only path that needs Go (1.26+). `clodex version` then
reports `dev`, because the release tag is stamped in at release build time:

```sh
go build -o clodex ./cmd/clodex
```

**2. Authenticate first.** If `~/.codex/auth.json` is missing, `clodex claude` fails. An existing
Codex CLI login is reused as-is; otherwise create one with either flow below, which writes the
shared Codex-compatible file atomically at mode `0600`, so the Codex CLI keeps using it too:

```sh
clodex auth login    # browser PKCE
clodex auth device   # headless device code, for machines with no browser
clodex auth status   # source, masked account, plan, token expiry
```

**3. Launch Claude Code through Clodex** (see [How it works](#how-it-works) for what this does).
Everything after `--` reaches `claude` untouched.

```sh
clodex claude
clodex claude --model gpt-5.6-sol:high:fast -- --print "Explain this repository"
```

**4. Or run the proxy standalone** and point an existing Claude Code at it. `clodex serve` runs in
the foreground, so use a second shell:

```sh
clodex serve --port 8484
```

```sh
export ANTHROPIC_BASE_URL=http://127.0.0.1:8484
export ANTHROPIC_AUTH_TOKEN=clodex-loopback-dummy   # must be set; not a secret, see Security
claude
```

## Commands

| Command | Purpose |
| --- | --- |
| `clodex serve [--port N] [--debug-wire]` | Run the proxy in the foreground on IPv4 loopback. `--port` overrides `CLODEX_PORT`; `--debug-wire` records **all** request/response pairs, not just failures |
| `clodex auth login` | Browser PKCE login; writes a Codex-compatible `~/.codex/auth.json` |
| `clodex auth device` | Headless device-code login, for machines without a browser |
| `clodex auth status` | Report credential source, masked account, plan, and expiry |
| `clodex claude [--model M] [--small-fast-model M] [-- <claude args>]` | Reuse or start the proxy, then launch Claude Code against it. `--model` sets the main model (`slug[:effort][:fast]`), `--small-fast-model` the model Claude Code uses for cheap background calls; everything after `--` goes to `claude` untouched |
| `clodex version` | Print the build version |
| `clodex licenses` | Print the third-party attributions embedded in the binary |
| `clodex help` | Print usage (also `--help`, `-h`) |

Environment is read first; these flags override it. `--port` and `--debug-wire` exist only on
`serve`, `--model` and `--small-fast-model` only on `claude`, which rejects any other bare argument
before `--`. `clodex claude` reads the rest of its config, port included, from the environment
only: to move the launcher off `8484`, set `CLODEX_PORT`.

## Models

The grammar is `slug[:effort][:fast]`. Effort runs weakest to strongest: `low`, `medium`, `high`,
`xhigh`, `max`, `ultra` — support is **not** uniform across models. `:fast` requests the fast speed
tier where the model advertises one, and must be the final suffix. The live catalog is authoritative
at runtime; below is the compiled offline fallback used when discovery is unavailable.

| Slug | Default effort | Supported efforts | `:fast` |
| --- | --- | --- | --- |
| `gpt-6-astra` | `medium` | low, medium, high, xhigh, max, ultra | yes |
| `gpt-5.6-sol` | `medium` | low, medium, high, xhigh, max, ultra | yes |
| `gpt-5.6-terra` | `medium` | low, medium, high, xhigh, max, ultra | yes |
| `gpt-5.6-luna` | `medium` | low, medium, high, xhigh, max | yes |
| `gpt-5.5` | `xhigh` | low, medium, high, xhigh | yes |
| `gpt-reserve` | `medium` | low, medium, high, xhigh, max | yes |
| `codex-auto-review` | `medium` | low, medium, high, xhigh, max | yes |

Every fallback model advertises a 272,000-token context window. Astra also advertises
a maximum context window of 872,000 tokens; selecting it does not expand the active window.

Run Astra with `clodex claude --model gpt-6-astra` or select an explicit effort,
for example `clodex claude --model gpt-6-astra:high:fast`. The fallback capabilities
come from the Codex subscription catalog, which can differ from the public API.
See [Astra verification](docs/ASTRA.md) for provenance and repeatable tests.

`gpt-reserve` and
`codex-auto-review` are marked `visibility: hide` upstream; Clodex does not filter on that field,
so they are still listed and selectable. For your account's real list, ask a running proxy:

```sh
curl -s http://127.0.0.1:8484/v1/models | jq -r '.data[].id'
```

`clodex claude` sets `ANTHROPIC_MODEL` to a canonical GPT id plus `[1m]` (for example
`gpt-5.6-sol:medium[1m]`). Current Claude Code strips that marker itself. `/v1/models` still
lists reversible `anthropic-clodex-…[1m]` twins so older pickers keep working, and the proxy
unwraps either form back to a catalog GPT id — a shim, not an alias policy or a context expansion.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `CLODEX_PORT` | `8484` | Loopback listen port |
| `CLODEX_MODEL` | `gpt-5.6-sol:medium` | Main model |
| `CLODEX_SMALL_FAST_MODEL` | `gpt-5.6-luna:low` | Small/fast model |
| `CLODEX_DEBUG_WIRE` | `false` | Wire dumps are failure-triggered and redacted, written under `~/.clodex/wire`; `true` (or `--debug-wire`) records **every** request/response pair |
| `CLODEX_EMPTY_RETRIES` | `10` | Retries for an empty upstream completion |
| `CLODEX_TRANSIENT_RETRIES` | `3` | Retries for a transient upstream failure |
| `CLODEX_RETRY_BASE_DELAY` / `CLODEX_RETRY_MAX_DELAY` | `250ms` / `5s` | Backoff floor and ceiling |
| `CLODEX_GLOBAL_RETRY_BUDGET` / `CLODEX_RETRY_BUDGET_WINDOW` | `100` / `1m` | Retries per window, and the window |
| `CLODEX_CIRCUIT_FAILURES` / `CLODEX_CIRCUIT_COOLDOWN` | `8` / `30s` | Failures that open the breaker, and its cooldown |

## Endpoints

All on `127.0.0.1` only, none authenticated.

| Route | Purpose |
| --- | --- |
| `GET /healthz` | Health probe the launcher uses: status, service name, version |
| `GET /status` | Auth summary, catalog freshness, rate limits, translation-warning and retry/breaker counters, active sessions, and a bounded ring of the last 64 calls' latency (`timing`: TTFT, total, upstream-wait, first-event, and commit-gate durations, plus attempt counts) |
| `GET /v1/models` | Catalog-derived model list — canonical GPT ids and their carrier twins |
| `POST /v1/messages` | Anthropic Messages, streaming or buffered, through one ordered reducer |
| `POST /v1/messages/count_tokens` | Offline exact `o200k_base` counting; no network call |

Clodex sets no output-token cap: the Codex backend exposes no output-token maximum and rejects an
output-limit field, so Anthropic `max_tokens` is accepted and warning-accounted rather than turned
into an invented cap. A genuine upstream `response.incomplete` still maps to `stop_reason: max_tokens`.

## Troubleshooting

| Symptom | What's happening | Fix |
| --- | --- | --- |
| `clodex claude` fails with `clodex claude requires credentials in …/auth.json; run clodex auth login or clodex auth device first` (wrapped with `open …/auth.json: no such file or directory`) | `clodex claude` requires real credentials before it will launch anything — unlike `clodex serve` alone, which starts fine with no auth and only fails once a request actually needs it. Auth commands honor `CODEX_HOME` the same way `serve` does | `clodex auth login` or `clodex auth device` first |
| `foreign listener on Clodex port at http://127.0.0.1:8484: ...` | Something else is already listening on that port and it isn't Clodex (it fails the `/healthz` identity check). Clodex refuses to reuse it and won't kill it | Free the port, or point Clodex elsewhere with `CLODEX_PORT` (`clodex claude` has no `--port` flag; `clodex serve` accepts `--port`) |
| `run Claude Code: proxy did not become healthy within <timeout>` or `...proxy exited before becoming healthy` | Clodex tried to spawn its own proxy and it never came up (or came up and died) | Run `clodex serve --port <N>` standalone in a second shell and read what it prints on startup — usually a config or auth problem |
| `run Claude Code: find claude executable: ...` | Claude Code isn't on `PATH` | Install Claude Code, or fix `PATH` |
| `run Claude Code: claude exited: exit status N` | Claude Code itself exited non-zero — `clodex claude`'s own exit code mirrors it | Not a Clodex failure; look at Claude Code's own stderr above this line |
| `POST /v1/messages` returns `invalid_request_error` with a message like `messages[1].role must be user, assistant, or system` | The request body itself is malformed for the shape Clodex/Claude Code expects | The message always names the exact JSON path (`messages[N].field`) — fix that field |
| Model list looks stale, or a model/effort you expect is missing | Discovery hits the live catalog over the network; if that fails, Clodex falls back to a cached copy and finally to the compiled offline list below — silently, with no error | Check `catalog.source` in `curl -s http://127.0.0.1:<port>/status` (`"live"` vs a fallback) |

## Limitations

Behavior you can observe. [`docs/EVIDENCE.md`](docs/EVIDENCE.md) is the full engineering ledger.

- **Late upstream events surface as errors.** A non-terminal event arriving after a terminal response
  makes the reducer fail: buffered mode can replace an already-complete result with a 502, and
  streaming can append an error event after `message_stop`.
- **Proxy-side stop sequences can truncate a non-text block.** After a match, streaming suppresses
  later deltas for every open block, so an unusual interleaving of text and tool/thinking blocks can
  cut a non-text block short. Buffered assembly preserves it.
- **`message_start` reports zero usage** — Codex supplies authoritative usage only at the terminal event.
- **A reasoning-only opening can look like a hang.** Clodex delays initial downstream bytes until
  semantic output, so a terminal-only retry can still return an honest 503; that gap carries no pings.
- **Image URLs are counted as URL text**, because offline counting cannot inspect remote dimensions.
  Decodable inline images use Codex's 32-pixel patch geometry.
- **Parallel tool calls force a request off Responses Lite.** The backend rejects the Lite path
  unless `parallel_tool_calls` is `false` (`X-OpenAI-Internal-Codex-Responses-Lite requires
  \`parallel_tool_calls\` to be false`), so a request that offers tools and has not set
  `disable_parallel_tool_use` is sent over the full Responses protocol instead, which preserves
  parallel tool calls on every model. Requests with no tools, or with parallel tool use disabled,
  still use Lite. Clodex enables the capability but does not override the model's choice — a model
  may still answer with sequential tool turns.

## Security

The proxy performs no authentication on any route: no `Authorization`, `x-api-key`, or bearer check
anywhere in the handler, and no `Host` or `Origin` validation. The
`ANTHROPIC_AUTH_TOKEN=clodex-loopback-dummy` the launcher sets is a literal non-secret constant —
Claude Code merely requires the variable to be set, and it protects nothing. Consequence: while
Clodex is running, any process on the machine that can reach `127.0.0.1:<port>` can spend your
ChatGPT subscription through it, and `/status` discloses your plan tier and rate-limit state. The
listener is forced to IPv4 loopback with no option to widen it, so other machines cannot reach it —
but that is a bind address, not access control.

Mitigation: run the proxy only while you need it, and treat the port as trusted-local-only — do not
forward it, tunnel it, or share the machine's loopback with untrusted code. Tokens and account
identifiers are redacted from status output, errors, and diagnostics.

## Credits and license

- [raine/claude-code-proxy](https://github.com/raine/claude-code-proxy) (MIT, © Raine Virta) —
  Anthropic-to-Codex translation, streaming, and failure-handling behavior was adapted from it.
- [OpenAI Codex CLI](https://github.com/openai/codex) `rust-v0.144.6` (Apache-2.0) — the
  token-estimate rules adapt its structural, encrypted-payload, and image estimation behavior.

Full terms are in [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md); the same attributions ship in
the binary and print with `clodex licenses`. Clodex itself is MIT — © 2026 Kunaii, [`LICENSE`](LICENSE).
