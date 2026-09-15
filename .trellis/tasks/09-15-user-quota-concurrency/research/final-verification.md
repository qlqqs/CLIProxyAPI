# 最终实施与验收记录

## 状态与范围

2026-09-15：实现和独立审查完成，全部最终门禁通过；未提交、未部署、未接触生产数据库。仍待用户确认 Phase 3.4 提交计划，之后才能归档本任务、记录会话；其他历史任务不处理。

已实现月额度兼容、跟随唯一账号真实窗口的 5h/7d 用户额度、用户/账号共享并发、默认全局 10 个等待请求、出队完整复核、持久化额度与去重账本，以及管理端和乘客端展示。周期未知只跳过对应短周期，并显示待同步；不伪造零费用。

## 独立审查与修复

- Codex 绝对 reset 优先于相对 reset，消除 map 遍历顺序依赖。
- 瞬时预检错误不能再次授权产生孤立 `in_progress`；出队重查等待期间变化的账号策略。
- 请求共享完成锁覆盖正文读取失败/取消的 HTTP 兜底；无执行记拒绝或取消，有可能执行但无法确认完成记 `incomplete`，不让重复终态污染记账门禁。
- 以上均有回归测试。无剩余已确认产品缺陷；未扩展 SDK 生命周期接口或新增网络超时。

## 最终质量门禁

由独立 `trellis-check` 执行并报告通过，主会话核对日志；此后仅补充文档和隔离 HTTP 验收，没有产品代码修改。

| 命令/检查 | 结果 |
| --- | --- |
| `gofmt -w .` | 完成 |
| `go test ./...` | 通过 |
| `go test -race ./internal/carpool/... ./internal/api ./sdk/cliproxy ./sdk/api/handlers/...` | 通过，SQLite 约 135.2 秒 |
| `go vet ./...` | 通过 |
| `go build -o test-output ./cmd/server && rm test-output` | 通过 |
| `CGO_ENABLED=0` 服务编译与产物清理 | 通过 |
| `go test -tags carpool_browser ./internal/carpool -run '^$'` | 通过 |
| `node --check internal/carpool/web/assets/app.js` | 通过 |
| UI Node 测试 | 78/78 通过 |
| `git diff --check` | 通过 |

本机原始日志：`/tmp/quota-check-{all,race,vet,ui}-final.log`。首次全量测试遇到一次 Pion DataChannel 偶发失败；隔离重跑与最终全量均通过，无关 WebRTC 代码未改动。

服务集成使用真实 SQLite、单连接和同步屏障，覆盖共享 Key/账号、取消、准入时间和价格冻结、禁用/撤销/换账号/改额度/记账故障后的出队拒绝。存储测试覆盖迁移、重启、明细清理后事件重放、冻结价格、窗口修正与月重置隔离。

## 浏览器与真实 HTTP 验收

仅启用 `CARPOOL_BROWSER=1 CARPOOL_BROWSER_LIMITS=1` 的隔离 fixture、假账号与假 executor，没有真实上游凭据/调用。Paseo 浏览器无可用 host，改用本机 headless Playwright Chromium。

- 管理员实际编辑 5h/7d 额度、用户及账号并发，保存后核对 API。
- 实际乘客登录/首次改密/生成请求：响应 `200, 200, 403`，最后原因为 `five_hour_quota_exhausted`。
- 已确认费用月/5h/7d 都是 `$0.09`，不是把一份账单扣三次；周期 reset 未因用量更新而移动。
- 缺失窗口 DTO 数值与 reset 为 null；界面显示待同步和覆盖提示。
- 桌面与 390px 移动视口检查，无水平溢出、无页面脚本错误。保存失败等交互由自动化 UI 测试覆盖，不声称逐项手工验收。
- 证据：`browser-results.json` 与 `screenshots/` 三张截图。

补充队列验收使用 Node `http.request` 的 `agent:false` 创建 12 个独立连接，避免浏览器自身 HTTP/1 连接池先行排队使服务端队列无法填满。测试 gate 只存在于 browser build-tag fixture 中：保持 1 个执行请求，10 个在服务端等待，剩余 1 个返回 `429 concurrency_queue_full`；释放后 11 个均返回 200。结果见 `queue-results.json`；验收后已释放 gate 并正常关闭 fixture，`TestCarpoolBrowserFixture` 最终 PASS。这是独立 HTTP 验收，不冒称浏览器能同时发送 12 个请求。

## 已知边界

单实例、一车一账号；窗口待同步存在用户已接受的短期限额保护空档；在途请求可以超额，不强行终止。费用与事件收据窄账本保守持久保留，不随普通明细清理。没有分布式限流、上游超时或自动部署。
