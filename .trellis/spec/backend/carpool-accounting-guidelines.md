# 拼车同步记账与运行中失败门禁

## 1. 适用范围与已确认取舍

修改 `sdk/cliproxy/usage` 发布、拼车记账 writer、请求授权或清理/关停时读取本规范。
2026-09-14 用户明确要求简化并批准：正常请求同步记账，运行中写库失败拒绝新生成，
缓存满时等待落库；崩溃后仅标记账务可能不完整，不因历史记录封锁整个服务。

**不要求**租约树、通用生产者跟踪、新结算迁移、全局容量体系、持久化 outbox 或人工
核对解锁流程。不保证崩溃期间费用精确恢复，不把未知费用当作零或按当前价格追溯猜算。
价格版本冻结与缺价前置不属于此轮修复。本规范取代之前全局启动核对封锁的候选契约。

新增多周期费用账本、请求共享完成锁及 HTTP 早退兜底契约见 `carpool-quota-concurrency-guidelines.md`；不同 outcome 的重复完成不能当作幂等。

## 2. 接口边界

```go
usage.WithSynchronousObserver(ctx context.Context, observer func(context.Context, usage.Record)) context.Context
usage.SynchronouslyObserved(ctx context.Context) bool
(*accounting.Writer).ObserveUsage(ctx context.Context, record usage.Record)
(*accounting.Writer).CheckAdmission(ctx context.Context) error
(*accounting.Writer).RetryPending(ctx context.Context) error
(*accounting.Writer).HandleNonBillableRequestCompletion(ctx context.Context, completion pluginapi.RequestCompletion)
```

`ControlConfig.AccountingAdmission` 是可选的服务器端生成授权检查函数。
`domain.ErrAccountingUnavailable` 对应固定 HTTP `503`、`error.code=accounting_unavailable`；
响应不包含数据库原始错误。已有 `QueueSize` 用于故障缓存容量，不新增前端配置体系。

## 3. 行为契约

- `Manager.Publish` 在原异步分发前执行可选请求同步观察；缺少观察入口的旧 Key 保持
  原异步行为。观察入口必须来自服务器，不来自客户端 header/正文。
- 模块用服务端 `AuthorizationSnapshot` 绑定 usage 与 completion 的请求 ID。没有拼车
  快照的全局事件不能污染拼车门禁；不能仅按 Record 内的任意 RequestID 归属账单。
- 同步路径沿用同一个 CPA Record；异步拼车插件跳过已经观察的投递。数据库仍按事件
  ID 幂等，费用和事件同事务保存。
- 客户端取消不丢弃已发生用量；重试只持有白名单事件和已经准备好的费用，不保留完整
  SDK Record 中的正文、凭据或原始 headers，重试不随当前目录变化改价。
- pending 达到容量上限时暂停当前生产回调，等待成功落库/释放空间。不能继续追加
  无界 slice 或每个事件创建一个重试 goroutine；不能持锁等待而阻止重试或门禁进展。
- 这里限制的是 writer 保留的 pending 条数，不宣称全进程内存或已有上游回调数量有界。
  存储长期故障可能延迟已放行请求结束，这是用户接受的取舍；不增加上游网络超时。
- 失败事实未全部成功补写前，新生成仍返回 503。单纯过了一段时间不能清除门禁；
  全部补写后还要走原有成员额度与 scope 校验，不能直接放行。
- 模型读取的非计费 completion 与计费故障缓存隔离，保留原 best-effort 行为，不影响
  查询原有身份、车辆与 scope 授权，也不能以此丢弃实际计费事件。
- 已知插件流收尾先发布 usage 再通知 completion；仅修复该确定性顺序，不把 channel
  关闭视为所有底层生产者已结束的证明，不增加通用租约/结算协议。
- 写库准备或持久化 panic 不得经外层 recover 后伪装健康；保留运行中故障状态。
  该运行中防护不等同于因历史 incomplete 记录永久封锁服务。
- 启动保留中断请求的 incomplete/费用不完整信息，但不根据这些历史记录拒绝所有新
  生成或永久暂停自动清理。无法证明的遗漏是已接受限制，不增加对账解锁依赖。
- 自动与显式清理、对应预览、账期删除均保护已有 `in_progress` 和 `incomplete`
  请求及其关联事件/账期；不因 HTTP 不是 in_progress 就直接删除 incomplete。
- 显式“重置本期用量”保留原业务契约：抵消已确认金额及未知累计，不删除原始事件或
  incomplete 标记；不要把普通清理的累计保护套到管理员明确确认的 reset 上。
- Close 无法排空或等待超时，返回错误并保留仍被 writer 使用的数据库；不要因本次
  close 失败使重试永久失去条件。正常生产者停止顺序仍由服务生命周期负责。

## 4. 校验与错误矩阵

| 条件 | 预期 |
| --- | --- |
| 无故障正常 Publish | 返回时费用已持久化 |
| 同步观察后的异步重投或重复事件 ID | 不双计费 |
| 写库失败、后续重试仍失败 | 保留记录，新生成 503 |
| 缓冲满 | 当前回调等待，pending 不超过配置上限，恢复后唤醒 |
| 非计费查询在故障期间持续发生 | 不填充计费 pending，不污染运行中门禁 |
| 客户端取消 | 已产生用量仍持久化或保留待重试 |
| 全部补写成功 | 可恢复正常授权，不绕过额度 |
| 重启发现历史 incomplete | 保留可能不完整标记，正常新请求不因此全局封锁 |
| 清理遇到 in_progress/incomplete | 预览与执行均保护请求、事件和相关账期 |
| 关停未排空 | 明确错误，不静默关闭仍被使用的数据库 |

## 5. 正常、基础与错误场景

- 正常：Publish 成功返回后即可读到账单，新请求基于已确认累计判断额度。
- 基础：旧全局 Key 没有拼车快照，继续异步分发，不创建拼车失败事实。
- 错误：单测直接调用 writer 就声称 SDK Publish 同步，绕过了原始异步边界。
- 错误：历史 incomplete 导致整个服务永久封锁，要求手工改库解锁，违背用户取舍。
- 错误：为了严格覆盖崩溃极端情况再次引入已否决的全局租约、容量或结算迁移。

## 6. 必须测试的断言

- 完整 Publish→writer→SQLite→下一次授权，而不是只测回调。
- 屏障证明同步等待；小容量故障注入证明满缓存、不超限、恢复唤醒、不丢账。
- 失败期间模型查询/旧 Key 不污染故障缓存；安全 HTTP 503。
- 取消、并发去重、Close 失败保库与再次关闭。
- 插件流末尾 usage 在 completion 前发生；不把完成时机的已接受限制隐藏掉。
- 重启保留 incomplete 信息却允许新请求；无迁移、无对账操作要求。
- 自动/手动清理和预览保护相同记录、事件与账期。
- 用隔离 SQLite、假 executor、channel/屏障，不用 Sleep 判断正确性，不碰生产数据。

## 7. 错误与正确验证示例

不足以证明完整链路同步：

```go
writer.HandleUsage(ctx, record)
```

应验证真实发布入口：

```go
ctx = usage.WithSynchronousObserver(ctx, writer.ObserveUsage)
manager.Publish(ctx, record)
// Assert persisted billing immediately, without draining async delivery first.
```

缓存容量断言与等待唤醒必须用同步屏障；测试通过不等于崩溃后精确恢复得到保证。


## 8. 关停后迟到回调的已接受边界

worker 退出后不自动重启。迟到回调若等待缓存空间，需要显式 `RetryPending` 或再次
`Close` 推进；存在等待者时 `RetryPending` 必须返回记账不可用，防止 Close 越过尚未
重新入队的记录关闭数据库。本轮不增加通用生产者 join，因此成功关停后仍出现生产者
属于未承诺的尾部场景，不能声称该场景有自动恢复保证。

## 9. 请求价格冻结与执行前校验（2026-09-14 追加交付）

本节是用户随后要求完成的价格功能，替代第 1 节中“不属于此轮”的历史范围描述；
不改变简化可靠性的取舍。

### 适用入口与签名

- `pricing.Freeze(*Catalog) *Snapshot`、`(*Manager).Snapshot() *Snapshot`：只读目录视图。
- `(*Snapshot).Lookup(string) (ModelPrice, bool)`：返回独立价格值；调用方不能修改目录。
- `runtime.NewAuthorizationSnapshot(..., ...*pricing.Snapshot)`：保留旧调用兼容性。
- `executor.WithRequestValidator(context.Context, RequestValidator) context.Context`；
  `RequestValidator` 签名为 `func(context.Context, string, Request) error`。
- `executor.ValidateRequest(context.Context, string, Request) error`：未安装钩子则无操作。

### 数据流契约

生成授权时获取一次目录快照，同一引用同时供执行前校验和事件计价使用。请求中途更新
目录不得改变重试、流式尾部或延迟异步事件的价格与 hash；更新后授权的新请求使用新目录。
`Manager.Current()` 返回防御性副本，嵌套 tier 和有理数也不能泄漏可变引用。

校验位于实际模型解析、账号选择及插件改写之后，上游调用之前。普通执行、流式、
模型池、刷新后重试、嵌套执行都应传递上下文。HTTP handler 即使从 `Background()` 派生
执行上下文，也必须继承拼车请求的 validator、同步 observer 和授权快照，同时保留调用方
取消语义；只复制 scope/request ID 不能满足此契约。

### 错误矩阵与示例

| 场景 | 结果 |
| --- | --- |
| 当前实际模型有价格 | 使用授权时目录继续执行 |
| 实际模型没有价格 | 本地 HTTP 422，`error.code=model_price_not_configured`，该次上游调用数不增加 |
| 客户端别名有价、解析后模型无价 | 拒绝，不按客户端别名猜价 |
| 本地校验拒绝 | 不更新账号冷却或健康失败状态，不扩大 scope |
| 旧全局 Key 未安装钩子 | 原行为不变 |
| 已执行事件缺必要 Token 维度 | 费用未知，不伪造零费用 |

正确：目录 A 授权 → 更新 B → 原请求校验/计价仍用 A → 后续请求用 B。
错误：writer 收到事件时重新读取 `Manager.Current()`，或只在入站模型名上校验。

### 必须覆盖的断言

必须通过真实 HTTP 链验证 422 和零上游调用，不能只直接调用 validator。用同步屏障或
显式更新控制 A/B 次序，核对持久化金额及 hash；覆盖异步回退、深拷贝 tiers、流式及重试。
禁止用新迁移、全局封锁或持久化 outbox 替代请求快照。

## 10. 清理与账期操作不能混用截止时间

`Control.PreviewRetention` 与 `RunRetention` 中，只有 `usage_details` 根据明细保留
天数计算过期 cutoff；永久保留时，显式管理员明细清理仍可针对当前已完成记录。
`reset_current_period`、`closed_periods` 必须使用当前时刻，不得减去 90/180/365 天。
否则预览和执行可能均返回成功，却根本没有重置当前账期。

必须在真实 SQLite 中固定时钟覆盖三个保留档位：已确认 0.225 重置后净额为 0，限额、
上车锚点和账期边界不变，历史/未来账期不被重置；`period_to == now` 的已结束账期可删除。
浏览器测试必须断言实际金额变化，不能只断言 HTTP 202 或成功 toast。
