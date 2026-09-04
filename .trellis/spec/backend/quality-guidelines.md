# Quality Guidelines

> Code quality standards for backend development.

---

## Overview

CLIProxyAPI targets Go 1.26 and favors small, explicit changes over broad
abstractions. Quality is enforced primarily through gofmt, Go tests, and a
clean server build; the repository does not currently define a project-wide
golangci-lint or staticcheck configuration. The pull-request workflow refreshes
the model catalog and builds cmd/server, but it does not replace local
go test ./... verification.

---

## Forbidden Patterns

- Do not use log.Fatal/log.Fatalf in server or library code. They terminate the
  process and bypass caller-controlled cleanup; return the error and log with
  logrus at the owning boundary.
- Do not panic in HTTP handlers for expected failures. Return a meaningful
  status and let GinLogrusRecovery handle truly unexpected panics.
- Do not add a timeout after an upstream connection is established. Timeouts
  are only allowed during credential acquisition and for the explicitly
  documented Codex WebSocket liveness, wsrelay session, management APICall, and
  fetch_antigravity_models exceptions.
- Do not put general helper/support files directly in
  internal/runtime/executor; use internal/runtime/executor/helps.
- Do not make a standalone internal/translator change without following the
  repository permission/issue workflow in AGENTS.md. Translator changes should
  normally be part of a broader owning-layer change.
- Do not bypass the canonical thinking pipeline. ApplyThinking parses suffixes
  (suffix wins over body), produces ThinkingConfig, validates/converts it
  centrally, then delegates to a ProviderApplier.
- Do not log tokens, credentials, authorization headers, DSNs, raw auth JSON, or
  raw prompts. Do not use fmt/log package output in place of structured logrus.
- Do not ignore close/rollback errors when they matter, replace wrapped causes
  with formatted strings, or reuse a shadowed err name that obscures the
  operation.
- Do not use wall-clock time.Sleep in TTL, expiry, ordering, or cache-eviction
  tests. Platform timer granularity and CI load make those tests flaky.
- Do not add non-English code comments. New Markdown documentation for this
  fork must use Simplified Chinese; keep identifiers, API fields, commands,
  and code snippets unchanged. Preserve the established language of
  user-visible strings.

---

## Required Patterns

- Run gofmt after any Go change and keep imports in goimports-style groups.
- Wrap errors with useful operation context and %w; inspect causes through
  errors.Is/errors.As.
- Propagate context.Context through database, network, auth, and long-running
  operations. Cancel goroutines and stop tickers/watchers during teardown.
- Handle deferred close errors when relevant, using an operation-specific
  variable name:

~~~go
defer func() {
    if errClose := body.Close(); errClose != nil {
        log.WithError(errClose).Warn("failed to close response body")
    }
}()
~~~

- Use method/operation suffixes for potentially shadowing errors (errStart,
  errRead, errClose, errMarshal). Keep changes KISS and reuse the package that
  owns an existing contract instead of duplicating provider/protocol logic.
- Use logrus structured fields and the safe diagnostic helpers. Clone mutable
  buffers/maps/headers before handing them to an asynchronous goroutine.
- Preserve filesystem credential permissions (0700 directories, 0600 files),
  SQL parameterization/context, and atomic/concurrency semantics in store code.
- Keep downstream-facing entry points and public wrappers under sdk and
  repository-only packages under internal. SDK implementation files routinely
  import internal packages, and existing wrappers or aliases may be backed by
  internal types; match the owning package's current API shape and check
  downstream compatibility when exported signatures change. Update
  registration/wiring and all affected protocol paths when a shared contract
  changes.
- Search before changing registries, model catalogs, provider keys, config
  fields, or template/list values; many have mirrored consumers or generated
  refresh paths.

---

## Testing Requirements

After a Go change, run:

~~~bash
gofmt -w .
go test ./...
go build -o test-output ./cmd/server && rm test-output
~~~

The clean cmd/server build is required even when focused tests pass. For a
single regression test use:

~~~bash
go test -v -run TestName ./path/to/pkg
~~~

Testing conventions visible in the repository:

- Keep unit tests adjacent to implementation in *_test.go; use test/ for
  cross-package protocol and translation integration tests.
- Prefer table-driven cases with descriptive names and explicit expected
  values/statuses. Assert behavior and externally visible state, not private
  implementation steps.
- Use t.TempDir for filesystem state, httptest.NewServer/Recorder for HTTP,
  gin.CreateTestContext for handlers, and small fake interfaces/drivers for
  stores and executors. Avoid real credentials and external services.
- Use t.Parallel only when the test and all package/global state are isolated.
  Many packages have registries, logger state, or process-wide configuration
  that must be restored with t.Cleanup instead.
- For concurrent code, synchronize deterministically and cover cancellation,
  closed channels, backpressure, stale writes, and cleanup. The build-tag files
  sdk/api/handlers/openai/race_disabled_test.go and race_enabled_test.go let
  tests adapt expectations under go test -race.
- Inject a nowFunc/mock clock, manipulate timestamps directly, or use explicit
  channels/conditions for TTL and ordering. A bounded time.After may protect a
  deadlock-sensitive test, but time.Sleep must not determine correctness.

Good examples include the fake SQL driver and concurrent merge cases in
internal/store/postgres_cooldown_store_test.go, deterministic queue assertions
in internal/redisqueue/queue_test.go, HTTP integration in
test/usage_logging_test.go, and secret-redaction coverage in
internal/logging/diagnostic_test.go.

---

## Code Review Checklist

- [ ] Scope is small, the owning package is clear, and no existing helper or
      contract was duplicated.
- [ ] Routes, SDK handlers, executors, translators, registries, stores, watcher
      reloads, and config serialization are wired end to end where applicable.
- [ ] Provider-facing changes preserve protocol-specific response/stream shapes
      and the canonical ThinkingConfig-to-ProviderApplier architecture.
- [ ] Errors retain their causes/statuses, request faults do not penalize
      credentials, and handlers cannot double-write or panic on expected input.
- [ ] Logs use logrus, contain useful bounded fields, and cannot leak secrets or
      block the client response.
- [ ] Resources, goroutines, channels, transactions, files, and response bodies
      are cleaned up on success, error, and cancellation paths.
- [ ] Concurrency changes are race-safe and tests are deterministic across
      platforms; mutable data crossing goroutines is cloned.
- [ ] Tests cover success, invalid input, provider failure, cancellation, and
      the regression being fixed at the appropriate unit/integration layer.
- [ ] Code comments are English, new documentation is Simplified Chinese,
      user-visible language is consistent, and config examples/docs are
      updated when a public option changes.

---

## Documentation Language Convention

**What**: Write all new documentation for this fork in Simplified Chinese.
This includes Trellis `prd.md`, research notes, `design.md`, `implement.md`,
new code-specs, and newly authored Markdown sections. Keep literal Go
identifiers, JSON/YAML fields, command lines, paths, and code snippets in their
source form.

**Why**: A single documentation language avoids ambiguous terminology and
ensures that future sessions continue the project's product and architecture
planning in the language selected by the maintainer. Code comments remain in
English because they follow a separate repository convention.

**Example**:

~~~markdown
## 凭据选择约束

请求必须携带稳定的 `carpool_id`，并在 `AuthManager` 选择候选凭据之前完成过滤。
~~~
- [ ] gofmt, go test ./..., and the required clean server build pass.

Reference implementations: internal/config/config_load.go,
internal/api/middleware/response_writer.go,
internal/store/postgres_cooldown_store_test.go,
internal/redisqueue/queue_test.go, and internal/logging/diagnostic_test.go.
