# Dependencies

`go.mod` is authoritative. Direct and runtime-transitive modules are recorded
here with the source and license evidence used to admit them.

## Go modules

| Module | Relationship | Version / source commit | License | Purpose and runtime behavior |
| --- | --- | --- | --- | --- |
| `github.com/tiktoken-go/tokenizer` | direct | `v0.8.1` / `ca39f5c7bff9edfc9d70012c5dfdf23b44f7971e` | MIT; full text and attribution in [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) | Exact `o200k_base` lexical counting. `codec/o200k_base_vocab.go` contains the generated vocabulary as compiled Go data; `tokenizer.Get(tokenizer.O200kBase)` performs no runtime file or network access. |
| `github.com/dlclark/regexp2/v2` | indirect through tokenizer | `v2.5.1` / `0ec61737c1e8483bb16083a916fa9a3bd21f91bb` | MIT; full text and attribution in [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) | Pure-Go regular-expression engine used by the tokenizer's o200k pre-tokenization pattern. |

Module versions, commits, and checksums were verified with `go mod download
-json`; license texts were inspected in each module archive and preserved in
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md). The offline
subprocess test starts with empty home/cache directories and dead HTTP(S)
proxies, then constructs and uses the tokenizer successfully.

### Token-estimate provenance

Clodex's structural and image rules adapt OpenAI Codex CLI `rust-v0.144.6`,
commit `5d1fbf26c43abc65a203928b2e31561cb039e06d` (Apache-2.0; full text and
attribution in [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)):

- `codex-rs/core/src/context_manager/history.rs:171-184` counts raw base
  instructions plus independently serialized history items;
- `history.rs:519-575` serializes item structure and replaces opaque payload
  lengths before estimation;
- `history.rs:524-536,580-688` supplies the conservative 7,373-byte fallback
  (1,844 tokens) and capped 32-pixel patch geometry for inline images;
- `codex-rs/utils/string/src/truncate.rs:71-83` defines that ceiling conversion.

Clodex retains those request boundaries, uses exact offline `o200k_base` for
text/structure, and applies the source's 32-pixel patch geometry to every
decodable inline image. A live 600×400 JPEG spot-check improved from 1,900 vs
319 upstream tokens to 303 vs 319. Malformed/unsupported inline encodings keep
the conservative 1,844-token fallback. HTTP(S) image URLs remain serialized URL
text because offline counting cannot know their dimensions. The root MIT
`LICENSE` is unchanged; third-party license texts remain separate.

## External services
| Service | Purpose | Secret | Where configured | Fallback when down |
| --- | --- | --- | --- | --- |
| ChatGPT Codex backend | Sole inference and live-model endpoint | ChatGPT OAuth in `~/.codex/auth.json` | Shared Codex CLI file; `clodex auth` writes the same shape | Use a compiled model catalog only for discovery; inference errors surface honestly |
| OpenAI OAuth authority | Browser PKCE, device-code login, and refresh | Refresh/access/ID tokens in the shared auth file | Exact official endpoints compiled into the OAuth client | Login/refresh fails honestly; no API-key fallback |

## Secrets
- Clodex supports ChatGPT-subscription OAuth only; `OPENAI_API_KEY` is preserved
  for Codex file compatibility but never used.
- `~/.codex/auth.json` remains mode 0600 and is updated by same-directory atomic
  rename after rereading disk state.
- No token may reach source, fixtures, status, wire dumps, errors, or evidence.
