# Live backend smoke transcript

Date: 2026-07-21. Secrets and opaque reasoning signatures are omitted. The
proxy was `/tmp/clodex-dev serve --port 18484`; all traffic stayed loopback.

```text
$ CLODEX_LIVE_TESTS=1 \
  CLODEX_LIVE_BASE_URL=http://127.0.0.1:18484 \
  CLODEX_LIVE_IMAGE=[OWNER_DOWNLOADS]/Dog.jpeg \
  go test -race ./internal/livesmoke -run TestLiveProxySmoke -v -count=1
=== RUN   TestLiveProxySmoke
=== RUN   TestLiveProxySmoke/catalog
    live models=140 first=gpt-5.6-sol
    non-stream text="CLODEX_LIVE_OK" count_tokens=34 live_input_tokens=23 output_tokens=23
=== RUN   TestLiveProxySmoke/stream
    stream events=message_start,content_block_start,content_block_delta,
      content_block_stop,content_block_start,content_block_delta,
      content_block_delta,content_block_delta,content_block_delta,
      content_block_stop,message_delta,message_stop text="STREAM_LIVE_OK"
=== RUN   TestLiveProxySmoke/tool_round_trip
    tool id_present=true input="live-tool" result="live-tool"
=== RUN   TestLiveProxySmoke/parallel_tool_calls
    parallel tools=[read_alpha read_beta] in one Anthropic response
=== RUN   TestLiveProxySmoke/image
    image answer="Great Dane — high confidence" count_tokens=303 live_input_tokens=319
--- PASS: TestLiveProxySmoke (12.63s)
PASS
ok github.com/Aotricx/Clodex/internal/livesmoke 13.948s
```

Separate live catalog discovery after applying Codex's source-defined missing
field default:

```text
=== RUN   TestLiveModelDiscovery
    live catalog source=live models=7 first=gpt-5.6-sol
--- PASS: TestLiveModelDiscovery (0.36s)
PASS
ok github.com/Aotricx/Clodex/internal/catalog 1.724s
```

The image is a 600×400 JPEG supplied by the owner. The model returned the exact
breed independently in the direct proxy smoke and Claude Code e2e.
