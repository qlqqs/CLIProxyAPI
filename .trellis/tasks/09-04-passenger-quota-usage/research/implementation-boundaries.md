# 实施边界与证据

## 现有可复用链路

| 能力 | 代码证据 | 本次使用方式 |
| --- | --- | --- |
| 浏览器改密门禁 | `internal/carpool/httpapi/api.go:95`、`:217`；`internal/carpool/web/assets/app.js:253`、`:835` | 保留鉴权与导航限制，只补原因、数据保留及重新登录说明 |
| 同车账号投影 | `internal/carpool/service/control.go:1037`；`internal/carpool/httpapi/api.go:398` | 在安全 DTO 上增加原生配额，不序列化整个 Auth |
| 原生配额来源 | `sdk/cliproxy/auth/quota_signals.go:15`；`internal/carpool/runtime/account_status.go:9` | 复用 Claude/Codex 被动观测；配额陈旧性单独用 `Quota.ObservedAt` 判断，阈值 5 分钟 |
| 请求身份与 scope | `internal/carpool/httpapi/proxy.go:113`；`internal/carpool/store/sqlite/authorization.go:17` | 在现有授权事务中检查当前成员账本、固定账期，不另起鉴权体系 |
| 上车或换车 | `internal/carpool/service/control.go:754`；`internal/carpool/store/sqlite/repository.go:378` | 原事务增加必填限额；调额单独操作，不能调用换车制造新关系 |
| 用量来源 | `sdk/cliproxy/usage/manager.go:22`；`internal/runtime/executor/helps/usage_helpers.go:379` | 复用 CPA `Record` 与原有逻辑请求 ID，不重新统计响应正文 |
| 互斥 Token 桶 | `sdk/cliproxy/usage/accounting.go:42`、`:253` | 采用 canonical breakdown，防止缓存和推理重复计费 |
| 幂等持久化 | `internal/carpool/store/sqlite/repository.go:719` | 同一个事件首次写入时同步更新账期汇总，重复事件不能增加费用 |
| 启动恢复 | `internal/carpool/store/sqlite/repository.go:638`；`internal/carpool/module.go:81` | 保留 `incomplete` 恢复，金额覆盖不能将缺失事件视为完整零费用 |
| 原有清理 | `internal/carpool/store/sqlite/retention.go:27`；`internal/carpool/retention.go:13` | 复用有界批处理与生命周期，增加明细/汇总隔离和有效保留设置 |

## 需要补齐的技术契约

1. `accounting.Writer` 的 `HandleUsage` 目前走 512 条有界队列，满队列直接丢弃并增加指标，
   见 `internal/carpool/accounting/writer.go:100`、`:174`。全局 usage manager 还另有异步分发。
   金额确认不能依赖这两级延迟或丢弃路径，需为拼车请求建立同一 `Record` 的可靠同步观察入口，
   事务确认费用后新请求才读到该金额；不改变旧全局 Key 的异步行为。
2. `sdk/cliproxy/auth/conductor_execution.go:495` 在候选账号选定后循环实际模型，
   `:505` 的插件还可能改写请求，`:513` 才进入 executor。计价校验必须在最终模型解析之后、
   实际上游执行之前；覆盖流式、非流式、401 刷新重试、模型池、mixed provider 和插件执行分支。
   本地缺价错误不能惩罚账号健康状态，不能扩大 `CredentialScope`。
3. 旧 `Detail.InputTokens`、`CachedTokens`、`ReasoningTokens` 不具有统一的相加语义。
   以 `TokenBreakdown` 为准；未知供应商或 `Quality` 非完整的数据不能猜测拆分。
4. `Record` 包含请求/响应 Service Tier，但公共 `Detail` 目前没有完整缓存 TTL 与多模态计价
   拆分。不同价格确实依赖缺失维度时标记费用未知，保留已知 Token，不伪装完整金额。
5. `runtime/routes.go` 允许 `/v1/responses`、`/v1/chat/completions`、`/v1/messages` 和 Gemini
   通用生成接口，这些接口并不等于纯文本负载。本次不扩大路由、不添加独立非 Token 计价，
   也不因名称为“文本”而假设所有费用都可还原。
6. 当前报表直接从明细聚合，清理会改变这些历史 Token 报表。新增金额账期汇总必须是独立
   持久事实；普通清理不能通过重新 `SUM(usage_events)` 归零或降低拦截金额。

## 价格数据参考

- 参考文件：`../sub2api/backend/resources/model-pricing/model_prices_and_context_window.json`。
  2026-09-05 本地快照含 196 个顶层键，不能假设等于 LiteLLM 的完整目录。
- 使用 JSON 解析核对了 `gpt-5.4` 的基础、缓存读取、flex、priority、272k 长上下文字段，
  以及 `claude-sonnet-4-5-20250929` 的基础、缓存读写、1 小时写入及 200k 长上下文字段。
- 上述为字段和计价行为参考，不是复制 `sub2api` 的 `pricing_service.go` 或
  `billing_service.go`。内置数据需保留实际来源、许可和内容哈希。
- 不使用相似模型模糊匹配，不引入镜像中的渠道倍率或账户余额业务。必要单价缺失和明确零价
  必须在 JSON 解码时区分；历史金额不跟随目录刷新。

## 规范优先级

`backend/database-guidelines.md` 的“没有迁移”描述针对旧核心存储，不能用于拼车 SQLite。
本模块以 `backend/carpool-guidelines.md`、现有递增 SQL 和 checksum runner 为准。
此处只记录适用范围，不在本任务顺手改写无关存储规范。
