# 拼车本地额度与并发排队规范

## 1. 适用范围 / 触发条件

修改拼车成员 5h / 7d USD 额度、可靠费用记账、授权准入、成员额度 DTO/表单、额度重置或用户/账号并发排队时读取。

当前有效用户额度只有 **5 小时**与 **7 天**两个本地窗口。`monthly_limit_nano_usd`、`billing_periods`、旧上游 `quota_windows` 等结构仅为历史数据库兼容、历史查询或迁移取证保留，不得重新进入当前准入、成员概览、成员用量或额度设置路径。

边界不变：额度按当前 `membership_id` 归属，因此同一成员的全部 Carpool API Key 共享额度；并发仍按稳定 `UserID` 与 `AuthID` 分别限制；账号原生 CPA 配额只展示，不替代本地 USD 额度。

## 2. 签名与数据库结构

```go
(*Store).GetMemberQuotaLimits(context.Context, string) (domain.MemberQuotaLimits, error)
(*Store).SetMemberLimits(context.Context, string, domain.MemberLimitsUpdate, *domain.AuditEvent) error
(*Store).MemberQuotaWindows(context.Context, string, time.Time) ([]domain.MemberQuotaWindow, error)
(*Store).RecordUsageEventBilled(context.Context, domain.UsageEvent, string, *int64, string, string) (domain.UsageEvent, error)
(*Store).PreviewRetentionJob(context.Context, string, string, time.Time) (domain.RetentionJob, error)
(*Store).ConfirmRetentionJob(context.Context, string, string, string) (domain.RetentionJob, error)
(*Control).PassengerQuota(context.Context, domain.User) (domain.MemberQuotaLimits, []domain.MemberQuotaWindow, error)
(*Control).SetMemberLimits(context.Context, domain.User, string, string, domain.MemberLimitsUpdate) (domain.Membership, domain.MemberQuotaLimits, error)
(*Control).CloseAdmission()
runtime.NewConcurrencyLimiter(int) (*runtime.ConcurrencyLimiter, error)
(*runtime.ConcurrencyLimiter).Acquire(context.Context, string, string) (*runtime.ConcurrencyPermit, error)
(*runtime.ConcurrencyPermit).Release()
```

- `memberships.five_hour_limit_nano_usd`、`memberships.weekly_limit_nano_usd`：非负整数；`0` 表示不限。
- `member_quota_windows`：以 `(membership_id, kind)` 为主键，`kind` 只能是 `5h` / `7d`，保存窗口起点、重置时间、已确认 nano-USD、未知费用计数、revision 与更新时间。
- `billed_event_receipts`：可靠费用事件的永久去重凭据；必须先于会被明细保留任务删除的数据判断重放。
- `quota_request_fees` 与旧 `quota_windows`：只用于旧数据库升级时回填仍有效的可靠费用，不再作为当前窗口事实来源。
- `monthly_limit_nano_usd` 与 `billing_periods`：保留历史兼容，不物理删列，不由当前成员/额度路由创建或更新。
- 已发布 migration 不可修改；新结构必须使用递增 migration，并同步 checksum、升级测试与旧库回填。

## 3. 请求、响应与行为合同

### 3.1 管理接口

创建成员请求：

```json
{
  "user_ref": "usr_xxx",
  "display_name": "成员",
  "five_hour_limit_usd": "10",
  "weekly_limit_usd": "50"
}
```

成员额度 PATCH 可部分更新：

```json
{
  "five_hour_limit_usd": "0",
  "weekly_limit_usd": "50.000000001",
  "concurrency_limit": 2
}
```

- 金额字段使用十进制 USD 字符串，最多 9 位小数；`"0"` 是唯一不限值。
- PATCH 省略字段保持原值；金额 `null`、空字符串、负数、指数、小数超长或溢出必须拒绝。
- `concurrency_limit` 为正整数；`null` 表示不限；`0`、负数、小数和超出安全范围拒绝。
- 旧客户端多传 `monthly_limit_usd` 时允许解码但必须忽略；响应不得返回该字段，也不得写回遗留月限额。

当前响应只输出：

```json
{
  "five_hour_limit_usd": "10",
  "weekly_limit_usd": "50",
  "concurrency_limit": 2,
  "quota_windows": [
    {
      "kind": "5h",
      "status": "active",
      "limit_usd": "10",
      "used_usd": "2.5",
      "remaining_usd": "7.5",
      "overage_usd": null,
      "period_from": "...",
      "reset_at": "...",
      "unknown_cost_events": 0,
      "data_complete": true
    }
  ]
}
```

状态只能按以下含义投影：

- `unlimited`：限额为 0；可显示已确认用量，但剩余为不限。
- `not_started`：尚无可靠费用建立窗口；已用为 0，`period_from` / `reset_at` 为 null，剩余等于完整正额度。
- `active`：窗口有效且已用小于限额。
- `exhausted`：已用等于限额。
- `overage`：已用大于限额；剩余显示 0，另给超额金额。

不得返回或展示 `pending_sync`，也不得把结构体零时间伪装成真实重置时间。

管理端与乘客端的成员/用户表格中，“个人额度”单元格只允许按顺序显示两行：`5h` 进度条与百分比、`7d` 进度条与百分比。不得在该单元格重复显示金额、限额、状态、剩余、超额、重置时间或费用警告。不限、未开始、无当前成员关系等没有有效分母的状态使用空轨道与 `—`，不能伪造 `0%`；概览页额度卡片不受此紧凑表格合同影响。

### 3.2 窗口与费用记账

- 5h：首笔可靠费用的请求开始时刻作为起点，重置时间为 `+5h`。
- 7d：首笔可靠费用请求在报表时区的自然日 00:00 作为起点，使用 `AddDate(0, 0, 7)` 得到重置时间，不能用固定 `7*24h` 破坏 DST 日历边界。
- 未知费用事件不创建、不重启已过期窗口；若其请求开始时刻属于当前有效窗口，可增加该窗口的未知计数。
- 已知且可靠的零费用仍是可靠费用，可以建立窗口。
- 同一事件在一个事务中写 usage event、永久 receipt、请求账单金额及两个窗口；任一步失败必须全部回滚。
- 同一 `event_id` 重放不得重复增加请求金额或任一窗口。
- 已开始请求允许最后一笔使金额超额；后续授权在上游调用前拒绝。
- 迟到费用按请求开始时刻归属：与当前窗口重叠的更早费用可向前修正窗口起点；完全早于当前窗口且不重叠的历史费用不能覆盖较新的窗口。
- 读取时已过期窗口投影为未开始/已用 0；下一笔可靠费用才持久化新窗口。

### 3.3 准入、队列与重置

- 非 billable 请求不消耗 USD 额度。
- 准入与排队出队复核都读取本地 5h / 7d；正额度且 `used >= limit` 时分别返回 `five_hour_quota_exhausted` / `weekly_quota_exhausted`。
- 当前路径不得同步上游窗口、创建月账期或用月额度兜底。
- 用户所有 Key 共用 `UserID` 并发，账号所有拼车用户共用 `AuthID` 并发；等待不持有数据库事务或任一执行名额。
- `reset_quota_windows` 必须删除选中的 5h / 7d 窗口行，包括零费用已启动窗口；不修改限额、并发、请求历史金额、receipt 或账号原生配额。后续首笔可靠费用重新开始窗口。

## 4. 校验与错误矩阵

| 条件 | 对外结果 / 存储行为 |
| --- | --- |
| 5h 或 7d 为 `"0"` | 不限；不因该窗口拒绝 |
| 创建成员缺少 5h 或 7d | 400 参数错误，不部分创建 |
| 金额为负数、空、null、指数、超过 9 位小数或溢出 | 400 参数错误，不部分写入 |
| 旧请求包含 `monthly_limit_usd` | 忽略；遗留列不变；响应不回显 |
| 正额度且有效已用 `>= limit` | 403，稳定额度错误码；上游零调用 |
| 窗口已过期且无新可靠费用 | 投影 `not_started`，不伪造 reset |
| 未知费用为首笔或发生在过期后 | 不创建/不重启窗口 |
| receipt 重放 | 返回既有结果，不重复扣费 |
| 额度窗口更新失败 | usage、receipt、请求金额和窗口全部回滚 |
| 重置预览后目标 revision/金额变化 | 确认失败，不部分重置 |
| 并发不足、队列未满 | 等待且上游零调用 |
| 等待队列已满 | 429 `concurrency_queue_full` |
| 关停停止准入 | 等待者唤醒，`concurrency_closed` |
| 同步记账不可用 | 503 `accounting_unavailable` |

## 5. 正常、基础与错误场景

- 正常：首笔已知费用同时建立 5h 与 7d，本次账单只增加一次；后续同窗口费用同时累加，达额后的下一请求被拒绝。
- 基础：两项限额都是 0，窗口仍可记录已确认用量，但界面显示“不限”；同一用户的多个 Key 共享窗口和并发。
- 正常：管理员只修改 7d，5h 与并发保持原值；旧客户端附带月字段也不能改变月列或当前逻辑。
- 正常：显式重置删除两个窗口，历史请求金额与 receipt 保留；下一笔可靠费用创建新起点。
- 错误：将 `0` 解释为“零额度”并立即拒绝。
- 错误：未知费用启动窗口，或页面刷新时按当前时间伪造窗口。
- 错误：先创建月账期再丢弃响应中的月字段；即使 UI 不显示，这仍是活跃月逻辑。
- 错误：只清空非零窗口，导致零费用启动的窗口重置时间仍保留。
- 错误：用客户端 Key 作为额度/并发归属，允许换 Key 绕过限制。

## 6. 必须测试的断言

- 金额解析：0、9 位小数、负数、空、null、指数与溢出；PATCH 独立字段更新；遗留月字段忽略且旧列不变。
- API：创建成员无需月字段；当前成员、乘客概览与成员用量响应没有 `monthly_limit_usd` / `billing` / `pending_sync`。
- UI：概览标题“7 天剩余额度”；不限、未开始、正常、等额用尽、超额与重置时间；成员行恰好两个窗口并保留独立并发列；恶意金额/状态不能注入 HTML。
- 存储：首笔费用建窗、5h 边界、7d 日历边界与 DST、过期投影、下一笔重启、零费用、未知费用、迟到重叠费用、完全过期迟到费用。
- 原子与幂等：重复 `event_id` 只累计一次；窗口写失败时 usage/receipt/请求金额全部回滚；一个费用不能因两个窗口变成双倍账单。
- 迁移：旧库升级保留遗留月字段、现有 5h/7d 设置、仍有效的可靠费用、未知计数与去重凭据。
- 重置：零计数窗口也进入预览；确认同时删除 5h/7d；目标变化时拒绝；历史请求金额不变。
- 队列：排队前和出队后都复核额度；同用户多 Key、达额等待者、取消、关停、队满及名额释放无泄漏。
- 使用注入时钟、明确时间戳或同步原语，不使用墙钟 `Sleep` 判定 TTL、过期或顺序。

## 7. 错误与正确示例

错误：未知费用启动本地窗口。

```go
if noWindow {
    createWindow(startedAt, nil) // 未知费用伪造了计时起点
}
```

正确：只有可靠费用建立窗口；未知费用只在已存在且匹配的窗口内增加未知计数。

```go
if noWindow && cost == nil {
    return nil
}
if noWindow {
    return createWindow(startedAt, cost)
}
```

错误：7d 固定增加 168 小时。

```go
resetAt := localMidnight.Add(7 * 24 * time.Hour)
```

正确：保持报表时区的七个自然日边界。

```go
resetAt := localMidnight.AddDate(0, 0, 7)
```

错误：当前接口不返回月字段，但成员列表内部仍调用 `EnsureBillingPeriod`。

正确：当前成员与乘客路径只读取 `MemberQuotaWindows`；月表仅留给明确标记的历史兼容查询与迁移测试。
