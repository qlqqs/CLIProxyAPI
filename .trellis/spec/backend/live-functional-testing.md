# 隔离实机功能测试规范

## 1. 范围与触发条件

当改动影响 HTTP 协议转换、SSE 终态、取消传播、配置热加载、请求日志或上游凭据处理时，应在单元/集成测试之外执行隔离实机功能测试。测试对象必须是当前工作区源码构建的独立实例，不得对已有生产样实例执行并发、故障注入或配置写入。

可复用 `xy_gateway` 已有测试环境作为 Mock 上游，但不得为 CLIProxyAPI 测试修改其代码。测试结束后必须释放全部临时监听端口，并复查原有实例健康状态。

## 2. 命令与入口

实机驱动器入口：

```bash
LIVE_TEST_ENABLE=1 \
LIVE_TEST_BASE_URL=http://127.0.0.1:18317 \
LIVE_TEST_API_KEY=live-test-client-key \
LIVE_TEST_MODEL=mock-openai/chat-model \
LIVE_TEST_PROTOCOL=openai-chat \
LIVE_TEST_EVIDENCE_PATH=/tmp/cliproxy-live/evidence/chat.jsonl \
go test ./test -run '^TestLiveFunctional$' -count=1
```

常规测试不得发起实机请求：

```bash
go test ./...
```

未设置 `LIVE_TEST_ENABLE=1` 时，`TestLiveFunctional` 必须跳过。

## 3. 输入、输出与环境合同

必需环境变量：

| 字段 | 合同 |
|---|---|
| `LIVE_TEST_BASE_URL` | 绝对 `http`/`https` URL，Host 必须解析为 loopback |
| `LIVE_TEST_API_KEY` | 仅使用虚构客户端 Key，不写入证据 |
| `LIVE_TEST_MODEL` | 隔离配置中的模型或别名 |
| `LIVE_TEST_EVIDENCE_PATH` | JSONL 文件；创建权限必须为 `0600` |

场景变量包括 `LIVE_TEST_PROTOCOL`、`LIVE_TEST_STREAM`、`LIVE_TEST_CONCURRENCY`、`LIVE_TEST_ROUNDS`、`LIVE_TEST_EXPECT_STATUS`、`LIVE_TEST_EXPECT_OUTCOME`、`LIVE_TEST_CANCEL_AFTER_EVENTS` 和 `LIVE_TEST_CANCEL_AFTER`。

证据只能保存请求 ID、状态码、Content-Type、耗时、事件数量、终态、结果分类及脱敏摘要。不得保存 Authorization、API Key、完整 prompt、完整响应正文或可恢复的敏感 Header。HTTP 客户端不得跟随重定向到其他地址。

## 4. 校验与错误矩阵

| 条件 | 预期分类/行为 |
|---|---|
| Base URL 非 loopback | 配置加载失败，不发送请求 |
| HTTP 状态不在预期范围 | `unexpected_status` |
| 非 2xx 错误正文不是 JSON | `invalid_json` |
| 成功 JSON 无协议必需输出或 usage | `invalid_json` |
| 流式响应不是 `text/event-stream` | `invalid_content_type` |
| Chat 缺 `finish_reason`、重复/缺失 `[DONE]` | `invalid_sse` |
| Responses 顺序错误、ID 不一致、缺 `response.completed` 或含 `[DONE]` | `invalid_sse` |
| Anthropic 顺序错误、缺 `message_stop` 或含 `[DONE]` | `invalid_sse` |
| 主动取消 | `canceled`，且后续恢复请求必须成功 |
| 传输中断或超时 | 分别记录 `transport` 或 `timeout` |

不完整 SSE 即使 HTTP 状态为 200，也不得判定为成功。

## 5. Good / Base / Bad 场景

- Good：三种协议的 JSON/SSE 正常请求均满足内容、usage、事件顺序和唯一终态要求。
- Base：同步 barrier 下执行小规模并发，所有请求拥有唯一测试请求 ID，结果数量与预期一致。
- Bad：注入畸形 JSON、不完整 SSE、上游错误、首事件后取消和配置替换；验证客户端结果、日志、恢复能力和端口清理。

发现凭据泄漏、数据破坏、进程崩溃或可能影响非隔离实例时，应立即停止扩大测试范围并保全证据。

## 6. 必需测试与断言点

- 单元测试：loopback 校验、状态范围解析、证据脱敏、JSON/SSE 状态机、取消分类。
- 实机协议测试：Chat、Responses、Anthropic 的 JSON 与 SSE 正常流。
- 并发测试：同步起跑，校验总请求数、每请求唯一 ID、无永久占用。
- 故障测试：400/5xx、畸形 JSON、不完整 SSE、取消后恢复。
- 日志测试：搜索虚构上游 Key，响应正文和持久化请求日志均不得泄漏。
- 清理测试：隔离 CLIProxyAPI 与 Mock 端口全部释放；已有实例 `/healthz` 仍成功。

完成代码变更后仍需运行：

```bash
git diff --check
go test ./test -run '^TestLiveDriver' -count=1
go test ./...
go build -o test-output ./cmd/server && rm test-output
```

## 7. 错误与正确示例

### 错误

```bash
LIVE_TEST_BASE_URL=https://real-upstream.example.com \
LIVE_TEST_API_KEY="$REAL_KEY" \
go test ./test -run '^TestLiveFunctional$'
```

该做法会把实机流量和真实凭据发送到非隔离环境，且无法保证测试副作用可控。

### 正确

```bash
LIVE_TEST_ENABLE=1 \
LIVE_TEST_BASE_URL=http://127.0.0.1:18317 \
LIVE_TEST_API_KEY=live-test-client-key \
LIVE_TEST_MODEL=mock-responses/responses-model \
LIVE_TEST_PROTOCOL=openai-responses \
LIVE_TEST_STREAM=true \
LIVE_TEST_EXPECT_OUTCOME=success \
LIVE_TEST_EVIDENCE_PATH=/tmp/cliproxy-live/evidence/responses.jsonl \
go test ./test -run '^TestLiveFunctional$' -count=1
```

该做法限定 loopback、使用虚构凭据，并产生最小化、可审计的脱敏证据。

## Carpool 专项合同

Carpool 实机测试必须满足以下额外约束：

- SQLite 路径不得位于 `os.TempDir()`；使用权限为 `0700` 的持久本地隔离目录，数据库和备份保持 `0600`。
- 首个管理员只能在服务停止时通过 `--carpool-bootstrap-admin` 和真实 TTY 创建。
- 本地 HTTP 测试显式设置 `session.cookie-secure: false`；登录必须携带同源 `Origin`，写操作还必须携带 `X-Carpool-CSRF`。
- 测试主链必须由管理员创建乘客、车辆和成员，分配 runtime 候选账号，再由乘客创建 `cpk_v1_*` Key 调用标准代理入口。不能用全局 API Key 代替 Carpool 鉴权。
- 并发与取消结论同时核对 Mock 上游到达数和 Carpool 请求明细中的 `outcome`、`reason_code`、`upstream_attempted`，不能只依赖 Gin 访问日志状态。
- 修改上游配置并热加载后，必须确认现有 `car_auth_assignments` 仍能关联 runtime AuthID。若候选重新出现为未分配、账号状态变为 `unknown/stale` 或请求出现 `no_available_accounts`，应判定车辆服务已中断。
- 停止实例后执行 SQLite `integrity_check`、外键检查和 `--carpool-backup`；重启后验证 Session、API Key、车辆关系、用量、账单、审计和保留任务仍可读取。
- 请求日志秘密扫描至少覆盖乘客 API Key、创建用户时临时密码和管理员重置密码；报告只记录命中文件数量，不复制秘密值。

### Carpool 错误与正确示例

错误做法：只调用 `/healthz` 和全局 API Key，便声称 Carpool 可用。这不会经过成员关系、账号分配、并发门禁或用量记账。

正确做法：

```text
bootstrap admin
  -> admin login + Origin/CSRF
  -> create passenger/car/membership
  -> assign cand_v1_* account
  -> passenger forced password change
  -> create cpk_v1_* key
  -> call /v1/chat/completions or /v1/responses
  -> verify usage/request detail/billing/audit
  -> cancel queued requests and verify recovery
  -> backup, restart, and verify persisted state
```
