# 拼车模块开发规范

## 1. 适用范围与触发条件

修改 `internal/carpool/**`、`carpool` 配置、用户 API Key 鉴权、凭据选择范围、
逻辑请求用量、`/carpool/` 页面或拼车运维命令时，必须遵守本规范。该模块是
CLIProxyAPI 进程内的可选业务模块，不是独立代理实现，也不能替代现有
`/management.html`。

## 2. 签名与持久化边界

### 配置与命令

```yaml
carpool:
  enabled: false
  database-path: "./data/carpool.db"
  report-timezone: "UTC"
  usage-retention-days: 90
  audit-retention-days: 180
  session:
    absolute-ttl: "24h"
    idle-ttl: "2h"
    cookie-secure: true
  trusted-origins: []
  trusted-proxy-cidrs: []
```

```text
--carpool-bootstrap-admin <username>
--carpool-backup <backup-path>
--carpool-restore <backup-path>
```

三个命令互斥，均要求 `carpool.enabled=true`。管理员密码只能通过 TTY 隐藏输入；
备份和恢复只能在服务停止后执行。

### HTTP 路由

- 静态入口：`GET /carpool`、`GET /carpool/`、
  `GET /carpool/assets/*filepath`。
- 浏览器 API：`/carpool/api/v1/session`、`/carpool/api/v1/me/*`、
  `/carpool/api/v1/admin/*`。
- 代理入口沿用现有协议路由；用户 Key 只开放
  `internal/carpool/runtime/routes.go` 显式列出的 HTTP 路由。
- 未知 `/carpool/api/*` 必须返回 JSON `404`，不得回退到前端 HTML。

### 数据库

SQLite schema 由 `internal/carpool/store/sqlite/migrations/*.sql` 递增管理。当前核心表：

```text
users, sessions, user_api_keys, cars, memberships,
car_auth_assignments, proxy_requests, proxy_request_auth_scopes,
usage_events, audit_events
```

禁止 handler 直接访问 `*sql.DB`。所有业务写入经 service/repository 完成；成员移车、
账号移车、状态变化及其成功审计必须在同一事务提交。

## 3. 核心契约

### 凭证和授权

- 拼车 Cookie、用户 API Key、旧全局 Key、Management Key 是四个互不替代的域。
- 用户 Key 格式为 `cpk_v1_<key_id>.<secret>`；明文只在创建响应出现一次，数据库
  只保存摘要。
- 启用拼车后，Bearer、`X-Api-Key`、`X-Goog-Api-Key`、`key`、`auth_token`
  必须先统一去重；出现两个不同值时，在任何 access provider 前拒绝并写入不含
  明文凭证的安全审计。
- 用户 Key 认证后，必须在同一事务固化用户、Key、成员、车辆和允许账号集合，
  再创建 `CredentialScope`。初选、重试、插件结果、pinned auth、affinity、mixed
  provider 和模型查询都只能缩小该集合。
- Home、Responses WebSocket、Realtime、wsrelay 及未在白名单中的路由必须在连接
  上游前拒绝用户 Key；旧全局 Key 保持原行为。

### 用户与车辆

- 角色创建后不可变；`carpool_admin` 不能成为乘客或创建用户代理 Key。
- 每名乘客最多有一个当前成员关系，每个 `AuthID` 最多有一个当前车辆分配；必须
  同时由 SQLite 唯一约束和事务实现保证。
- `PATCH /carpool/api/v1/admin/cars/:car_ref` 是部分更新：省略字段保留原值，
  `seat_limit:null` 清除席位上限，非空字段传 `null` 返回 `422`，空对象返回 `422`。
- 历史关系只写结束时间，不物理删除。逻辑请求保存开始时的成员展示名和账号分配
  快照，后续改名或移车不得改写历史。

### 会话、用量和审计

- 会话 Cookie 必须为 `HttpOnly`、`SameSite=Strict`、Path `/carpool/`；生产环境
  `Secure=true`。登录校验 Origin，Cookie 写操作同时校验 Origin 与 CSRF。
- `proxy_requests` 一行代表一个逻辑请求；`usage_events` 代表上游实际尝试，二者
  通过稳定 `request_id` 关联。重试不能增加逻辑请求数。
- `usage_known=false` 表示上游没有可靠 usage；未知值不得序列化成已知零。
- 认证和授权拒绝只记录固定 reason code、公开引用或不可逆指纹。禁止记录密码、
  Cookie、Authorization、用户 Key、上游凭据、请求/响应正文和原始错误体。
- 模块关闭顺序必须先停止生产者/全局 usage 分发，再注销命名订阅、排空 writer、
  checkpoint WAL、关闭 SQLite。

## 4. 校验与错误矩阵

| 条件 | 行为 |
|---|---|
| `carpool.enabled=false` | 不建库、不注册路由/provider，不改变旧代理行为 |
| 配置非法、schema 过新或迁移失败 | 启动失败；输出安全结构化日志，不降级为无 scope |
| 无效、撤销、过期用户 Key | `401`；仅写安全认证审计，不创建逻辑请求 |
| Key 有效但无成员、车辆禁用、空账号集合或路由禁用 | `403`；恰好一条 `rejected` 请求事实和授权审计 |
| 多位置出现不同凭证 | `401`；provider 调用次数必须为零 |
| 席位不足、并发成员/账号唯一约束冲突 | `409`；事务最终状态保持唯一 |
| Cookie 写操作缺 Origin 或 CSRF | `403`；不得执行业务写入 |
| SQLite 忙 | `503` 并返回 `Retry-After: 1` |
| 未知拼车 API | JSON `404`，不返回 `index.html` |

迁移失败时 `audit_events` 可能尚不存在或不可写，因此结构化启动日志是强制事实；
只有审计表已存在且可安全写入时才补持久化事件，绝不能为了审计继续运行未知 schema。

## 5. Good、Base 与 Bad 场景

- **Good**：乘客 Key 固化车辆 A 的两个账号；重试和插件调度始终只选择这两个账号，
  多次实际 usage 都归属于同一逻辑请求。
- **Base**：旧全局 Key 使用原凭据池，不带 `CredentialScope`，也不进入拼车报表。
- **Bad**：selector 在 scope 外找到健康账号并继续执行；这是跨车越权，必须返回
  分类错误，不能静默回退。
- **Good**：车辆 PATCH 只发送 `{"name":"新名称"}`，说明和席位保持不变。
- **Bad**：用 Go 零值代表 JSON 字段省略，导致未提交的 `description` 或
  `seat_limit` 被清空。

## 6. 必须测试的断言

- 配置默认关闭、非法配置 fail-fast、SDK 与 CLI 生命周期一致。
- SQLite migration/checksum、未知 schema、并发唯一约束、崩溃恢复、保留清理、
  停机备份恢复及 `0600` 文件权限。
- 四类凭证权限矩阵、多位置冲突、禁用/撤销即时失效和 exclusive provider 冲突。
- selector、retry、plugin、pinned、affinity、mixed、Home 与 scoped models 的隔离。
- 会话、Origin、CSRF、首次改密、API Key 一次展示、DTO/日志/审计金丝雀脱敏。
- 非流式、流式取消、重试、未知 usage、`incomplete` 恢复和历史快照聚合。
- `/management.html` 无回归，`/carpool/` 无 Node 构建，桌面/移动真实浏览器主流程。

最终至少执行：

```bash
gofmt -w .
go vet ./...
go test ./...
go test -race ./internal/carpool/...
go build -o test-output ./cmd/server
CGO_ENABLED=0 go build -o test-output ./cmd/server
```

## 7. Wrong 与 Correct

### Wrong

```go
// A retry silently drops the request scope and can select another car's account.
response, err := manager.Execute(ctx, provider, request, executor.Options{})
```

### Correct

```go
options := executor.Options{CredentialScope: scope, RequestID: requestID}
response, err := manager.Execute(ctx, provider, request, options)
```

`scope` 必须来自服务端授权快照，不能由客户端 header、query 或 body 构造。
