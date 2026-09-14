# 拼车请求明细与清理确认规范

## 1. 适用范围

修改管理端请求明细分页、事件展开、保留设置和清理确认时读取本规范。
本规范承接 `carpool-accounting-guidelines.md` 的价格冻结、同步记账和不完整事实保护。
只使用现有 SQLite 作业表和原子事务，不引入迁移、outbox、租约、跨进程续跑体系。

## 2. 接口与存储

```go
func (*Control) AdminUsageRequests(context.Context, domain.User, UsageRequestQuery) (UsageRequestPage, error)
func (*Control) PreviewRetention(context.Context, domain.User, string) (domain.RetentionJob, error)
func (*Control) RunRetention(context.Context, domain.User, string, string) (domain.RetentionJob, error)
func (*Store) PreviewRetentionJob(context.Context, string, string, time.Time) (domain.RetentionJob, error)
func (*Store) ConfirmRetentionJob(context.Context, string, string, string) (domain.RetentionJob, error)
```

`retention_jobs.confirmation` 保存服务器生成的目标/状态快照，不能接受客户端提交的目标
列表，不能从 HTTP 返回原始 confirmation。`job_id` 是引用，不替代管理员权限、同一
操作、原发起管理员、CSRF/Origin 检查。

## 3. 请求与响应契约

### 明细

- `GET /carpool/api/v1/admin/usage/requests` 每页固定 25 条；省略 limit 或传 25。
- `next_cursor` 固定首屏 `[from,to)`、规范化筛选指纹及末项时间/ID。翻页不重新计算
  today，也不允许用游标覆盖当前保留范围；旧版本、换筛选、坏 tuple 均拒绝。
- 响应 `period.from/to` 是实际查询范围，不是原样回显可为空的入参。
- 列表不包含全部事件。展开时调用 `/admin/usage/requests/:request_id`，显示加载状态，
  失败允许手动重试；不能把列表未提供 events 当作确实没有上游事件。
- 请求与实际模型 OR 匹配，其余筛选 AND 组合；某事件匹配模型/账号不意味着展开仅返回
  该事件。`cache_read_tokens`、`cache_write_tokens` 与原 `cached_tokens` 均为安全数字。
  零与 null 不同；缺维度显示上游未提供，不能 `Number(null)` 伪造零。
- `billing_status=unknown` 或仍有费用未知事件时，金额只能标为已确认小计/费用不完整。
  自动清理分批删除事件后，即使当前明细 unknown 计数降为零，持久计价状态仍有效。

### 清理

```json
{"operation":"reset_current_period"}
```

发送至 `POST /admin/retention/preview`，返回 `job_id`、`operation`、`status`、
`expected_count`、`in_flight_count`、`confirmation_required`、`expires_at`、`batch_limit`。
确认调用 `POST /admin/retention/jobs`：

```json
{"job_id":"<服务器返回的引用>","operation":"reset_current_period","confirm":true}
```

- 预览最多 15 分钟有效；每次最多 1000 个逻辑请求/账期。更多数据重新预览下一批，
  不自动连续重置或删除。当前期净费用/未知数均为零的账期不占重置批次，避免续批饥饿。
- 验证所有目标后，在同一 immediate SQLite 事务中执行整请求删除或期修改、作业结果、
  审计。任一步错误整批回滚，不能忽略 Finish 错误后声称成功。
- 已完成 job 再次确认返回原结果，即使之后有新费用也不得再次重置。并发确认同样如此。
  完成后的重放不受预览 TTL 影响，仍检查管理员与操作。
- `usage_details` 固定预览选中的已完成请求；之后完成或新插入的请求不混入该批。
  `closed_periods/reset_current_period` 使用当前时刻，不减明细保留天数。
- 期版本、金额或相关状态变化、跨期、过期预览返回冲突。所有路径保护
  `in_progress/incomplete`，但显式本期重置仍允许抵消此前已确认累计。
- 页面展示实际作业状态、实际删除/重置数量及保护数量。响应不确定时可用原 ID 查询，
  再由用户显式重试；不自动重发破坏性 POST。

### 保留设置与自动清理

- 数据库覆盖 `90/180/365/0/null`；`0` 永久，`null` 回到实际配置而非硬编码 90。
  当前配置本身仍要求至少 30 天，不声称已支持配置文件显式 0。
- 保存成功响应与随后 GET、重启后读取必须一致，保留设置与审计同事务。
- 自动 cleaner 每个批次重读有效策略，读取失败跳过，不猜用配置默认值删除数据。
  更新影响下一批，不取消已经开始的事务；不承诺保存按钮立即触发清理。
- 永久保留不能用 Unix epoch 代替“没有可删明细”，因为合法的 epoch 之前时间也会被删。
  当前实现使用 SQLite int64 微秒最小值作为严格小于条件的下界；审计保留独立执行。
- 新手工确认按整请求原子删除。既有自动 cleaner 仍按事件批处理，不能宣称也已经改成
  整请求算法；独立账期金额、未知累计和持久未知计价标记必须保留。

## 4. 校验与错误矩阵

| 条件 | 预期 |
| --- | --- |
| 明细页 limit 非 25、坏游标或换筛选 | 422，不执行错误查询 |
| 保留设置读取失败 | 明细查询报错；自动清理不调用删除 |
| 确认缺 job_id 或 confirm 非 true | 422 `confirmation_required` |
| 已过期、跨期、状态变化、操作/发起者不符 | 409 `retention_preview_invalid` |
| 非管理员 | 403；不能凭 job_id 绕过权限 |
| 首次有效确认 | 202，completed，返回实际计数 |
| 已完成引用重放 | 同一作业同一结果，不追加审计或再次影响费用 |
| 数据/作业结果/审计任一写入失败 | 全事务回滚，不伪报成功 |

## 5. 正常、基础与错误案例

正常：预览净费用 $0.225 → 确认重置 → 新消费 $0.045 → 重复确认原 ID → 仍为 $0.045。
基础：零用量重置预览 expected_count=0；不会为了凑满一批反复修改已归零账期。
错误：每次确认重新计算目标或创建全新作业；用户网络重试可能擦掉新的消费。

## 6. 必须有的测试

- 256 种筛选组合、同时间排序、多事件展开、跨午夜 25/25/5、插入后上界不变。
- 保留设置真实 Close/Open，配置 47/400 与页面覆盖矩阵，审计失败回滚。
- 三操作 1001 目标的两批推进；单请求 1201 事件仍整请求删除。
- 同一 ID 并发/重启后重放、过期/跨期/版本变化、管理员隔离、事务各阶段注入失败。
- 自动策略失败不删、批间切换生效、永久保留 1960 年数据但仍清理过期审计。
- 真实浏览器断言事件 DOM 有实际 Token/费用，不能只断言 details 已打开；清理前后数量
  必须实际减少，重置后新费用不会被重复确认清掉。

## 7. 错误与正确写法

错误：`RunRetention(ctx, actor, operation)` 每次重新计算范围并重置。
正确：`RunRetention(ctx, actor, operation, jobID)` 验证服务器保存的预览，原子提交并重放结果。

错误：`item.events` 未出现就显示“没有上游事件”。
正确：区分未加载、加载失败、真实空数组；成功明细响应才用于渲染事件。
