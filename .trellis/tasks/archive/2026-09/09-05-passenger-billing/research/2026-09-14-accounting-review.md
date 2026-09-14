# 2026-09-14 同步记账恢复实施记录

## 授权和范围

用户明确同意恢复现有 `09-05-passenger-billing`，不新建任务。本轮修复请求侧同步
用量持久化、失败门禁和确定性测试；不提交或归档。现有未提交 UI、金额与保留功能均保留。

## 根因与防回归

- 根因属于跨层链路遗漏：把插件回调内部同步写库误当成整个 CPA 发布链路同步。
  SDK 发布先入队，旧测试直接调用插件，绕过了真正的异步边界。
- 写入失败只记录日志/计数，没有与后续授权联动，无法保证金额确认失败后停止放行。
- 结构修复：可选请求级同步观察入口置于异步分发前；保留旧全局 Key 的异步行为。
- 验证修复：完整发布链路立即读取 SQLite，下一次授权检查门禁/限额，用同步屏障验证
  阻塞、并发与关停，不以等待队列排空代替同步断言。
- 规范已写入 `.trellis/spec/backend/carpool-accounting-guidelines.md`。

## 实现交接后的已执行检查

| 检查 | 结果 |
| --- | --- |
| 修改前 `go test ./...` | 通过，建立全量基线 |
| `gofmt -w .` | 已执行 |
| `go test ./...` | 通过 |
| `go vet ./...` | 通过 |
| 实现代理：拼车、usage、access、API、auth、handlers 定向与 race | 通过 |
| 正常服务器构建 | 通过，删除本轮测试二进制 |
| `CGO_ENABLED=0` 服务器构建 | 通过，删除本轮测试二进制 |
| `git diff --check` | 通过 |
| 独立检查代理 | 已完成，修复两项 P1，仍有三项 P1 阻断 |

日志路径为本机临时证据，不是仓库永久附件：

- `/tmp/carpool-billing-baseline-01a09e4b.log`
- `/tmp/carpool-billing-final-test-01a09e4b.log`
- `/tmp/carpool-billing-final-vet-01a09e4b.log`
- `/tmp/carpool-accounting-race.log`
- `/tmp/carpool-final-module-race.log`

## 已知限制与独立复核重点

本轮不是持久化 outbox；内存失败事件在进程崩溃后不能自动重放。启动时根据已有未完成
计费请求保守阻止新生成，需要人工核对实际账务，不能通过删除明细或修改状态直接声称
修复了缺失费用。当前没有自动核对/修复命令。

实现交接记录指出：旧版本未标记为非计费的模型查询、终态后迟到且写入失败的 usage、
显式管理清理对核对证据的影响，仍需独立检查其真实影响；不能用自动清理暂停代替完整
恢复与证据保护设计。

后续尚未实施：请求价格目录冻结、每次实际执行前缺价拦截、浏览器验收。本轮成功也不
代表父任务或金额子任务可以完成归档。


## 独立检查结果及主会话核实

已修复并加入回归：

- 模块仅接受服务端拼车授权快照中的 usage 和 completion，防止旧全局 Key 的请求
  因找不到拼车账单而污染门禁。
- 准备计价/同步持久化阶段 panic 时设置待核对状态，空重试不能恢复健康，继续传播异常。

尚未关闭的阻断项：

| 优先级 | 证据 | 影响和后续方案 |
| --- | --- | --- |
| P1 | `sdk/api/handlers/handlers_stream.go` 插件流 defer 先 `lifecycle.complete` 后发布 usage | 终态后的 usage 写入失败只留内存，重启查询只查 `incomplete`，可能恢复放行。需独立的生产者完成/账务结算持久化契约。 |
| P1 | `accounting/writer.go` 的 `pending` slice 不受 `QueueSize` 限制 | 故障队首阻塞时，仍允许的模型 GET completion 可持续积累。需有界背压或持久化缓冲，并区分非计费完成记录。 |
| P1 | `store/sqlite/retention_settings.go` 清理选择 `outcome <> 'in_progress'` | 可删除 `incomplete` 待核对事实，使重启失去门禁依据。需保护未结算事实或独立持久化核对状态。 |
| P2 | 启动核对仅有数据库事实扫描，无审计化恢复入口 | 旧模型查询可能误封锁；需明确升级分类及核对操作，不指导用户随意改库解锁。 |

主会话已核对以上生产代码，不把测试通过等同于可靠性验收通过。本轮质量门禁的测试/
构建项通过不抵消这些设计阻断。继续修复需回到本子任务设计，明确持久化结算标记、
容量策略和清理保护；不需要新建任务，但不能默默扩成数据库迁移与运维功能。

## 独立检查修复后的最终复跑

主会话再次执行 `gofmt -w .`、`go test ./...`、`go vet ./...`、正常和
`CGO_ENABLED=0` 服务构建、`git diff --check`，全部成功（命令链退出码 0）。
日志：`/tmp/carpool-billing-reviewed-test.log`、`/tmp/carpool-billing-reviewed-vet.log`。
独立检查代理修复后的拼车与 usage race 通过，日志为 `/tmp/carpool-check-race.log`。
测试输出二进制已删除；未提交、未归档，未进行真实服务浏览器或生产数据操作。
