# 拼车用户 Key 请求路由调用图

## 1. 目的与结论

本文冻结第一阶段拼车用户 Key 可访问的代理路由、共同调用链、凭据范围传播点和必须
提前拒绝的旁路。本文只描述普通代理入口；`/carpool/` 业务页面与 API、
`/management.html`、Management API、健康检查和 OAuth 回调不属于用户 Key 代理
白名单。

核心结论：普通 HTTP 非流式、Count 和流式 handler 最终都可以进入
`AuthManager`，且 `Execute`、`ExecuteCount`、`ExecuteStream` 的每轮重试都按值复用同一
份 `executor.Options`。因此，不可扩张的 `CredentialScope` 可以在统一 selection
eligibility 中强制执行。以下入口不能依赖这条保证：direct executor plugin、Responses
WebSocket、Codex live/realtime、Codex alpha search、wsrelay、Home dispatch 和当前全局
模型列表。第一阶段对这些旁路采用显式拒绝或专用安全投影，不允许静默回退到全局
账号池。

当前通用 scope 核心已经实现，但用户 Key 的授权快照注入、路由白名单中间件、scoped
模型投影和 direct executor plugin 拒绝尚属于后续阶段。在这些条件完成并通过逐入口
测试前，不得启用用户 Key 代理流量。

## 2. 普通 HTTP 调用图

```text
internal/api/server_routes.go
  路由组挂载 AuthMiddleware
        |
        v
internal/api/server_middleware.go
  access.Manager.Authenticate(request)
  -> Gin: userApiKey / accessProvider / accessMetadata
        |
        v
协议 handler（OpenAI / Claude / Gemini / Codex HTTP）
        |
        v
BaseAPIHandler.GetContextWithCancel
  -> 保留 request context 与逻辑 request ID
        |
        v
executeWithAuthManagerFormats / executeCountWithAuthManager /
executeStreamWithAuthManagerFormats
  -> 构造一次 executor.Options
  -> 注入 RequestID、CredentialScope 和非敏感 caller scope
        |
        +---- direct executor plugin target ----> 第一阶段 scoped 请求拒绝
        |
        v
AuthManager.Execute / ExecuteCount / ExecuteStream
  -> 每轮 retry/failover 按值复制同一 Options
        |
        v
pickNext / pickNextMixed
  -> authSelectionEligibility.allows
       = CredentialScope + auth kind + credential policy + free-auth policy
  -> fast scheduler 或 legacy selector
  -> single provider 或 mixed providers
  -> plugin scheduler（候选输入过滤 + 返回值复核）
  -> pinned auth / affinity 只能缩小或命中 scope 内候选
        |
        v
provider executor -> usage reporter -> lifecycle completion
```

### 2.1 入站身份与上下文

- `internal/api/server_routes.go:62-128` 给 OpenAI、Codex direct 和 Gemini 路由组挂载
  `AuthMiddleware`。
- `internal/api/server_middleware.go:159-175` 调用 `access.Manager.Authenticate`，并把
  `Principal`、provider 标识和 metadata 写入 Gin 上下文。
- `sdk/api/handlers/handlers.go:395` 的 `GetContextWithCancel` 从请求上下文继承 request
  ID，并把 Gin context 带入后续执行链。
- 用户 Key provider 必须只把稳定、非敏感标识放入 `Principal` 和 metadata；不得把
  明文 Key 作为 caller scope 或日志字段。

### 2.2 Options 构造与重试传播

- 非流式：`sdk/api/handlers/handlers_execution.go:45-109`。
- Count：`sdk/api/handlers/handlers_execution.go:118-165`。
- 流式：`sdk/api/handlers/handlers_stream.go:295` 起。
- `sdk/api/handlers/handlers_execution.go:52-54`、`:121-123` 与
  `sdk/api/handlers/handlers_stream.go:308` 证明 direct executor plugin 会在
  `AuthManager` 之前分流，必须单独拒绝 scoped 请求。
- `sdk/cliproxy/auth/conductor_execution.go:121`、`:180`、`:232` 分别是 Execute、
  Count 和 Stream 入口；`:139`、`:198`、`:255` 的重试循环按值复用 Options。

授权中间件必须在请求开始事务中冻结账号 ID 集合，handler 只能从可信 context 构造
`CredentialScope`。header、query 或 body 中任何同名客户端字段都不得覆盖该值。scope
建立后，管理员移除成员或账号不会扩大或替换当前请求的快照；运行时禁用、模型支持、
冷却和 credential policy 仍可继续缩小候选。

## 3. 统一选择边界

`sdk/cliproxy/auth/conductor_selection.go:80` 的
`authSelectionEligibilityForRequest` 是范围、kind、credential policy 和 free-auth 条件的
组合入口。`allows` 对每个候选先执行 `CredentialScope.Allows(Auth.ID)`。

| 分支 | 源码锚点 | scope 契约 |
| --- | --- | --- |
| legacy single | `conductor_selection.go:1500` 附近 | 候选先过滤，selector 返回值再次校验 |
| fast single | `conductor_selection.go:1760` 附近 | scheduler 候选使用共同 eligibility，返回值再次校验 |
| legacy mixed | `conductor_selection.go:1823` 附近 | 各 provider 候选均过滤，返回值再次校验 |
| fast mixed | `conductor_selection.go:1939` 附近 | mixed scheduler 使用共同 eligibility，返回值再次校验 |
| plugin scheduler | `conductor_selection.go:760-850` | 输入只含 scope 内候选；插件返回范围外 AuthID 时返回 `credential_scope_violation`，不得回退 |
| scheduler predicate | `sdk/cliproxy/auth/scheduler.go:585` | 内建调度器只接收 `eligibility.allows` 的候选 |
| pinned auth | selection metadata 路径 | scope 外 pin 视为无可用候选，不得强制命中 |
| session affinity | selector/scheduler affinity 路径 | scope 外旧亲和视为 miss，可在 scope 内重新选择 |
| Home | `conductor_home.go:947-953` | scope 非 nil 时在 Home dispatch 前返回 `credential_scope_unsupported` |
| Antigravity credits fallback | `conductor_home.go:1252-1266` | fallback 候选同样经过共同 eligibility |

`CredentialScope` 定义在 `sdk/cliproxy/executor/types.go`：

- `nil` 表示旧请求，不实施范围限制，保持现有全局 Key 行为。
- 通过构造器得到的非 nil 空集合拒绝所有 AuthID。
- 构造时去空、去重、排序并复制；`IDs()` 返回副本，调用方不能修改内部集合。
- plugin/selector 返回 scope 外凭据属于安全违规，响应使用稳定错误码且不得泄露完整
  AuthID 或候选列表。

## 4. 路由分类

白名单必须按 HTTP 方法和规范化路由模式精确匹配。不能因为 Gin 使用通配参数就开放
整个 `*action`，也不能把同一路径的 GET Upgrade 与 POST HTTP 混为一类。

### 4.1 完成安全接线后允许

| 方法与路径 | 协议 | 开放条件 |
| --- | --- | --- |
| `GET /v1/models` | OpenAI | 使用 scope 内账号生成模型投影，不读取全局可用性作为结果 |
| `POST /v1/chat/completions` | OpenAI | 非流式和 HTTP streaming 均贯通同一 scope |
| `POST /v1/completions` | OpenAI | 非流式和 HTTP streaming 均贯通同一 scope |
| `POST /v1/messages` | Claude | 非流式和 HTTP streaming 均贯通同一 scope |
| `POST /v1/responses` | OpenAI Responses | 仅普通 HTTP/SSE，不含 GET WebSocket |
| `POST /backend-api/codex/responses` | Codex direct | 仅普通 HTTP/SSE，不含 GET WebSocket |
| `GET /v1beta/models` | Gemini | 使用 scope 内账号生成模型投影 |
| `GET /v1beta/models/{model}` | Gemini | 只允许精确的单模型读取语义 |
| `POST /v1beta/models/{model}:generateContent` | Gemini | 普通 HTTP 生成 |
| `POST /v1beta/models/{model}:streamGenerateContent` | Gemini | HTTP/SSE 流式生成 |
| `POST /v1beta/interactions` | Gemini | 通过 AuthManager 执行；必须覆盖 native interaction 分支测试 |

以上路由还必须满足三个共同条件：

1. 用户 Key 鉴权成功后，同一事务写入逻辑请求与完整账号快照，再构造 scope。
2. model router 若返回 direct executor plugin target，在进入插件前对 scoped 请求返回
   协议兼容 403；provider target 仍必须走 AuthManager。
3. 每个协议的非流式、流式、retry/failover 和插件调度测试均未出现 scope 外 AuthID。

### 4.2 第一阶段明确拒绝

| 方法与路径 | 拒绝原因 |
| --- | --- |
| `POST /v1/images/generations`、`POST /v1/images/edits` | 首期未完成独立入口、模型路由及用量覆盖验证 |
| `/v1/videos*` 与 `/openai/v1/videos*` | 包含创建、编辑、扩展、查询与内容下载等旁路，首期不支持 |
| `POST /v1/messages/count_tokens` | Count 首期不进入用户 Key 白名单，避免把估算请求混入逻辑用量契约 |
| `GET /v1/responses`、`GET /backend-api/codex/responses` | Responses WebSocket；Upgrade 后具有独立长连接生命周期 |
| `POST /v1/responses/compact`、`POST /backend-api/codex/responses/compact` | 未完成逐入口 scope 与用量验证 |
| `POST /v1/alpha/search`、`POST /backend-api/codex/alpha/search` | handler 自行选择 Auth 并直接发起 HTTP，不走共同 Execute 链 |
| `/v1/live*`、`/v1/realtime*` | Codex live/realtime 自行选择 OAuth，且包含 WebSocket/sideband 生命周期 |
| wsrelay 动态路径（默认 `/v1/ws`） | 动态挂载 handler，不经过 BaseAPIHandler 与共同 selection 链 |
| `/v1beta/models/{model}:countTokens` 及其他 `*action` | Gin 通配路由存在，但不在精确白名单 |
| 其他未知代理路由、方法或 Upgrade 请求 | 默认拒绝；白名单不得通过前缀或模糊匹配扩张 |

对已成功认证的用户 Key，拒绝必须发生在 handler、WebSocket Upgrade、Home dispatch、
direct executor plugin 和任何上游连接之前，并同步记录一条 `rejected` 逻辑请求事实。
旧全局 Key 仍按 nil scope 走现有行为，不受此用户白名单影响，也不写拼车报表。

## 5. 旁路证据与处理方式

### 5.1 Direct executor plugin

BaseAPIHandler 在构造 AuthManager 请求前检查 model router 的 executor plugin target：
非流式位于 `handlers_execution.go:52`，Count 位于 `handlers_execution.go:121`，流式位于
`handlers_stream.go:308`。该插件直接执行，统一 selector 无法兜底。

处理：在三处分流的共同 helper 上读取可信授权快照；scope 非 nil 时返回稳定 403，并
增加非流式、Count、流式测试。不能把全局凭据候选或 scope 明文传给 executor plugin。

### 5.2 Responses WebSocket

`sdk/api/handlers/openai/openai_responses_websocket.go:267` 在独立 handler 内 Upgrade。
它不等价于 POST Responses 的 HTTP/SSE 执行链。

处理：路由白名单按“方法 + 路径 + 是否 Upgrade”匹配，在 Upgrade 前拒绝用户 Key。

### 5.3 Codex live/realtime

`internal/client/codex/live/websocket.go:58-59`、`live.go:217-221`、
`sideband.go:369-376` 自行构造 Options 并调用 `selectOAuth`。这些长连接还包含 client
secret、sideband 和 call 控制路由。

处理：第一阶段全部拒绝用户 Key；不能只依赖 Home 内部的 scope 拒绝，因为本地
`SelectAuthByKind` 仍可能在缺少 scope 时选择全局账号。

### 5.4 Codex alpha search

`internal/api/server_routes.go:311-380` 自行读取请求、构造无授权快照的 Options，通过
`SelectAuthWithCredentialPolicy` 或 Home 选择账号，随后直接发 HTTP。

处理：第一阶段在调用 handler 前拒绝用户 Key，不尝试在该专用实现中复制 scope 与
用量接线。

### 5.5 wsrelay

`internal/api/server_routes.go:503-535` 允许动态挂载 WebSocket handler，默认路径是
`/v1/ws`。conditional auth 只负责入站认证，不构造 AuthManager Options。

处理：用户 Key 路由中间件必须识别 Upgrade 和动态 wsrelay 路径，并在调用外部
handler 前拒绝。拼车关闭或旧全局 Key 请求维持原行为。

### 5.6 模型列表

`GET /v1/models` 与 Gemini 模型路由当前从全局 registry/Home 状态生成结果，不经过
一次真实 selection。即使执行请求受 scope 保护，全局模型列表仍可能泄露其他车辆的
账号能力。

处理：根据请求开始时冻结的 AuthID 集合和只读 AuthManager 模型投影取并集；再应用
现有模型别名、排除与协议格式化。空 scope 返回空列表或协议定义的无可用结果，绝不
回退全局列表。

### 5.7 Home

通用核心已在 `conductor_home.go:951` 对 enforced scope fail-closed，确保误入 Home
时不会调用远端 dispatch。用户入口仍应更早返回路由不支持错误，以便记录准确的
`rejected` 事实并避免产生与普通执行错误混淆的诊断。

## 6. 发布阻断测试

首期白名单启用前必须至少通过以下测试：

- 两辆车使用不相交账号集合，对每个白名单路由分别执行非流式与流式请求，记录的每个
  AuthID 都属于请求开始快照。
- 对 Execute、Count 和 Stream 强制首选失败并触发多轮 retry/failover；Options 的
  request ID 和 scope 在所有执行调用中相同。
- 分别注入恶意 legacy selector、plugin scheduler、pinned AuthID 和旧 affinity；均
  不能执行范围外账号，plugin 越界不回退全局池。
- model router 返回 provider target 时仍受 scope；返回 executor plugin target 时在
  插件调用前拒绝。
- 对所有拒绝路由分别测试 Bearer、`X-Api-Key`、`X-Goog-Api-Key`、`key`、
  `auth_token`，确认在 Upgrade/handler/上游连接前返回 403。
- scoped `/models` 只包含允许账号能够服务的模型；空 scope 和账号消失场景不回退
  全局 registry。
- nil scope 的旧全局 Key 在普通 HTTP、HTTP streaming、WebSocket/realtime、Home 和
  wsrelay 上保持现有行为，且不产生拼车请求或用量事实。

只要任一拟开放入口绕过共同 eligibility、direct executor plugin 未 fail-closed、模型
投影泄露全局能力，或任一重试执行了 scope 外 AuthID，该入口就不得加入运行时白名单。
