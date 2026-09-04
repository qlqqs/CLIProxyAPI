# 剩余决策审查结论

## 审查方式

本结论来自 `brainstorm-carpool-decisions` 频道中主代理与
`decision-reviewer` 的多轮独立审查。审查依次覆盖产品边界、反方复核、
数据与权限契约、身份生命周期和运维策略、跨层调用图与发布阻断项。

用户已经授权由代理组收敛剩余决策，因此本文件给出最终选择，不再保留
需要用户逐项回答的产品问题。无法仅凭现有代码证明的实现细节被转换为
技术探针或发布门槛，不改变第一阶段产品行为。

## 已冻结的产品决策

### 用量与额度

- 第一阶段只展示用量，不执行用户、车辆或账号硬额度；现有上游额度、
  冷却与不可用逻辑继续由原凭据管理器处理。
- 乘客提供“今日”“最近 7 天”“最近 30 天”三个固定周期；管理员另可
  查询 UTC 半开区间 `[from, to)`。所有边界由服务端回显。
- 权威请求指标来自逻辑代理请求：总请求数、成功数、失败数、拒绝数和
  取消数。Token 指标来自实际收到的用量事件，两类事实不得混为一表。
- 展示输入、输出、缓存、推理和总 Token 的已知合计，并单独展示未知用量
  事件数。供应商未返回 usage、解析器无法判断字段是否存在或流程异常时，
  `usage_known=false`，绝不能把未知用量显示成 `0 Token`。
- 重试不会增加逻辑请求数；如果多个上游尝试确实产生已知 Token，则其
  实际消耗都进入 Token 合计。第一阶段不把这些数据用于计费。
- 用量默认保留 90 天，审计默认保留 180 天，均可配置；长期归档、合规
  擦除和权威计费推迟。

### 成员与账号可见性

- 同车成员使用管理员配置的 `display_name`。它是车内展示名称，不等同于
  登录名或真实姓名；当前同车展示名必须唯一。
- 乘客可见本人和同车成员在选定周期内的聚合请求数、已知 Token 与未知
  用量数。乘客接口不返回他人的登录名、邮箱、内部 ID、模型明细、时间线、
  单请求记录、提示词或响应正文。
- 成员离车后立即从当前成员列表消失；历史报表仍按请求开始时固化的车辆
  和展示名快照归属，不因改名或换车重写。
- 每次上游账号分配生成一个仅用于该分配关系的公开 `account_ref`，并要求
  管理员填写安全标签。乘客看不到原始 `AuthID`、凭据标签、邮箱、文件名、
  Token、API Key、原始元数据或详细上游错误。
- 账号状态 DTO 固定为 `healthy`、`degraded`、`unavailable`、`disabled`、
  `unknown`，同时返回 `observed_at` 与 `stale`。状态来自现有运行时观测，
  页面查询不会主动探测上游；超过 5 分钟的观测按陈旧处理，UI 最多每
  60 秒刷新一次。

### 身份与凭证生命周期

- 拼车管理员和乘客使用同一套拼车用户库，但拼车管理员不会因此获得现有
  Management API 权限；现有管理密钥也不能作为拼车网页登录会话。
- 角色创建后不可修改。管理员不能拥有车辆成员关系、创建或使用用户代理 Key；
  同一自然人如需同时管理与乘车，使用两个独立账号保持权限边界。
- 首个拼车管理员由一次性 CLI 交互创建。密码必须从 TTY 隐藏读取并二次
  确认，不提供默认密码，也不允许把明文密码放进命令行或日志。
- 网页使用服务端随机不透明会话和 HttpOnly Cookie；会话绝对有效期与
  空闲有效期可配置，默认分别为 24 小时和 2 小时。
- 程序化代理使用带保留前缀的用户 API Key。Key 只在创建时显示一次，
  数据库仅保存查找标识和随机秘密的不可逆摘要，支持命名、可选过期、
  单独撤销和无中断轮换；第一阶段不设固定的每用户 Key 数量上限。
- 无中断轮换是“先创建新 Key、切换客户端、再撤销旧 Key”的操作流程，不提供
  专用原子 rotate 端点。
- 修改或重置密码会撤销该用户全部网页会话，默认不撤销 API Key；管理员
  可显式执行“撤销全部 API Key”。禁用用户时，会话和全部 API Key 都失效。
- 用户 API Key 不接受 Cookie 代替；Cookie 鉴权的写操作执行同源校验与
  CSRF 防护。
- 会话 Cookie 固定 `SameSite=Strict`。登录要求 Origin 与当前站点或显式可信
  Origin 匹配；其他 Cookie 写操作同时要求 Origin 与 CSRF，缺失、`null` 或跨站
  登录请求被拒绝。
- 会话 Token 只保存摘要；当前随机 CSRF 值可以明文保存在会话行，因为它没有
  HttpOnly 会话 Cookie 时不能独立认证。页面刷新返回同一会话的当前值，多标签页
  共享且互不轮换；该值仍禁止进入日志、URL 或浏览器持久存储。
- 登录限速按规范化用户名指纹与可信客户端地址组合。只有 TCP 对端命中显式
  `trusted-proxy-cidrs` 时才读取转发地址；进程重启清空内存桶是首期接受限制。

### 车辆、成员和账号分配

- 每名乘客至多有一个当前成员关系；每个上游 `AuthID` 至多有一个当前车辆
  分配。两项规则都由 SQLite 部分唯一索引兜底，而不只依赖服务层检查。
- 车辆支持可选的 `seat_limit`。设置后，加入或换车不能超过上限；降低上限
  到当前人数以下时返回冲突，不隐式移除成员。
- 换车与账号移车均在单个短事务中结束旧关系并创建新关系，并发冲突返回
  `409`。用户、车辆、成员关系和账号分配只禁用、退役或写入 `ended_at`，
  不做破坏历史引用的物理删除。
- Key 认证成功后先生成 request ID；授权成功时同事务固化 `user_id`、`car_id`、
  `membership_id` 以及 `AuthID → assignment/account_ref/safe_label/provider`
  映射。该事务提交即定义请求开始，此后即使尚未连接上游也继续使用该不可扩张
  快照；管理事务只影响之后开始的请求，运行时全局禁用等仍可缩小候选。
- Key 认证失败只记安全审计。Key 已认证但用户、成员、车辆、账号集合或入口授权
  失败时，写一条归属可空的 `rejected` 请求事实并在连接上游前返回。

### 兼容范围

- 现有全局 `api-keys` 保持原行为，继续代表受信任的全局客户端；它没有
  用户或车辆身份、不会进入拼车报表，也不能访问 `/carpool/api/*`。
- 管理密钥、拼车 Cookie、用户 API Key、旧全局 Key 四个凭证域互不替代。
- 用户 Key 兼容 Bearer、`X-Api-Key`、`X-Goog-Api-Key`、`key` 和
  `auth_token`。拼车启用时在所有 provider 前跨位置提取：相同值去重，不同值冲突
  拒绝，避免旧全局 Key 与用户 Key 由注册顺序择一。启用拼车后匿名代理访问关闭；
  与 exclusive 前端鉴权插件同时启用属于启动配置错误。
- Home 模式第一阶段在接触上游前拒绝用户 API Key，但旧全局 Key 的 Home
  行为保持不变；拼车网页和管理功能仍可使用。
- 用户 API Key 仅开放已经通过端到端隔离测试的 HTTP 与 HTTP 流式入口。
  Responses WebSocket、Realtime、wsrelay 及其他未证明隔离的长连接入口
  明确拒绝用户 Key；旧全局 Key 保持现状。
- `/management.html` 及其远程更新机制保持不变。新 `/carpool/` 使用本仓库
  内的浏览器原生 HTML/CSS/ES Module，由 `go:embed` 随二进制发布，不引入
  Node 构建前置条件，也不复制管理面板的运行时远程下载机制。
- 前端采用 hash 路由，只显式注册 `/carpool/` 与 `/carpool/assets/*filepath`，
  不使用会和 API 冲突的 `/carpool/*` catch-all。未知拼车 API 通过现有 NoRoute
  链的独立分支返回 JSON 404，不能回退为 HTML，原插件 NoRoute 行为保持不变。

## 已冻结的技术边界

### 模块形态

采用“模块化单体后端 + 独立前端入口”：

```text
/management.html ── 现有管理密钥 ── 现有 Management API

/carpool/ ── 拼车 Cookie ── /carpool/api/v1/* ── internal/carpool

兼容代理 HTTP ── 用户 API Key ── 授权快照 ── CredentialScope
                                        └────── SQLite 用量归属
```

拼车领域、SQLite 仓库、认证、业务 API、审计、报表和静态资源放入新的
`internal/carpool/**`。宿主只增加少量通用契约：入站身份桥接、不可扩张的
凭据范围、逻辑请求生命周期标识和明确的用量字段存在性。业务授权不进入
`internal/translator/**`，也不在各供应商 executor 中重复实现。

### 凭据范围

`CredentialScope` 是只读、不可由客户端或插件扩大的允许 `AuthID` 集合：

```text
全局当前候选
∩ 请求开始时允许的 AuthID
∩ 当前供应商、模型、冷却与可用性条件
→ 原有路由策略、插件调度、会话亲和、重试和故障转移
```

- 用户请求缺少 scope 或 scope 为空时默认拒绝。
- 旧全局 Key 的 scope 为 `nil`，保留原有无限定行为。
- scope 在首次选择、快速调度、传统选择、混合供应商、插件返回值、重试、
  故障转移和会话亲和命中处都必须生效。
- 会话亲和的 `caller_scope` 与安全边界 `credential_scope` 是两个概念；旧的
  亲和账号不在当前范围内时按 miss 处理。

### 用量事实

现有代码已经证明：

- `sdk/api/handlers/handlers_interceptors.go:88` 会为普通逻辑请求生成 UUID，
  并通过 `RequestCompletion` 恰好完成一次成功、失败、拒绝或取消终态。
- `sdk/cliproxy/auth/conductor_execution.go:121` 的执行、计数和流式重试复用
  同一 `Options`，适合贯穿请求 ID 和授权范围。
- `internal/runtime/executor/helps/usage_helpers.go:331` 的 reporter 每实例通过
  `sync.Once` 上报一次，但 `EnsurePublished` 当前会用全零 Token 补记录。
- `sdk/cliproxy/usage.Record` 当前没有逻辑 `request_id` 和 `usage_known`，因此
  现状仍不能区分“真实 0”与“未知”，也不能直接声明事件等于逻辑请求。

最终采用两层模型：`proxy_requests` 保存逻辑请求和终态，正式枚举包含
`in_progress|succeeded|failed|rejected|canceled|incomplete`；`usage_events`
保存每次实际收到的统一用量事件，并通过新增的 `request_id` 关联。请求数只查
前者，Token 只汇总后者，`incomplete` 只进入 coverage。

另设 `proxy_request_auth_scopes`，在请求开始事务中固化每个允许 AuthID 的
assignment、公开账号引用、安全标签和 provider。异步 usage 必须从这张请求快照
取得账号归属，不按落库时的当前关系或时间区间反推。是否能稳定标记 attempt 序号
仍由探针决定；第一阶段接口不承诺 attempt 级明细，因此该细节不会改变产品行为。

`usage_events` 不重复保存 `car_id` 或 `user_id`，而是通过 `request_id` 联表
`proxy_requests` 的请求开始快照；账号维度使用事件中由请求账号快照复制的
`assignment_id`。这避免重复车辆归属发生漂移，车辆事件查询由
`proxy_requests(car_id,request_id)` 与 `usage_events(request_id,requested_at)`
两侧索引支持。

进程启动时在注册路由、provider 和 writer 前，把库中前一进程遗留的全部
`in_progress` 原子标为 `incomplete`。单实例边界使此时不存在当前进程的合法长
请求，因此不设置容易误判的时间阈值。保留清理按事件 `requested_at` 与请求
`completed_at` 分别计算，二者共用 `usage-retention-days`；只有不存在保留期内
usage 子行时才删除终态请求及其 scope，且普通清理永不删除 `in_progress`。若
极端迟到的 usage 已找不到父请求或 scope，只安全丢弃并计数，不重建漂移归属。

## 实现前必须完成的技术探针

以下是工程证据门槛，不是待用户决定的产品问题：

1. 建立所有代理路由到 selector 的调用图，列出普通 HTTP、HTTP 流式、
   Responses WebSocket、Realtime、wsrelay 与 Home 的身份和 scope 传播路径。
2. 用内存 scope 强制两辆车各自只允许两个账号，覆盖首选失败、重试、混合
   供应商、快速调度、插件越界返回和范围外会话亲和。
3. 实测非流式成功/失败、重试成功、多次失败、流式正常结束、客户端中断、
   上游中断和无 usage 响应时的生命周期与 usage 事件数量、顺序和字段。
4. 验证 `modernc.org/sqlite` 或等价纯 Go 驱动在 Go 1.26、`CGO_ENABLED=0`、
   外键、部分唯一索引、WAL、`busy_timeout`、并发事务和停机备份恢复下工作。
5. 验证内嵌静态资源不会要求 Node、不会影响现有 `/management.html`，并为
   `/carpool/` 设置独立 CSP、缓存和 SPA fallback。

任一用户 Key 入口只要不能证明从认证到每次选择都携带 scope，就不得进入
第一阶段白名单。

## 不能降级的发布条件

- 车辆凭据隔离与默认拒绝。
- 四个凭证域分离。
- 成员和账号当前分配的数据库唯一约束及事务换车。
- 未知用量不当作零，重试不重复计算逻辑请求。
- 用户换车、账号移车后历史归属不漂移。
- 乘客 DTO、日志、审计和持久化事实不泄露敏感内容。
- 旧全局 Key 与 `/management.html` 行为无回归。
- 每个对用户 Key 开放的协议入口都有隔离与取消/流式测试。

## 明确推迟

- 用户自助上车、邀请、下车和换车。
- Home 用户调度与多实例共享业务数据库。
- PostgreSQL 拼车仓库。
- WebSocket/Realtime/wsrelay 用户访问，除非后续单独完成请求级撤权设计。
- 硬额度、计费、支付和严格一次记账。
- 单请求详情、提示词或响应内容查询。
- 在线热备份、长期归档与合规删除工作流。
- 合并或 fork 现有管理中心前端。

## 审查结论

第一阶段可行，且不需要另起代理服务或重写供应商执行器。最小安全路径是
在现有 CPA 进程内增加拼车领域模块，并把车辆允许账号集合做成所有本地
选择路径的强制交集。最大风险不是 SQLite 性能，而是某条重试、插件、亲和
或长连接路径绕过范围，以及把不完整 usage 误报为零；上述发布门槛专门用来
阻断这两类错误。
