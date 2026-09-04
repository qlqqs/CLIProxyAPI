# Directory Structure

> How backend code is organized in this project.

---

## Overview

CLIProxyAPI is a Go 1.26.0 module (`github.com/router-for-me/CLIProxyAPI/v7`) that
exposes OpenAI-, Gemini-, Claude-, and Codex-compatible HTTP/WebSocket APIs.
The `cmd` packages are small process entry points; reusable server behavior is
kept in `internal`; the embeddable public surface is under `sdk`. Keep a new
feature in the layer that owns its contract and avoid reaching around that
layer through package globals.

---

## Directory Layout

```
cmd/
├── server/main.go                         # production entry point
├── fetch_antigravity_models/main.go       # catalog utility
├── fetch_codex_models/main.go              # catalog utility
└── validate_codex_models/main.go           # catalog validation utility
internal/
├── api/                                    # Gin server, routes, middleware
│   ├── handlers/management/                # management-only endpoints
│   └── modules/amp/                         # Amp routes and reverse proxy
├── auth/<provider>/                         # provider authentication flows
├── client/<provider>/                       # provider HTTP/WebSocket clients
├── config/                                  # config loading and validation
├── cache/                                   # request/reasoning replay caches
├── managementasset/                         # config snapshots/assets
├── registry/                                # model registry and updater
├── runtime/executor/                        # provider execution adapters
│   └── helps/                               # executor support helpers
├── store/                                   # optional persistence backends
├── translator/<provider>/                   # protocol translations
├── watcher/                                 # file/config hot reload
├── wsrelay/                                 # WebSocket relay sessions
├── tui/                                     # Bubbletea terminal UI
└── logging/                                 # global and request logging
sdk/
├── api/handlers/{openai,gemini,claude}/     # protocol-facing handlers
├── auth/                                    # default file token store
├── cliproxy/                                # embeddable service and core APIs
│   └── usage/                               # usage accounting and token events
├── config/                                  # public SDK configuration types
└── pluginapi/, pluginhost/, pluginstore/    # plugin contracts and storage
test/                                        # cross-package integration tests
```

---

## Module Organization

- Route registration and server-wide middleware belong in
  `internal/api/server_routes.go`, the other `server_*.go` files (including
  `server_middleware.go`), and `internal/api/middleware/`. The route file wires
  endpoints; it should not become a provider implementation.
- Protocol behavior belongs in the matching SDK handler package. For example,
  `/v1/chat/completions` is registered by
  `internal/api/server_routes.go` and implemented by
  `sdk/api/handlers/openai/`; Gemini and Claude follow the same split.
- Management endpoints share `internal/api/handlers/management/handler.go`
  and are split into focused files such as `config_basic.go` and
  `config_lists.go`.
- Provider-specific authentication, clients, and executors stay under their
  respective `internal/{auth,client,runtime/executor}` packages. Executors and
  their unit tests are the only files directly under `internal/runtime/executor`;
  supporting code goes in `internal/runtime/executor/helps/`.
- Configuration, model discovery, persistence, and reload logic stay in
  `internal/config/`, `internal/registry/`, `internal/store/`, and
  `internal/watcher/` respectively. Shared public lifecycle behavior is in
  `sdk/cliproxy/` (for example `service.go` and `service_auth.go`).
- Keep provider translation in `internal/translator/<provider>/` (and
  `common/`) and preserve the canonical thinking flow in
  `internal/thinking/`: `ApplyThinking` parses suffix overrides, normalizes to
  `ThinkingConfig`, validates/converts centrally, and delegates provider output
  to `ProviderApplier`. A translator-only change is restricted by the project
  contribution rules and must not be used as a shortcut for business logic.
- Put tests next to the package they exercise. Use the root `test/` package only
  for cross-module integration behavior.

---

## Naming Conventions

- Use lowercase Go package and directory names, with provider names matching
  the existing key (`openai`, `gemini`, `claude`, `codex`).
- Use descriptive underscore-separated file names for multiword concerns
  (`server_routes.go`, `config_basic.go`, `postgres_cooldown_store.go`). Keep
  `main.go` for command entry points and `*_test.go` for tests.
- Keep exported types/functions documented in Go style; use unexported helpers
  when a contract is package-local. Name files after the responsibility, not a
  generic `utils` bucket.
- Downstream entry points and public wrappers live under `sdk/`; downstream
  applications should import those packages rather than this module's
  `internal/` packages. Repository-owned SDK implementations routinely import
  `internal/`, and some wrappers or aliases are backed by internal types, so
  `sdk/` -> `internal/` is not a forbidden dependency direction. Follow the
  owning SDK package's existing API shape and check downstream compatibility
  before moving types or changing exported signatures. Protocol handlers
  should reuse shared helpers rather than duplicate them.

---

## Examples

- [Route wiring and protocol boundaries](../../../internal/api/server_routes.go)
- [Management handler split](../../../internal/api/handlers/management/config_basic.go)
- [Executor plus helper boundary](../../../internal/runtime/executor/codex_executor_execute.go)
  and [executor helpers](../../../internal/runtime/executor/helps/)
- [Embeddable service lifecycle](../../../sdk/cliproxy/service.go)
- [Protocol handler package](../../../sdk/api/handlers/openai/)
