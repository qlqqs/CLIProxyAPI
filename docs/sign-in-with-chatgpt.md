# Sign in with ChatGPT

This provider logs in with the OAuth flow used by the [pi coding agent](https://github.com/earendil-works/pi)
instead of the Codex device/browser flow, and serves traffic against the public
OpenAI API host.

## Why it is a separate provider

The two OpenAI flows are different client registrations, not two URLs for the same thing:

| | `codex` | `chatgpt` (this provider) |
|---|---|---|
| Authorization endpoint | `auth.openai.com/oauth/authorize` | `auth.openai.com/api/accounts/authorize` |
| Token endpoint | `auth.openai.com/oauth/token` | `auth.openai.com/api/accounts/oauth/token` |
| Client id | fixed (`app_EMoamEEZ73f0CkXaXp7hrann`) | dynamic registration (`dynamic_agent_client` → issued `oaiapp_…`) |
| Scopes | `openid profile email offline_access` | adds `resource.invoke` + `chatgpt.tokens.use.direct`, `resource=https://api.openai.com/v1` |
| Account id | present in the ID token, sent as `Chatgpt-Account-Id` | **not present** — the grant carries no account claim |
| Upstream host | `chatgpt.com/backend-api/codex` | `api.openai.com/v1` |

Because the token is scoped to `api.openai.com`, the Codex backend rejects it
(`401 rejected_by_access_enforcement` / `no_matching_rule`), and because there is no
account id the Codex executor's headers cannot be built. Hence a separate authenticator,
executor and credential shape.

## Logging in

The flow has no loopback listener, so it works on a headless host: sign in in any
browser, then hand the callback URL back to the proxy. The browser will fail to load
that URL (`http://127.0.0.1:1455/auth/callback?code=…`) — that is expected; copy the
full address from the address bar.

### CLI

```bash
cli-proxy-api -chatgpt-login -config /path/to/config.yaml
```

### Management API

```bash
# 1. start a login and open the returned url
curl -s "http://127.0.0.1:8317/v0/management/chatgpt-auth-url" \
  -H "Authorization: Bearer $MANAGEMENT_KEY"
# → {"status":"ok","url":"https://auth.openai.com/api/accounts/authorize?...",
#    "state":"…","redirect_uri":"http://127.0.0.1:1455/auth/callback","provider":"chatgpt"}

# 2. paste the full callback URL from the browser
curl -s -X POST "http://127.0.0.1:8317/v0/management/chatgpt-auth-callback" \
  -H "Authorization: Bearer $MANAGEMENT_KEY" -H 'content-type: application/json' \
  -d '{"state":"…","redirect_url":"http://127.0.0.1:1455/auth/callback?code=…&state=…&client_id=oaiapp_…"}'
# → {"status":"ok","provider":"chatgpt","file_name":"chatgpt-<hash>.json"}
```

Optional query parameters on the start call: `agent_name_hint` (default `Pi`),
`device_id` (UUID; otherwise generated once as `.chatgpt-device-id` in `auth-dir`),
`proxy_url` (per-account egress, also stored on the credential).

A started login is single-use and expires after 30 minutes.

## Credential file

`auth-dir/chatgpt-<hash>.json`:

```json
{
  "type": "chatgpt",
  "access_token": "…",
  "refresh_token": "…",
  "client_id": "oaiapp_…",
  "scopes": ["openid", "profile", "email", "offline_access", "resource.invoke", "chatgpt.tokens.use.direct"],
  "expired": "2026-10-02T18:18:07Z",
  "last_refresh": "2026-10-02T10:18:07Z"
}
```

`client_id` is the id the authorization server issued for this login; refresh requests
are rejected without it, so it must survive credential edits.

## Runtime behaviour

- Upstream: `https://api.openai.com/v1/responses`, plain bearer request. No
  `originator`, no `Chatgpt-Account-Id`, no Codex cloaking headers, no `codex_cli_rs`
  user agent.
- The endpoint's contract is enforced after translation and payload config:
  `store` is always present and `false`, `input` is always a JSON array, and
  `max_output_tokens`, `max_completion_tokens`, `temperature`,
  `prompt_cache_retention`, `prompt_cache_options`, `top_p` and `truncation` are
  removed (the endpoint answers `Unsupported parameter: <name>`).
- Upstream always streams; non-streaming callers are served by aggregation, and
  `output` is rebuilt from `response.output_item.done` because `response.completed`
  carries an empty `output` array on this endpoint.
- Refresh replays the issued client id, rotates the refresh token and rewrites
  `expired` / `last_refresh`.
- The provider publishes the Codex Pro model group (the same set the Codex OAuth path
  uses when the plan is unknown). An auth with no registered models is never selected
  by the scheduler, so this registration is required, not cosmetic. Narrow it with
  `oauth-excluded-models: chatgpt: [...]` if needed.

## Known limitations

- No `account_id`: the grant does not carry one, so features that need
  `Chatgpt-Account-Id` (Codex backend, some quota endpoints) do not apply.
- `base_url` cannot be overridden from the auth file yet; the credential always uses
  `https://api.openai.com/v1`. Set it in memory (`attributes.base_url`) if you embed the
  SDK.
- Quota signals and websocket transport are codex/claude/xai features; this provider
  uses HTTP only and reports no quota signals.
