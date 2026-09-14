# 清理确认引用与原子执行证据

## 2026-09-14 实施边界

原链路在每次 `confirm:true` 时重新选择并执行重置，重试可能抹去第一次确认后新增消费；
删除、作业完成和审计也没有同一提交边界，`FinishRetentionJob` 错误被忽略。
本次只替换确认作业链路，不修改价格、同步记账、自动清理或设置读写契约，不新增迁移、
租约、outbox、后台续跑或恢复系统。

## HTTP 握手

1. `POST /carpool/api/v1/admin/retention/preview`，正文 `{"operation":"reset_current_period"}`。
   返回 `job_id`、`status:"queued"`、`expected_count`、`in_flight_count`、`expires_at`、
   `batch_limit:1000` 与 `confirmation_required:true`；计数是本批，不是所有可清理数据。
2. `POST /carpool/api/v1/admin/retention/jobs`，正文包含相同的 `operation`、`job_id` 与
   `confirm:true`。缺确认或 ID 返回 `422 confirmation_required`，不保留无引用入口。
3. 首次成功与已完成引用重试都返回 `202`，并提供相同持久化结果。相同引用永远不再次执行。
   `GET /carpool/api/v1/admin/retention/jobs/:job_id` 返回相同结果，仅原管理员可读。
4. 到期、跨管理员、动作不匹配、目标变化、跨期或旧无绑定记录返回
   `409 retention_preview_invalid`，需要重新预览。数据库错误沿原安全错误映射返回，
   不把执行错误伪装成作业成功。

前端不能提交 `confirmation`、目标 ID 集或版本；这些仅由服务端生成并保存在原表。

## SQLite 提交边界

- `PreviewRetentionJob` 在事务中按确定顺序选最多 1000 个逻辑请求或账期。
  `retention_jobs.confirmation` 保存格式版本、cutoff、目标 ID 与观察状态；管理员、动作和
  创建时刻使用同一行已有字段，预览从创建起最多有效 15 分钟。
- 明细绑定完成时间、outcome 与事件数量，确认时重新验证；不会纳入预览之后新插入或
  后来完成的请求。保留 `in_progress/incomplete` 与相关事件。
- 账期绑定边界、revision、confirmed、baseline、unknown；新增消费、调额或其他重置
  导致预览冲突。本期确认还验证当前时刻仍处于预览账期。
- 关闭账期的确认重新检查受保护请求，再清除可空关联并删除汇总，不删请求或事件。
- 已归零且无未知累计的账期不进入重置预览：否则第 1001 个目标会被前 1000 个已归零
  目标反复遮挡，无法通过下一次显式预览处理剩余批次。此规则仅排除无操作对象。
- 原 DSN 已配置 `_txlock=immediate`。确认事务依次完成全体目标验证、完整逻辑对象变更、
  真实受影响行数检查、完成结果保存及成功审计。竞争确认等待后读取已完成结果。
- 任一 SQL、结果写入或审计失败均整批回滚：目标不变、作业仍为 `queued`、没有成功审计。
  问题解决且预览仍有效时可显式用同一引用重试，不自动重发。
- 重置审计记录各期 ID、revision、原 confirmed、原 baseline 与原 unknown；抵消金额可由
  原 confirmed 减原 baseline 核对。新 baseline 为原 confirmed，未知累计归零。
- 保留完整请求删除事务；单请求事件行数没有固定上限。1000 是逻辑对象上限，不承诺
  1000 条物理事件、固定执行时间或跨批自动完成。

## 测试覆盖

`store/sqlite/retention_jobs_test.go`：

- 同时两次确认只产生一次重置及一次审计，响应相同；已完成引用超过有效期仍只返回旧结果。
- 重置前事件重放不恢复旧费用；重置后新消费与未知事件不被重复确认抹去。
- 15 分钟精确边界、跨管理员/动作、缺引用记录、旧空 confirmation、revision、无 revision
  金额变化、未知数变化、边界变化、跨期与预览后新增费用均拒绝。
- 三种操作分别注入业务变更、结果写入、审计写入故障，证明全部回滚及同引用显式重试。
- 1201 个事件的一个逻辑请求整体删除，金额账本不变；新插入、后来完成和 incomplete
  请求均保留。
- 每种操作 1001 个目标分为 1000+1 两次预览确认；第三次预览为空。
- 关闭账期保护 incomplete 及新出现的保护状态，成功删除仅解关联，不删明细。
- 关闭并重新打开 SQLite 后，queued 确认和 completed 幂等结果仍有效。
- 明细 `< cutoff` 精确边界保持；不把账期操作减去明细保留天数。

`service/retention_test.go` 保留并适配原 90/180/365、本期金额、边界、限额与锚点回归。
`service/retention_jobs_test.go` 验证角色、缺 ID、跨管理员读取及数据库错误传播。
`httpapi/retention_jobs_test.go` 覆盖真实 SQLite 的 HTTP 握手、Origin/CSRF、缺确认、动作
不匹配、实际归零、重复结果、GET 结果与 revision 冲突。全部使用隔离数据库与固定时钟，
不使用 `Sleep`，不连接上游或生产数据。

## 检查命令

```bash
go test ./internal/carpool/store/sqlite ./internal/carpool/service ./internal/carpool/httpapi \
  -run 'TestConfirmedRetention|TestManualRetention|TestBillingRetention|TestUsageDetailsRetains' -count=1
go test -race ./internal/carpool/store/sqlite ./internal/carpool/service ./internal/carpool/httpapi \
  -run 'TestConfirmedRetention|TestManualRetention|TestBillingRetention|TestUsageDetailsRetains' -count=1
# 服务器编译输出到 mktemp 创建的独立目录；成功后仅删除本次生成的文件及空目录。
go build -o "$build_dir/test-output" ./cmd/server
```

不声称完成全仓测试或浏览器验收；这些由主会话统筹。本次只进行上述定向验证与编译。

本轮结果：上述三包定向普通测试通过（SQLite 4.165s、service 0.451s、HTTP 0.114s）；
定向 `-race` 通过（SQLite 56.995s、service 3.972s、HTTP 1.985s）。最后补充的精确 cutoff
测试另行通过普通及 `-race`（1.874s）。最终服务器编译、所改 Go 文件格式化及
`git diff --check` 通过。
