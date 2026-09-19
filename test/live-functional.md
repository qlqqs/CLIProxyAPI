# 实机功能测试驱动器

`test/live_functional_driver_test.go` 提供显式启用的 HTTP/SSE 实机驱动器。普通 `go test ./...` 不会访问外部服务；只有 `LIVE_TEST_ENABLE=1` 时，`TestLiveFunctional` 才会执行请求。

驱动器只接受 `localhost`、`127.0.0.0/8` 或 `::1` 等 loopback 地址，并拒绝跟随 HTTP 重定向，避免把测试流量误发到公网。它支持 OpenAI Chat、OpenAI Responses 和 Anthropic Messages 的 JSON/SSE 校验、同步起跑并发、按事件数或时间取消、唯一请求 ID，以及权限为 `0600` 的 JSONL 证据文件。

## 必需变量

| 变量 | 说明 |
|---|---|
| `LIVE_TEST_ENABLE=1` | 显式启用实机请求；未设置时测试会跳过 |
| `LIVE_TEST_BASE_URL` | 隔离 CLIProxyAPI 的 loopback URL，例如 `http://127.0.0.1:18317` |
| `LIVE_TEST_API_KEY` | 隔离实例的虚构客户端 Key |
| `LIVE_TEST_MODEL` | 本场景使用的下游模型或别名 |
| `LIVE_TEST_EVIDENCE_PATH` | 追加写入的 JSONL 文件路径 |

## 场景变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `LIVE_TEST_PROTOCOL` | `openai-chat` | 可选 `openai-chat`、`openai-responses`、`anthropic` |
| `LIVE_TEST_PATH` | 按协议选择 | 可覆盖为 `/v1/chat/completions`、`/v1/responses`、`/responses` 或 `/v1/messages` 等本地路径 |
| `LIVE_TEST_STREAM` | `false` | `true` 时要求 `text/event-stream` 并校验协议终态 |
| `LIVE_TEST_CONCURRENCY` | `1` | 每轮同步起跑的请求数，最大 `1000` |
| `LIVE_TEST_ROUNDS` | `1` | 重复轮数，最大 `1000` |
| `LIVE_TEST_RUN_ID` | 自动生成 | 只允许字母、数字、点、下划线和连字符 |
| `LIVE_TEST_SCENARIO` | 协议名 | 写入证据的场景标签 |
| `LIVE_TEST_REQUEST_TIMEOUT` | `30s` | 单请求安全上限 |
| `LIVE_TEST_EXPECT_STATUS` | `200-299` | 支持单值、范围和逗号组合，例如 `400-499,503` |
| `LIVE_TEST_EXPECT_OUTCOME` | `success` | 可选 `success`、`failure`、`canceled` |
| `LIVE_TEST_CANCEL_AFTER_EVENTS` | `0` | SSE 收到指定数量的数据事件后取消；用于确定性取消测试 |
| `LIVE_TEST_CANCEL_AFTER` | 空 | 经过指定时长后取消，例如 `250ms`；用于首事件前取消或挂起请求 |
| `LIVE_TEST_PROMPT` | 固定虚构提示词 | 仅用于构造请求，绝不会写入 JSONL；只应使用虚构内容 |

## 执行示例

非流式并发：

```bash
LIVE_TEST_ENABLE=1 \
LIVE_TEST_BASE_URL=http://127.0.0.1:18317 \
LIVE_TEST_API_KEY=live-test-client-key \
LIVE_TEST_MODEL=mock-chat \
LIVE_TEST_PROTOCOL=openai-chat \
LIVE_TEST_CONCURRENCY=50 \
LIVE_TEST_ROUNDS=10 \
LIVE_TEST_SCENARIO=C-001-chat-json \
LIVE_TEST_EVIDENCE_PATH="${TEST_ROOT}/evidence/c-001-chat-json.jsonl" \
go test ./test -run '^TestLiveFunctional$' -count=1
```

Responses SSE：

```bash
LIVE_TEST_ENABLE=1 \
LIVE_TEST_BASE_URL=http://127.0.0.1:18317 \
LIVE_TEST_API_KEY=live-test-client-key \
LIVE_TEST_MODEL=mock-responses \
LIVE_TEST_PROTOCOL=openai-responses \
LIVE_TEST_STREAM=true \
LIVE_TEST_CONCURRENCY=30 \
LIVE_TEST_SCENARIO=C-002-responses-sse \
LIVE_TEST_EVIDENCE_PATH="${TEST_ROOT}/evidence/c-002-responses-sse.jsonl" \
go test ./test -run '^TestLiveFunctional$' -count=1
```

收到首个 SSE 数据事件后取消：

```bash
LIVE_TEST_ENABLE=1 \
LIVE_TEST_BASE_URL=http://127.0.0.1:18317 \
LIVE_TEST_API_KEY=live-test-client-key \
LIVE_TEST_MODEL=mock-slow-chat \
LIVE_TEST_PROTOCOL=openai-chat \
LIVE_TEST_STREAM=true \
LIVE_TEST_CANCEL_AFTER_EVENTS=1 \
LIVE_TEST_EXPECT_OUTCOME=canceled \
LIVE_TEST_SCENARIO=E-009-cancel-after-event \
LIVE_TEST_EVIDENCE_PATH="${TEST_ROOT}/evidence/e-009-cancel.jsonl" \
go test ./test -run '^TestLiveFunctional$' -count=1
```

预期上游错误：

```bash
LIVE_TEST_ENABLE=1 \
LIVE_TEST_BASE_URL=http://127.0.0.1:18317 \
LIVE_TEST_API_KEY=live-test-client-key \
LIVE_TEST_MODEL=mock-error \
LIVE_TEST_PROTOCOL=anthropic \
LIVE_TEST_EXPECT_STATUS=500-599 \
LIVE_TEST_EXPECT_OUTCOME=failure \
LIVE_TEST_SCENARIO=E-002-upstream-unavailable \
LIVE_TEST_EVIDENCE_PATH="${TEST_ROOT}/evidence/e-002.jsonl" \
go test ./test -run '^TestLiveFunctional$' -count=1
```

## 校验规则

- 每轮所有 goroutine 先到达 barrier，再统一放行。
- 每个请求携带唯一的 `X-Live-Test-Request-ID`；响应中的 `X-Request-Id`、`OpenAI-Request-Id` 或 `X-CPA-Trace-Id` 会作为可选关联字段保存。
- JSON 成功响应按协议检查输出文本、终态字段和可解析 usage。
- Chat SSE 要求唯一 `[DONE]`、至少一个 `finish_reason`，且 usage 不重复。
- Responses SSE 要求 `response.created` 在前、至少一个 delta、唯一且 ID 一致的 `response.completed` 在后，并拒绝 `[DONE]` 泄漏。
- Anthropic SSE 要求 `message_start` 在前、至少一个 `content_block_delta`、唯一 `message_stop` 在后，并拒绝 OpenAI `[DONE]` 泄漏。
- 非 2xx 错误响应要求是 JSON；传输失败、超时、取消和协议错误会分别分类。

## 证据与敏感信息

JSONL 每行只包含场景、请求 ID、状态码、Content-Type、首事件/总耗时、事件计数、终态、结果分类和结构化响应摘要。摘要只保存经过安全字符约束的 JSON 顶层字段名、字节数、短哈希，或 SSE 事件类型与字节数；不安全的响应元数据会改存短哈希，不保存响应正文。

驱动器不会把 `Authorization`、API Key 或完整提示词写入证据，并在写入前再次检查 API Key 与提示词是否意外出现。不要把真实凭据或真实用户内容放入任何 `LIVE_TEST_*` 变量。

聚焦回归测试：

```bash
go test ./test -run '^TestLiveDriver' -count=1
```
