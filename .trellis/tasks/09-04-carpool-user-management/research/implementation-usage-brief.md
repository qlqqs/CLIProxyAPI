# `usage-core` 实现简报

## 目标

完成实施计划阶段 0.2，并实现阶段 6 的公共、向后兼容部分：逻辑 request ID
贯穿 handler 与执行 Options，统一 usage 记录增加事件 ID、请求 ID 和明确的
`usage_known`，`EnsurePublished` 不再把未知伪装为已知零。

## 文件所有权

- `sdk/api/handlers/handlers_*` 中 request lifecycle 的最窄改动与测试。
- `sdk/cliproxy/usage/**`。
- `sdk/pluginapi/types.go` 中 Usage DTO 的向后兼容字段及相关适配/测试。
- `internal/runtime/executor/helps/usage_helpers.go` 及直接相关测试。
- 新建并维护 `research/usage-lifecycle-probe.md`。

不要实现 SQLite writer、carpool HTTP 中间件或 selector scope。`Options.RequestID`
由 `scope-core` 负责；若尚未出现，可先在自己的代码中保留清晰接点并在报告中指出，
不要同时编辑 `sdk/cliproxy/executor/types.go`。

## 硬约束

- 非拼车请求继续自动生成 request ID，已有插件生命周期行为不回归。
- 中间件预置 request ID 时 tracker 必须复用，完成回调恰好一次。
- `Publish`/`PublishFailureWithDetail` 等明确解析到 usage 的路径标记 known；
  `EnsurePublished` 标记 unknown。不得依据 Token 是否非零推断字段存在。
- 新字段保持零值兼容，并同步内部/plugin/RPC DTO 映射。
- 调查 attempt 序号是否可靠；无证据则明确保留空值，不猜测。
- 不修改 provider translator，不记录或持久化正文、header、原始错误。
- 你不是仓库中唯一工作的代理。不要回退或覆盖其他代理的改动；遇到共享文件变化，
  基于最新内容调整并报告。

## 验证

运行 usage、handler、pluginhost 和 helper 聚焦测试及 `gofmt`。报告实测事件语义、
无法覆盖的 reporter 调用点与测试结果，不要提交 Git commit。
