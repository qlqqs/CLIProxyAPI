# 本轮独立收尾审查（2026-09-14）

## 范围与规范优先级

本报告仅针对 `09-05-admin-usage-retention` 本轮收尾，按 `design.md` 最后的
“本轮最小执行契约”审查；不恢复旧租约、outbox、迁移或复杂清理恢复设计。
未修改 `design-previews`、样式、计价算法、生产数据；未调用真实上游。

独立核对实际调用链：明细 HTTP → `Control.AdminUsageRequests` → SQLite；
预览/确认 HTTP → service → SQLite 事务 → 作业结果/审计；详情按需加载与保留操作 UI。
没有仅依据工作单完成声明判断通过。

## 发现与处理

### 已修复：请求费用不完整提示依赖可被清理的事件计数

- 位置：`internal/carpool/web/assets/app.js` 的 `requestDetailMarkup`。
- 证据：旧判断只看 `unknown_cost_events > 0`。真实 SQLite 自动清理一个事件后，
  请求可以同时具有 `billing_status=unknown`、已确认金额 `0.225`、剩余未知事件数 `0`；
  账期的金额和未知累计没有改变。旧 UI 却不再显示“已确认小计，费用不完整”。
- 复现：审查者新增 `TestCheckerAutomaticEventBatchPreservesUnknownBillingStatusAndLedger`
  验证真实存储状态；`test/carpool-retention-checker.test.mjs` 调用应用实际渲染函数，
  对 `0.225`、已知 `0`、`null` 三例先得到失败。
- 修复：经主会话转交原 UI 负责人，判断增加 `item.billing_status === "unknown"`。
  审查者不与活动负责人交叉修改同一产品文件。修复后三例通过；真正已计价的零不误标未知。

### 已复核：保留设置读取失败不能回退为另一可读范围

`AdminUsageRequests` 原有读取失败回退配置的路径已由查询负责人修复。
当前直接传播错误，回归测试断言 `ListUsageRequests` 不被调用。
自动 cleaner 同样在每批读取失败时停止剩余批次，不执行默认保留清理。

### 已复核：1000 项重置后必须能预览剩余项

后端负责人发现并修复重复选择已归零账期的问题。重置预览现在只选择净额非零或未知
累计非零的当前账期；三种操作均验证 `1001 → 首批1000 → 次批1 → 空批0`。
确认已完成作业仍先返回原结果，不受新筛选规则影响，不会重置后续消费。

## 独立增加的测试文件

- `internal/carpool/service/usage_requests_checker_test.go`：真实 SQLite，180 天首屏后改为
  90 天，旧游标必须拒绝；改为永久后可复用原固定时间范围，返回剩余一项。
- `internal/carpool/httpapi/usage_dto_checker_test.go`：请求/事件 DTO 金丝雀验证内部用户、
  成员、AuthID、assignment、scope、价格目录及存储确认内容不泄漏；列表不含事件数组，
  详情才展开；已知零、未知金额及缓存缺失不混淆。
- `internal/carpool/store/sqlite/retention_projection_checker_test.go`：旧自动事件批处理
  删除未知事件后仍保留请求 `unknown` 状态和已确认金额，账期所有累计保持。
- `test/carpool-retention-checker.test.mjs`：费用不完整提示的三项反例及已知零正例。

## 已检查的行为与证据

| 范围 | 实际核对与定向测试 |
| --- | --- |
| 逻辑请求计数和复合筛选 | SQL 使用 `EXISTS`，不把尝试数量变成请求数量；查询负责人 256 组合测试、请求或任一事件模型、公共引用解析、精确 ID、全事件展开均已重跑 |
| 分页 | 固定25；首屏 `[from,to)` 写入游标，按 `(started_at,request_id)` 排他倒序；跨午夜55条25/25/5、新插入、未来上界收窄、筛选变更、畸形游标、当前保留范围均有断言 |
| HTTP 与脱敏 | 默认和自定义返回实际 period，非法 limit/游标422；详情 DTO 不序列化完整内部对象；确认快照不对外返回 |
| 手工确认 | 服务端预览绑定管理员、动作、目标、状态和时间；15分钟到期、跨期、版本/金额/未知状态变更及不匹配均拒绝；真实 HTTP 缺引用/确认422、过期或冲突409，Origin/CSRF 门禁生效 |
| 原子性与重复提交 | immediate transaction 验证全部目标后变更，整请求子事件/主行、账期、作业结果、审计同事务；三类操作分别注入变更/作业结果/审计失败并验证回滚与同 ID 显式重试；并发确认、已完成后新增已知/未知消费及重复事件不再次清零 |
| 目标保护和续批 | 预览后新插入或后来完成的请求不纳入；`in_progress/incomplete` 及受保护账期不删；1201事件整请求删除；1000目标上限及下一批可推进；已结束账期删除保留请求/事件并解除引用 |
| 设置与自动清理 | 90/180/365/永久/null关闭重开、配置47天恢复响应、审计独立；每批重读，固定一次 pass 时钟；永久最小int64微秒截止保证1960年测试事件也不删除 |
| UI | 懒加载真实详情、手动重试、安全 Token 字段与零/缺失；筛选 hash 恢复；确认复用原 job_id、实际结果数、409重新预览、不明确失败只安全 GET 一次，不自动重发破坏操作 |

## 验证记录

已由审查者执行通过：

```bash
go vet ./internal/carpool/service ./internal/carpool/httpapi ./internal/carpool/store/sqlite ./internal/carpool
go test ./internal/carpool/service ./internal/carpool/httpapi ./internal/carpool/store/sqlite ./internal/carpool -run 'TestChecker|TestAdminUsageRequests|TestAdministratorUsageRequests|TestUsageRequests|TestUsageEventResponse|TestConfirmedRetention|Test.*Retention' -count=1
go test -race ./internal/carpool/service ./internal/carpool/httpapi ./internal/carpool/store/sqlite ./internal/carpool -run 'TestChecker|TestAdminUsageRequests|TestAdministratorUsageRequests|TestUsageRequests|TestUsageEventResponse|TestConfirmedRetention|Test.*Retention' -count=1
node --check internal/carpool/web/assets/app.js
node --test test/carpool-retention-checker.test.mjs test/carpool-retention.test.mjs
```

Node 定向组合当前为24项（UI工作单20项＋审查者4项），不是历史所有 JS 测试的总数。
`gofmt -l internal/carpool` 无输出，`git diff --check` 通过。
`go build -o <mktemp目录>/test-output ./cmd/server` 已成功并删除临时产物。
最后的重置续批和 UI 一行修复另外定向重跑；最终命令结果见本报告追加记录。

## 未修复问题与明确验收边界

本轮最小契约内没有尚未处理的已证实阻塞项。以下是主会话确认的范围边界，不按旧 PRD
自动标记已完成，也不因此增建恢复系统：

1. 整请求原子删除只承诺新的管理员确认作业。既有自动 `CleanupRetention` 仍按事件批次
   清理，过渡期详情事件数反映剩余事件；金额账本独立保护。本次修复费用未知提示，而非
   重写自动算法或保存新的请求级累计计数。
2. 游标不是签名或数据库事务快照；固定范围内的迟到回填、并行清理或状态变化仍可能影响
   实时 total。不得将跨午夜/新请求上界验证等同于不可变数据集保证。
3. 保留文件配置仍沿用现有合法自定义正天数；`0=永久` 是数据库管理端覆盖，未扩展配置
   文件对 `0` 的支持。旧聚合报表的其他历史行为不在本轮查询修复内。
4. 历史 PRD 的主行总 Token、安全 Key 名称、完整时间筛选 UI、全面布局重设计等没有因此
   获得全量通过声明；当前 Token 在展开事件中，主行展示安全 Key 引用。
5. 本审查不重复启动全仓测试或真实浏览器。主会话已报告首轮 Chromium 的30次合成消费、
   25+5分页、详情、筛选恢复、冲突、重置、重复确认保留新增费用、32项删除及移动端通过；
   最后补丁后的 Chromium 重跑及全仓 vet/test/build 由主会话记录，不冒充审查者亲自运行。
6. 主会话已新增 `.trellis/spec/backend/carpool-retention-guidelines.md` 并接入 backend
   index。审查者核对该规范与最终代码一致：固定游标、确认事务/续批、自动清理边界与
   持久未知状态展示均已同步。本报告不是全部父任务验收或自动提交授权。


## 最后补丁复验与交回

最终未知状态 UI 修复、重置续批筛选及 HTTP 冲突测试调整后，再次通过：

```bash
node --test test/carpool-retention-checker.test.mjs test/carpool-retention.test.mjs
go test ./internal/carpool/store/sqlite ./internal/carpool/httpapi ./internal/carpool/service -run 'TestChecker|TestConfirmedRetentionCapsEachOperation|TestConfirmedRetentionHTTP|TestAdminUsageRequestsRetentionReadFailure' -count=1
go test -race ./internal/carpool/store/sqlite ./internal/carpool/httpapi ./internal/carpool/service -run 'TestChecker|TestConfirmedRetentionCapsEachOperation|TestConfirmedRetentionHTTP|TestAdminUsageRequestsRetentionReadFailure' -count=1
go vet ./internal/carpool/store/sqlite ./internal/carpool/httpapi ./internal/carpool/service
```

另外重跑并通过新增加的持久作业 Close/Open、严格 cutoff 边界与 service 管理员/错误传播
测试：`TestConfirmedRetentionQueuedAndCompletedSurviveReopen`、
`TestConfirmedRetentionUsageCutoffExcludesBoundary`、
`TestConfirmedRetentionServiceRequiresAdminBoundJobAndPropagatesErrors`。

最终隔离目录 server build、`git diff --check`、`gofmt -l internal/carpool` 均通过。
审查至此完成，不再修改代码或其他工作单文件。无待修复的本轮已证实阻塞项；主会话继续
执行最后全仓门禁及重建后的真实浏览器验收，并保留上文的历史范围边界。
