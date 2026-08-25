# Codex subscription authentication

Branch: bangkok

## What this does

Lets the local loopy CLI use an existing ChatGPT/Codex subscription without an
API key. A provider explicitly opts into the Codex Responses transport and
reads credentials from Pi first, then the Codex CLI.

## Goal

With this config and an existing `pi /login openai-codex` or `codex login`, a
user can select `gpt-5.4` and run loopy through the ChatGPT Codex backend:

```json
{
  "providers": {
    "codex": {
      "name": "Codex",
      "api": "openai-codex-responses",
      "auth": "codex",
      "baseUrl": "https://chatgpt.com/backend-api"
    }
  },
  "models": {
    "gpt-5.4": {
      "providers": ["codex"],
      "context": 272000,
      "maxOut": 128000
    }
  }
}
```

## Non-goals

- Hosted login, settings UI, bridge sync, or WebSocket transport.
- A Codex model catalog; configured `context` and `maxOut` remain authoritative.
- Sending credentials to logs, transcript, session storage, or model context.

## Design

- `internal/codexauth` loads Pi's `~/.pi/agent/auth.json` first, then Codex
  CLI's `~/.codex/auth.json`; it derives missing account/expiry data from JWT
  claims, refreshes tokens within five minutes of expiry, and atomically
  preserves the selected auth file's unknown fields.
- `internal/llm` gains the small client interface used by `agent.Agent`.
  `OpenAI` remains the compatible chat-completions implementation; `Codex`
  maps loopy messages/tools to Responses input and parses its SSE events.
- `internal/tui` chooses the client by `Provider.API`; Codex providers check
  that an existing login is available without requiring `apiKey`.

## Prior art

- Codex's official source stores CLI credentials under `tokens` with
  `access_token`, `refresh_token`, and `account_id`; its refresh request uses
  `grant_type=refresh_token` at `https://auth.openai.com/oauth/token`.
- The attached plan supplies Pi's `openai-codex` auth shape and the local SSE
  request contract. The implementation intentionally stays inside that scope.

## Test plan

- `internal/codexauth/auth_test.go`: Pi/Codex parsing, JWT claim extraction,
  expiry refresh, and field-preserving auth-file updates.
- `internal/llm/codex_test.go`: request path, headers, body, text/thinking
  deltas, function calls, usage, and non-streaming completion.
- `internal/tui/model_cmd_test.go`: a Codex-auth provider builds with no API
  key and gives the login hint when auth is absent.

## Docs plan

- Add setup/config instructions to `README.md`.
- Map the feature and its tests in `docs/features.md` and mark the roadmap item.

## Tasks

1. [x] Add authentication source and tests.
2. [x] Add Responses client behind the existing-agent client interface and tests.
3. [x] Route the provider, bypass catalog fetches, and cover startup validation.
4. [x] Document the configuration, run checks, and perform a Ponytail review.
