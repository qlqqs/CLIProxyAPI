# 2026-09-19 Carpool 实机功能测试报告

## 结论

根据用户纠正，本轮将测试重点调整为 Carpool。使用当前工作区构建的 CLIProxyAPI 独立实例、独立 SQLite、虚构账号和 `xy_gateway` 现有 Mock，完成了管理员、乘客、车辆、账号分配、配额、API Key、真实代理请求、用量记账、并发排队、取消、请求明细、保留任务、审计、备份和重启恢复测试。

核心业务链路整体可用，但发现一项 **P1 Carpool 可用性缺陷**：上游配置热加载导致 runtime AuthID 变化后，已有车辆账号分配仍显示在列表中，却立即失去可用账号；所有乘客请求返回 403，必须由管理员删除旧分配并重新分配新候选才能恢复。

## 环境

- 日期：2026-09-19（UTC）
- CLIProxyAPI：`127.0.0.1:18318`
- 被测 Git HEAD、工作区补丁、Go 版本和二进制 SHA256：见证据目录中的 `build-*` 文件；二进制摘要复核通过
- Carpool 数据根：`/home/qlqq/.local/state/cliproxy-carpool-live-20260919T0534Z`
- SQLite：上述目录的 `data/carpool.db`
- xy_gateway App：`127.0.0.1:19720`
- xy Mock AI：`19999`
- 现有实例 `8317`：仅作健康对照
- 全部用户、密码、API Key 和上游凭据均为虚构值
- 未修改 `xy_gateway` 代码

Carpool SQLite 不能位于系统临时目录，因此没有复用 `/tmp/cliproxy-live-*` 作为数据库目录。数据库、Cookie、证据及生成凭据均保存在权限为 `0700` 的隔离根目录中。

## 已通过场景

### 身份、会话和权限

- 通过真实 TTY 执行 `--carpool-bootstrap-admin`，完成迁移并创建首个管理员。
- 缺少同源 `Origin` 的登录返回 403。
- 管理员登录返回 201、HttpOnly Session Cookie 和 CSRF Token。
- 缺少 `X-Carpool-CSRF` 的写操作返回 403。
- 管理员创建两名乘客；临时密码只在创建响应中出现。
- 乘客临时密码登录后，访问工作台接口返回 403，必须先改密。
- 改密返回 204，旧 Session 立即失效；新密码可重新登录。
- 乘客访问管理员接口返回 403，管理员访问乘客专属接口返回 403。
- 管理员重置乘客密码后旧 Session 失效；乘客 API Key 按设计仍可调用代理。
- 用户被禁用后 Session 与现有 API Key 立即失效；重新启用不会自动恢复旧 Key。

### 用户、车辆、成员和配额

- 创建车辆及 `seat_limit=1` 成功。
- 第一名乘客上车成功，第二名乘客上车返回 409，座位上限生效。
- 月限额、5 小时限额、7 天限额和用户并发限制可写入并正确返回。
- 清除短周期限额后字段恢复为 null。
- 用户列表游标分页正常，非法 `limit=201` 返回 422。

### 账号分配与代理链路

- 管理员账号候选使用不透明 `cand_v1_*`，响应未暴露原始 AuthID 或上游 Key。
- 候选账号分配到车辆成功，可设置账号并发上限为 1。
- 乘客创建 `cpk_v1_*` API Key；后续 Key 列表不返回明文 Token。
- 使用乘客 API Key 调用 `/v1/models` 成功，只返回车辆可用模型。
- 使用乘客 API Key 调用 `/v1/chat/completions` 成功，xy Mock 返回固定文本和 10/15/25 usage。
- 账号文件安全列表成功；不支持的 OpenAI API Key JSON 和缺少 access token 的 Codex JSON 均返回 422，未写入账号目录。

### 用量、价格和账单

- 乘客车辆页、成员用量页、账号状态页均返回 200。
- 正常 Chat 请求计入 10 input、15 output、25 total tokens。
- 请求明细列表包含请求结果、账单状态、金额和事件数量。
- 单请求详情包含 provider、上游模型、usage、定价结果和 `cost_usd=0.0000275`。
- 价格目录不可用或形状不匹配时使用内置 LiteLLM 快照；状态接口明确返回 `source=bundled-litellm`。
- 请求日志扫描未发现乘客 API Key、创建时临时密码或管理员重置密码进入持久化请求日志。

### 并发、排队与取消

- 账号并发限制设置为 1 后，同步发起 3 条慢流。
- xy Mock 只收到 1 条上游请求，证明其余请求没有越过账号并发门禁。
- 客户端取消后，1 条已到上游请求记录为 `canceled/client_canceled`；2 条等待请求记录为 `rejected/request_canceled` 且 `upstream_attempted=false`。
- 恢复普通上游并重新分配账号后，后续请求成功，没有观察到永久占槽。

### 保留、审计、备份和重启

- 保留天数可从配置默认 90 改为数据库覆盖 180，再恢复为配置默认。
- `reset_current_period` 预览成功；缺少确认返回 422；确认执行返回 202。
- 重复确认返回相同完成结果，任务查询结果一致。
- 审计事件包含 retention job 的 actor、action、target 和结果；审计游标分页正常，非法游标返回 422。
- 服务停止后执行 `--carpool-backup` 成功；备份权限为 `0600`，原 DB 与备份 SHA-256 一致。
- 使用同一 SQLite 重启后，管理员 Session、乘客 Session、车辆关系、新 API Key、用量和审计均可继续读取。
- `PRAGMA integrity_check=ok`，外键违规数为 0。

## 缺陷

### P1：上游配置热加载后 Carpool 已分配账号静默失效

**操作**

1. 将一个 OpenAI-compatible 候选账号分配给车辆并验证乘客请求成功。
2. 原地修改同一凭据的 `base-url`，触发正常配置热加载。
3. 再次使用同一乘客 API Key 请求模型。

**实际结果**

- runtime 生成了新的 AuthID，并出现新的未分配 `cand_v1_*` 候选；
- 原车辆账号分配仍出现在 `items` 中，页面数据没有明确表示该分配已失联；
- 乘客账号状态变为 `unknown/stale`；
- 所有代理请求返回 403，并以 `no_available_accounts` 记录；
- 恢复原 `base-url` 又产生另一 AuthID，仍需再次人工删除和分配。

**影响**

普通的上游地址、代理或凭据相关配置调整会中断该车辆全部乘客服务。管理员容易看到“账号仍已分配”而误认为配置正常，只有删除旧分配并选择新候选才能恢复。

**建议**

为配置型凭据提供跨非身份字段变更的稳定账号标识，或在 runtime AuthID 变化时迁移 Carpool 分配。至少应在已分配 AuthID 不存在时把账号明确标记为 orphaned/unavailable，并提供可审计的重新绑定操作，不能仅返回笼统 403。

## 观察与限制

- 5 小时/7 天限额刚创建时显示 `pending_sync`；在同步基线建立前代理请求会被拒绝。本轮把它视为保守门禁设计，并在并发测试前清除了短周期限额。
- 慢流测试的一个请求已经发送 200/SSE Header 后客户端取消，因此 Gin 访问日志显示 200；Carpool 业务明细正确记录为 canceled。判断业务结果应以 Carpool 请求明细为准，不能只看访问日志状态。
- xy Mock、HTTP Proxy 和 SOCKS Mock 监听 `*`，不是 loopback；这是现有 `xy_gateway` 测试夹具的安全限制，本轮未修改其代码。测试结束后必须精确停止进程并确认端口释放。
- Paseo 当前没有浏览器自动化 Host，本仓库也没有可直接导入的 Playwright 依赖，因此本轮未执行真实浏览器点击和截图；已完成真实静态入口、HTTP API 以及 Node 前端回归。

## 证据

Carpool 实机证据根目录：

```text
/home/qlqq/.local/state/cliproxy-carpool-live-20260919T0534Z
```

主要证据：

- `evidence/admin-ops-summary.json`
- `evidence/admin-usage-requests.body`
- `evidence/admin-usage-after-cancel.body`
- `evidence/account-list-after-reload.body`
- `evidence/account-list-restored.body`
- `evidence/database-state.json`
- `evidence/database-sha256.txt`
- `evidence/secret-scan-summary.txt`
- `evidence/listeners.txt`
- `evidence/build-git-head.txt`
- `evidence/build-worktree.patch`
- `evidence/build-binary.sha256`
- `evidence/build-binary-verify.txt`
- `logs/server.log`
- `logs/server-restart.log`

## 清理结果

Carpool 隔离实例和 `xy_gateway` 测试环境均已停止。确认 `18318/19720/19999/19997/19996` 无监听，现有 `127.0.0.1:8317/healthz` 仍返回 200。一次性 Cookie、CSRF、乘客 API Key 和含一次性密码的原始证据已经删除或替换为脱敏版本；保留证据文件权限统一为 `0600`。
