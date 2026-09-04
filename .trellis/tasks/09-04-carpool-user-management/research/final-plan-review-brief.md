# 最终规划独立审查任务

请继续作为本任务的反方审查者，读取当前工作区中的最新文件：

- `.trellis/tasks/09-04-carpool-user-management/prd.md`
- `.trellis/tasks/09-04-carpool-user-management/design.md`
- `.trellis/tasks/09-04-carpool-user-management/implement.md`
- `.trellis/tasks/09-04-carpool-user-management/research/current-architecture.md`
- `.trellis/tasks/09-04-carpool-user-management/research/remaining-decisions-review.md`
- 两份 JSONL 上下文清单

只做规划审查，不修改文件，不实现业务代码。重点检查：

1. 三份文档是否相互一致，是否仍有未解决的用户所有产品决策。
2. 车辆账号隔离是否覆盖初选、重试、故障转移、快速/传统/mixed/plugin 调度、
   pinned/affinity、模型列表、Home 和未开放长连接。
3. 逻辑请求与 usage event 两层模型、`usage_known`、流式取消、重试实际 Token、
   崩溃窗口和历史归属是否自洽且可测试。
4. SQLite 表、部分唯一索引、席位并发、迁移、保留与停机备份是否可执行。
5. 四个凭证域、密码/会话/Key 生命周期、CSRF、DTO 和日志审计是否有泄密或越权。
6. 无 Node 的嵌入前端、现有 `/management.html` 和旧全局 Key 兼容是否冲突。
7. 实施顺序、回滚点、验证命令和 AC1 至 AC14 是否完整且没有不必要的首期扩张。
8. JSONL 是否是有效、非空且相关的 spec/research 清单。
9. 上轮指出的六项缺口是否已经完整闭合：认证失败与认证后授权拒绝的事实边界、
   多标签页 CSRF、`incomplete` 正式枚举、请求账号归属快照、快照提交后的撤权
   语义，以及可信代理登录限速。
10. Bearer、`X-Api-Key`、`X-Goog-Api-Key`、`key`、`auth_token`、匿名访问和
    exclusive provider 冲突是否在 PRD、设计、实施与测试中一致。
11. 上次复审指出的 schema/运维问题是否闭合：车辆用量通过请求快照联表且索引
    可执行；父子事实按各自 cutoff 安全清理；启动只恢复前一进程遗留请求；格式化
    不覆盖无关用户改动。
12. 最后四项低风险契约是否已一致：请求与用量共用 90 天配置、
    `SameSite=Strict`、登录 Origin 强校验、父快照已清理时迟到 usage 安全丢弃计数。
13. 角色和 Key 轮换是否闭合：角色创建后不可改，管理员不能乘车或持有用户代理
    Key；轮换是先创建、切换、再撤销，不引入原子 rotate 端点。
14. 路由和认证边界是否闭合：静态资源不与 API 路由冲突，未知 API 不回退 HTML；
    多位置不同凭证在所有 provider 前拒绝；限速准入发生在 Argon2id 前。

请按严重级别给出具体 `文件:章节` 发现和最小修订建议；如果不存在发布阻断问题，
明确说明“可以提交给用户进行最终规划批准”。最后以 `done` 结束。
