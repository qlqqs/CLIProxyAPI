# 拼车多周期额度与并发排队规范

## 1. 适用范围

修改拼车短周期额度、并发策略、授权、请求收尾、费用保留或相关 DTO/表单时读取。本功能面向单实例、一车一账号；不修改 Home、上游原生额度或未启用拼车的请求。沿用现有月账期、冻结价格和同步记账失败门禁。

## 2. 签名与持久化

```go
(*Store).GetMemberQuotaLimits(context.Context, string) (domain.MemberQuotaLimits, error)
(*Store).SetMemberLimits(context.Context, string, domain.MemberLimitsUpdate, *domain.AuditEvent) error
(*Store).GetUserConcurrency(context.Context, string) (*int, error)
(*Store).GetAccountConcurrency(context.Context, string) (*int, error)
(*Store).SetAccountConcurrency(context.Context, string, *int, *domain.AuditEvent) error
(*Store).ObserveQuotaWindows(context.Context, []domain.QuotaWindowObservation) error
(*Store).MemberQuotaWindows(context.Context, string, string, time.Time) ([]domain.MemberQuotaWindow, error)
(*Control).CloseAdmission()
runtime.NewConcurrencyLimiter(int) (*runtime.ConcurrencyLimiter, error)
(*runtime.ConcurrencyLimiter).Acquire(context.Context, string, string) (*runtime.ConcurrencyPermit, error)
(*runtime.ConcurrencyPermit).Release()
```

- `004_quota.sql` 新增成员 5h/7d 配置、稳定 UserID/AuthID 并发策略、`quota_windows`、`quota_request_fees` 和 `billed_event_receipts`。旧迁移不可修改。
- `ProxyAuthorization.CheckOnly` 只验证并回滚，不创建请求/审计，连懒创建月账期也不提交；`ExpectedMembershipID` / `ExpectedAuthID` 为执行时围栏。
- 成员 PATCH：`/admin/cars/:car_ref/members/:member_ref/quota` 的 `monthly_limit_usd`、`five_hour_limit_usd`、`weekly_limit_usd`、`concurrency_limit`；账号 PATCH：`/admin/cars/:car_ref/accounts/:account_ref/concurrency`。
- `carpool.concurrency-queue-capacity` 默认 **10**，整个模块共用，不包含在途请求。配置解析须拒绝小数、零、负数，不接受 YAML 隐式截断；同步默认值省略规则。

## 3. 行为契约

### 额度与真实账号周期

- 金额以 nano-USD 保存、十进制字符串交互。新增字段省略保持原值、null 取消限制；金额零为零额度，并发只接受正整数或不限。原月额度不能通过 HTTP null 取消。
- 5h/7d 同车跟随唯一账号，月账期不变。Claude 使用明确窗口类型，Codex 用明确分钟数识别；缺失依据不能将任意主次窗口强行当作 5h/7d。
- Codex 同时有绝对和相对 reset 时绝对值优先，不能由 map 遍历顺序决定。相对值基于观测时刻，不基于读取页面的时刻。
- 已观测、尚未结束的窗口继续有效；首次未知、过期没有下一窗口、无法识别时 `PendingSync`，只跳过对应短周期，不跳过月额度、并发或记账失败门禁。用户已接受此保护空档。
- 待同步 DTO 的已用/剩余/重置为 null，不把结构体零值展示为真实零；前端明确“周期待同步”。覆盖起点晚于窗口起点时说明统计不完整。
- 重复/乱序观测不能重置费用；边界修正保留窗口身份和已归属费用。不能仅以精确 reset timestamp 变化触发清零，不能用本地无限递推伪造账号未来周期。
- 费用使用实际执行准入时的请求时间/价格。长请求结算晚于 reset 仍计入冻结窗口；待同步期间费用保留，获取窗口后补归属，不改价格、不补造旧未计价费用。

### 账本、清理与去重

- 一个事件在同一事务更新请求/月累计/短周期可靠费用；多个周期校验不能把实际账单金额叠加三份。
- 仅对独立可靠的 `quota_request_fees` 求和，不以普通请求明细作为额度唯一来源。保守保留这些窄记录，不将明细清理转换成额度重置。
- `billed_event_receipts` 在请求/账期/scope 查询之前判断重放；即使明细与已结束月账期已删除，也不能重新扣款或用新价格计算。
- 原“重置本期”仍只重置月账期，UI 明确其范围。不要顺带清空短周期。

### 队列与生命周期

- 用户所有 Key 共用 UserID 限制，账号所有拼车用户共用 AuthID 限制；不按临时分配 ID 拆分。
- 用户与账号名额原子取得；等待不占用任何一类执行名额、不持数据库事务、不启动上游。就绪者按先后顺序放行，跳过因自己用户额度而阻塞的条目。
- 只有需要等待且队列已满时返回队满 429。等待可被客户端取消或关停终止，不新增网络/排队 timeout 或 Sleep 轮询。
- 调高/取消并发策略唤醒等待者，调低不终止已开始请求。持久化策略更新与内存发布须避免旧读取覆盖新值。
- 出队复查用户、Key、成员/账号、周期金额及 writer 健康。实际开始时间与价格在出队时冻结，排队前的成功预检不能授权过期请求。
- 对瞬时存储错误或取消，不能使用空拒绝理由和 `WithoutCancel` 再做可成功的授权，否则会产生无执行的 `in_progress` 记录。补写拒绝只允许明确拒绝原因。
- HTTP guard 覆盖鉴权之后的早退，幂等释放名额；单次执行尝试结束不等于逻辑请求结束。真流完成、无效正文、取消都必须验证。
- `AuthorizationSnapshot.CompleteOnce(func())` 使用请求共享的生命周期状态，快照复制也不能产生独立完成锁。真实 completion 与 HTTP 失败兜底只允许一个进入 writer，包括首次完成仍等待持久化重试时；不能依赖 Store 对不同 outcome 的重复完成幂等。
- `ScopedFailedRequestCompletion` 仅处理拼车允许的非 GET 请求，在 HTTP 状态 >=400 或 context 取消时兜底；正常成功响应不推测执行完成。正文读取失败且未进入执行验证时记 `rejected`，取消记 `canceled`，两者 `UpstreamAttempted=false`。
- 冻结价格验证器标记“可能已执行”，不是证明上游确已尝试；已有验证器导致无法安装标记时也保守处理。`Writer.HandleHTTPFallbackCompletion` 对可能已执行的请求记 `incomplete` / `http_ended_before_completion`，不伪造已结束，并复用现有 retain/retry 顺序。
- `Service.Shutdown` 在等 HTTP 排空之前调用 `CloseAdmission`，否则等待者可能阻碍正常关停。数据库不得先于仍工作的同步 writer 关闭。
- 多账号不做新周期协调；若出现多个候选账号且配置了账号并发上限，明确拒绝不支持场景，而不是静默跳过账号上限。排队后也要重查此条件。

## 4. 校验与错误矩阵

| 条件 | 对外结果 |
| --- | --- |
| 新增限制未配置 | 不限制该项，旧月额度继续有效 |
| 已配置的窗口待同步 | 显示待同步；允许保护空档，不伪造数字 |
| 5h/7d 已达额 | HTTP 403，`five_hour_quota_exhausted` / `weekly_quota_exhausted` |
| 并发不足、队列未满 | 等待且上游零调用 |
| 第 11 个需要等待的请求，默认队列已有 10 个 | HTTP 429，`concurrency_queue_full` |
| 关停停止准入 | 等待者唤醒，`concurrency_closed` |
| 同步记账失败 | 保留 HTTP 503 `accounting_unavailable` |
| 负金额、零/小数并发、非法 null 月额度 | 按现有参数错误路径拒绝，不部分写入 |
| 出队成员/账号变更 | 拒绝并释放名额，不扩大原授权 scope |

## 5. 正常、基础与错误场景

- 正常：一个请求获两类名额，产生一次费用后完成释放；后续等待者复核通过再执行。
- 基础：四项新增限制为空，月额度与既有协议仍按原行为执行；模型查询不占新增生成名额。
- 错误：用客户端 Key 当用户计数键、持有账号名额等用户、依据页面刷新清零、把未知用量显示 0。
- 错误：旧价格事件重放时先要求请求明细还存在，或缺少去重凭据使清理后重放重新扣款。

## 6. 必须测试的断言

- 真实 SQLite CheckOnly 不留记录、不持跨队列事务；最终请求时间/价格不同于入队时。
- 同用户多 Key、同账号多用户、默认 10/第 11 个队满、取消/授予竞态、FIFO 与跳过阻塞者、调限、关停。
- 等待中禁用/撤销/换车/换账号/月达额/记账故障，出队不能调用上游，名额无泄漏。
- 缺失和首次周期、两周期独立、重复/乱序/修正、绝对 reset 优先、跨界长流、待归属费用、重启、明细清理后重放。
- 同步记账与审计回滚、金额溢出、月重置不影响短周期。
- 真实 HTTP 正文读取失败/取消不遗留 `in_progress`；复制快照共享完成锁、冲突完成不污染 writer、无法确认执行时保留 `incomplete`。
- 真实 HTTP 非流/流/无效正文/取消、稳定错误码及一次性释放；桌面/移动表单、未同步、超额和保存失败状态。
- 用注入时钟、synctest 或 channel 同步，不能以墙钟 Sleep 判定正确性。

## 7. 错误与正确示例

错误：`SUM(usage_events.cost_nano_usd)` 作为唯一短周期账本，用户清理明细后可重新获得额度。

正确：同步写入独立费用事实与事件收据；普通清理不删除其额度/去重作用，窗口重复观测也不重置。

错误：HTTP 完成或一次 SDK 尝试完成直接无条件释放、重复写入不同 outcome。

正确：请求持有同一名额至逻辑请求收尾，幂等释放；收尾事件必须保护已记录的真实 outcome，不能用早退兜底覆盖或污染 writer 门禁。
