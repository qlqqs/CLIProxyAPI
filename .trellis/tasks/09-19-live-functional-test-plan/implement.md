# CLIProxyAPI Carpool 优先实机功能测试执行计划

> 状态：Carpool 主链与支撑性协议测试已于 2026-09-19 执行。以下清单按用户最新要求将 Carpool 置于主路径；通用协议矩阵仅作为上游和并发夹具验证。

## 阶段 0：冻结版本、隔离目录和保护现有环境

- [x] 创建非系统临时目录 `${CARPOOL_TEST_ROOT}`，建议 `${HOME}/.local/state/cliproxy-carpool-live-${TEST_RUN_ID}`。
- [x] 设置根目录、auth、data、evidence 为 `0700`，SQLite、备份及含会话材料的证据为 `0600`。
- [x] 确认当前 `8317` 实例只作健康对照，不发送测试负载或读取其认证材料。
- [x] 确认 `18318/19720/19999/19998/19997/19996` 无冲突；冲突时更换整组端口，不终止未知进程。
- [x] 保存测试配置、PID、日志和证据到隔离根目录。
- [x] 将通用构建阶段保存的 Git HEAD、工作区补丁、Go 版本和被测二进制 SHA256 复制到 Carpool 证据目录，并重新校验二进制摘要。

## 阶段 1：基线、构建和专项测试

- [x] 执行 `go test ./...`。
- [x] 执行 Carpool 相关 Go 测试和重点 `-race`。
- [x] 从当前工作区构建隔离 `cli-proxy-api`。
- [x] 运行 Node 前端回归。
- [x] 保存 Go/进程/端口环境证据。

## 阶段 2：启动 xy_gateway 与本地价格夹具

- [x] 在隔离根目录生成临时 `tsx` 启动器，导入 `xy_gateway/tests/globalSetup.ts`。
- [x] 使用自定义测试端口启动现有 global setup，不设置 `TEST_REAL_API=true`，不修改 `xy_gateway` 源码。
- [x] 验证测试服务 `/welcome`、Mock `/models` 和 `XY_TEST_ENV_READY`。
- [x] 价格目录未访问公网；本轮配置的本地 URL 返回非价格目录形状，模块按设计回退到内置 LiteLLM 快照，并通过 pricing 状态接口确认。
- [x] 保存 xy_gateway 日志起始快照；停止时调用原有 teardown 并精确清理记录的进程。

## 阶段 3：Carpool 初始化和身份会话

- [x] 生成仅包含虚构上游凭据的 Carpool 配置，监听 `127.0.0.1:18318`。
- [x] 将 SQLite 固定到 `${CARPOOL_TEST_ROOT}/data/carpool.db`，不得位于 `/tmp` 等 `os.TempDir()` 子目录。
- [x] 设置 `carpool.session.cookie-secure: false`、同源 `trusted-origins` 和独立 auth 目录。
- [x] 在服务停止时通过真实 TTY 执行 `--carpool-bootstrap-admin`。
- [x] 启动实例并验证 `/healthz`、`/carpool` 静态入口和数据库权限。
- [x] 验证登录 Origin、Session Cookie、CSRF、强制改密、角色隔离和旧 Session 失效；本轮未单独执行显式注销按钮流程。

## 阶段 4：用户、车辆、成员、账号和配额

- [x] 创建管理员/乘客流程所需用户，验证临时密码只在创建响应中出现。
- [x] 创建车辆并验证 `seat_limit`。
- [x] 创建成员关系并验证月、5 小时、7 天额度和用户并发限制。
- [x] 列出 runtime 候选，确认使用不透明 `cand_v1_*` 且不暴露 AuthID/上游 Key。
- [x] 分配正常账号并设置账号并发限制。
- [x] 验证账号文件安全投影和非法导入，不连接真实 OAuth。

## 阶段 5：乘客 API Key、代理、用量和账单

- [x] 乘客改密后重新登录并创建 `cpk_v1_*` Key；列表只返回引用和状态。
- [x] 使用乘客 Key 调用 `/v1/models`，确认只看到车辆可用模型。
- [x] 使用乘客 Key 调用标准 Chat/Responses 入口，确认请求经过车辆成员关系和账号分配。
- [x] 核对 Mock 固定 usage、Carpool 聚合、请求明细、账单、价格状态和审计。
- [x] 验证禁用用户、重置密码、Session 与 API Key 生命周期。

## 阶段 6：并发排队、取消和恢复

- [x] 将车辆切换到慢账号并把账号并发限制设为 1。
- [x] 使用 barrier/首事件信号同步启动慢流，不以固定 `sleep` 作为唯一判据。
- [x] 取消已执行和等待请求，核对 Mock 上游到达数。
- [x] 核对 Carpool 请求明细的 `outcome`、`reason_code` 和 `upstream_attempted`，不只看 Gin 200/SSE Header。
- [x] 恢复普通账号并执行成功请求，证明队列和并发槽位收敛。

## 阶段 7：保留、审计、SQLite 和热加载

- [x] 验证保留设置的配置默认、数据库覆盖和恢复默认。
- [x] 验证 preview、缺少确认、confirm、重复确认和任务查询。
- [x] 验证审计分页和非法游标。
- [x] 停止实例后执行 `PRAGMA integrity_check`、外键检查和 `--carpool-backup`。
- [x] 使用同一 SQLite 重启，验证 Session、API Key、车辆关系、用量、账单、审计和保留任务。
- [x] 修改上游配置并热加载，重新检查已有分配与 runtime 候选关联。
- [x] 记录 P1：runtime AuthID 变化后旧分配静默失联，乘客请求变为 `no_available_accounts`，需人工重绑。

## 阶段 8：日志、安全和 UI

- [x] 扫描请求日志中的乘客 Key、创建临时密码、重置密码和虚构上游 Key；报告只记录文件命中数，不复制秘密值。
- [x] 确认含 Cookie、CSRF、临时密码或明文乘客 Key 的原始 HTTP 证据保持 `0600` 且不进入 Git。
- [x] 运行现有 Node 前端回归和静态入口冒烟。
- [ ] 使用真实浏览器执行桌面/窄屏点击和截图；当前无浏览器自动化 Host 且仓库无 Playwright 依赖，记录为环境阻塞。

## 阶段 9：支撑性通用协议和 live driver

- [x] 使用隔离 `18317` 实例验证 Chat、Responses、Anthropic 的非流式与流式基线。
- [x] 使用显式启用的 Go live driver 执行同步起跑、SSE 终态、首事件后取消和不完整流。
- [x] 驱动器默认不进入普通 `go test ./...`，只接受 loopback，拒绝重定向，JSONL 权限为 `0600`。
- [x] 执行 50 并发 × 2 轮、三协议 SSE 并发、取消后恢复和重点 `-race`。
- [x] 记录通用协议发现的凭据反射、畸形 JSON、原子替换 watcher 和不完整 SSE 风险；这些不替代 Carpool 主报告。

## 阶段 10：清理、复测与报告

- [x] 先对 CLIProxyAPI、价格服务和 xy_gateway 测试环境发送 SIGTERM，并等待优雅退出。
- [x] 仅在等待窗口后按记录 PID/进程组精确强制清理，不使用宽泛 `pkill`。
- [x] 确认测试端口释放，当前 `8317` 实例仍健康。
- [x] 输出 `research/carpool-live-run-2026-09-19.md` 作为主要报告。
- [x] 输出 `research/live-run-2026-09-19.md` 作为支撑性协议报告。
- [ ] 证据审阅完成后删除包含明文测试 Session、临时密码和乘客 Key 的隔离目录。

## 验证命令集合

```bash
# 代码门禁
gofmt -d test/live_functional_driver_test.go test/live_functional_driver_unit_test.go
git diff --check
go test ./test -run '^TestLiveDriver' -count=1
go test ./internal/carpool/... -count=1
go test ./...
go build -o test-output ./cmd/server && rm test-output

# 重点 race；执行时根据实际受影响包补充
go test -race ./internal/api/... ./internal/logging/... ./internal/wsrelay/... ./internal/carpool/... ./sdk/cliproxy/auth/...

# Carpool 健康和静态入口
curl -fsS http://127.0.0.1:18318/healthz
curl -fsS http://127.0.0.1:18318/carpool/ >/dev/null

# 进程与端口证据
ss -lntp
ps -o pid,ppid,etime,%cpu,%mem,rss,vsz,cmd -p "$(cat "${CARPOOL_TEST_ROOT}/cli-proxy.pid")"
```

## 风险点与回滚点

- **误用系统临时数据库**：Carpool 配置生成后检查绝对路径；若位于 `os.TempDir()` 下立即停止。
- **误用真实凭据**：只从新隔离配置和本轮响应读取虚构凭据；发现真实 Token 立即停止并销毁明文副本。
- **误连公网**：上游和价格目录必须为 loopback；日志或监听证据不符合时立即停止。
- **敏感证据残留**：原始登录/Header/Key 响应只保存在 `0700` 根目录下的 `0600` 文件，不提交、不粘贴到报告，审阅后删除。
- **账号热加载失联**：热加载后必须重新核对 `items`、`candidates` 和乘客请求；失败时保留旧/新候选证据并按管理员重绑恢复。
- **并发驱动器自身错误**：先以 1 和 2 并发校准，再扩大；客户端、Mock 和 Carpool 明细必须交叉一致。
- **磁盘增长**：每阶段检查隔离根目录大小，达到 5 GiB 停止。
