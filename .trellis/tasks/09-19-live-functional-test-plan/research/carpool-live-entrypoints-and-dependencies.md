# Research: Carpool 实机功能测试真实入口与依赖

- Query: 梳理当前代码库 Carpool 实机功能测试所需的真实入口、最小隔离配置、HTTP 路由与请求体、SQLite 初始化、xy_gateway Mock 接入、并发与日志场景、可复用 helper/fixtures。
- Scope: mixed（CLIProxyAPI 当前工作区源码 + 本机 `/home/qlqq/workspace/xy_gateway` 测试夹具；未访问公网）
- Date: 2026-09-19

## Findings

### 1. 结论摘要与需要先修正的既有方案假设

1. **当前代码中的 Carpool 默认值是启用，不是关闭。** `DefaultCarpoolConfig()` 返回 `Enabled: true`，配置加载又在 YAML 反序列化前注入该默认值；因此任何不测试 Carpool 的隔离实例都应显式写 `carpool.enabled: false`，不能依赖省略配置关闭模块。见 `internal/config/carpool.go:80-95`、`internal/config/config_load.go:63-82`、`config.example.yaml:44-65`。
2. **Carpool SQLite 不能放在系统临时目录。** `DatabasePathForConfig()` 会拒绝 `os.TempDir()` 内的路径，因此任务设计中的 `/tmp/cliproxy-live-.../data/carpool.db` 对当前源码会启动失败。见 `internal/config/carpool.go:185-210`。Carpool 实机根目录应改到持久本地目录，例如 `${HOME}/.local/state/cliproxy-live-${RUN_ID}`。
3. **本地明文 HTTP 必须关闭 Secure Cookie。** 设置 `carpool.session.cookie-secure: false`；登录和所有写操作仍必须发送同源 `Origin`，写操作还必须发送登录响应中的 `csrf_token` 到 `X-Carpool-CSRF`。见 `internal/carpool/httpapi/api.go:244-294,296-361`。
4. **价格目录 URL 必须是合法 http(s) URL。** 不能直接配置本地文件路径。为了完全离线，可在 `127.0.0.1:19998` 暴露仓库内置 JSON，并将 `pricing.catalog-url` 指向该地址。见 `internal/config/carpool.go:114-169`。
5. **Carpool 配置变更不会热重建模块。** 运行时检测到 Carpool 配置变化只记录“需重启”；测试启停、数据库路径、TTL、保留策略等变化都应重启独立实例。见 `sdk/cliproxy/service_config.go:123-136`。
6. 建议保留原核心协议实例端口 `18317`，另开 Carpool 实例 `18318`，避免两个测试层共享数据库、Cookie、日志和生命周期。

### 2. 启用 Carpool 的最小隔离拓扑

建议目录与端口：

| 组件 | 地址/路径 | 说明 |
|---|---|---|
| Carpool CLIProxyAPI | `127.0.0.1:18318` | 从当前工作区构建的唯一被测实例 |
| Carpool SQLite | `${HOME}/.local/state/cliproxy-live-${RUN_ID}/data/carpool.db` | 必须位于系统临时目录之外 |
| Carpool auth 目录 | `${TEST_ROOT}/auths` | 仅放虚构测试材料 |
| 本地价格目录 | `127.0.0.1:19998` | 静态暴露内置价格 JSON，避免公网 |
| xy_gateway App | `127.0.0.1:19720` | 由其现有 `globalSetup.ts` 启动 |
| xy_gateway Mock AI | `127.0.0.1:19999` | Responses 正常与慢流上游 |
| xy_gateway HTTP/SOCKS Mock | `127.0.0.1:19997/19996` | 本轮 Carpool 基线不必使用，但 global setup 会启动 |

建议从 `${TEST_ROOT}` 作为工作目录启动 CLIProxyAPI，以隔离相对 `logs/`；同时将应用 stdout/stderr 重定向到 `${TEST_ROOT}/logs/cli-proxy.log`。`ResolveLogDirectory()` 优先使用可写的相对 `logs`，否则回退到 auth 目录。见 `internal/logging/global_logger.go:162-180`。

#### 2.1 可复制的最小 YAML

以下配置只使用虚构上游 Key；`normal` 和 `slow` 两个前缀让测试能明确选择当前车辆应绑定的候选账号。Codex executor 会在 `base-url` 后追加 `/responses`，所以慢账号最终 URL 是 `/responses/slow/responses`，仍会命中 xy Mock 的 `url.includes("/responses/slow")` 分支。见 `internal/runtime/executor/codex_executor_execute.go:76`、`internal/runtime/executor/codex_executor_stream.go:82`、`xy_gateway/tests/helpers/mockServer.ts:227-276`。

```yaml
host: "127.0.0.1"
port: 18318
auth-dir: "/home/<user>/.local/state/cliproxy-live-<RUN_ID>/auths"

# 不配置真实全局客户端 Key；乘客创建出的 cpk_v1_* Key 用于代理请求。
debug: true
request-log: true
logging-to-file: false
usage-statistics-enabled: true
request-retry: 0
max-retry-credentials: 0

remote-management:
  allow-remote: false
  disable-control-panel: true

carpool:
  enabled: true
  database-path: "/home/<user>/.local/state/cliproxy-live-<RUN_ID>/data/carpool.db"
  report-timezone: "UTC"
  usage-retention-days: 30
  audit-retention-days: 30
  concurrency-queue-capacity: 2
  pricing:
    catalog-url: "http://127.0.0.1:19998/model_prices_and_context_window.json"
  session:
    absolute-ttl: "24h"
    idle-ttl: "2h"
    cookie-secure: false
  trusted-origins:
    - "http://127.0.0.1:18318"
  trusted-proxy-cidrs: []

codex-api-key:
  - api-key: "synthetic-carpool-normal-upstream-key"
    prefix: "normal"
    base-url: "http://127.0.0.1:19999"
    request-retry: 0
    models:
      - name: "gpt-4o"
        alias: "carpool-responses"
        force-mapping: true
        is-compat: true

  - api-key: "synthetic-carpool-slow-upstream-key"
    prefix: "slow"
    base-url: "http://127.0.0.1:19999/responses/slow"
    request-retry: 0
    models:
      - name: "gpt-4o"
        alias: "carpool-responses"
        force-mapping: true
        is-compat: true
```

价格目录启动：

```bash
TEST_ROOT="${HOME}/.local/state/cliproxy-live-${TEST_RUN_ID}"
mkdir -p "${TEST_ROOT}"/{bin,auths,data,logs,evidence,configs,xy}
chmod 700 "${TEST_ROOT}" "${TEST_ROOT}/auths" "${TEST_ROOT}/data" "${TEST_ROOT}/evidence"

python3 -m http.server 19998 \
  --bind 127.0.0.1 \
  --directory /home/qlqq/workspace/CLIProxyAPI/internal/carpool/pricing/data \
  >"${TEST_ROOT}/logs/pricing-http.log" 2>&1 &
echo $! >"${TEST_ROOT}/pricing-http.pid"
```

首个管理员必须在服务停止时通过真实 TTY 创建：

```bash
cd "${TEST_ROOT}"
"${TEST_ROOT}/bin/cli-proxy-api" \
  --config "${TEST_ROOT}/configs/carpool.yaml" \
  --carpool-bootstrap-admin qa-admin
```

该命令不能用普通 pipe 喂密码；`readConfirmedCarpoolPassword()` 明确要求 TTY。Bootstrap 会打开数据库、执行迁移并创建唯一首个管理员。见 `cmd/server/carpool_commands.go:126-182,212-270`。

正式启动：

```bash
cd "${TEST_ROOT}"
"${TEST_ROOT}/bin/cli-proxy-api" \
  --config "${TEST_ROOT}/configs/carpool.yaml" \
  --local-model \
  >"${TEST_ROOT}/logs/cli-proxy.log" 2>&1 &
echo $! >"${TEST_ROOT}/cli-proxy.pid"
```

静态入口为 `GET /carpool`、`GET /carpool/`、`GET /carpool/assets/*filepath`；浏览器 API 根为 `/carpool/api/v1`。乘客生成代理请求时使用 `Authorization: Bearer cpk_v1_...` 调用 `/v1/responses`，正常账号的模型写 `normal/carpool-responses`，慢账号写 `slow/carpool-responses`。

### 3. 会话、权限与通用请求头

所有 Carpool API 都会通过 `carpoolRequestID` 返回 `X-Request-ID`，并设置 `Cache-Control: no-store`。见 `internal/carpool/httpapi/api.go:96-104,169-182`。

#### 登录

```http
POST /carpool/api/v1/session
Origin: http://127.0.0.1:18318
Content-Type: application/json

{"username":"qa-admin","password":"..."}
```

成功返回 `201`、`Set-Cookie: cpa_carpool_session=...` 和 JSON 中的 `csrf_token`。当前代码设置 Cookie 为 `HttpOnly`、`SameSite=Strict`、Path `/`；同时清理旧的 `/carpool/` Path Cookie。见 `internal/carpool/httpapi/api.go:296-326,1126-1135`。

#### 登录后的读请求

```http
Cookie: cpa_carpool_session=<session>
```

#### 登录后的写请求

```http
Cookie: cpa_carpool_session=<session>
Origin: http://127.0.0.1:18318
X-Carpool-CSRF: <login-response.csrf_token>
Content-Type: application/json
```

临时密码登录的用户在改密前只能读取当前 session 并调用 `POST /me/password`；其他认证 API 会返回 `403 password_change_required`。改密成功返回 `204` 并清除原 session，必须重新登录。见 `internal/carpool/httpapi/api.go:261-273,343-361`、`internal/carpool/httpapi/api_flow_test.go:126-152`。

### 4. 管理员/乘客、账号、车辆、凭据、配额、用量、明细与清理 HTTP 路由

所有路由注册点见 `internal/carpool/httpapi/api.go:89-144`；账号文件/OAuth 路由见 `internal/carpool/httpapi/account_management.go:65-80`。

#### 4.1 会话与乘客自身

| 方法与路由 | 权限 | 请求体/参数 | 关键结果 |
|---|---|---|---|
| `POST /carpool/api/v1/session` | 未登录 | `{"username":"...","password":"..."}` + Origin | `201`，Cookie、CSRF、角色、改密标志 |
| `GET /carpool/api/v1/session` | Cookie | 无 | 当前用户与 session |
| `DELETE /carpool/api/v1/session` | Cookie + mutation headers | 无 | `204`，注销并清 Cookie |
| `POST /carpool/api/v1/me/password` | Cookie + mutation headers | `{"current_password":"...","new_password":"..."}` | `204`，旧 session 失效 |
| `GET /carpool/api/v1/me/api-keys?cursor=&limit=&q=` | 乘客 | 列表参数 | 只返回引用和状态，不返回明文 Key |
| `POST /carpool/api/v1/me/api-keys` | 乘客 + mutation headers | `{"name":"live-driver","expires_at":null}` | `201`；明文 `api_key` 只在本次响应出现 |
| `DELETE /carpool/api/v1/me/api-keys/:key_ref` | 乘客 + mutation headers | 无 | `204`，立即撤销 |
| `GET /carpool/api/v1/me/car` | 乘客 | 无 | 车辆、成员关系、账单和 quota windows |
| `GET /carpool/api/v1/me/members/usage?period=today|7d|30d` | 乘客 | `period` | 成员聚合用量 |
| `GET /carpool/api/v1/me/accounts?period=today|7d|30d` | 乘客 | `period` | 车辆账号聚合，不暴露 AuthID |

API Key 请求体见 `internal/carpool/httpapi/api.go:382-399`；完整管理员到乘客流程见 `internal/carpool/httpapi/api_flow_test.go:88-251`。

#### 4.2 用户管理

| 方法与路由 | 请求体/参数 | 关键行为 |
|---|---|---|
| `GET /carpool/api/v1/admin/users?cursor=&limit=&q=` | 无 body | 管理员列表 |
| `POST /carpool/api/v1/admin/users` | `{"username":"rider","display_name":"乘客","role":"passenger"}` | `201`，一次性返回 `temporary_password` |
| `PATCH /carpool/api/v1/admin/users/:user_ref` | `{"default_display_name":"新名称","status":"active|disabled"}`；字段可部分提交 | 禁用会使 session/乘客 Key 失效 |
| `POST /carpool/api/v1/admin/users/:user_ref/reset-password` | 无 body | 返回新的 `temporary_password` |
| `GET /carpool/api/v1/admin/users/:user_ref/api-keys?cursor=&limit=&q=` | 无 body | 管理员查看 Key 引用/状态 |
| `DELETE /carpool/api/v1/admin/users/:user_ref/api-keys/:key_ref` | 无 body | 撤销单个 Key |
| `DELETE /carpool/api/v1/admin/users/:user_ref/api-keys` | 无 body | 撤销该用户全部 Key |

请求体定义见 `internal/carpool/httpapi/api.go:545-579,619-640`。

#### 4.3 车辆、成员和配额

| 方法与路由 | 请求体/参数 | 关键行为 |
|---|---|---|
| `GET /carpool/api/v1/admin/cars?cursor=&limit=&q=` | 无 body | 车辆列表 |
| `POST /carpool/api/v1/admin/cars` | `{"name":"Live Car","description":"实机测试","seat_limit":1}` | 创建车辆 |
| `PATCH /carpool/api/v1/admin/cars/:car_ref` | `name`、`description`、`seat_limit`、`status` 的部分更新 | `seat_limit:null` 清除上限；其他非空字段传 null 无效 |
| `GET /carpool/api/v1/admin/cars/:car_ref/members` | 无 body | 当前成员、账单和所有 quota windows |
| `POST /carpool/api/v1/admin/cars/:car_ref/members` | `{"user_ref":"usr_...","display_name":"乘客 A","monthly_limit_usd":"25.00"}` | 把用户移入该车；月限额为必填字符串金额 |
| `PATCH /carpool/api/v1/admin/cars/:car_ref/members/:member_ref/quota` | 见下方 | 部分更新四类限制 |
| `DELETE /carpool/api/v1/admin/cars/:car_ref/members/:member_ref` | 无 body | 结束当前成员关系，不改写历史快照 |

配额更新示例：

```json
{
  "monthly_limit_usd": "25.00",
  "five_hour_limit_usd": "5.00",
  "weekly_limit_usd": "15.00",
  "concurrency_limit": 1
}
```

规则：至少提交一个字段；`monthly_limit_usd` 不能为 null；`five_hour_limit_usd`、`weekly_limit_usd`、`concurrency_limit` 可传 null 以清除，非空 concurrency 必须大于 0。见 `internal/carpool/httpapi/api.go:661-779`、`internal/carpool/httpapi/limits.go:11-53`。

#### 4.4 车辆账号候选、分配与并发

| 方法与路由 | 请求体/参数 | 关键行为 |
|---|---|---|
| `GET /carpool/api/v1/admin/cars/:car_ref/accounts` | 无 body | 返回已分配 `items` 和运行时 `candidates`；候选使用不透明 `cand_v1_*` |
| `POST /carpool/api/v1/admin/cars/:car_ref/accounts` | `{"candidate_ref":"cand_v1_...","safe_label":"Mock Codex Normal"}` | 将候选账号分配给车辆 |
| `DELETE /carpool/api/v1/admin/cars/:car_ref/accounts/:account_ref` | 无 body | 结束账号分配 |
| `PATCH /carpool/api/v1/admin/cars/:car_ref/accounts/:account_ref/concurrency` | `{"concurrency_limit":1}` 或 `{"concurrency_limit":null}` | 设置/清除账号并发上限 |

候选来自整个 runtime auth catalog，并按 provider/候选引用排序，不仅限 Codex；响应不会泄露原始 AuthID。见 `internal/carpool/service/control.go:1030-1071`、`internal/carpool/httpapi/api.go:791-835,1222-1235`、`internal/carpool/httpapi/limits.go:103-123`。

建议流程：先通过 candidates 的 `provider` 和逐个分配后的真实请求验证识别 normal/slow 候选；车辆同一时刻只绑定一个测试账号。正常流程绑定 normal；并发排队场景前删除 normal 分配并绑定 slow。

#### 4.5 账号文件、启停和 OAuth

这些路由属于管理员 Cookie API，不是原版 management Key API：

| 方法与路由 | 请求体/参数 | 说明 |
|---|---|---|
| `GET /carpool/api/v1/admin/auth-files` | 无 body | 安全投影后的账号文件列表 |
| `POST /carpool/api/v1/admin/auth-files?name=test.json` | `Content-Type: application/json`，原始 body `{"type":"codex","access_token":"synthetic"}`；或仅一个 `file/files` multipart part | 单 JSON 仅接受带 `access_token` 的 Codex 授权；同名返回 409，不覆盖 |
| `PATCH /carpool/api/v1/admin/auth-files/status` | `{"name":"test.json","auth_index":"...","disabled":true}` | 持久启停账号；`disabled` 必填 |
| `POST /carpool/api/v1/admin/codex-auth-url` | `{}` | 发起 Codex OAuth，返回 state/url；实机 Mock 基线不应连接真实 OAuth |
| `GET /carpool/api/v1/admin/get-auth-status?state=...` | 无 body | 轮询当前 session 所拥有的 OAuth state |
| `POST /carpool/api/v1/admin/oauth-callback` | `{"provider":"codex","state":"...","code":"...","error":"","redirect_url":"..."}` | 手工提交回调 |

JSON 导入约束见 `internal/carpool/httpapi/account_management.go:192-273`；状态和 OAuth body 见 `internal/carpool/httpapi/account_management.go:316-445`。`sub2api-data` v1 批量导入要求：顶层 `type=sub2api-data`、`version=1`、1–100 个账号；每个账号必须 `platform=openai`、`type=oauth`、`credentials.access_token` 非空，最终正规化为 Codex JSON。见 `internal/carpool/httpapi/account_import.go:12-132`。

本轮接 xy_gateway 时优先使用 YAML 中的虚构 `codex-api-key`，不要为满足功能链而导入伪 OAuth 文件；账号导入应作为独立控制面验证，且不得调用真实 OAuth URL。

#### 4.6 用量聚合、请求明细、价格和账期

| 方法与路由 | 查询参数 | 说明 |
|---|---|---|
| `GET /carpool/api/v1/admin/usage` | `period=today|7d|30d`，或成对 `from/to` UTC RFC3339；可加 `car_ref`、`user_ref`、`account_ref`、`group_by` | 管理聚合报表 |
| `GET /carpool/api/v1/admin/usage/requests` | `period` 或成对 `from/to`；可加 `car_ref`、`user_ref`、`account_ref`、`api_key_ref`、`model`、`outcome`、`billing_status`、`request_id`、`cursor`、`limit=25` | 逻辑请求明细列表；当前 limit 只能是 25 |
| `GET /carpool/api/v1/admin/usage/requests/:request_id` | 路径参数 | 单请求展开，包含 usage events |
| `GET /carpool/api/v1/admin/billing/periods` | `membership_id`、`car_id`、可选 `from/to` | 最多 100 个账期 |
| `GET /carpool/api/v1/admin/pricing` | 无 | 当前价格目录可用性、hash、来源和刷新状态 |
| `GET /carpool/api/v1/admin/audit-events` | `cursor`、`limit`、`before`、`before_id` | 管理审计分页 |

`from/to` 必须同时出现且使用 UTC（offset 必须为 0），不能同时传 `period`。聚合解析见 `internal/carpool/httpapi/api.go:1319-1356`；明细解析见 `internal/carpool/httpapi/api.go:935-960`。明细列表含 request outcome、billing status、金额和事件计数；单项展开额外返回事件级 provider/model/token/price 状态。见 `internal/carpool/httpapi/api.go:876-925`。

#### 4.7 保留设置与显式清理

| 方法与路由 | 请求体/参数 | 说明 |
|---|---|---|
| `GET /carpool/api/v1/admin/retention` | 无 | 返回 override/effective/default/source |
| `PATCH /carpool/api/v1/admin/retention` | `{"days":30}`、`{"days":0}` 或 `{"days":null}` | 0=永久保留；null=恢复配置默认 |
| `POST /carpool/api/v1/admin/retention/preview` | `{"operation":"usage_details|closed_periods|reset_current_period"}` | 返回服务器保存的 `job_id`、计数、过期时间，要求确认 |
| `POST /carpool/api/v1/admin/retention/jobs` | `{"operation":"...","job_id":"job_...","confirm":true}` | 有效确认返回 `202`；缺确认返回 422，过期/状态变化返回 409 |
| `GET /carpool/api/v1/admin/retention/jobs/:job_id` | 路径参数 | 网络结果不确定时查询，不自动重放破坏性 POST |

请求体和响应映射见 `internal/carpool/httpapi/api.go:1009-1093`。清理实机验证应先生成成功、取消、拒绝及至少一条完整 usage event，再做 preview/confirm；确认前后查询明细、账期、聚合和审计，不只检查 HTTP 状态。

### 5. SQLite 与初始化流程

#### 5.1 打开和迁移

`carpoolsqlite.Open()` 的真实流程：

1. 解析绝对路径；创建父目录并在新建时设为 `0700`；创建 DB 并强制 `0600`。
2. 默认 busy timeout 为 5 秒、最大连接数为 4。
3. DSN 强制 `_foreign_keys=1`、WAL、`synchronous=NORMAL`、`_defensive=1`、DQS 关闭、`_txlock=immediate`。
4. Ping、验证 PRAGMA、加载内嵌迁移、验证历史 checksum 后依次迁移。

见 `internal/carpool/store/sqlite/store.go:36-96,148-188`。

当前迁移版本 1–4：`initial`、`billing`、`retention`、`quota`。数据库 schema 比程序更新、checksum 不匹配、迁移不连续或历史有缺口都会 fail-fast，不会降级启动。见 `internal/carpool/store/sqlite/migrations.go:25-105,133-157`。

#### 5.2 首次管理员与启动顺序

推荐顺序：

1. 创建非 `/tmp` 的 `${TEST_ROOT}`、配置、价格目录服务、xy_gateway Mock。
2. 保证 CLIProxyAPI 服务未运行，执行 `--carpool-bootstrap-admin qa-admin`，在 TTY 中输入虚构密码。
3. 启动 CLIProxyAPI；模块再次打开同一 DB 并验证迁移。
4. 登录管理员，创建乘客/车辆/成员，分配候选账号。
5. 乘客临时密码登录并改密，重新登录后创建 `cpk_v1_*` Key。
6. 用该 Key 对 `/v1/responses` 发送代理请求。

模块启动时会把崩溃遗留的运行中请求恢复为可能不完整，并记录 `carpool_accounting_possibly_incomplete` 警告，但不会因此全局阻止新生成。见 `internal/carpool/module.go:75-104`。

#### 5.3 数据库证据

建议每阶段在服务停止或只读安全窗口保存：

- DB 文件、`-wal`、`-shm` 的权限和大小；
- `PRAGMA journal_mode`、`foreign_keys`、`schema_version`；
- `schema_migrations`；
- `users`、`cars`、`memberships`、`car_auth_assignments` 的公开引用和状态；
- `proxy_requests`、`usage_events`、账期/配额事实、`audit_events`、`retention_jobs` 的计数和 request ID，不导出摘要、Cookie 或凭据列。

不要让第二个 CLIProxyAPI 进程同时打开同一 Carpool DB；备份/恢复/bootstrap 命令都要求主服务停止。

### 6. 接入现有 xy_gateway Mock

#### 6.1 必须传入的环境变量

```bash
TEST_PORT=19720
TEST_BASE_URL=http://127.0.0.1:19720
TEST_DB_PATH="${TEST_ROOT}/xy/test.db"
TEST_UPSTREAM_MOCK_URL=http://127.0.0.1:19999
TEST_PROXY_PORT=19997
TEST_SOCKS_PORT=19996
TEST_CLEANUP=false
```

不要设置 `TEST_REAL_API=true`。`TEST_DB_PATH` 不能省略，否则默认会写入 xy_gateway 仓库根目录的 `test.db`。来源：`/home/qlqq/workspace/xy_gateway/tests/config.ts:12-35,87-139`。

继续使用任务设计中的一次性 tsx 启动器导入 `setup()`/`teardown()`，但补上 `TEST_DB_PATH`。`globalSetup.ts` 会初始化 DB、启动 Mock AI、HTTP/SOCKS proxy、xy test app 并创建其自身管理员；退出时应调用原 teardown。见 `/home/qlqq/workspace/xy_gateway/tests/globalSetup.ts:104-188`。

#### 6.2 Mock 能力

当前路由分支见 `/home/qlqq/workspace/xy_gateway/tests/helpers/mockServer.ts:227-276`：

- Chat：正常、`error`、`unavailable`、`balance`、`malformed`、`incomplete`、`disconnect`、`slow`、`created`；
- Responses：正常、`error`、`incomplete`、`slow`、`complete-then-hang`；
- Anthropic：正常、`error`、`malformed`、`stream-error`、`incomplete`、`slow`；
- `GET /models`；
- `GET /_test/requests` 请求捕获接口。

Carpool 基线优先用 Responses：正常 Responses、Responses error、incomplete、slow 和 complete-then-hang 都会调用 `captureRequest()`，可用 `/_test/requests` 做确定性上游到达计数。见 `mockServer.ts:578-629,1280-1288,1402-1446`。捕获只保留最近 500 条，见 `mockServer.ts:142-159`。

#### 6.3 日志与监听 caveat

- xy 日志目录固定为 `<xy_gateway>/log/test`；`app.log` 和 `mockerServer.log` 每次 setup 会覆盖，不能用环境变量改到 `${TEST_ROOT}`。启动前保存旧快照，测试结束立即复制到 `${TEST_ROOT}/evidence/xy/`。见 `config.ts:131-138`、`globalSetup.ts:108-127`。
- Mock 使用 `server.listen(port)`，未显式绑定 `127.0.0.1`；启动后必须用 `ss -lntp` 核对监听地址。见 `mockServer.ts:72-97`。若监听到非 loopback，这是现有 fixture 的安全限制，应隔离网络或停止，不修改 xy_gateway 源码。
- 普通 Chat/Anthropic 路径不是所有分支都会写 `capturedRequests`；这类场景应以 Mock 日志和 CLIProxyAPI 日志交叉验证，不能把 `/_test/requests` 空白误判为没到上游。

### 7. 并发、取消与日志实机场景

#### 7.1 确定性并发排队方案

建议把 `carpool.concurrency-queue-capacity` 设为 2，并配置：

- 成员 `concurrency_limit=1`；
- 当前 slow 账号 `concurrency_limit=1`；
- 车辆只绑定 slow 候选；
- 请求模型使用 `slow/carpool-responses`，`stream:true`。

步骤：

1. 读取 `GET /_test/requests` 记录 baseline。
2. 启动第 1 个慢流，必须在客户端收到首个 Responses SSE 事件后才判定 active 槽位已占用。
3. 启动第 2、3 个请求作为 waiter；通过 `/_test/requests` 的 baseline/delta 证明它们尚未到上游。
4. 启动第 4 个需要等待的请求；队列容量为 2 时应返回 429，错误码应为 `concurrency_queue_full`。
5. 取消一个 waiter，再提交替代请求，证明取消从队列移除且容量恢复。
6. 取消 active；恰好一个最早可运行 waiter 应到达上游，不能同时放行两个。
7. 继续取消/完成余下请求，最终切回 normal 账号，发送 `normal/carpool-responses` 成功请求。
8. 查询 `/admin/usage/requests` 和单项事件，核对 `canceled`、`rejected`、`succeeded` 分类，最终没有永久 `in_progress`，账号/成员槽位没有泄漏。

并发器同时原子获取用户和账号槽位；队列满只拒绝本来需要等待的新请求；取消和 grant 竞争会在解锁后释放 permit；permit 使用 `sync.Once` 幂等释放。见 `internal/carpool/runtime/concurrency.go:105-168,170-229`。

代码内确定性单测 helper 已使用 `testing/synctest.Wait()` 验证入队、唤醒和资源清空，实机驱动器应模仿“可观察 barrier + 上游到达计数”，不要仅靠固定 `sleep`。见 `internal/carpool/runtime/concurrency_test.go:17-84`。

#### 7.2 其他必须场景

- **配额拒绝**：把月/5 小时/周限额设到低于已确认金额，下一次生成应在上游前拒绝；`/_test/requests` 不增加，明细为 `rejected`。
- **账号移除/禁用**：移除车辆账号或通过 `/admin/auth-files/status` 禁用文件账号后，乘客 Key 不得跨车或回退到未分配账号。
- **乘客禁用**：管理员将乘客设为 disabled 后，现有 session 和 `cpk_v1_*` Key 应失效；重新启用不应自动恢复旧 Key。该行为已有 API flow 回归。见 `internal/carpool/httpapi/api_flow_test.go:242-250`。
- **进程重启**：慢流/取消后停止服务，重启同一 DB；检查遗留请求被标为不完整而不是永久运行中，同时新 normal 请求仍可进入。
- **清理保护**：显式清理和自动保留不得删除 `in_progress`/`incomplete` 事实；先 preview，再完成/改变目标，旧 job 应冲突而非删除变化后的数据。

#### 7.3 日志与敏感信息核验

- Carpool 控制 API 无论 `request-log` 是否启用都不得创建 credential request log；代理路径仍应记录。见 `internal/api/middleware/carpool_request_logging_test.go:16-67`。
- 每次控制 API 保存 `X-Request-ID`；代理请求另外在实机驱动证据中保存服务端 request ID。
- 扫描 `${TEST_ROOT}/logs`、复制的 xy 日志和脱敏 JSONL，禁止出现：管理员/乘客密码、临时密码、Cookie、CSRF、`cpk_v1_*` 明文、两个虚构上游 Key、导入 JSON 的 access token、Authorization header、完整 prompt/响应正文。
- 正常日志、request-log、SQLite `proxy_requests`/`usage_events`/`audit_events` 应能通过 request ID、公开引用和时间窗关联；不要求把秘密写入任何一侧。
- 一旦发现任一凭据明文落盘，立即停止后续测试、保留最小脱敏证据并按安全缺陷处理。

### 8. 可复用 helper 与 fixtures

| 文件 | 可复用内容 | 限制 |
|---|---|---|
| `internal/carpool/httpapi/api_flow_test.go:27-251` | 管理员登录、CSRF、创建乘客、临时密码改密、创建车辆/成员、候选账号分配、创建/撤销 Key、角色隔离、禁用用户完整流程 | helper 为包内未导出，实机驱动不能直接 import；可复制 HTTP 步骤/断言 |
| `internal/carpool/httpapi/api_flow_test.go:285-359` | request、Cookie、JSON 解码、字段/鉴权断言 helper | `httptest` 内存路由，不是工作区二进制 |
| `internal/carpool/browser_fixture_test.go:33-100` | 真实 HTTP API + SQLite + 虚构 executor + 本地价格目录；支持浏览器主流程、配额/记账 | build tag `carpool_browser`；它是专用测试进程，不等价于当前构建二进制实机 |
| `test/carpool-browser.mjs`、`test/carpool-accounts-browser.mjs`、`test/carpool-retention-browser.mjs`、`test/carpool-sub2api-browser.mjs`、`test/carpool-redesign-browser.mjs` | Playwright UI、账号、保留、sub2api、响应式布局验收 | 需要 fixture readiness、Playwright/Chromium；脚本会拒绝非 synthetic/非 loopback |
| `ACCOUNT_MANAGEMENT.md:80-94` | 浏览器 fixture 的编译、启动和 Playwright 命令 | 文档命令面向 fixture，不是本实机服务 |
| `test/live_functional_driver_test.go:1-120,294-370` | loopback 限制、Responses SSE、同步起跑 barrier、取消、状态分类、0600 JSONL、秘密扫描 | 默认需 `LIVE_TEST_ENABLE=1`；当前主要是协议请求驱动，不负责 Cookie 管理控制面 |
| `test/live_functional_driver_unit_test.go` | live driver 配置、状态机和证据脱敏单测 | 不建立 Carpool 业务数据 |
| `internal/carpool/runtime/concurrency_test.go:17-84` | `synctest.Wait` 的无 sleep 并发编排模式 | 仅包内 limiter 单测 |
| `internal/carpool/store/sqlite/store_test.go:292` 及 repository/retention 测试 | `openTestStore`、固定 `nowFunc`、真实 SQLite schema/事务测试 | helper 未导出；不能跨包直接 import |
| `/home/qlqq/workspace/xy_gateway/tests/globalSetup.ts` | 现有 App + Mock AI + proxy + DB 生命周期 | 日志路径固定，Mock listen 未绑定 loopback |
| `/home/qlqq/workspace/xy_gateway/tests/helpers/mockServer.ts` | Responses 正常/异常/慢流和 `/_test/requests` | 捕获上限 500；并非所有协议分支都捕获 |

现有实机驱动器可直接把乘客创建的 `cpk_v1_*` 设为 `LIVE_TEST_API_KEY`：

```bash
LIVE_TEST_ENABLE=1 \
LIVE_TEST_BASE_URL=http://127.0.0.1:18318 \
LIVE_TEST_API_KEY='<一次性乘客 Key>' \
LIVE_TEST_MODEL=normal/carpool-responses \
LIVE_TEST_PROTOCOL=openai-responses \
LIVE_TEST_STREAM=true \
LIVE_TEST_CONCURRENCY=1 \
LIVE_TEST_ROUNDS=1 \
LIVE_TEST_EVIDENCE_PATH="${TEST_ROOT}/evidence/carpool-responses.jsonl" \
go test ./test -run '^TestLiveFunctional$' -count=1
```

对于“先登录管理员、创建乘客/车/成员/账号、再提取 Key”的控制面，建议新增独立外部脚本或扩展 `test` 包的 opt-in 实机测试；本研究不修改业务代码或测试代码。

## Files Found

### 当前任务与工作流

- `.trellis/workflow.md` — 当前 Trellis 阶段与角色工作流。
- `.trellis/tasks/09-19-live-functional-test-plan/task.json` — 任务元数据。
- `.trellis/tasks/09-19-live-functional-test-plan/prd.md` — 实机功能测试目标、范围、约束和验收标准。
- `.trellis/tasks/09-19-live-functional-test-plan/design.md` — 现有隔离拓扑与 xy_gateway 启动设计；其中 `/tmp` Carpool DB 假设需修正。
- `.trellis/tasks/09-19-live-functional-test-plan/implement.md` — 分阶段执行计划；Carpool 阶段需采用本研究的非临时 DB 根目录。
- `AGENTS.md` — 项目命令、架构、Go/日志/超时/测试约束及中文 Markdown 规则。

### CLIProxyAPI 代码

- `internal/config/carpool.go` — 默认值、校验、价格 URL、DB 路径和临时目录拒绝。
- `internal/config/config_load.go` — YAML 反序列化前注入 Carpool 默认配置。
- `config.example.yaml` — 当前公开配置字段及 Codex API Key 模型映射格式。
- `internal/carpool/module.go` — SQLite、价格目录、Control、accounting writer 的模块装配与恢复。
- `internal/carpool/httpapi/api.go` — Carpool 路由、会话、管理员/乘客 API、用量、明细、保留与审计。
- `internal/carpool/httpapi/limits.go` — 多周期/并发限制请求体和账号并发更新。
- `internal/carpool/httpapi/account_management.go` — 账号文件导入、启停和 Codex OAuth 控制面。
- `internal/carpool/httpapi/account_import.go` — sub2api-data v1 导入约束与正规化。
- `internal/carpool/service/control.go` — 不透明候选引用和 runtime auth catalog 投影。
- `internal/carpool/store/sqlite/store.go` — DB 权限、连接、WAL/PRAGMA 和打开流程。
- `internal/carpool/store/sqlite/migrations.go` — 当前 1–4 迁移和 checksum fail-fast。
- `cmd/server/carpool_commands.go` — bootstrap/backup/restore、停服要求和 TTY 密码读取。
- `internal/carpool/runtime/concurrency.go` — 用户/账号双维度并发和全局等待队列。
- `internal/api/middleware/carpool_request_logging_test.go` — 控制面永不落 request-log 的安全合同。
- `internal/logging/global_logger.go` — 应用日志目录解析。
- `sdk/cliproxy/service_config.go` — Carpool 配置热加载仅提示重启。
- `internal/runtime/executor/codex_executor_execute.go`、`codex_executor_stream.go` — base URL 追加 `/responses`。
- `internal/carpool/httpapi/api_flow_test.go` — 端到端 HTTP 业务 fixture。
- `internal/carpool/browser_fixture_test.go` — opt-in 真实浏览器 fixture。
- `test/live_functional_driver_test.go` — 已有 loopback 实机协议驱动器。
- `test/live_functional_driver_unit_test.go` — 驱动器单元测试。
- `internal/carpool/runtime/concurrency_test.go` — 确定性并发 helper。
- `internal/carpool/store/sqlite/store_test.go` — SQLite 测试 store helper。
- `test/carpool-*.mjs` — 浏览器回归脚本。
- `ACCOUNT_MANAGEMENT.md` — 账号管理与浏览器 fixture 运行说明。

### xy_gateway 本机外部夹具

- `/home/qlqq/workspace/xy_gateway/tests/config.ts` — 测试环境变量、默认 DB 和固定日志目录。
- `/home/qlqq/workspace/xy_gateway/tests/globalSetup.ts` — Mock、代理、App、DB 的 setup/teardown。
- `/home/qlqq/workspace/xy_gateway/tests/helpers/mockServer.ts` — Mock 路由、Responses 捕获和 `/_test/requests`。

## Code Patterns

- 默认开启：`internal/config/carpool.go:80-95` + `internal/config/config_load.go:63-82`。
- DB 不得位于临时目录：`internal/config/carpool.go:185-210`。
- 价格 URL 只接受无 userinfo 的 http(s)：`internal/config/carpool.go:164-169`。
- 路由总表：`internal/carpool/httpapi/api.go:89-144`。
- 写操作 Origin + CSRF：`internal/carpool/httpapi/api.go:244-258`。
- 首次改密门禁：`internal/carpool/httpapi/api.go:261-273`。
- Cookie 当前 Path `/`：`internal/carpool/httpapi/api.go:1126-1135`。
- 账号候选不暴露 AuthID：`internal/carpool/service/control.go:1030-1071`。
- 明细固定 `limit=25`：`internal/carpool/httpapi/api.go:935-960`。
- SQLite `0700/0600`、WAL、immediate transaction：`internal/carpool/store/sqlite/store.go:36-96,148-188`。
- 迁移历史 fail-fast：`internal/carpool/store/sqlite/migrations.go:63-105,133-157`。
- TTY bootstrap：`cmd/server/carpool_commands.go:166-182,212-236`。
- 并发 queue full/cancel/release：`internal/carpool/runtime/concurrency.go:121-168,213-229`。
- 控制 API 排除 request-log：`internal/api/middleware/carpool_request_logging_test.go:16-67`。
- xy Mock 路由按 URL contains 匹配：`/home/qlqq/workspace/xy_gateway/tests/helpers/mockServer.ts:227-276`。
- xy Responses 请求捕获：`/home/qlqq/workspace/xy_gateway/tests/helpers/mockServer.ts:578-629,1402-1446`。
- 实机 barrier：`test/live_functional_driver_test.go:294-349`。

## External References

- 本研究没有访问公网文档。
- 外部测试依赖仅为本机 checkout：`/home/qlqq/workspace/xy_gateway`。
- xy_gateway 版本/commit 未在本研究中执行 Git 命令读取；正式实机执行应由主会话把其 commit/hash 作为证据记录，但研究代理按规则不执行任何 Git 操作。

## Related Specs

- `.trellis/spec/backend/carpool-guidelines.md` — Carpool 鉴权、车辆、代理白名单、SQLite、会话和审计总合同。
- `.trellis/spec/backend/carpool-accounting-guidelines.md` — 同步记账、运行中写库失败门禁、崩溃 incomplete 与清理保护。
- `.trellis/spec/backend/carpool-quota-concurrency-guidelines.md` — 多周期配额、用户/账号并发、请求完成和释放合同。
- `.trellis/spec/backend/carpool-retention-guidelines.md` — 明细固定分页、preview/confirm 清理和保留覆盖合同。
- `.trellis/spec/backend/live-functional-testing.md` — loopback 实机驱动、SSE/取消、0600 证据和停止条件。
- `.trellis/spec/backend/logging-guidelines.md` — logrus、request-log 和敏感信息禁止项。

## Caveats / Not Found

1. 用户要求先读取任务 jsonl，但研究代理角色隔离规则明确禁止读取 `implement.jsonl` 和 `check.jsonl`；本研究只读取了 `task.json`、`prd.md`、`design.md`、`implement.md`、工作流、spec 和源码，未加载两份 jsonl。
2. 当前源码与部分 Trellis spec 存在漂移：spec 中仍写“配置默认关闭”和 Cookie Path `/carpool/`，而当前代码实际是默认开启、Cookie Path `/`。实机测试应以当前源码为准，同时把该漂移交给主会话评估是否更新 spec。
3. 既有设计的 `${TEST_ROOT}=/tmp/...` 可用于核心非 Carpool 实例，但不能作为当前 Carpool DB 路径；Carpool 阶段必须使用非系统临时目录。
4. xy_gateway Mock 未显式 bind loopback，且日志固定覆盖仓库内 `log/test`；这是现有夹具限制，本任务不能修改其源码，只能在运行时核验、复制证据和安全停止。
5. xy_gateway 不提供 Carpool Cookie/CSRF/SQLite/管理 API fixture；这部分必须由 CLIProxyAPI 自身的 HTTP 流程驱动完成。
6. 大部分 Go helper 未导出，不能被新的外部实机程序直接 import；可复用的是步骤、数据结构和断言模式，而不是二进制链接接口。
7. 本研究未实际启动服务、未执行 destructive retention、未向真实 OAuth/上游发请求；所有入口和依赖结论来自当前源码静态核对。
