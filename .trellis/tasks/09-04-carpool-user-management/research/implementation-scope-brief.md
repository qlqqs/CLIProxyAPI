# `scope-core` 实现简报

## 目标

完成实施计划阶段 0.1、0.4 与阶段 1：建立代理入口到 selector 的真实调用图，
实现不可扩张的 `CredentialScope`，并证明所有本地选择、重试、插件、pinned 与
affinity 路径都不会选中范围外 `AuthID`。

## 文件所有权

- `sdk/cliproxy/executor/**` 中的 scope/Options 契约与测试。
- `sdk/cliproxy/auth/conductor_selection.go`、`scheduler.go` 及直接相关测试。
- 如确有必要，可改 `sdk/cliproxy/auth/conductor_execution.go` 和 Home dispatch
  的最窄边界，但不得改 provider executor 或 translator。
- 新建并维护 `research/request-routing-callgraph.md`。

`sdk/cliproxy/executor/types.go` 同时由本工作负责加入向后兼容的 `RequestID` 字段，
供 `usage-core` 使用；不要实现 usage 逻辑。

## 硬约束

- `nil` scope 必须保持旧行为；非 nil 空 scope 必须默认拒绝。
- scope 构造时复制、去重并排序，不能暴露可变内部集合。
- 插件候选输入先过滤、插件返回值再次校验；范围外结果不得回退全局池。
- 覆盖传统/快速 scheduler、single/mixed、retry/failover、plugin、pinned、affinity、
  Execute/Count/ExecuteStream 和 Home 拒绝路径。
- 不修改 `internal/translator/**`。
- 你不是仓库中唯一工作的代理。不要回退或覆盖其他代理的改动；遇到共享文件变化，
  基于最新内容调整实现，并在报告中说明。

## 验证

至少运行受影响包的聚焦测试和 `gofmt`。报告文件清单、调用图结论、未覆盖入口与
测试结果，不要提交 Git commit。
