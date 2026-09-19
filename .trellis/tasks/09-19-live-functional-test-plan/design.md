# CLIProxyAPI 实机功能测试设计

## 1. 测试目标与原则

本方案以 **Carpool 功能正确性** 为主要验收对象，通用协议链路仅作为 Carpool 上游、并发和故障注入的支撑层。测试优先判断身份、车辆/成员/账号、配额、持久化、记账、并发和异常恢复是否正确；吞吐量只用于产生并发交错，不作为容量承诺。

核心原则：

1. **隔离优先**：不写入或压测当前 `8317` 实例，不复用其数据库、账号目录和日志。
2. **证据优先**：每个结论至少保留客户端结果、CLIProxyAPI 日志、Mock 日志或数据库快照中的一种；关键并发结论至少保留两类证据。
3. **确定性优先**：并发场景使用开始屏障、连接就绪、终态计数和最终收敛断言，不以固定 `sleep` 作为唯一依据。
4. **先正常后异常**：先证明基线链路可用，再注入 4xx/5xx、断流、取消和热加载，避免把环境错误误判为产品缺陷。
5. **Mock 边界清晰**：`xy_gateway` Mock 的响应能力是测试夹具，不将其不支持的协议或字段当成 CLIProxyAPI 缺陷。

## 2. 被测对象与测试拓扑

### 2.1 被测构建

从当前工作区直接构建，并记录可复现信息：

```bash
TEST_RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
TEST_ROOT="/tmp/cliproxy-live-${TEST_RUN_ID}"
mkdir -p "${TEST_ROOT}"/{bin,auths,logs,evidence,configs,data}
git rev-parse HEAD > "${TEST_ROOT}/evidence/git-head.txt"
git status --short > "${TEST_ROOT}/evidence/git-status.txt"
git diff --binary > "${TEST_ROOT}/evidence/worktree.patch"
go version > "${TEST_ROOT}/evidence/go-version.txt"
go build -o "${TEST_ROOT}/bin/cli-proxy-api" ./cmd/server
sha256sum "${TEST_ROOT}/bin/cli-proxy-api" > "${TEST_ROOT}/evidence/binary.sha256"
```

工作区有未提交修改时，`git-head.txt`、`git-status.txt` 和 `worktree.patch` 共同标识真实被测版本。

### 2.2 进程和端口

| 组件 | 建议地址 | 数据目录 | 用途 |
|---|---|---|---|
| 现有 CLIProxyAPI | `127.0.0.1:8317` | 现有运行目录 | 只读健康对照 |
| Carpool 隔离 CLIProxyAPI | `127.0.0.1:18318` | `${CARPOOL_TEST_ROOT}`（非 `/tmp`） | 主要被测实例 |
| 通用协议支撑实例 | `127.0.0.1:18317` | `${TEST_ROOT}` | Mock 和 live driver 支撑验证 |
| xy_gateway 测试服务 | `127.0.0.1:19720` | xy_gateway 测试 DB | 由现有全局测试环境启动 |
| xy_gateway Mock AI | `127.0.0.1:19999` | xy_gateway `log/test` | 上游 Mock |
| xy_gateway Mock HTTP 代理 | `127.0.0.1:19997` | 无 | 代理场景 |
| xy_gateway Mock SOCKS5 | `127.0.0.1:19996` | 无 | 代理场景 |

建议使用非默认测试端口，避免与其他测试会话的 `9720/9999/9997/9996` 冲突。

### 2.3 数据流

```text
管理员/乘客 HTTP 驱动 + cpk_v1_* Key
        |
        v
Carpool CLIProxyAPI 127.0.0.1:18318
        |
        +-- Session / CSRF / 车辆 / 成员 / 账号分配
        +-- 标准代理入口 --> xy Mock :19999
        +-- usage / billing / request detail / audit
        +-- 非 /tmp 的隔离 Carpool SQLite

通用 live driver --> CLIProxyAPI :18317 --> xy Mock :19999

xy_gateway Test App :19720 仅由其原有 globalSetup 管理，
用于证明整个测试环境已正常初始化；不作为 CLIProxyAPI 的被测对象。
```

## 3. xy_gateway 测试环境启动方式

不修改 `xy_gateway` 代码。执行阶段在 `/tmp` 生成一次性启动器，直接调用其现有 `tests/globalSetup.ts`：

```bash
XY_ROOT=/home/qlqq/workspace/xy_gateway
cat > "${TEST_ROOT}/xy-test-env.ts" <<'TS'
import { setup, teardown } from "/home/qlqq/workspace/xy_gateway/tests/globalSetup.ts";

let closing = false;
async function close(code: number) {
    if (closing) return;
    closing = true;
    try { await teardown(); } finally { process.exit(code); }
}

process.on("SIGINT", () => void close(130));
process.on("SIGTERM", () => void close(143));
process.on("uncaughtException", error => {
    console.error(error);
    void close(1);
});
process.on("unhandledRejection", error => {
    console.error(error);
    void close(1);
});

await setup();
console.log("XY_TEST_ENV_READY");
setInterval(() => {}, 60_000);
TS

(
  cd "${XY_ROOT}"
  TEST_PORT=19720 \
  TEST_BASE_URL=http://127.0.0.1:19720 \
  TEST_UPSTREAM_MOCK_URL=http://127.0.0.1:19999 \
  TEST_PROXY_PORT=19997 \
  TEST_SOCKS_PORT=19996 \
  TEST_CLEANUP=false \
  npx tsx "${TEST_ROOT}/xy-test-env.ts"
) >"${TEST_ROOT}/logs/xy-test-env.log" 2>&1 &
echo $! > "${TEST_ROOT}/xy-test-env.pid"
```

健康确认：

```bash
grep -q 'XY_TEST_ENV_READY' "${TEST_ROOT}/logs/xy-test-env.log"
curl -fsS http://127.0.0.1:19720/welcome >/dev/null
curl -fsS http://127.0.0.1:19999/models | jq . >/dev/null
```

Mock 日志同时检查：

```bash
tail -n 100 "${XY_ROOT}/log/test/mockerServer.log"
```

若全局环境启动失败，应先停止测试，不允许绕过它临时另写 Mock。

## 4. CLIProxyAPI 测试配置

### 4.1 基础配置

首个配置文件只包含虚构凭据：

```yaml
host: "127.0.0.1"
port: 18317
auth-dir: "/tmp/cliproxy-live-<RUN_ID>/auths"
api-keys:
  - "live-test-client-key"
remote-management:
  allow-remote: false
  secret-key: "live-test-management-key"
  disable-control-panel: true
carpool:
  enabled: false
debug: true
request-log: true
logging-to-file: false
usage-statistics-enabled: true
proxy-url: ""
request-retry: 1
max-retry-credentials: 0

openai-compatibility:
  - name: "xy-openai"
    prefix: "mock-openai"
    base-url: "http://127.0.0.1:19999"
    request-retry: 0
    api-key-entries:
      - api-key: "mock-openai-key-a"
      - api-key: "mock-openai-key-b"
    models:
      - name: "gpt-3.5-turbo"
        alias: "chat-model"
        force-mapping: true

codex-api-key:
  - api-key: "mock-codex-key"
    prefix: "mock-responses"
    base-url: "http://127.0.0.1:19999"
    request-retry: 0
    models:
      - name: "gpt-4o"
        alias: "responses-model"
        force-mapping: true
        is-compat: true

claude-api-key:
  - api-key: "mock-claude-key"
    prefix: "mock-claude"
    base-url: "http://127.0.0.1:19999"
    request-retry: 0
    models:
      - name: "claude-3-haiku-20240307"
        alias: "claude-model"
        force-mapping: true
```

实际执行前必须通过一次启动试探确认三个 executor 生成的上游路径与 Mock 路由匹配。若配置字段因当前源码发生变化，应以当前 `config.example.yaml` 和启动错误为准修正临时配置，但不得修改产品默认配置。

### 4.2 异常配置变体

通过复制基础配置并仅修改 `base-url` 注入不同故障：

| 变体 | OpenAI base URL | Codex base URL | Claude base URL |
|---|---|---|---|
| 正常 | `http://127.0.0.1:19999` | 同左 | 同左 |
| 业务错误 | `.../chat/completions/error` | `.../responses/error` | `.../messages/error` |
| 不可用 | `.../chat/completions/unavailable` | 使用不可监听端口 | 使用不可监听端口 |
| 畸形正文 | `.../chat/completions/malformed` | 无现成对应项 | `.../messages/malformed` |
| 不完整流 | `.../chat/completions/incomplete` | `.../responses/incomplete` | `.../messages/incomplete` |
| 慢流/挂起 | `.../chat/completions/slow` | `.../responses/slow` | `.../messages/slow` |
| 中途断连 | `.../chat/completions/disconnect` | 无现成对应项 | 无现成对应项 |
| 完成后挂起 | 无现成对应项 | `.../responses/complete-then-hang` | 无现成对应项 |

Mock 通过 `url.includes(...)` 选择行为，因此变体前缀后追加 executor 固定路径仍能命中对应处理器。每次切换后必须从 CLIProxyAPI 日志观察热加载成功，并用单请求重新建立基线。

### 4.3 进程启动与证据

```bash
"${TEST_ROOT}/bin/cli-proxy-api" \
  --config "${TEST_ROOT}/configs/base.yaml" \
  --local-model \
  >"${TEST_ROOT}/logs/cli-proxy.log" 2>&1 &
echo $! > "${TEST_ROOT}/cli-proxy.pid"

curl -fsS http://127.0.0.1:18317/healthz | tee "${TEST_ROOT}/evidence/healthz.json"
```

使用 `--local-model` 避免远程模型更新引入非确定性。

## 5. 测试分层与执行门禁

### L0：环境和构建门禁

任何一项失败都停止后续实机测试：

1. `go test ./...` 通过；失败时先记录既有回归，不继续把实机异常归为新缺陷。
2. 当前工作区构建成功。
3. 四个测试端口未被占用。
4. xy_gateway 环境和 Mock 健康。
5. 隔离 CLIProxyAPI `/healthz` 返回 200。
6. 日志目录、PID 文件和配置文件都位于 `${TEST_ROOT}`。

### L1：核心功能冒烟

| ID | 场景 | 核心断言 |
|---|---|---|
| F-001 | 无 API Key 调用 `/v1/models` | 返回 401/403，响应不泄漏配置或上游凭据 |
| F-002 | 合法 API Key 调用 `/v1/models` | 200；只出现配置模型/别名；不重复、不出现禁用项 |
| F-003 | Chat 非流式 | 200；结构合法；文本、finish reason、usage 可解析；模型映射符合配置 |
| F-004 | Chat 流式 | SSE 顺序正确；有终态；客户端可在终态后结束；usage 不重复 |
| F-005 | Responses 非流式 | 200；`response` 结构合法；输出文本和 usage 可解析 |
| F-006 | Responses 流式 | 有 `response.created`、delta、`response.completed`；序号和终态一致 |
| F-007 | Anthropic 非流式 | 200；content、stop reason、usage 符合 Anthropic 结构 |
| F-008 | Anthropic 流式 | `message_start` 到 `message_stop` 顺序合法；无 OpenAI `[DONE]` 泄漏 |
| F-009 | Header/模型重写 | Mock 捕获的模型是上游名；下游响应模型按 `force-mapping` 返回别名；真实客户端 Key 不转发到上游 |
| F-010 | 错误路由/未知模型 | 返回稳定 4xx；不发送到错误上游；日志能关联 request ID |

### L2：协议和边界

| ID | 场景 | 核心断言 |
|---|---|---|
| P-001 | OpenAI Chat 入口到 OpenAI-compatible 上游 | 字段、工具和 usage 无结构性丢失 |
| P-002 | Responses 入口到 Chat-compatible 上游 | CLIProxyAPI 完成双向转换，终态可解析 |
| P-003 | Anthropic 入口到 OpenAI-compatible 上游 | system/messages/tool 结构转换正确 |
| P-004 | OpenAI 入口到 Claude 上游 | Claude Mock 收到合法 Messages 请求，下游仍为 OpenAI 结构 |
| P-005 | 流式工具调用 | argument 分片可重组，tool call ID/index 稳定 |
| P-006 | 空消息、Unicode、长字段和非法 JSON | 合法边界成功；非法输入稳定返回 400，不 panic |
| P-007 | 路径别名 `/responses` 与 `/v1/responses` | 两条入口结果和错误语义一致 |
| P-008 | `/responses/compact` | 支持时验证非流式契约；Mock 不匹配时记录为夹具缺口，不伪造通过 |

### L3：异常和恢复

| ID | 注入 | 通过标准 |
|---|---|---|
| E-001 | 上游 400 | 下游状态/错误对象合理；不错误重试；不冷却无关凭据 |
| E-002 | 上游 503 | 按配置执行重试/切换；尝试次数不超限；最终错误可解释 |
| E-003 | malformed JSON | 返回 502 类上游协议错误；进程保持健康；记录错误证据 |
| E-004 | SSE 无终态 | 不伪报成功；日志标记失败或不完整；连接和槽位最终释放 |
| E-005 | socket 中途断开 | 客户端收到可识别失败；后续请求正常 |
| E-006 | 首事件后持续挂起 | 客户端取消后服务端 goroutine/连接/并发槽位收敛 |
| E-007 | Responses 完成后上游仍挂起 | 客户端已取得完整终态时行为符合协议设计；不得重复终态/usage |
| E-008 | 客户端在首 token 前取消 | 上游请求被取消；无成功用量；后续请求不被占槽 |
| E-009 | 客户端在若干 token 后取消 | 日志区分取消与上游错误；无 panic/死锁；记账符合实际规则 |
| E-010 | Mock 整体停止再恢复 | 停止时快速失败或等待符合设计；恢复后无需重启被测进程即可成功 |

### L4：并发功能正确性

并发不是单纯检查“全成功”，而是验证状态机和资源收敛。

#### C-001 同步起跑

- 规模：10、50、100 个非流式请求。
- 方法：所有 goroutine/进程先等待 barrier，再同时放行。
- 断言：响应数量等于请求数量；每个请求只有一个终态；无重复 request ID；成功/失败总和一致；服务保持健康。

#### C-002 流式并发

- 规模：10、30 个流式请求。
- 断言：每条流内部事件顺序正确，流之间允许交错；所有流最终完成或按注入规则失败；无串流和模型串号。

#### C-003 混合协议

- 同时运行 Chat、Responses、Anthropic 的流式与非流式请求。
- 断言：不同协议响应 Content-Type、终态、usage 和错误形状互不污染。

#### C-004 取消风暴

- 启动一批慢流，确认已收到首事件后同时取消 50%。
- 断言：取消请求释放资源；未取消请求仍完成；新请求可立即进入；日志数量和终态分类正确。

#### C-005 热加载竞争

- 持续发请求时原子替换配置，在正常与错误变体之间切换，再恢复正常。
- 断言：进程不退出；完整请求使用一致配置快照；允许切换窗口内出现预期旧/新结果，但不得出现解析半状态、空凭据或 panic。

#### C-006 多凭据调度

- 同一 provider 配置两个虚构 API Key，发送不少于 40 个请求。
- 通过 Mock 捕获和 CLI 调度日志确认两个凭据均被使用，且单次请求只选一个凭据。
- 某凭据进入错误/冷却后，其余凭据继续服务；恢复后重新参与。
- 若 Mock 对目标端点没有保存请求历史，则以 CLI 的脱敏 auth ID/调度日志为主，不要求暴露原始 Key。

### L5：日志、用量和安全

| ID | 检查 | 通过标准 |
|---|---|---|
| L-001 | request ID 关联 | 客户端 Header、应用日志、错误日志/请求日志可关联同一次请求 |
| L-002 | 数量一致性 | 发起数 = 成功 + 失败 + 客户端取消；不能静默丢请求或重复记录 |
| L-003 | usage | Mock 固定 usage 在下游和统计中不重复、不跨请求串号 |
| L-004 | 错误日志 | 400、503、畸形响应、不完整流的记录类型和状态合理 |
| L-005 | 敏感信息扫描 | 日志中不存在测试 API Key、Authorization、管理 Key 和真实环境凭据 |
| L-006 | 请求正文策略 | 普通日志不意外暴露完整提示词；开启 request-log 时符合既有配置契约 |
| L-007 | 日志轮换/上限 | 在小上限配置下产生大量错误，确认清理不删除当前活跃文件且进程不中断 |

扫描示例只使用本次虚构标记：

```bash
rg -n 'live-test-client-key|live-test-management-key|mock-openai-key|mock-codex-key|mock-claude-key|Authorization: Bearer' \
  "${TEST_ROOT}/logs" "${TEST_ROOT}/auths/logs" && exit 1 || true
```

不得扫描或打印现有运行实例的真实认证文件内容。

### L6：配置、生命周期和持久化

| ID | 场景 | 通过标准 |
|---|---|---|
| S-001 | 合法配置热加载 | 新模型/别名生效；旧请求不中断 |
| S-002 | 非法配置热加载 | 拒绝新配置并保留最后有效状态；进程不退出 |
| S-003 | SIGTERM 优雅停止 | 停止接收新请求；活跃正常流按设计完成；挂起/取消流最终退出；数据库和日志可读 |
| S-004 | 重启 | 使用同一临时数据目录重启，配置和允许持久化的数据一致，不重复迁移或损坏 |
| S-005 | Carpool 隔离数据库 | 使用非系统临时目录的独立 SQLite，验证登录、账号、配额、请求明细、保留、备份与重启，不触碰现有数据库 |
| S-006 | 管理 API | 鉴权、读写配置、请求日志查询和非法输入均符合状态码契约 |

Carpool 是主要测试实例，固定监听 `18318`；数据库必须位于 `${CARPOOL_TEST_ROOT}/data/carpool.db`，且 `${CARPOOL_TEST_ROOT}` 不得处于 `os.TempDir()` 下。`18317` 仅用于无状态通用协议支撑验证。

### L7：WebSocket、UI 和无法由 Mock 覆盖的项目

1. `xy_gateway` Mock 不提供 Codex/XAI WebSocket 上游，因此 WebSocket 协议细节不做伪实机结论。
2. WebSocket 必须运行现有 `internal/runtime/executor` 与 `internal/wsrelay` 的专项测试和 `-race`；若未来出现现成兼容 Mock，再升级为外部实机链路。
3. 管理页面和 carpool 页面使用浏览器对隔离实例执行登录、列表、错误提示、日志/用量展示和窄屏冒烟；同时运行现有 Node 浏览器回归脚本。
4. UI 检查不得连接当前 `8317` 数据库，不上传真实账号文件。

## 6. 自动化驱动器设计

建议在 CLIProxyAPI 仓库的 `test/` 下新增一次性可复用的 Go 实机测试驱动器，但只有用户批准实施后才创建。驱动器职责：

- 读取 `LIVE_TEST_BASE_URL`、`LIVE_TEST_API_KEY` 和场景参数；
- 使用 barrier 同步并发开始；
- 解析 JSON、SSE 和取消场景；
- 为每次请求生成唯一标识；
- 输出 JSONL 结果，包括场景、请求 ID、状态、首事件耗时、总耗时、终态和错误类别；
- 不记录 Authorization 和完整提示词；
- 失败时保留最小响应摘要和对应日志时间窗；
- 支持 `go test -run TestLive -count=1`，默认在未设置显式环境变量时跳过，避免污染常规测试。

若不新增驱动器，可用 `curl` 完成 L1/L2 冒烟，但 L3/L4 的确定性和复测能力会明显下降。

## 7. 缺陷判定与分级

| 级别 | 判定示例 |
|---|---|
| P0 | 数据库损坏、凭据泄漏、请求串流到其他用户、无法停止且持续造成影响 |
| P1 | 核心协议正常请求失败、并发死锁/槽位永久泄漏、重复扣费、进程崩溃 |
| P2 | 特定异常路径状态码/日志/重试错误、热加载不一致、部分协议转换错误 |
| P3 | 可恢复的日志字段、错误文案、UI 展示或低风险兼容性问题 |

判定规则：

- 先独立重复三次；并发问题至少用相同种子重复十轮。
- 删除故障注入后若问题消失，不代表是 Mock 问题；需要核对 Mock 日志确认实际响应。
- 仅在 xy_gateway 自身测试环境不符合其文档行为时标记“环境阻塞”，不归入 CLIProxyAPI 缺陷。
- 每个缺陷记录被测二进制摘要、配置摘要、请求样例、期望/实际、日志时间窗和最小复现命令。

## 8. 停止条件与安全边界

出现下列任一情况立即停止并保留现场：

- 发现日志包含真实凭据或当前运行实例的数据；
- 测试请求误发往公网或非 `127.0.0.1` 上游；
- 当前 `8317` 实例的 CPU、日志或数据库出现测试流量迹象；
- 被测进程持续增长且取消/停止后不收敛；
- 临时磁盘占用超过 5 GiB；
- xy_gateway 测试环境开始使用真实 API 模式；
- 数据库路径不在 `${TEST_ROOT}`。

## 9. 清理与恢复

```bash
for name in cli-proxy xy-test-env; do
  pid_file="${TEST_ROOT}/${name}.pid"
  if [ -f "${pid_file}" ]; then
    kill -TERM "$(cat "${pid_file}")" 2>/dev/null || true
  fi
done

# 等待正常退出后才按需强制终止，不立即 SIGKILL。
for name in cli-proxy xy-test-env; do
  pid_file="${TEST_ROOT}/${name}.pid"
  [ -f "${pid_file}" ] || continue
  pid="$(cat "${pid_file}")"
  for _ in $(seq 1 40); do
    kill -0 "${pid}" 2>/dev/null || break
    sleep 0.25
  done
  kill -KILL "${pid}" 2>/dev/null || true
done
```

证据审阅完成前不删除 `${TEST_ROOT}`。确认无需保留后再执行：

```bash
rm -rf -- "${TEST_ROOT}"
```

不得使用宽泛的 `pkill node`、`pkill cli-proxy-api` 或删除共享 `/tmp` 路径。

## 10. 测试报告结构

```text
运行 ID：
日期：
Git HEAD / 工作区状态 / 二进制 SHA256：
环境：Go、OS、CPU、内存：
配置摘要：
执行阶段与命令：
通过/失败/阻塞数量：
P0/P1/P2/P3 缺陷：
并发收敛证据：
日志与敏感信息检查：
Mock 限制与未覆盖项：
复测结果：
证据目录：
最终结论：可发布 / 有条件通过 / 不通过：
```

## 11. Carpool 优先补充设计

Carpool 使用独立 `127.0.0.1:18318` 实例和非 `/tmp` SQLite 根目录。管理员通过真实 TTY bootstrap；浏览器写操作使用同源 `Origin`、Session Cookie 和 `X-Carpool-CSRF`。车辆绑定 `xy_gateway` Mock 对应的 runtime 候选账号，乘客创建 `cpk_v1_*` Key 后通过标准代理入口产生真实 usage、billing、request detail 和 audit 数据。

并发验证以车辆账号并发限制为门禁：慢流占用唯一账号槽，其他请求同步进入等待；取消后使用 Carpool 请求明细断言 `upstream_attempted`、`outcome` 和 `reason_code`，再恢复普通账号验证槽位收敛。配置热加载后必须重新检查已分配账号与 runtime 候选的关联，不只检查 `/healthz`。
