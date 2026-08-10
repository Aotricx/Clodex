# Clodex v0.2.1 public release evidence

Evidence date: 2026-08-10. Repository: `github.com/Aotricx/Clodex`.

This dossier records reproducible checks against the exact source candidate used
for the first public release. Publication starts from a fresh Git root: previous
private Git history, tags, releases, workflow logs, and local development tooling
are not migrated into the public repository.

## Source and privacy boundary

The public snapshot contains 117 tracked paths, including 87 Go files. Its export
excludes private planning documents and all ignored local agent configuration.
Before publication, every exported path and blob was scanned for:

- local agent/tooling directories and private planning paths;
- owner filesystem paths, personal email addresses, and organization markers;
- known private-history object identifiers;
- private-key headers, GitHub tokens, OpenAI-style keys, AWS access keys, and
  JWT-shaped values.

The path and private-marker scans returned no matches. Credential-shaped matches
are limited to deliberate invalid test vectors in `internal/fixtures` and
`internal/redact`; those tests prove that fixtures reject secrets and diagnostics
redact them. The public Git root is created with the GitHub noreply identity and
is independently rescanned after an unauthenticated clone.

## Local quality gate

The candidate was checked with:

```text
$ GOTOOLCHAIN=go1.26.5 go version
go version go1.26.5 darwin/arm64
$ test -z "$(git ls-files -z '*.go' | xargs -0 gofmt -l)"
PASS
$ GOTOOLCHAIN=go1.26.5 go vet ./...
PASS
$ GOTOOLCHAIN=go1.26.5 go build ./...
PASS
$ GOTOOLCHAIN=go1.26.5 go test ./... -race -count=1
PASS: 31 packages
$ GOTOOLCHAIN=go1.26.5 go mod verify
all modules verified
$ git diff --check
PASS
$ GOTOOLCHAIN=go1.26.5 go run golang.org/x/vuln/cmd/govulncheck@latest ./...
No vulnerabilities found.
```

The default test run is hermetic. Live account, model-discovery, proxy, and Claude
end-to-end suites remain explicit opt-in checks because they require installed
clients or authenticated external services.

## Workflow policy

`internal/workflowpolicy` passed on Go 1.26.5. It pins both GitHub Actions by full
commit SHA, requires format/vet/race/build gates, checks all six release targets,
and enforces the exact twelve-file release asset set. The test workflow runs the
full gate on Linux plus native vet/build checks on macOS ARM64, Windows AMD64, and
Windows ARM64. The release workflow creates a draft first and publishes it only
after verifying every expected asset exists.

## Six static cross-builds

Each artifact used `CGO_ENABLED=0`, `-trimpath`, stripped symbols, and
`-X main.version=v0.2.1`. All six checksum files verified successfully, and the
native artifact printed exactly `v0.2.1` from its `version` command.

| Artifact | Bytes | SHA-256 |
| --- | ---: | --- |
| `clodex-darwin-amd64` | 22,358,816 | `34397568896138df4943b0d1fc53d1852d743e7529c4b3db1c18fba35f676f8c` |
| `clodex-darwin-arm64` | 21,831,202 | `0657d59e9b5e9625e45565ae2b7995b0de26cc2a52ae8760f6b6170d62c412de` |
| `clodex-linux-amd64` | 19,861,666 | `aebadb5cb36c8f835eaeb5bac446bd66e5d7026aef1d4b2eb811c676d3bd0b4f` |
| `clodex-linux-arm64` | 19,202,210 | `5a60b3a5f72743953066041582b871d97c5f1d9316c0e803d8371c2653a97ba2` |
| `clodex-windows-amd64.exe` | 20,885,504 | `d10ac60d27e708e1fd2bfab55474b3da6853519709892c6fb1d5de23c5011c05` |
| `clodex-windows-arm64.exe` | 20,086,784 | `4c4e344057f83ebc3e885bcfd765e94cf02306823d453d90c5dc265116c79cc4` |

Release binaries are rebuilt by GitHub Actions, so their hashes may differ from
these macOS-hosted cross-builds while remaining reproducible from the same source,
version injection, and target matrix. Published assets are validated against their
workflow-generated checksum files before the release becomes public.

## Demonstrated behavior and limits

The hermetic suite covers Anthropic request validation and SSE translation,
encrypted reasoning replay, tool and image round trips, retry and circuit-breaker
behavior, OAuth state handling, catalog/model mapping, offline token counting,
redaction, launcher lifecycle, loopback-only binding, and workflow policy.

Clodex remains a local, single-user adapter. It is not a hosted service, public
gateway, multi-tenant proxy, general model router, or provider abstraction. It
supports ChatGPT Codex subscription OAuth only and deliberately rejects
non-loopback listening. External client and upstream behavior can change after
this evidence date; opt-in live suites are the compatibility check for those
moving boundaries.
