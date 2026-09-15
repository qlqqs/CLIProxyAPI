# Backend Development Guidelines

> Repository-specific conventions for the Go CLIProxyAPI backend.

---

## Overview

CLIProxyAPI is a Go 1.26 proxy server (github.com/router-for-me/CLIProxyAPI/v7)
with Gin HTTP/WebSocket routes compatible with OpenAI, Gemini, Claude, and Codex
clients. Process entry points live in cmd/, reusable implementation in
internal/, and the embeddable/public contracts in sdk/. These guides describe
patterns observed in the repository, including its file-based default storage,
optional PostgreSQL/Git/object stores, provider execution pipeline, structured
logging, and deterministic test practices.

Read the guide for the layer you are changing, then trace the contract across
route -> SDK handler -> auth manager/executor -> provider client/translator ->
response/logging. Keep the canonical thinking representation and existing
provider boundaries intact.

---

## Pre-Development Checklist

Before writing or reviewing backend code:

- [ ] Read `AGENTS.md`, this index, the guides that govern the affected layer,
      and [Quality Guidelines](./quality-guidelines.md).
- [ ] Identify the owning package and trace the real request/data path through
      every affected API, SDK, executor, translator, store, watcher, and
      registry boundary.
- [ ] Search for mirrored provider keys, model catalogs, config fields,
      templates, and protocol-specific consumers before changing a shared
      value or contract.
- [ ] Confirm that the change preserves the canonical thinking pipeline,
      downstream-facing `sdk/` versus repository-only `internal/` boundary,
      and the timeout and translator restrictions in `AGENTS.md`.

---

## Guidelines Index

| Guide | Description | Status |
|-------|-------------|--------|
| [Directory Structure](./directory-structure.md) | Module organization, package boundaries, and file layout | Complete |
| [Database Guidelines](./database-guidelines.md) | Store interfaces, SQL/file persistence, schema, and transactions | Complete |
| [Error Handling](./error-handling.md) | Error types, propagation, classification, and API responses | Complete |
| [Quality Guidelines](./quality-guidelines.md) | Forbidden/required patterns, testing, and review checks | Complete |
| [Logging Guidelines](./logging-guidelines.md) | Logrus levels, structured fields, request capture, and redaction | Complete |
| [拼车模块开发规范](./carpool-guidelines.md) | 用户、车辆、SQLite、凭据隔离、用量与前端契约 | 完成 |
| [拼车同步记账规范](./carpool-accounting-guidelines.md) | 请求侧同步观察、失败门禁、重试与关停契约 | 完成 |
| [拼车明细与清理确认规范](./carpool-retention-guidelines.md) | 固定范围分页、确认引用、重复确认、策略读取及真实事件展开 | 完成 |
| [拼车额度与并发排队规范](./carpool-quota-concurrency-guidelines.md) | 账号同步窗口、可靠短周期账本、双维并发与有界队列 | 完成 |
| [服务生命周期规范](./lifecycle-guidelines.md) | 关停顺序、超时预算与回归测试 | 完成 |

---

## Quality Check

Before declaring backend work complete:

- [ ] Follow [Quality Guidelines](./quality-guidelines.md); after Go changes,
      run `gofmt -w .`, `go test ./...`, and the required clean server build.
- [ ] Verify affected routes and protocols end to end, including error status,
      stream termination, cancellation, retry/cooldown, and cleanup behavior.
- [ ] Verify logs and persisted data cannot expose credentials, tokens, DSNs,
      authorization headers, or request bodies through ordinary diagnostics.
- [ ] Confirm tests are deterministic and cover the regression and relevant
      failure paths; update config examples and these specs when a public or
      repository-wide contract changes.
- [ ] 对延迟执行的关停路径，确认超时 context 在真正开始关停时创建，而不是在服务
      启动时预创建；至少覆盖未来 deadline 与长时间运行后的真实关停。

---

## How to Use These Guidelines

- Documented examples point to real files; follow the owning package rather than
  copying a nearby implementation into a new generic utility.
- Requirements in AGENTS.md remain authoritative for commands, timeout limits,
  translator permissions, language, and cleanup. These guides add repository
  detail; they do not loosen those rules.
- When a feature crosses API, execution, storage, or logging boundaries, map the
  data/error flow and update every affected consumer and test.
- Treat persisted auth/config data and request bodies as sensitive even when a
  diagnostic or request-log mode is enabled.

---

**Language**: Existing documents in this directory may still be in English,
but all new documentation and newly authored sections for this fork must be
written in Simplified Chinese. Code identifiers and executable examples remain
unchanged.
