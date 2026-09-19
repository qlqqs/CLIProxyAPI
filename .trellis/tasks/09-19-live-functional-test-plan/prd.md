# CLIProxyAPI Carpool 优先实机功能测试方案

## 目标

设计并执行一套可重复、隔离的实机测试，以发现 **Carpool** 的功能缺陷为首要目标。核心验收必须经过真实管理员/乘客 HTTP 会话、车辆与成员关系、runtime 账号分配、乘客 API Key、标准代理入口、用量与账单、并发排队、保留任务、审计以及 SQLite 生命周期。

OpenAI Chat、Responses、Anthropic 和 SSE 驱动器仍在范围内，但定位为 Carpool 上游链路、并发取消和故障注入的支撑工具，不再作为本任务的主要交付结论。

## 背景与已确认事实

- 当前开发机具备 Go 1.26、Docker、12 核 CPU、31 GiB 内存和较高文件描述符上限，能够执行并发与长连接测试。
- CLIProxyAPI 当前已有运行实例监听 `8317`，并使用现有账号目录与数据库；本任务不得对该实例进行破坏性测试。
- Carpool 包含管理员/乘客角色、Session/CSRF、车辆/成员/账号分配、月/5 小时/7 天额度、用户与账号并发、乘客 API Key、同步记账、请求明细、保留任务、审计、备份与恢复。
- Carpool SQLite 会拒绝系统临时目录中的数据库路径；专项根目录必须位于非 `os.TempDir()` 的本地隔离目录。
- `xy_gateway` 的现有 Vitest 全局环境可提供 OpenAI Chat、Responses、Anthropic、错误、慢响应、不完整流和断连等 Mock 行为。
- 用户明确要求：需要 Mock 时直接启动 `xy_gateway` 现有测试环境，不新增或修改 `xy_gateway` 代码。
- 当前工作区存在用户未提交修改，测试和检查不得覆盖、回滚或擅自修改这些文件。

## 范围内

### P0：Carpool 主验收

1. 使用当前工作区源码构建独立 CLIProxyAPI，并在 `127.0.0.1:18318` 启动 Carpool 实例。
2. 使用非系统临时目录的独立 SQLite、auth、日志和证据目录；通过真实 TTY bootstrap 首个管理员。
3. 覆盖管理员与乘客完整会话：Origin、CSRF、强制改密、角色隔离、密码重置、禁用/启用和 Session 生命周期。
4. 覆盖用户、车辆、席位、成员、月/5 小时/7 天额度、用户并发和账号并发。
5. 从 runtime 候选分配车辆账号，由乘客创建 `cpk_v1_*` Key，经 `/v1/models` 和标准代理入口产生真实 usage、billing、request detail 和 audit 数据。
6. 覆盖慢流排队、等待取消、已执行取消、队列/槽位释放、恢复请求和业务终态分类。
7. 覆盖请求明细、聚合用量、价格状态、账期、保留设置、preview/confirm/idempotency 和审计分页。
8. 覆盖 SQLite `integrity_check`、外键检查、备份、同库重启及持久状态恢复。
9. 覆盖上游配置热加载后既有 `car_auth_assignments` 与 runtime AuthID 的关联连续性。
10. 执行日志与证据秘密扫描，不把 Cookie、CSRF、临时密码、乘客 Key、上游 Key 或完整正文写入提交内容。

### P1：支撑性协议与生命周期验证

1. 复用 `xy_gateway` 现有测试生命周期，验证 Mock 健康、日志采集和安全停止。
2. 使用显式启用的 Go live driver 验证 Chat、Responses、Anthropic 的 JSON/SSE、同步起跑、取消和不完整流。
3. 验证配置热加载、优雅停止、重启、WebSocket/`wsrelay` 专项测试和相关 `-race`。
4. 对 Mock 不覆盖的原生 Gemini、WebSocket 或浏览器项目明确记录环境缺口，不伪造通过。

## 范围外

- 修改 CLIProxyAPI 产品代码来掩盖测试发现；缺陷只记录、最小复现和建议修复。
- 修改 `xy_gateway` 代码或为其新增独立 Mock 启动脚本。
- 使用真实付费上游或真实 OAuth 凭据。
- 对当前 `8317` 实例及其账号、数据库、日志执行写入、并发或故障注入。
- 生产容量规划、长期 soak test 和硬件极限基准。
- 在没有浏览器自动化 Host/依赖时声称已完成真实浏览器点击或截图。

## 约束

- Carpool 根目录必须位于非系统临时目录，例如 `${HOME}/.local/state/cliproxy-carpool-live-${RUN_ID}`；目录权限 `0700`，数据库、备份和含敏感会话材料的证据权限 `0600`。
- 测试端口、数据库、auth、Cookie、日志和 PID 文件必须与当前 `8317` 实例隔离。
- 乘客代理请求必须使用本轮创建的 `cpk_v1_*` Key，不能用全局 API Key 替代 Carpool 鉴权链路。
- 登录必须携带同源 `Origin`；写操作必须同时携带 Session Cookie 和 `X-Carpool-CSRF`。
- 并发验证使用 barrier、首事件信号或业务状态，不以固定 `sleep` 作为正确性依据。
- 并发与取消必须交叉核对客户端结果、Mock 上游到达数和 Carpool 请求明细中的 `outcome`、`reason_code`、`upstream_attempted`。
- 方案必须区分自动化断言、人工证据核验和环境无法覆盖项。
- 证据可在受限隔离目录短期保存真实测试 Session/临时凭据以驱动流程，但不得复制到 Markdown、Git diff 或最终报告；审阅后按清理流程销毁。

## 验收标准

- [x] Carpool 被明确设为主要验收对象，通用协议测试降为支撑层。
- [x] 独立 Carpool 实例使用非 `/tmp` SQLite 和虚构凭据，不污染当前 `8317` 实例。
- [x] 管理员/乘客会话、Origin/CSRF、强制改密和角色隔离完成实机验证。
- [x] 用户、车辆、成员、账号分配、配额和并发策略完成实机验证。
- [x] 乘客 API Key 经标准代理入口产生真实 usage、billing 和 request detail。
- [x] 慢流排队、取消、业务终态和恢复请求完成交叉验证。
- [x] 保留任务、审计、SQLite 完整性、备份和同库重启完成验证。
- [x] Carpool 实机报告记录通过项、环境限制和 P1 热加载账号失联缺陷。
- [x] live driver 默认跳过、限制 loopback、拒绝重定向并生成最小化 `0600` JSONL 证据。
- [x] 规范记录 Carpool 专项合同、秘密扫描和生命周期要求。
- [ ] 真实浏览器点击与截图；当前无浏览器自动化 Host 且仓库无 Playwright 依赖，记录为环境阻塞。
- [x] Carpool 证据目录保存 Git HEAD、工作区补丁、Go 版本和被测二进制 SHA256，并完成摘要复核。

## 关键决策

- 主要结论以 `research/carpool-live-run-2026-09-19.md` 为准；通用协议结果见 `research/live-run-2026-09-19.md`。
- 被测对象是当前工作区源码构建的隔离实例；当前 `8317` 仅作只读健康对照。
- `xy_gateway` 仅通过现有测试生命周期启动，不修改其源码。
- Carpool 数据根使用非系统临时目录；通用协议驱动器的无状态证据可使用受控临时目录。
- 发现凭据泄漏、数据库损坏、请求串用户或进程失控时立即停止扩大测试范围并保全证据。
