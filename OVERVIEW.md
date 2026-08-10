# Overview

## What is this?
**Clodex** runs **GPT (OpenAI) models inside Claude Code**. It is a local proxy
that sits between Claude Code (which speaks the Anthropic Messages API) and the
ChatGPT Codex backend, translating requests and responses both ways so
that a GPT model can drive Claude Code's agentic workflow as if it were Claude.

## Who uses it?
A developer who wants to use OpenAI models with the Claude Code harness — pointing
Claude Code at Clodex's local endpoint instead of the Anthropic API. Single-user,
local-first; not a hosted or multi-tenant service.

## Invariants that OVERRIDE default best practice
These are the non-obvious rules an agent must **not** "improve away". They follow
from the nature of a local model-translation proxy and from project policy:

- **Loopback only.** The proxy binds to `127.0.0.1` — never `0.0.0.0` or a public
  interface. Exposing an OAuth-backed local adapter to the network would invite
  abuse. "Make it reachable from another machine" is a
  wrong-for-this-project request.
- **Translation must be lossless.** System prompts, multi-turn tool use, *parallel*
  tool calls, streaming, and token-usage accounting must round-trip faithfully
  between the two wire formats. Dropping tool-call fidelity or approximating token
  counts "for simplicity" is a correctness bug, not a shortcut.
- **No fake errors, no stubs, no placeholder success.** Every path is a real
  implementation or an honest, surfaced failure. Never fabricate a success response
  or swallow an upstream error.
- **One backend and auth mode.** ChatGPT Codex over subscription OAuth only. No
  provider abstraction, API-key path, gateway mode, or configurable upstream URL.
- **No output-token cap.** Current Codex request/catalog schema exposes no output
  limit; Anthropic `max_tokens` is accepted and warning-accounted, never guessed.
- **Public source, local-only runtime.** Source and release artifacts are public,
  but Clodex remains a single-user loopback tool. Public source does not authorize
  a hosted deployment, multi-tenant service, or non-loopback listener.

## What we are NOT building
- Not a hosted/SaaS proxy, not multi-tenant, not a public gateway.
- Not a general LLM router or load balancer — the job is faithful Anthropic⇄Codex
  translation for Claude Code, nothing broader.
- Not a reimplementation of Claude Code or of either provider's SDK.

## Why the constraints exist
Clodex's whole value is *fidelity*: if the translation isn't lossless, GPT-in-Claude-Code
behaves subtly differently from the real thing and the tool is worthless. Its whole
*safety* is that it's local: it uses real OAuth credentials, so it must never listen beyond
loopback. Keep both properties sacred.
