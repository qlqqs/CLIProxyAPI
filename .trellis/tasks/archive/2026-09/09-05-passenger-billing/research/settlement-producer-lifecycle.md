# Research: 结算用量生产者生命周期

- Query: 不依赖 HTTP 终态，如何证明非流、流、取消、插件嵌套和重试的全部 CPA 用量生产者已经结束；最小可行钩子与有界背压要求。
- Scope: internal（仅本地源码；不研究 SQLite 结算结构、保留或恢复操作）
- Date: 2026-09-14

## Findings

### 结论与任务上下文

已读本任务 `design.md` 最后“第二轮：可靠结算设计补充”、`research/2026-09-14-accounting-review.md`。设计仍写背压“待确认”，本次调研以派发指令中用户已接受“有界缓冲满后等待持久化、允许响应延迟、不丢已发生用量”为准。

**不能把 HTTP completion、任一包装流 channel 关闭、第一次 Publish 或 usage.Manager 排空，当作全部生产者结束。** 最小可靠抽象应为服务器可选的请求根作用域及可派生的生产者租约：先登记所有权，再允许执行/启动 goroutine；在最后一次可能发布及其同步持久化确认之后归还。普通 HTTP 结束只归还根所有权，不封杀已有子生产者。当前源码没有足够契约让“只改 handlers/conductor/usage helper、不改 provider 或 pluginhost”覆盖所有执行器。

### 文件与具体代码证据

| 文件、函数、行号 | 发现与所有权含义 |
|---|---|
| `internal/carpool/module.go:170-187`，`AuthenticatedRequestHook` | 服务端授权快照存在时才安装同步 observer，并强制绑定快照 RequestID；适合增加可选根 scope，不能以 API Key 字符串判断是否启用。根最终释放仍需请求中间件边界。 |
| `sdk/cliproxy/usage/observer.go:8-40`；`manager.go:401-427`，`Publish` | 同步 observer 在全局异步队列之前调用，关闭 manager 后仍调用 observer。`publishing` 仅覆盖已进入 Publish 的调用；无法统计将来才创建 reporter/调用 Publish 的执行者。同步之后 `queue=append(...)` 本身没有容量限制。 |
| `sdk/cliproxy/executor/types.go:298-322` | `Response` 只有 payload/metadata/headers；`StreamResult` 只有 headers/chunks，无生产者 join/Done。不能从类型契约推导发布者完成。 |
| `sdk/cliproxy/executor/lifecycle.go:9-32` | `ExecutionLifecycle.Bind/End` 是资源清理契约，不是 goroutine join；关闭资源不等于用量已发布。不可直接复用 End 表示账务完成。 |
| `sdk/api/handlers/handlers_execution.go:45-110,119-178` | 常规非流/Count 顺序调用 auth manager、响应拦截器、completion。作用域必须包住路由/前后拦截器及 completion 插件派发，不仅包住 executor.Execute。 |
| 同文件 `181-233,236-272` | 插件直连与 Count 有独立路径；非流插件自身 reporter 在主调用后发布，nestedTracker 命中时跳过。CredentialScope 已强制的插件执行当前被拒绝（187-188、241-242），不应因此把所有插件拦截器视为不存在。 |
| `sdk/api/handlers/handlers_stream.go:30-99,154-174`，`streamWithPluginExecutor` | reporter 在调用插件前创建，成功后由 goroutine 发布。defer 实际顺序是关闭 errChan、关闭 dataChan、completion、usage；这是“输出关闭早于发布”的直接反例。启动错误和 nil result 另走同步分支。 |
| 同文件 `301-360,552-592,602-610,676-708` | 常规流也提前关闭下游 channel；取消/终端错误可停止读取而上游仍活着。handler 自己还有 bootstrap retry，会调用新的 `AuthManager.ExecuteStream`，不能只等最后一次成功流。 |
| `sdk/api/handlers/handlers_interceptors.go:97-118,128-152` | completion observer 在插件完成通知之前；`sync.Once` 只去重通知，不等待插件、publisher、重试。若 observer 同步等待全体完成，会阻塞后面的插件派发，存在自等待/遗漏新孩子的风险。 |
| `sdk/api/handlers/handlers_context.go:27-67` | nested tracker 只是互斥保护的 called 布尔值；不是活跃子请求数量、注册屏障或结束信号。 |
| `sdk/api/handlers/model_execution.go:97-143,236-309` | ExecuteModel/ExecuteModelStream 标记 nested 后进入共享执行路径。模型流包装 goroutine 在 context 取消或错误时直接关闭输出，仍不能证明底层结束。 |
| `sdk/cliproxy/auth/conductor_execution.go:121-178,232-303,513-529` | 外层请求重试、credits 回退、实际 Execute 与 401 刷新重试都可能产生独立尝试；必须保留每次尝试所有权，不能覆盖旧尝试。 |
| `sdk/cliproxy/auth/conductor_stream.go:12-20,123-204` | `discardStreamChunks` 新建无 join 的 drain goroutine。`wrapStreamResult` 取消转发时启动 drain 后关闭 out（181-190），发布者可仍在底层流内。 |
| 同文件 `206-255,311-344,366-413` | 模型池、401 刷新、bootstrap 错误重试/切模型会并存旧 drain 和新执行；注册要在每次 ExecuteStream 调用之前，不能到成功返回之后才登记。 |
| `sdk/cliproxy/auth/conductor_home_execution.go:198-228,300-339` | Home 有独立 Execute 调用点及 wrapHomeStream；取消可先 End/releaseAttempt/close(out)，不等待底层流。资源租约与生产者租约需分离。 |
| `internal/runtime/executor/helps/usage_helpers.go:69-94,135-144,326-379` | reporter 构造无注册，TrackFailure 在 err=nil 时不发布；EnsurePublished 只是 once 发布，非关闭。PublishAdditionalModel 绕过主 once，可另外发布。把第一次 Publish 当作结束会漏附加模型；只在构造时加计数而无显式归还会泄漏。 |
| `internal/runtime/executor/codex_executor_request.go:488` | 附加图像模型使用 PublishAdditionalModel，是多事件/单 reporter 的实际调用点。 |

### 必须纳入的异步所有权类别

1. **同步入口中的异步工作**：不能假设非流无 goroutine。`internal/runtime/executor/antigravity_executor_execute.go:330-371` 启动扫描/发布 goroutine，外层 373-386 收集并再次调用 reporter；收到 Err 时外层可以在底层清理完成前返回。注册子生产者必须在 `go` 前，外层 Execute 返回只释放外层租约。
2. **真正的 provider 流生产者**：流 API 返回后 scanner/WS goroutine 持有 reporter。示例 `gemini_executor.go:344-346`、`gemini_vertex_executor.go:668-670,817-819`、`aistudio_executor.go:294-296` 先注册 `defer close(out)` 再 EnsurePublished，原始流正常关闭前确实先发布；这是局部证明，不是 SDK 总契约。`openai_compat_executor.go:412-429` 的延迟 StreamUsageBuffer 发布也在原始 out 关闭前。
3. **WS 会话和每次生成分离**：`codex_websockets_stream.go:484-519`、`xai_websockets_executor.go:666-695` 的每请求 goroutine 与会话读循环不同；应等待每请求用量拥有者，不等待长寿命 WS 会话关闭。Codex 另有同步完成流分支（475-481），也不能漏归还。
4. **包装器提前退出**：除了 handlers/auth/Home，`internal/runtime/executor/kimi_thinking_replay.go:447-479` 的 replay wrapper 在取消时先关闭自身 out；仅在 conductor 等返回的 StreamResult.Chunks 不足以证明内部 provider 已结束。
5. **旧尝试排空与新重试并行**：auth discard goroutine、handler bootstrap retry、Home/credits/401/模型池都须保留旧生产者，不能只追踪“当前 attempt”。drain 不是用量拥有者的替代证明；拥有者结束信号必须来自实际执行体。
6. **插件 adapter 自己的 reporter 与多级转换流**：`internal/pluginhost/adapters_executors.go:740-764,767-838` 的观察流在 defer 发布；但外层 `translateExecutorStreamChunks:551-587`、`mapExecutorStreamChunks:1085-1117` 都可先因取消结束。最终 adapter channel 关闭仍不能 join 观察流。
7. **插件 RPC 调用先返回取消、内部仍执行**：`internal/pluginhost/client_guard.go:24-51` 启动 inner.Call goroutine，ctx 取消可提前返回；`calls` 只服务插件关停，不关联计费请求。子租约必须在 33 行 go 前登记，在内部真实返回后归还，不能在外层 Call 返回时归还。
8. **完成插件继续触发嵌套执行**：`internal/pluginhost/adapters_interceptors.go:189-216` 的 CompleteRequestExcept 使用 WithoutCancel 并逐插件启动 goroutine。HTTP/completion 后仍有合法工作来源；须在派发前登记孩子，并在回调执行期间允许其生成孙孩子。
9. **脱离 HTTP 取消的嵌套模型流**：`internal/pluginhost/host_model_stream_callbacks.go:23-46` 从 callback context 派生 WithoutCancel 流并由 bridge/cleanup 取消；cleanup 的 cancel 不等于模型生产者结束。`ExecuteModelStream` 的子 scope 必须跟随真实底层发布者，不能跟随 RPC stream ID 或 bridge 输出关闭。
10. **全局 usage 异步投递**：这是计费 observer 之后的消费者，不是本请求新费用的合法隐式根。不得为旧全局 Key 强加等待/容量/结算语义；若某插件在异步 usage 回调中启动新的计费执行，需要另行明确服务器授权/作用域契约，不能把已结束 scope 自动复活。

### 最小可行设计（建议契约，不是现有实现）

**首选 context 中的可选所有权树，避免把计费含义塞进现有 StreamResult。** 命名可为 `ProducerScope` / `Lease`，具体 API 由实施确定。

- 服务端完成授权且取得容量预留后、任何路由/拦截器/上游工作前建立 root，根身份取授权快照。整个请求只创建一个账务根；嵌套 handler/completion 不能重新创建或封闭根。
- 每个活跃 Lease 可以 `Fork` 孩子；Fork 与该 Lease 的 `Release` 共享锁/状态，释放后的父句柄不能再 Fork。孩子登记必须发生于启动 goroutine、提交回调或把控制权交给外部执行体之前。**HTTP 结束只释放 root 这个句柄；不能禁止仍活跃孩子 Fork，也不能允许拿着旧 context 的任何人无限登记。** 这解决计数降为零与迟到注册竞态；裸 WaitGroup 的 Add/Wait 不足以定义这个契约。
- handler 根/执行租约包住路由、所有拦截器、重试调度和完成插件的“派发”；流成功时明确转交给转发 goroutine，所有同步拒绝/错误/nil result 归还一次。异步插件派发/inner.Call/模型回调各在自身真实所有权边界 Fork/Release。
- 实际 provider scanner/WS/非流内部 goroutine 在启动前 Fork，并在全部发布与 deferred 发布之后 Release。同步 Execute 持有外层租约；成功 Stream 调用需要显式转移而非在函数返回时结束。helper 可提供 no-op aware 的租约辅助，但不能仅凭 NewUsageReporter/TrackFailure/EnsurePublished 猜测退出。
- `Publish` 必须在活跃拥有者内执行，直到同步 observer 确认持久化，或成功将安全事件所有权转给一个有界持久化队列；后一方案要有独立“已接收未确认”计数。**生产结束与持久化确认是两个条件**：全部租约归还且全部已发生事件被确认，才发一次结算就绪通知。通知不能在 scope 锁内执行，也不能在 completion observer 内同步等自己/后续插件。
- request ctx 取消只影响网络转发，不取消已发生事件的持久化等待。panic/缺失结束信号不能当成功；保留未结算状态并报告。关停先停止新 root/插件派发，等待已登记拥有者和确认，再关 writer；超时明确未排空，不能伪造 Release。
- 不要求所有不发布用量的纯转发 goroutine都计费，但其若还可调用插件/创建孩子，就必须持有 Lease。不要为了等待而为每个 channel 新开永久 goroutine；优先在已有拥有者退出时通知。

**若不愿编辑 provider**：只有逐一证明“所返回的原始 channel 正常/取消/错误关闭前，所有同步及 deferred 发布已完成，且没有脱离的孩子”，才能对该执行器建立本地受信适配器；仍需排除/修复 replay/plugin wrapper。当前返回类型无保证，不能普遍采用此方案。可选新增 `ProducerDone` 也不是免费的替代：所有 provider 必须正确设置、所有 wrapper 传播并合并，错误返回也需有所有权释放通道；nil 不能解释为完成。现有 context 租约更容易覆盖返回 error 但内部仍执行的情况。

### 需要明确扩大到哪些文件，不能隐瞒的范围

若要覆盖仓库现有全部路径，需要最少增加生命周期调用点（不改 Token 解析/翻译规则）：

- 已确认流生产者：`antigravity_executor_stream.go:174`、`claude_executor_stream.go:271`、`codex_executor_stream.go:286`、`codex_websockets_stream.go:484`、`xai_executor_stream.go:70`、`xai_websockets_executor.go:666`、`kimi_executor.go:323`、`gemini_executor.go:344,541`、`gemini_vertex_executor.go:668,817`、`aistudio_executor.go:294`、`openai_compat_executor.go:412,654`、`codex_openai_images.go:242,433`（均位于 `internal/runtime/executor/`）。
- 非流内部生产者 `antigravity_executor_execute.go:330`，及上述同步启动/提前返回分支。普通非流可通过统一调用边界持有租约，不能因此漏内部 fork。
- pluginhost adapter、guarded client、completion 派发和 nested callback；这已经超出“仅 helpers 加字段”的范围。服务端可信插件如果任意保留 context 并在返回后再启动工作，不合作登记就无法证明完成；需禁止已关闭租约重新执行，并在连接上游前拒绝，而不是费用发生后拒收。

### 有界背压与容量预留

只把 pending slice 改成固定 channel **不证明总内存有界**：每个阻塞 Publish 的调用栈仍持有完整 Record、原始 headers、上游响应/扫描缓存；每个取消后继续活跃的 provider、drain、插件 RPC 都是等待者。

1. 在上游开始前原子预留请求/活跃 producer 容量，不能只做 `CheckAdmission` 健康查询。HTTP 取消/返回不能释放仍有孩子或待确认记录的预留。容量用尽时拒绝未开始的新生成请求，不能为已发生用量报“队列满”然后丢弃。
2. 有请求上限还不够：插件可 fan-out、连续 retry 可留下多个 drain。每次 Fork/attempt 在实际启动前也要预留，限制全局及单请求同时活跃数/嵌套深度；不够时拒绝尚未发生的新子执行。不要让父生产者拿着最后一个槽等待孩子同一资源，形成容量死锁。已有工作发布需要使用它预留的槽，不能再争抢启动槽。
3. 固定队列 Q、安全事件单项上限 S、活跃持有事件的生产者上限 P，计费保留量至少按 `(Q+P)*S` 核算；另列 scanner/响应/插件正文等执行缓存的边界。附加模型可顺序多事件，不等于必须一次预留无限事件，但每个 producer 的并发 Publish 数必须有约束。超长身份/模型字段需在执行前校验；费用已经发生后才发现记录过大不能丢账。
4. 确认后的全局 SDK 异步 queue 仍是 `append`，慢旧插件会积累 Record/context（可能保留 scope）。本轮可以只声称“计费故障缓冲和等待者有界”，不能声称全进程内存有界。若要后者，必须单独设计可选路径的投递边界，不能暗改旧 Key 异步语义。
5. 等待/唤醒用有限生产者上的 condition/channel；锁不得覆盖持久化、等待孩子或插件调用。写库恢复可由单一受控重试者唤醒；不得每个失败事件新建循环 goroutine。非计费模型读取不得共享“失败 completion 无限排队”机制。

### 实施顺序与确定性测试要求

1. 先定义可选 Lease 的 Fork/Release/确认状态及容量预算；根注入与旧 Key no-op 测试优先。
2. 先修 handlers/pluginhost/实际 provider 所有权边界，再连接“结算就绪”服务器 observer；HTTP completion 继续表达网络结果，不担任账务屏障。
3. 用 channel 屏障逐项证明：
   - HTTP/dataChan/errChan 已结束但 provider Publish 尚被阻塞时，不触发就绪；释放持久化屏障后恰好一次。
   - root 已归还但活跃插件孩子随后 Fork 孙孩子合法；已释放父 Fork 与最后一次 Release 竞争不复活根。
   - 非流内部 goroutine、stream 启动 error/nil/空流、取消、scanner 错误、附加模型、deferred 发布、panic。
   - 401/模型池/handler bootstrap/credits/Home 重试，旧流 drain 未结束而新流成功，仍等待旧发布者。
   - completion 插件延迟嵌套；guarded Call 提前取消；WithoutCancel 嵌套模型流；adapter/replay wrapper 输出提前关闭。
   - 满容量时大量新请求被执行前拒绝，已放行 producer/事件数量不继续增长；取消不偷还槽；恢复全部已发生事件恰好确认；父子容量不会死锁。
   - 同步 observer 与异步重复投递不双计；无授权快照旧 Key 行为不变；关停失败不伪造完成。
4. 必须跑定向 `go test -race`；测试用屏障/显式计数，禁止通过 Sleep 或“等全局队列空了”证明生产者完成。本次仅研究，未运行产品测试或改动产品代码。

### 相关规范与外部引用

- 已读 `.trellis/workflow.md`、`.trellis/spec/backend/index.md`、`quality-guidelines.md`、`lifecycle-guidelines.md`、`carpool-accounting-guidelines.md`。重点遵守生产者先停/消费者后排空、取消后已发生用量保留、旧 Key 可选行为、确定性测试与不增加上游建立后的超时。
- 外部参考：无。本报告仅依据当前工作树源码，不依赖外部 SDK 版本或公开网页；用户提供的仓库要求 Go 1.26+。

## Caveats / Not Found

- 未找到通用的“全部用量 producer 已退出”接口；现有 StreamResult/ExecutionLifecycle/nestedTracker/Manager.publishing 均不足。
- 上述 provider 行号是定位实施调用点，不声称每个 provider 的每条取消分支都已经保证发布已观测 Token。生命周期补丁不能自动修复原先没有发布用量的分支，也不能恢复上游未返回的真实费用；需逐 provider 回归。
- 未证明第三方插件任意后台工作会遵守生命周期。若实现不愿触及 provider/pluginhost，必须缩小受支持计费执行器集合并在上游前拒绝不具备契约的路径，不能声称全覆盖。
- 不涉及 SQLite 表、迁移、结算写入 SQL、保留/清理和人工恢复方案；交给主会话。工作树可能继续变化，实施前复核行号。
