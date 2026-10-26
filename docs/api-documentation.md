# freedeep Go API Reference

## Overview

freedeep is a local Go proxy for DeepSeek Web Chat. It exposes three compatible API surfaces:

- OpenAI Chat Completions
- Anthropic Messages
- OpenAI Responses

The proxy is experimental and relies on DeepSeek's private Web API. DeepSeek can change that API without notice. Use the official DeepSeek API for production workloads that require a supported contract or SLA.

By default the server listens only on `127.0.0.1:9655`. Never expose it to a network without setting `PROXY_API_KEY` or `PROXY_API_KEY_FILE`.

## Architecture

```text
OpenAI / Anthropic / Responses client
                 |
                 v
        freedeep Go server
          |       |       |
          |       |       +-- account pool and per-agent sessions
          |       +---------- native DeepSeekHashV1 proof of work
          +------------------ DeepSeek Web HTTP/SSE client
                                 |
                                 v
                         chat.deepseek.com
```

The executable and all packages use the Go standard library only. The runtime container is built `FROM scratch` and contains the static executable plus CA certificates.

## Authentication

Run the browser login helper on a machine with Chrome, Chromium, or Edge:

```bash
freedeep login
```

This creates `deepseek-auth.json`. The file contains a live DeepSeek Web session and must never be committed, published, or copied into a container image. It is excluded by `.gitignore` and `.containerignore`.

You may also import a ready auth file, the bundled Chrome extension export, or a browser cookie export:

```bash
freedeep import --input ./cookies.json
```

For multiple accounts, place one JSON file per account in a private directory and set `DEEPSEEK_AUTH_DIR`. Accounts rejected with `401`, `403`, or `429` enter a cooldown before they can be selected again.

## Starting the server

```bash
freedeep serve
```

For a headless process:

```bash
NON_INTERACTIVE=1 freedeep serve
```

To listen outside the local machine, require an independent proxy key:

```bash
HOST=0.0.0.0 \
PROXY_API_KEY='replace-with-a-long-random-value' \
REQUIRE_PROXY_API_KEY=1 \
freedeep serve
```

Clients can send the key as either:

```http
Authorization: Bearer replace-with-a-long-random-value
```

or:

```http
x-api-key: replace-with-a-long-random-value
```

The proxy key is separate from the DeepSeek credentials and is never forwarded upstream.

## Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/` | Liveness information |
| `GET` | `/health` | Liveness probe |
| `GET` | `/readyz` | Readiness probe; `503` when no account is ready |
| `GET` | `/v1/models` | Supported model aliases |
| `GET` | `/v1/model-capabilities` | Alias mapping and capabilities |
| `GET` | `/api/model-capabilities` | Compatibility alias for model capabilities |
| `GET` | `/v1/sessions` | Active local agent sessions |
| `POST` | `/v1/chat/completions` | OpenAI Chat Completions-compatible API |
| `POST` | `/v1/messages` | Anthropic Messages-compatible API |
| `POST` | `/v1/responses` | OpenAI Responses-compatible API |
| `POST` | `/reset-session?agent=<id>` | Reset one local agent session |
| `POST` | `/reset-session?agent=all` | Reset all local agent sessions |

`/`, `/health`, and `/readyz` are public so orchestration systems can probe the process. All other endpoints require the proxy key when one is configured.

## OpenAI Chat Completions

```bash
curl http://127.0.0.1:9655/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "deepseek-chat",
    "user": "agent-1",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": false
  }'
```

Set `stream` to `true` for an SSE response. Thinking models return reasoning separately as `reasoning_content` when `DEEPSEEK_SHOW_REASONING` is enabled.

## Anthropic Messages

```bash
curl http://127.0.0.1:9655/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "deepseek-chat",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

The adapter accepts Anthropic system blocks, messages, tools, metadata, and streaming requests. `metadata.user_id` is used as the preferred session key when provided.

## OpenAI Responses

```bash
curl http://127.0.0.1:9655/v1/responses \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "deepseek-chat",
    "user": "agent-1",
    "input": "Hello",
    "stream": false
  }'
```

The adapter accepts plain string input or Responses-style input items and converts output to Responses events or JSON.

## Models

Use `GET /v1/models` for the authoritative list exposed by the running binary and `GET /v1/model-capabilities` for the underlying Web mode, reasoning, search, file, and vision flags.

Common aliases include:

| Alias | Behaviour |
| --- | --- |
| `deepseek-chat` | Default non-thinking mode |
| `deepseek-reasoner` | Thinking mode |
| `deepseek-chat-search` | Default mode with Web search |
| `deepseek-reasoner-search` | Thinking mode with Web search |
| `deepseek-expert` | Expert Web mode |

Model availability is controlled by DeepSeek and can change independently of this project.

## Tools

DeepSeek Web does not expose native structured tool calls. The proxy converts client tool schemas into a guarded text protocol and parses the model's response back into the format required by each compatible API.

The parser supports strict and lenient JSON extraction, nested objects, escaped strings, legacy tool markup, and DSML-style tool calls. Because generation is model-driven, tool selection is less reliable than an official API with native function calling.

## Session isolation and recovery

The proxy maintains one DeepSeek Web chain per local session key. A key is selected from an explicit agent/user identifier, the `X-Agent-Session` header, or the remote client address, depending on the API format and available fields.

Local history is retained in a bounded buffer. When an upstream chain expires, becomes too long, or returns a recoverable empty response, the proxy creates a fresh DeepSeek session and retries with compacted context. Send `/new` as the user message or call `/reset-session` to start fresh explicitly.

## Configuration

Copy `.env.example` to `.env` for a documented configuration template. The executable loads `./.env` first; real environment variables take precedence.

Important settings:

| Variable | Default | Purpose |
| --- | --- | --- |
| `HOST` | `127.0.0.1` | Bind address |
| `PORT` | `9655` | HTTP port |
| `PROXY_API_KEY` | empty | Key required by protected endpoints |
| `PROXY_API_KEY_FILE` | empty | Read the proxy key from a mounted secret |
| `REQUIRE_PROXY_API_KEY` | `0` | Fail startup when no proxy key is configured |
| `PROXY_CORS_ORIGINS` | loopback only | Additional exact browser origins |
| `DEEPSEEK_AUTH_PATH` | `./deepseek-auth.json` | One or more comma-separated auth files |
| `DEEPSEEK_AUTH_DIR` | empty | Directory containing account JSON files |
| `DEEPSEEK_ACCOUNT_COOLDOWN_MS` | `600000` | Account cooldown after upstream rejection |
| `DEEPSEEK_MAX_PROMPT_CHARS` | `80000` | Prompt compaction budget in characters |
| `DEEPSEEK_MAX_RETRIES` | `2` | Fresh-session recovery attempts |
| `DEEPSEEK_MAX_CONCURRENT` | `24` | Maximum concurrent completions |
| `DEEPSEEK_REQUEST_DEADLINE_MS` | `120000` | Whole-request retry/continuation budget |
| `DEEPSEEK_FETCH_TIMEOUT_MS` | `60000` | Upstream request/header timeout |
| `DEEPSEEK_STREAM_IDLE_TIMEOUT_MS` | `90000` | Maximum upstream stream silence |
| `DEEPSEEK_SHOW_REASONING` | `1` | Return model reasoning |
| `NON_INTERACTIVE` | `0` | Skip the terminal menu |

See `.env.example` for every supported setting and its security notes.

## Errors

Errors use a JSON `error` object appropriate to the selected compatibility API. Common statuses are:

| Status | Meaning |
| --- | --- |
| `400` | Invalid request/model or context still too long after compaction |
| `401` | Missing/incorrect proxy key or rejected DeepSeek login |
| `403` | Browser origin not allowed |
| `413` | Request body exceeds 10 MiB |
| `429` | DeepSeek rate limit or every account is cooling down |
| `502` | Empty upstream answer or malformed generated tool call |
| `503` | No usable account or local concurrency limit reached |
| `504` | Upstream request deadline or stream idle timeout |

## Diagnostics

```bash
freedeep doctor
freedeep doctor --offline
freedeep probe
```

`doctor` validates auth files and, unless `--offline` is supplied, checks the live PoW flow. `probe` tests the currently configured model modes against DeepSeek Web.

## Source layout

| Path | Responsibility |
| --- | --- |
| `cmd/freedeep` | CLI, login flow entry points, server startup |
| `internal/server` | HTTP handlers, adapters, SSE output, recovery engine |
| `internal/deepseek` | DeepSeek Web client, model mapping, upstream stream parser |
| `internal/pow` | Native `DeepSeekHashV1` solver |
| `internal/state` | Account pool and per-agent sessions |
| `internal/prompt` | Transcript conversion and context compaction |
| `internal/toolcall` | Tool schema injection and response parsing |
| `internal/authcli` | Login import and diagnostics |
| `internal/cdp` | Minimal Chrome DevTools Protocol client |
| `internal/livetest` | Opt-in smoke tests against a real logged-in proxy |

## Validation

```bash
go test ./...
go vet ./...
go test -race ./...
go build ./cmd/freedeep
```

The regular test suite uses a fake upstream and does not require credentials or network access. Live tests are opt-in through `FREEDEEP_LIVE_URL`; see the main README for the command.
