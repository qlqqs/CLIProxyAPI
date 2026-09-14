# 保留设置验收映射（2026-09-14）

## 本轮最小契约与修改边界

- `GET/PATCH /admin/retention` 的 `default_days` 必须反映服务器实际配置；数据库覆盖
  为 `null` 时，写入响应的 `effective_days` 与随后的读取相同，不能暂时伪报 90。
- 数据库只存可空覆盖，页面允许 `90/180/365/0/null`；合法自定义配置仍用于回退。
- 已确认根因：`SetRetentionOverride` 没有配置参数，返回 `GetRetentionSettings(ctx, 90)`；
  `Control.UpdateRetentionSettings` 直接返回该结果。最小修复只改 Control 的这个方法，
  成功写入后复用带实际配置的 `RetentionSettings`，不改共享 repository 签名。
- 新增隔离 SQLite 重启测试与清理策略测试；随后获准仅扩展 `retention.go` 安全修复，
  不改作业、迁移、计费、API handler 或前端。

## 已核实契约

- `GetRetentionSettings` 每次读取数据库，不缓存覆盖；`null` 不会写死配置默认值。
- `retentionCleaner.cleanupPass` 每批次读取有效设置；单轮 `now` 固定，审计截止时间独立来自 `auditRetention`。
- 永久模式使用最小 `int64` 微秒作为截止值，SQL 的 `< cutoff` 不可能匹配任何已保存时间戳；
  不依赖“所有记录晚于 Unix epoch”的未声明假设，也不改变 schema 或清理 repository。
- 设置写入与成功审计在同一事务；非法输入及审计失败的回滚已用真实重启验证。

## 已保留的产品边界

- 父设计第 7 节写有“配置支持显式 0 永久”，但当前配置校验要求至少 30 天，
  `NewControl` 要求正 duration，`GetRetentionSettings` 将非正默认值归一为 90。
  本次授权明确维持现有配置 `>=30` 与页面数据库覆盖 `0` 的区别，不扩展配置解析/校验。
- worker 默认轮次间隔 24 小时；每批次重新读取配置，保存不会主动触发一轮清理。
  批次间改为永久会阻止后续批次删除明细，但不会取消已经开始的事务，也不提供读设置与
  清理事务之间的全局锁；“即时生效”不等同于取消当前事务。
- 支持数据库设置的 store 一旦读取失败，记录固定 `reason_code=retention_settings_unavailable`
  并停止剩余轮次；不输出原始错误，不回退配置执行删除。此时审计清理也延后至下次成功轮次。
  不支持设置能力的旧 store 仍保持配置策略，未引入回调、缓存、超时或恢复体系。

## 新增验收证据

| 验收项 | 测试与实际断言 |
| --- | --- |
| 自定义默认值与更新响应一致 | `TestRetentionSettingsPersistenceAndConfiguredResponse` 用 90、47、400 天配置逐一切换 `90/180/365/0/null`，同时检查 `DefaultDays/EffectiveDays/Source/OverrideDays` |
| 真正重启持久化 | 每次更新都关闭 SQLite，再从同一路径重新 `Open` 并重建 Control；不是重复查询同一个连接 |
| 恢复配置而非写死默认值 | 重开后再用 `configDays+17` 构造 Control；覆盖存在时维持覆盖，`null` 时跟随新的配置值 |
| 审计提交独立可查 | 每次设置更新恰有一条成功审计，关闭重开仍存在；读取设置不会新增审计 |
| 非法值不写入 | `TestRetentionSettingsRejectInvalidAndRollbackAuditFailure` 拒绝 `-1/30/47/91/366` 页面覆盖；合法自定义配置不等于合法页面覆盖 |
| 设置与审计原子性 | 先写永久覆盖，再以无效审计尝试恢复默认；重开后仍是永久，且无半条成功审计 |
| 默认 90 | 未覆盖且 `GetRetentionSettings(ctx, 0)` 归一到 90；标准 90 配置在重启矩阵中覆盖 |
| 下一轮立即使用数据库新策略 | `TestRetentionCleanerReloadsSQLitePolicyAndKeepsAuditIndependent` 使用同一 cleaner、真实 SQLite、固定时钟，依次切换 `365/180/90/0/null`，配置回退为 47 天 |
| 永久不误停审计清理 | 永久轮次保留 1960 年（epoch 以前）的请求及事件；180 天以前审计仍删除，恰好 180 天边界审计保留 |
| 保留截止精度 | 请求、事件均检查 cutoff 前 1 微秒删除，边界及后 1 微秒保留；无 `time.Sleep` |

修复前定向运行在配置 47、400 的首个更新响应上失败，实际 `DefaultDays=90`；修复后同组测试通过。
本轮没有修改 `retention_settings.go`：其旧写入方法没有配置上下文，真实配置解释留在持有配置的 Control。
新增测试均使用 `t.TempDir()` 和虚构身份/摘要，不接触生产库或真实上游。

## 校验记录

已通过：

```bash
go test ./internal/carpool/store/sqlite ./internal/carpool -run 'TestRetentionSettings|TestRetentionCleanerReloadsSQLitePolicy' -count=1
go test ./internal/carpool/... ./internal/config
go test -race ./internal/carpool/...
```

已对本代理修改的 Go 文件运行 `gofmt -w`，`git diff --check` 通过。
`go build -o "$build_dir/test-output" ./cmd/server` 通过；`build_dir` 为 `mktemp -d` 创建的
隔离 `/tmp` 目录，完成后只删除本次产物。没有对其他代理正在修改的文件执行全仓格式化。

首次 `go test ./...` 遇到并行实施中的 `service/usage_requests_test.go` 对尚未存在
`Page.Period` 字段的引用，服务包测试编译失败；该文件不属本代理，未修改。
第二次全仓测试遇到另一组并行实施的接口过渡错误：`service/retention_test.go` 仍使用旧的
`PreviewRetention` 返回值与 `RunRetention` 参数。需主会话待相关实施完成后重跑，
不能将已通过的定向测试、race 或 server build 等同于最终全仓验收。

## 扩展授权后的安全回归

- `TestRetentionCleanerSettingsReadFailureSkipsRemainingBatches` 分别注入首批读取失败、
  首批成功后第二批读取失败；失败后的 `CleanupRetention` 调用数为零，不能使用默认策略。
- `TestRetentionCleanerRechecksPermanentPolicyBetweenBatchesWithFrozenClock` 用确定顺序的读取
  结果模拟 90 天→永久，证明第二批改用不可匹配截止值；只读取一次时钟，审计截止保持不变。
- 真实 SQLite 永久样本改为 1960 年。修复前确实被 epoch 策略删除；修复后请求及事件均保留。
  这证明原 epoch 截止值不能覆盖 repository 实际接受的全部时间戳。
- 上述三个回归在旧实现上均失败，新实现定向运行通过；扩展后不再与其他代理竞争跑全仓测试。

扩展修复后通过：

```bash
go test ./internal/carpool ./internal/carpool/store/sqlite -run 'TestRetentionCleaner|TestRetentionSettings' -count=1
go test -race ./internal/carpool ./internal/carpool/store/sqlite -run 'TestRetentionCleaner|TestRetentionSettings' -count=1
go test ./internal/carpool -run 'TestRetentionCleaner' -count=3
```

扩展后的 server build 亦通过，仍使用独占 `/tmp` 输出目录并删除自己的产物。
