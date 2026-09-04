# 请求生命周期与用量事件探针

## 目的

本文记录阶段 0.2 对现有请求生命周期、重试和用量事件的源码探针结果，并冻结阶段 6
公共契约。结论只覆盖当前代码可证明的行为，不把推测写成实现保证。

## 关键结论

1. handler 为一次逻辑请求创建一个 `requestLifecycleTracker`，普通、Count、流式和
   plugin executor 路径都只调用一次终态完成。`sync.Once` 防止同一 tracker 重复完成。
2. 上层中间件可通过 `WithRequestLifecycleID` 预置 request ID；未预置时继续生成 UUID，
   保持旧行为。相同 ID 同时写入 `Options.RequestID` 和 usage context。
3. AuthManager 在同一逻辑请求内执行凭据切换和 retry；它复用传入的 context、request
   和 Options，不会创建新的 handler lifecycle。
4. 每次进入具体 executor 都会重新创建 `UsageReporter`。因此一个逻辑请求可以有多个
   usage event，每个事件描述一个实际 executor attempt，而不是一个用户请求。
5. `UsageReporter` 的主事件由 `sync.Once` 限制为每个 reporter 最多一条；Codex 图片工具
   可以额外产生独立模型事件。所有事件使用同一个 request ID，但每条事件有独立 UUID
   `EventID`。
6. `UsageKnown` 由上游 usage 字段是否真实存在决定，不能由 token 是否非零决定。显式
   全零 usage 是 known；没有 usage、只看到 service tier、无明细失败和
   `EnsurePublished` 是 unknown。
7. 当前没有稳定传入 reporter 的 attempt 序号。AuthManager 的外层 retry round、单轮内
   凭据尝试、刷新后重试和 handler 流式 bootstrap retry 不是同一个计数维度，因此本阶段
   不生成 `attempt_no`，避免伪造顺序。

## 调用图

```text
handler
  -> 创建或复用 requestLifecycleTracker(request_id)
  -> usage.WithRequestID(ctx, request_id)
  -> Options.RequestID = request_id
  -> AuthManager.Execute / ExecuteCount / ExecuteStream
       -> retry round
          -> 单轮内选择一个或多个 Auth
             -> 每次 executor 调用创建新的 UsageReporter
                -> 0 或 1 条主 usage event
                -> 可能有额外模型 usage event
  -> tracker.complete(...)，整个逻辑请求恰好一次
```

核心证据：

- `sdk/api/handlers/handlers_interceptors.go`：tracker 复用预置 ID，否则生成 UUID；
  `complete` 通过 `sync.Once` 发送终态。
- `sdk/api/handlers/handlers_execution.go`：普通、Count 和非流式插件路径创建一次 tracker，
  并把相同 ID 写入 context 与 Options。
- `sdk/api/handlers/handlers_stream.go`：流式和流式插件路径在 goroutine 外创建一次 tracker；
  bootstrap retry 继续复用同一 context、request 和 Options。
- `sdk/cliproxy/auth/conductor_execution.go`：`Execute`、`ExecuteCount` 和 `ExecuteStream`
  在内部循环 retry，但不创建 handler lifecycle；每个实际上游调用使用新的 attempt context。
- `internal/runtime/executor/helps/usage_helpers.go`：reporter 在构造时冻结 request ID 和
  `AuthID`，每条记录生成新的 `EventID`，主事件使用 reporter 自身的 `sync.Once`。

## 场景矩阵

下表中的事件数是公共层能证明的数量。具体 provider 在创建 reporter 之前失败，或某条
尚未接入 reporter 的调用路径，可能产生 0 条事件；因此不能把“每次请求至少一条 usage”
当成全局事实。

| 场景 | 逻辑完成数 | usage event 数 | `AuthID` | `UsageKnown` | 调用侧顺序 |
|---|---:|---:|---|---|---|
| 普通成功，响应含 usage | 1 | 每个实际 attempt 至多 1 条主事件 | 该 attempt 选中的 Auth | true，包括显式全零 | executor 发布后返回，handler 再完成 |
| 普通成功，响应不含 usage | 1 | 已接入 reporter 的 attempt 至多 1 条 | 该 attempt 选中的 Auth | false | `EnsurePublished` 入队后 handler 完成 |
| 普通失败，无 usage 明细 | 1 | 已创建 reporter 的失败 attempt 至多 1 条 | 该 attempt 选中的 Auth | false | executor 发布失败后，AuthManager 决定 retry 或返回 |
| 普通失败，已解析 usage 明细 | 1 | 已创建 reporter 的失败 attempt 至多 1 条 | 该 attempt 选中的 Auth | true | 同上 |
| AuthManager 多凭据 retry | 1 | 可有 N 条，各 attempt 独立 | 各自实际 Auth | 各事件独立判定 | 所有 attempt 结束后才有逻辑终态 |
| 流式成功并含 usage | 1 | 每个实际 stream attempt 至多 1 条 | 该 attempt 选中的 Auth | true | executor 在流结束时发布；handler 在转发结束时完成 |
| 流式成功但无 usage | 1 | 已接入 reporter 的 attempt 至多 1 条 | 该 attempt 选中的 Auth | false | `EnsurePublished` 产生 unknown |
| 流式中断/取消，之前含 usage | 1，终态为 failed 或 canceled | 已接入 reporter 的 attempt 至多 1 条 | 该 attempt 选中的 Auth | true | buffer 携带已解析用量发布失败事件 |
| 流式中断/取消，无 usage | 1，终态为 failed 或 canceled | 已接入 reporter 的 attempt 至多 1 条 | 该 attempt 选中的 Auth | false | 无明细失败事件，不伪造零 token |
| plugin executor 非流式/流式 | 1 | 外层非嵌套执行至多 1 条 | 空，插件路由没有 CPA Auth | 按响应字段存在性判定 | 嵌套 host model 执行抑制外层重复事件 |
| Count 成功或失败 | 1 | 当前为 0 | 不适用 | 不适用 | 只有 request completion |
| Codex 图片工具附加模型 | 不新增 | 主事件之外再增加 1 条 | 与主 reporter 相同 Auth | true | 附加事件和主事件共享 request ID、EventID 不同 |

## ID 与归属语义

- `RequestID` 表示下游的一次逻辑请求。AuthManager retry、凭据切换和流式 bootstrap retry
  必须继续使用同一个值。
- `EventID` 表示一条 usage event，用于 writer 幂等去重。每次构造事件都生成新 UUID；
  additional-model 事件不能复用主事件 ID。
- `AuthID` 在 `NewUsageReporter` 时从该次 executor 收到的 Auth 冻结。后续异步消费不得
  根据当前账号分配关系重算归属，而应查询同一 request 的账号快照。
- plugin executor 没有 CPA Auth，事件 `AuthID` 为空。拼车用户第一阶段禁止 plugin
  executor 路由，因此该空值不会成为拼车账号归属来源。
- 旧调用方未提供 request ID 时，新字段保持零值；插件 DTO 与 RPC JSON 解码继续兼容。

## known 与 unknown 判定

### 标记为 known

- `Publish(detail)`：调用方已经解析到 usage 节点，即使所有 token 都是 0。
- `PublishFailureWithDetail(detail, err)`：失败响应明确带 usage 明细。
- OpenAI/Codex/Claude/Gemini/Interactions/Antigravity 插件响应中存在对应 usage 节点或
  token 字段。
- 流 buffer 已观察到真实 usage 节点。

### 标记为 unknown

- `EnsurePublished` 在结束前没有观察到 usage。
- `PublishFailure(err)` 没有 usage 明细。
- 流中只有 `service_tier` 等元数据，没有 usage 节点。
- plugin executor 成功响应没有 usage 节点。

unknown 事件的 token 字段可能因 Go 零值表现为 0，但业务 writer 和 API 必须由
`UsageKnown=false` 区分“未知”与“已知为零”，聚合 coverage 也必须单独计算。

## 顺序与异步边界

源码调用顺序通常是 executor 先调用 `PublishRecord`，AuthManager 返回后 handler 再调用
`complete`。但两个通知通道都异步：usage manager 使用单写消费队列，pluginhost 的
request completion 为独立 goroutine。因此持久化 writer 不能假设实际到达顺序，必须允许：

- completion 先于 usage event 到达；
- 同一 request 的多个 usage event 依次迟到；
- 父请求或 scope 已按保留策略删除后才到达的极端迟到事件被安全丢弃并计数。

## Count 路径结论

`ExecuteCountWithAuthManager` 和 plugin Count 已接入同一个 request lifecycle 与
`Options.RequestID`，所以逻辑请求事实可以正确完成。但现有 provider `CountTokens`
实现没有统一创建 `UsageReporter`，plugin Count 也没有发布 usage，因此当前事件数为 0。

第一阶段不应把 Count 返回的“待计算文本 token 数”误记成上游消费 usage。若产品将来需要
Count 调用次数，应使用逻辑请求事实单独统计；若需要上游计费事件，则必须逐 provider
确认真实计费语义后再新增契约。

## additional-model 结论

Codex 图片工具在 `publishCodexImageToolUsage` 中先确保主 reporter 已发布，再调用
`PublishAdditionalModel`。结果是一次逻辑请求可出现主模型和图片模型两条事件：

- 两条记录 `RequestID` 相同；
- 两条记录 `EventID` 独立；
- 两条记录使用同一冻结 `AuthID`；
- 附加事件只有在存在非零图片 token 明细时发布，当前没有“显式零图片 usage”存在性契约。

## 未证明与发布限制

1. 静态检索确认主要 HTTP/流式 executor 路径创建 reporter，但尚未对每个 provider、每个
   早退分支逐条做故障注入。因此 writer 必须接受“有 completion、无 usage event”。
2. handler 自己的流式 bootstrap retry 与 AuthManager 内部 retry 是两层机制；当前只有
   request ID 能稳定关联，不能稳定给出统一 attempt 序号。
3. WebSocket、Realtime、wsrelay 和 Home 不在第一阶段用户 Key allowlist；本文不承诺这些
   入口的拼车 lifecycle/usage 完整性。
4. usage 与 completion 的异步消费者不保证跨通道到达顺序；数据库写入层必须按 request ID
   做可重入、可迟到处理，而不能依赖墙钟推断。
5. writer 禁止持久化 `Failure.Body`、headers、请求/响应正文和任意 metadata map；公共 DTO
   保留这些旧字段不代表拼车数据库可以写入它们。

## 回归测试锚点

- `sdk/api/handlers/handlers_interceptors_test.go`：自动 UUID、预置 ID、恰好一次完成，以及
  execute/count/stream 的 Options 与 completion ID 一致。
- `sdk/api/handlers/handlers_plugin_executor_usage_test.go`：Options、completion 和 usage event
  使用同一 ID；缺失 usage 为 unknown，显式全零为 known。
- `internal/runtime/executor/helps/usage_helpers_test.go`：known/unknown、流式 tier-only、全零
  usage、事件 UUID 和 additional-model 独立 ID。
- `sdk/cliproxy/usage/manager_test.go`：request ID context 读写及空值兼容。
- `sdk/pluginapi/types_test.go`、`internal/pluginhost/adapters_test.go`：DTO JSON 往返与 adapter
  映射保留新字段，旧记录零值兼容。
