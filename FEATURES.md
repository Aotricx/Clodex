# Features

| Capability | Status | Source path(s) |
| --- | --- | --- |
| Anthropic routes on forced `127.0.0.1` | implemented | `internal/server` |
| Stateless full-history request translation | implemented | `internal/anthropic`, `internal/requestprep`, `internal/translate` |
| Text, thinking/signatures, tools/results, images, web search | implemented | `internal/translate`, `internal/reducer` |
| Exact ordered Anthropic SSE and buffered reducer | implemented | `internal/codexstream`, `internal/reducer`, `internal/anthropicstream` |
| Parallel tool blocks and interleaved reasoning | implemented and live-tested | `internal/reducer`, `internal/livesmoke` |
| Honest errors, bounded retries, breaker, PR 68/70/71 pins | implemented | `internal/failure`, `internal/retry`, `internal/engine`, fixtures |
| Cooperative ChatGPT OAuth/browser/device/status | implemented | `internal/auth`, `internal/oauth`, `internal/commandauth` |
| Live catalog, effort/fast IDs, Claude model discovery | implemented | `internal/catalog`, `internal/model`, `internal/server/models.go` |
| Offline o200k `count_tokens` | implemented and live-calibrated | `internal/tokenizer` |
| One-command Claude launcher with real compaction budget | implemented and e2e-tested | `internal/launcher`, `cmd/clodex` |
| Status telemetry (retry/breaker, and a bounded per-call latency ring: TTFT, total, upstream-wait, first-event, commit) and redacted diagnostics | implemented | `internal/status`, `internal/redact`, `internal/upstream` |
| Six-target public-safe CI and release automation | implemented | `.github/workflows`, pinned by `internal/workflowpolicy` |
