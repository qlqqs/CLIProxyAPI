# 关停上下文过期缺陷复盘

## 1. 根因类别

- **类别**：E（隐式假设）与 D（测试覆盖缺口）。
- **具体原因**：`Service.Run` 在启动时创建 30 秒 shutdown context，再由长期 defer
  捕获。代码隐含假设服务会在 30 秒内退出；真实服务运行更久后，关停开始时 context
  已经过期，writer 排空和 WAL checkpoint 立即失败。

## 2. 为什么此前没有发现

1. 单元测试中的服务生命周期远短于 30 秒，症状不会自然出现。
2. 原实现从语法上看具备 timeout 和 cancel，常规静态检查无法判断预算起点错误。
3. 首次真实浏览器验收持续超过 30 秒后发送 Ctrl-C，稳定复现
   `context deadline exceeded`，才提供了能区分“资源关闭慢”和“context 预先过期”
   的证据。

## 3. 防复发机制

| 优先级 | 机制 | 具体动作 | 状态 |
|---|---|---|---|
| P0 | 架构 | 在 deferred closure 内创建关停 context | 已完成 |
| P0 | 测试 | 断言关停回调收到的 context 未过期且有未来 deadline | 已完成 |
| P1 | 集成 | 真实进程运行超过旧预算后 SIGINT，检查 writer/WAL 关闭日志 | 已完成 |
| P1 | 文档 | 新增服务生命周期 code-spec 和 review 矩阵 | 已完成 |

## 4. 系统性扩展

- **相似问题**：所有“先创建短 timeout，稍后在 defer/信号回调中使用”的代码都可能
  提前耗尽预算。当前仓库其他 `context.WithTimeout` 调用经搜索均位于即时操作或实际
  Close 路径，没有发现同类长期捕获。
- **设计改进**：由触发关停的边界拥有预算创建；子资源只接收调用方 context。
- **流程改进**：生命周期变更除单元测试外，必须做一次运行时间超过旧 deadline 的
  真实进程关停冒烟。

## 5. 知识固化

- [x] 新增 `.trellis/spec/backend/lifecycle-guidelines.md`。
- [x] 在后端规范索引中加入生命周期检查入口。
- [x] 保留 `TestShutdownOnRunExitCreatesFreshContext` 回归测试。
- [x] 记录真实关停前后日志证据。
