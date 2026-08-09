# ChatGPT subscription authentication

QED can call the ChatGPT Codex backend with a ChatGPT subscription while
retaining QED's own Runtime, Tool loop, Extensions, Sessions, and Evidence

This path is distinct from `openai-responses`, which uses an OpenAI API key and
API billing. A ChatGPT plan does not become an API key, and an API key does not
authenticate `openai-codex`

## Sign in

Browser login uses OAuth authorization code flow with PKCE and a loopback
callback. QED validates the callback state before exchanging the code

```sh
qed auth login --auth-profile personal
```

Use `--no-open` to print the authorization URL without launching a browser

On a headless machine, use the device-code flow

```sh
qed auth login --auth-profile server --device-code
```

Device-code login must be available for the ChatGPT account. QED prints the
verification URL and one-time user code, then waits for authorization

Profile names let one QED installation keep independent credentials without
putting tokens in project configuration

```sh
qed auth status
qed auth status --auth-profile personal
qed auth logout --auth-profile personal
```

`status` returns only the profile name, expiry, plan metadata, and email when
present. It never prints tokens or the ChatGPT account ID. Logout removes the
local profile first and then attempts remote token revocation

## Run a model

The model identifier is explicit because model access depends on the account,
plan, and current backend catalog

```sh
qed run \
  --provider openai-codex \
  --auth-profile personal \
  --model "<codex-model-id>" \
  --prompt "Reply with a short greeting"
```

The equivalent Provider profile is

```json
{
  "protocol": "openai-codex",
  "model": "<codex-model-id>",
  "auth_profile": "personal"
}
```

`openai-codex` uses a fixed HTTPS backend. It rejects `base_url`, `token_env`,
`max_output_tokens`, and `api_version`, preventing ChatGPT credentials from
being redirected to a configured endpoint

## Credential storage and refresh

QED stores its own named profiles in `qed/auth.json` below the directory
returned by Go's `os.UserConfigDir`. It does not read or modify another
application's credential file

The file contains raw ID, access, and refresh tokens. On Unix systems, QED
creates the containing directory with mode `0700`, writes the file atomically
with mode `0600`, and rejects a symlink or a file with broader permissions.
Concurrent QED processes coordinate updates with a local lock. Protect this
file like any other long-lived credential and do not commit it, place it in a
project, or publish it

QED refreshes credentials shortly before expiry. If the backend rejects a
request with HTTP 401, the Provider reloads or refreshes the named profile and
retries that request once. Token values are excluded from CLI status and QED's
structured diagnostics

## Protocol status

The OAuth and backend behavior follow the current ChatGPT Codex client
contract. QED identifies itself with its own `qed` originator rather than
impersonating another client

This integration is experimental because model availability and the ChatGPT
backend contract can change independently of the public OpenAI API. QED
currently sends full Codex Responses over SSE. It does not implement backend
model discovery, the internal Responses Lite dialect, or WebSocket transport

See OpenAI's current [authentication documentation](https://learn.chatgpt.com/docs/auth)
for the upstream account and device-code behavior
