# 提交计划（待用户确认）

工作区变更均为本任务主会话及受委派代理实施产物，无未识别改动。核心与 UI 共享 API 契约，放在同一功能提交，避免提交中间状态无法编译或展示失配。不得推送或部署。

## 1. `feat(carpool): add aligned user quotas and queued concurrency limits`

- `config.example.yaml`
- `internal/api/server_middleware.go`
- `internal/carpool/accounting/writer.go`
- `internal/carpool/browser_fixture_test.go`
- `internal/carpool/domain/quota.go`
- `internal/carpool/domain/types.go`
- `internal/carpool/httpapi/api.go`
- `internal/carpool/httpapi/limits.go`
- `internal/carpool/httpapi/limits_test.go`
- `internal/carpool/httpapi/proxy.go`
- `internal/carpool/module.go`
- `internal/carpool/module_quota_checker_test.go`
- `internal/carpool/module_test.go`
- `internal/carpool/ops/backup_test.go`
- `internal/carpool/runtime/authorization.go`
- `internal/carpool/runtime/concurrency.go`
- `internal/carpool/runtime/concurrency_test.go`
- `internal/carpool/runtime/quota_projection.go`
- `internal/carpool/runtime/quota_projection_checker_test.go`
- `internal/carpool/service/control.go`
- `internal/carpool/service/quota.go`
- `internal/carpool/service/quota_checker_test.go`
- `internal/carpool/service/quota_test.go`
- `internal/carpool/service/quota_test_helpers_test.go`
- `internal/carpool/store/sqlite/authorization.go`
- `internal/carpool/store/sqlite/billing.go`
- `internal/carpool/store/sqlite/migrations.go`
- `internal/carpool/store/sqlite/migrations/004_quota.sql`
- `internal/carpool/store/sqlite/quota_limits.go`
- `internal/carpool/store/sqlite/quota_periods.go`
- `internal/carpool/store/sqlite/quota_test.go`
- `internal/carpool/store/sqlite/store_test.go`
- `internal/carpool/web/assets/app.css`
- `internal/carpool/web/assets/app.js`
- `internal/config/carpool.go`
- `internal/config/carpool_test.go`
- `internal/config/config_yaml.go`
- `internal/util/nocopy_invariant_test.go`
- `sdk/cliproxy/service_lifecycle.go`
- `test/carpool-progress.test.mjs`
- `test/carpool-redesign.test.mjs`

## 2. `docs(carpool): record quota contracts and verification evidence`

- `.trellis/spec/backend/carpool-accounting-guidelines.md`
- `.trellis/spec/backend/carpool-quota-concurrency-guidelines.md`
- `.trellis/spec/backend/carpool-retention-guidelines.md`
- `.trellis/spec/backend/index.md`
- `.trellis/tasks/09-15-user-quota-concurrency/check.jsonl`
- `.trellis/tasks/09-15-user-quota-concurrency/commit-plan.md`
- `.trellis/tasks/09-15-user-quota-concurrency/design.md`
- `.trellis/tasks/09-15-user-quota-concurrency/implement.jsonl`
- `.trellis/tasks/09-15-user-quota-concurrency/implement.md`
- `.trellis/tasks/09-15-user-quota-concurrency/prd.md`
- `.trellis/tasks/09-15-user-quota-concurrency/research/browser-results.json`
- `.trellis/tasks/09-15-user-quota-concurrency/research/final-verification.md`
- `.trellis/tasks/09-15-user-quota-concurrency/research/integration-evidence.md`
- `.trellis/tasks/09-15-user-quota-concurrency/research/queue-results.json`
- `.trellis/tasks/09-15-user-quota-concurrency/research/screenshots/desktop-pending.png`
- `.trellis/tasks/09-15-user-quota-concurrency/research/screenshots/mobile-edit-limits.png`
- `.trellis/tasks/09-15-user-quota-concurrency/research/screenshots/mobile-passenger-overage.png`
- `.trellis/tasks/09-15-user-quota-concurrency/task.json`

## 确认后的收尾

两个工作提交完成后，按 Trellis 流程仅归档本任务并记录会话；不归档其他历史任务，不执行 push。当前未执行提交。
