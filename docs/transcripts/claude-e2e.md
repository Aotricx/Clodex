# Claude Code end-to-end transcript

Date: 2026-07-21. Installed Claude Code reported `2.1.141`; the supplied
`2.1.216` machine fact had drifted. Raw JSONL remains mode 0600 outside git;
this transcript retains semantic events and usage but omits session IDs,
machine plugin paths, and opaque signatures.

## Launcher context correction

```text
$ CLODEX_PORT=18484 CLODEX_MODEL=gpt-5.4-mini:low \
  /tmp/clodex-dev claude --model gpt-5.4-mini:low -- \
  --print 'Reply exactly WINDOW_FIXED_OK' --output-format json
result=WINDOW_FIXED_OK
model=anthropic-clodex-gpt-5.4-mini:low[1m]
reported_context_window=1000000
```

Binary inspection proved the environment auto-compact value is separately
clamped to model inference. Launcher set `CLAUDE_CODE_AUTO_COMPACT_WINDOW=272000`;
therefore the carrier prevents Claude's unknown-model 200k cap while compaction
still occurs at the real catalog budget. No backend output-token cap exists.

## Mini image tool round-trip

```text
assistant tool_use Read {file_path:[OWNER_DOWNLOADS]/Dog.jpeg}
assistant text "Great Dane — 98% confidence"
result subtype=success is_error=false num_turns=2
usage input_tokens=31764 output_tokens=92 permission_denials=[]
model=anthropic-clodex-gpt-5.4-mini:low[1m] contextWindow=1000000
```

The Read tool's image result became an Anthropic `tool_result` containing the
JPEG, translated to a Codex function output image, then identified correctly.

## Xhigh multi-turn tools and visible thinking

```text
assistant thinking "Designing dual tool calls"
assistant tool_use Read {file_path:.../alpha.txt}
assistant tool_use Read {file_path:.../beta.txt}
assistant text "ALPHA_MARKER+BETA_MARKER"
result subtype=success is_error=false num_turns=3
usage input_tokens=1855 output_tokens=171 permission_denials=[]
model=anthropic-clodex-gpt-5.6-sol:xhigh[1m] contextWindow=1000000
```

`gpt-5.6-sol` is Responses Lite and pinned Codex deliberately sends
`parallel_tool_calls:false`; the two built-in Read calls were therefore separate
tool turns. Parallel response fidelity is demonstrated by the live direct proxy
test above, which returned two tool blocks in one Anthropic response. This
model-choice/Responses-Lite behavior is recorded rather than mislabeled as a
parallel Claude Code turn.
