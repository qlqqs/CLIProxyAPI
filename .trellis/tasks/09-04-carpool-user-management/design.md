# 拼车用户管理第一阶段技术设计

## 1. 设计摘要

采用模块化单体：在同一个 CPA 进程内新增 `internal/carpool/**`，使用独立
SQLite 数据库保存拼车业务数据；沿用现有协议 handler、凭据管理器、路由、
重试、冷却和 executor，只在公共边界增加用户身份、不可扩张凭据范围、逻辑
请求生命周期和用量关联字段。

前端是与现有管理中心代码和权限均独立的浏览器原生静态应用：

```text
浏览器
├── /management.html
│   └── 现有管理密钥 → 现有 Management API
└── /carpool/
    └── 拼车 Cookie → /carpool/api/v1/* → Carpool Service → SQLite

代理客户端
├── 旧全局 Key → 现有全局凭据池（行为不变，不进入拼车报表）
└── 用户 API Key → 授权快照 → CredentialScope → 现有选择/重试/executor
                                      ├── proxy_requests
                                      └── usage_events
```

第一阶段关闭时不创建数据库、不注册拼车鉴权提供者或路由，不改变当前 CPA
行为。第一阶段不增加独立后端服务，不复制代理流水线，不修改 translator 来
实现业务授权。

## 2. 设计原则与不变量

1. **默认拒绝**：用户 Key 没有完整授权快照或允许账号集合为空时，任何上游
   连接都不能发生。
2. **范围只能缩小**：请求开始时的允许 `AuthID` 集合是上限，后续全局禁用、
   模型不匹配和冷却可以继续缩小，任何重试、插件或亲和逻辑都不能扩大。
3. **归属不可漂移**：用户和账号换车只影响新请求；历史按请求开始时的关系
   快照查询。
4. **请求与消耗分层**：逻辑请求数来自 `proxy_requests`，上游实际 Token 来自
   `usage_events`。重试不会制造多个用户请求，但实际已产生的 Token 不丢失。
5. **未知不是零**：只有解析器确认 usage 字段存在时 `usage_known=true`；全零
   补记录、缺字段和无法判断都属于未知。
6. **最小披露**：乘客 API 使用独立白名单 DTO，绝不直接序列化 `auth.Auth`、
   数据库模型或管理 API 响应。
7. **兼容优先**：旧全局 Key、`/management.html`、协议翻译、流式格式和 Home
   的旧通道维持原行为。
8. **单实例诚实边界**：SQLite 仅供一个 CPA 进程使用；不以共享文件方式伪装
   多实例数据库。

## 3. 包与职责边界

建议目录按职责拆分，最终文件名可在不改变边界的前提下微调：

```text
internal/carpool/
├── module.go                 # 生命周期、依赖装配、关闭与健康状态
├── config.go                 # 拼车配置规范化与校验
├── domain/                   # 领域类型、状态枚举、错误，不依赖 Gin/SQLite
├── service/                  # 用户、车辆、分配、授权、报表与审计用例
├── store/sqlite/             # database/sql 仓库、迁移、查询、事务
├── access/                   # 用户 API Key 的 sdk/access.Provider
├── httpapi/                  # Cookie/CSRF/RBAC 中间件、DTO 与 Gin handlers
├── runtime/                  # AuthManager 安全投影、scope 与用量/lifecycle 桥接
└── web/                      # HTML/CSS/ES Module 及 go:embed 入口
```

跨现有层的修改限定为：

- `internal/config/**` 与 `config.example.yaml`：增加 `carpool` 配置并接入加载、
  规范化、脱敏和测试。
- `cmd/server/main.go`、`sdk/cliproxy/**`、`internal/api/server_options.go`：初始化、
  注册路由/鉴权/观察器和有序关闭；CLI 与 SDK 构建路径行为一致。
- `internal/api/server_routes.go`、`server_middleware.go`：在所有代理路由共用的
  认证后边界识别用户 Key、拒绝禁用入口并附加授权上下文。
- `sdk/cliproxy/executor/types.go`：增加不可变 `CredentialScope` 和逻辑请求 ID
  的显式执行契约。
- `sdk/cliproxy/auth/conductor_selection.go`、`scheduler.go` 及相关选择测试：让
  所有候选选择统一调用同一个 eligibility 判定。
- `sdk/api/handlers/handlers_*`：将同一逻辑请求 ID 贯穿 Options、完成回调和
  HTTP 流式终态。
- `sdk/cliproxy/usage/manager.go`、`sdk/pluginapi/types.go`、
  `internal/runtime/executor/helps/usage_helpers.go`：增加事件 ID、请求 ID 和明确
  的 `usage_known`，保持新增字段向后兼容。

不得把业务数据库、审计或通用授权代码放入 `internal/runtime/executor/`；不得
为每个供应商复制一套车辆过滤；不得把 `/carpool/` 覆盖到
`internal/managementasset/**`。

## 4. 配置契约

建议配置形态：

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

规则：

- `database-path` 相对路径以配置文件目录解析；启用时创建父目录，权限遵循
  `0700` 目录、`0600` 文件。拒绝明显的临时目录和网络文件系统自动推断只做
  警告，运维契约仍明确禁止共享写入。
- `report-timezone` 必须是有效 IANA 时区。数据库时间全部为 UTC；预设周期先在
  报表时区计算边界，再转 UTC，以 `[from,to)` 查询并回显。
- 用量保留期不得短于 30 天；审计保留期必须为正。清理使用可注入时钟，测试不
  依赖 `time.Sleep`。
- `cookie-secure=false` 只允许显式配置并记录安全警告，供本地 HTTP 开发使用；
  反向代理部署必须配置 TLS 和可信 Origin。
- 第一阶段 `enabled`、数据库路径和会话安全配置变更需重启；不在运行时搬迁或
  重开数据库。未知配置值启动失败并给出不含秘密的错误。
- 旧全局 `api-keys` 不被复制到拼车配置。管理员希望关闭旧通道时继续从现有
  `api-keys` 配置移除，不引入隐式迁移。
- 启用拼车后，代理入口至少存在拼车用户 provider，因此原先“零 provider 时
  匿名放行”的行为不再适用；匿名请求返回认证错误，旧全局 Key 仍按原行为工作。
- `FrontendAuthProviderExclusive` 会把全局 provider 列表收窄到一个插件，与拼车
  Key 和旧全局 Key 共存契约冲突。第一阶段检测到两者同时启用时 fail-fast，并
  提示管理员二选一；功能关闭时不改变 exclusive 插件现状。
- `trusted-proxy-cidrs` 默认为空。只有 TCP 对端属于该列表时，登录限速才读取受信
  转发地址；否则始终使用 `RemoteAddr`，避免伪造 `X-Forwarded-For` 绕过限速。

## 5. SQLite 与迁移

### 5.1 驱动与连接

首选 `modernc.org/sqlite`，硬要求是 Go 1.26 与 `CGO_ENABLED=0` 构建可用。
实施第一步用一次性探针验证；若不满足，只能换用等价纯 Go 驱动，不能悄悄改成
依赖 CGO 的 `mattn/go-sqlite3`。

连接初始化必须对连接池中的每条连接生效：

- `foreign_keys=ON`
- `journal_mode=WAL`
- `busy_timeout=5000`
- `synchronous=NORMAL`

连接数保持小且显式配置在代码中；具体值由并发探针确定。所有写操作使用
`BeginTx(ctx, ...)`，事务尽量短，不在事务内调用网络或凭据管理器。

### 5.2 迁移

- SQL 迁移使用递增版本、内嵌资源和固定 checksum。
- `schema_migrations(version, name, checksum, applied_at)` 是唯一版本源。
- 新库自动从零前向迁移；已有库按序迁移。单个可事务化迁移必须原子提交。
- checksum 不匹配、迁移中断或数据库版本高于二进制支持版本时，拼车模块拒绝
  启动；现有代理是否继续启动由 `carpool.enabled` 决定，启用状态下不允许静默
  降级为无隔离模式。
- 不提供自动向下迁移。升级前执行停机备份；回滚代码时同时恢复对应备份，或
  关闭拼车模块让旧全局通道继续工作。

## 6. 数据模型

所有 ID 使用服务端生成的随机 UUID；所有时间以 UTC RFC 3339 精度或等价整数
时间保存。业务对象不物理删除。以下只列关键字段，所有可变主表另含
`created_at`、`updated_at`。

### 6.1 `users`

| 字段 | 约束与用途 |
|---|---|
| `id` | 主键，内部使用 |
| `username` / `username_normalized` | 原始登录名与规范化值；后者唯一 |
| `default_display_name` | 新成员关系的默认公开名，不自动取邮箱 |
| `role` | `carpool_admin` 或 `passenger` |
| `status` | `active` 或 `disabled` |
| `password_hash` | Argon2id PHC 字符串，包含版本、参数与盐 |
| `password_version` | 改密/重置递增，参与会话校验 |
| `must_change_password` | 管理员重置后为真 |
| `disabled_at` | 禁用时间，可空 |

用户名限制为稳定的 ASCII 登录标识，规范化后全局唯一；展示名允许 UTF-8，并
单独做长度、控制字符和规范化校验。密码长度 12 至 128 个字符；不使用人为复杂度
组合规则。Argon2id 参数写入摘要并支持登录后重哈希，初始实现采用 64 MiB、
3 次迭代、并行度 2、16 字节盐和 32 字节输出，实施时以目标机器压测确认不会
造成不可接受的登录阻塞。`role` 创建后不可修改；管理员修改用户的 API 不接受
该字段，数据库枚举约束只允许两种角色。

### 6.2 `sessions`

| 字段 | 约束与用途 |
|---|---|
| `id` | 主键 |
| `user_id` | 外键 `users(id)` |
| `token_digest` | SHA-256 摘要，唯一；明文随机值只在 Cookie 中 |
| `csrf_token` | 当前随机 CSRF 值；不是会话凭证，不可单独认证 |
| `password_version` | 创建时版本；与用户不一致即失效 |
| `created_at` / `last_seen_at` | 创建和最后活动时间 |
| `expires_at` | 绝对过期时间 |
| `revoked_at` / `revoke_reason` | 可空，保留审计语义 |

会话 Token 与 CSRF Token 均使用至少 256 bit CSPRNG。会话 Token 只存摘要；
CSRF 值可以明文保存在会话行，因为它缺少 HttpOnly 会话 Cookie 时不能认证，
但仍不得写入日志、URL 或浏览器持久存储。`GET /session` 返回同一会话的当前值，
多个标签页不会因刷新互相失效；创建新会话或权限提升时生成新值，撤销会话时一并
失效。`last_seen_at` 可限频更新，但判断空闲过期使用数据库已有值加安全余量，
不能延长绝对有效期。

### 6.3 `user_api_keys`

明文格式为 `cpk_v1_<key_id>.<secret>`：`key_id` 仅用于索引定位，`secret` 至少
256 bit。数据库保存 `key_id`、`user_id`、用途名称、`SHA-256(secret)`、
`created_at`、可空 `expires_at`、限频 `last_used_at`、可空 `revoked_at` 和原因。

- 仅用户 Key provider 处理 `cpk_v1_`；配置校验拒绝旧全局 Key 使用该前缀。
  provider 从 Bearer、`X-Api-Key`、`X-Goog-Api-Key`、`key` 和 `auth_token`
  读取候选，以保持现有协议客户端兼容。拼车启用时，代理公共提取边界先收集所有
  位置经各自语法解析后的非空值：完全相同的值去重，存在两个不同值则在 provider
  分派前拒绝；唯一值再按前缀交给对应 provider，避免注册顺序决定认证结果。
- 比较摘要使用常量时间函数。
- 只有 `passenger` 可以创建或通过用户 Key 认证；provider 每次认证仍校验用户
  角色与状态。`carpool_admin` 只能撤销乘客 Key，不能为自己创建或使用该凭证。
- 撤销行保留；列表仅返回 Key 名称、短前缀、创建/过期/最后使用/撤销时间。
- 数据库泄露不能直接还原秘密；高熵秘密无需使用慢密码哈希。
- 第一阶段不增加原子 rotate API。轮换是“创建新命名 Key → 切换并验证客户端 →
  撤销旧 Key”的显式流程，两个 Key 的生命周期和审计互相独立。

### 6.4 `cars`

包含 `id`、公开 `car_ref`、名称、可空描述、可空正整数 `seat_limit`、状态
`active|disabled|retired`、`version`、可空 `disabled_at` 和 `retired_at`。

- `disabled` 可重新启用；`retired` 不可恢复并拒绝新关系。
- 降低 `seat_limit` 和新增成员在同一写事务中读取当前人数。
- 状态或分配变化递增 `version`，便于日志与未来缓存失效；第一阶段授权路径不做
  正向缓存，以保证下一请求直接读到已提交状态。

### 6.5 `memberships`

包含 `id`、公开 `member_ref`、`user_id`、`car_id`、`display_name`、规范化
`display_name_key`、`started_at`、可空 `ended_at`/`ended_reason`、
`created_by_user_id`。

关键索引：

```sql
CREATE UNIQUE INDEX memberships_one_current_car
ON memberships(user_id) WHERE ended_at IS NULL;

CREATE UNIQUE INDEX memberships_current_display_name
ON memberships(car_id, display_name_key) WHERE ended_at IS NULL;
```

换车事务顺序为：校验用户/目标车/席位 → 结束旧关系 → 创建新关系 → 审计 →
提交。唯一约束失败统一映射为 `409 membership_conflict`。

### 6.6 `car_auth_assignments`

包含 `id`、公开 `account_ref`、`car_id`、外部稳定 `auth_id`、`safe_label`、
规范化 `safe_label_key`、`provider_snapshot`、`started_at`、可空
`ended_at`/`ended_reason`、`created_by_user_id`。

关键索引：

```sql
CREATE UNIQUE INDEX car_auth_one_current_car
ON car_auth_assignments(auth_id) WHERE ended_at IS NULL;

CREATE UNIQUE INDEX car_auth_current_safe_label
ON car_auth_assignments(car_id, safe_label_key) WHERE ended_at IS NULL;
```

`auth_id` 不设数据库外键，因为权威来源是内存凭据管理器。创建分配前从 manager
快照验证存在，事务内仍依靠唯一索引解决并发。凭据随后消失时保留关系和历史，
但不进入有效 scope。每次移车生成新的 `account_ref`，避免跨车辆关联。

### 6.7 `proxy_requests`

每个拼车用户逻辑请求一行：

| 字段 | 说明 |
|---|---|
| `request_id` | 用户 Key 请求由认证后中间件生成；非拼车请求仍由 handler 生成，主键 |
| `user_id` / `api_key_id` | 已成功认证的 Key 身份，非空 |
| `car_id` / `membership_id` | 授权失败时可空；成功时非空 |
| `member_ref_snapshot` / `display_name_snapshot` | 授权失败时可空的历史安全展示快照 |
| `scope_hash` / `scope_size` | 成功授权时的摘要与数量；拒绝时可空/为零 |
| `source_format` / `requested_model` / `stream` | 非正文请求元数据 |
| `started_at` / `completed_at` | 生命周期时间 |
| `outcome` | `in_progress|succeeded|failed|rejected|canceled|incomplete` |
| `status_class` / `reason_code` | 只存 HTTP 类别和安全枚举，不存原始错误 |
| `upstream_attempted` | 是否实际开始过上游尝试 |

Key 秘密验证成功后，中间件生成 request ID，并在同一授权事务中写入：授权成功
写 `in_progress` 与完整归属；用户禁用、无成员、车辆无效、空 scope 或禁用入口
写 `rejected` 与能安全确定的可空归属。无效、撤销或过期 Key 不写该表，只写安全
审计。开始记录失败则请求以 `503` 失败，不能访问上游。终态按 `request_id`
幂等更新，禁止从终态回到 `in_progress`。每次启动在注册路由、provider 和启动
异步 writer 之前，把数据库中全部前一进程遗留的 `in_progress` 行原子更新为正式
枚举 `incomplete`，并设置 `completed_at` 与安全原因码 `process_interrupted`。
单实例契约保证此时不存在本进程合法长请求，因此不使用容易误判的墙钟阈值；
`incomplete` 只进入 coverage，不计入成功、失败、拒绝或取消。

### 6.8 `proxy_request_auth_scopes`

授权成功时与 `proxy_requests` 同事务写入一行/允许账号，字段包括
`request_id`、`auth_id`、`assignment_id`、`account_ref_snapshot`、
`safe_label_snapshot` 和 `provider_snapshot`，主键为 `(request_id, auth_id)`。

这张表是请求级不可变的 `AuthID → 分配归属` 映射。它同时供
`CredentialScope` 构造、selected-auth 校验和异步 usage 归属使用；账号在请求
开始后改名、撤销或移车都不会改变该映射。禁止在 usage 到达时按当前关系或时间
区间反推 assignment。

### 6.9 `usage_events`

每次统一 usage reporter 实际发布一行，而不是假设每个事件等于一个用户请求：

| 字段 | 说明 |
|---|---|
| `event_id` | 发布端生成 UUID，主键，用于幂等 |
| `request_id` | 外键 `proxy_requests(request_id)` |
| `event_seq` / `attempt_no` | 事件顺序；attempt 无法证明时可空 |
| `auth_id` / `assignment_id` | 从 `proxy_request_auth_scopes` 取得的内部关联 |
| `account_ref_snapshot` / `safe_label_snapshot` | 历史安全展示 |
| `provider` / `model` | 规范化运行元数据 |
| `usage_known` | usage 字段存在性，不由 Token 是否非零推断 |
| Token 列 | `input/output/cached/reasoning/total`，未知时全部为 NULL |
| `failed` / `status_class` | 安全结果信息，不保存错误体 |
| `requested_at` / `recorded_at` | 上游尝试和落库时间 |

逻辑请求统计只查 `proxy_requests`。Token 合计只求和 `usage_known=true` 的非空
值；未知用量展示为事件数，并可按 `request_id` 去重另给覆盖率。多次重试的已知
Token 都是实际上游消耗，允许累计，但第一阶段不用于收费。`usage_events` 不重复
保存 `car_id` 或 `user_id`：车辆和用户归属始终通过 `request_id` 连接请求开始时
的 `proxy_requests` 快照，账号归属使用事件中从请求账号快照复制的
`assignment_id`，避免两份车辆快照产生不一致。

### 6.10 `audit_events`

包含 `id`、`occurred_at`、可空 `request_id`、`actor_type`、安全
`actor_ref`、`action`、`target_type`、安全 `target_ref`、`result`、
`reason_code` 与白名单 `metadata_json`。

审计 builder 只接受按 action 定义的结构体，禁止 handler 传任意 map。未知用户
登录失败使用不可逆用户名指纹；不得保存密码、Cookie、Authorization、Key、
完整 `AuthID`、IP 查询串、提示词、响应或原始错误体。

## 7. 身份、会话与授权流程

### 7.1 首个管理员

CLI 增加 `--carpool-bootstrap-admin <username>` 命令模式，加载配置和 SQLite，
从 TTY 隐藏读取并确认密码。仅当数据库中从未存在拼车管理员时成功；并发执行由
数据库唯一初始化记录保证只有一个提交。其他管理员由已有拼车管理员在网页创建。

管理员重置乘客密码时由服务端生成一次性临时密码并只在该响应显示，设置
`must_change_password=true` 并撤销全部会话；用户下次登录只能进入改密流程。

### 7.2 网页会话

- `POST /carpool/api/v1/session` 先要求非 `null` Origin 与当前站点或显式
  `trusted-origins` 精确匹配，再按用户名指纹和可信客户端地址原子取得限速准入，
  只有准入后才查询用户并执行 Argon2id；未知用户名仍验证固定 dummy hash。认证
  结果更新桶，成功后创建会话与 CSRF 值；缺失/不可信 Origin 被拒绝，用户名不
  存在和密码错误返回同一 `401`。
- `GET /carpool/api/v1/session` 返回当前角色、公开资料和当前 CSRF Token，不
  返回 Cookie 值。该接口读取会话行保存的非 bearer CSRF 值，不轮换，因此多个
  标签页可并发使用；新登录产生的新会话使用新值。
- Cookie 名使用独立保留名称，设置 `HttpOnly`、`SameSite=Strict`、Path
  `/carpool/`，生产配置设置 `Secure`；业务响应统一 `Cache-Control: no-store`。
- 修改密码、登出和管理员重置都通过事务撤销相应会话。
- 登录限速键由“规范化用户名的不可逆指纹 + 客户端地址”组成。默认只信任 TCP
  `RemoteAddr`；仅当直接对端命中 `trusted-proxy-cidrs` 时解析转发地址。桶为
  单实例有界内存状态，进程重启会清空，这是第一阶段接受的限制。

### 7.3 用户 API Key 请求

1. 拼车启用时，代理公共凭证提取器先跨 Bearer 与其余四个 header/query 位置
   得到唯一值；相同值去重，不同值冲突直接拒绝，任何 provider 都不得先行接受。
   `sdk/access.Provider` 只识别 `cpk_v1_`，按 `key_id` 查行并常量时间比对摘要。
   无效、撤销或过期属于认证失败，只产生安全审计。
2. Key 认证成功后，公共代理中间件立即生成逻辑 request ID。用户 provider 的
   `Principal` 使用不可逆稳定调用者范围，不放明文 Key；`accessMetadata` 只带
   provider、内部 user/key 引用。
3. 中间件在一个事务中校验用户、成员、车辆、当前分配和路由白名单。失败时写入
   一条 `rejected` 请求事实及安全原因码，然后在 handler 前返回；成功时构造
   `AuthorizationSnapshot` 并写入 `proxy_requests` 与所有
   `proxy_request_auth_scopes` 行。
4. `AuthorizationSnapshot` 保存不可变的
   `AuthID → assignment_id/account_ref/safe_label/provider` 映射；scope 只包含
   运行时仍存在的映射键。客户端不能提供或修改映射。
5. 成功事务的 request ID 与快照放入 request context；handler 的 lifecycle
   tracker 复用该 ID，不再另生成。Home/Upgrade 与禁用路由也走第 3 步的
   `rejected` 终态，且不会接触上游。

第一阶段不做正向授权缓存，因此管理事务提交后的下一请求直接读 SQLite。授权与
BeginRequest 事务提交即定义请求开始；此后即使尚未建立上游连接，已构造快照的
请求也继续使用原集合，集合在整个逻辑请求与所有重试中不可变。运行时全局禁用、
模型约束和冷却仍可缩小候选。

## 8. `CredentialScope` 与选择器接入

在 `sdk/cliproxy/executor` 定义不可变值对象：构造函数复制、去重并排序 ID；内部
集合不导出，只提供 `Enforced()`、`Allows(id)` 和安全复制方法。

- `Options.CredentialScope == nil`：旧 SDK/旧全局 Key，保持原行为。
- 非 nil 且为空：强制没有任何账号，返回 `auth_not_found` 的安全映射。
- 非 nil 且有值：`authSelectionEligibility.allows()` 首先检查范围，再执行当前
  kind、credential policy、免费账号等既有条件。

必须逐项接入并测试：

1. 传统单供应商候选与 mixed provider 候选。
2. 快速 scheduler 的 `scheduledAuthPredicate`。
3. 插件 scheduler 收到的候选列表和插件返回 `AuthID` 的二次校验。
4. pinned auth 与会话亲和命中；范围外视为 miss，不允许直接使用。
5. Execute、Count、ExecuteStream 的每轮重试和故障转移。
6. 模型别名、模型池与 provider fallback 重新构造候选的路径。
7. Home dispatch：检测到非 nil scope 时在发送 Home 请求前明确拒绝。

`caller_scope` 继续按用户/Key 隔离亲和，不能用 `car_id` 代替；否则同车成员会
错误共享会话。用户或账号换车后，旧亲和目标因不在新 scope 中自然失效。

## 9. 逻辑请求与用量数据流

```text
用户 Key 秘密认证
    ├── 失败 → 仅安全审计
    └── 成功 → 中间件生成 request_id
                    ↓（单事务）
        授权成功 → proxy_requests=in_progress + 请求账号快照行
        授权失败 → proxy_requests=rejected（归属字段可空）→ 返回
    ↓
Options{RequestID, CredentialScope} 贯穿 Execute/Count/Stream 与重试
    ├── 每个 reporter 发布 UsageRecord{event_id, request_id, usage_known, AuthID}
    │      └── 从请求账号快照取归属并持久化 usage_events
    └── RequestCompletion 恰好一次更新 proxy_requests 终态
```

现有 `RequestCompletion` 已提供逻辑 ID 与成功、失败、拒绝、取消终态；需要让
tracker 优先复用中间件生成的 ID，并让相同 ID 进入执行 Options。非拼车请求仍
由 tracker 自行生成 ID。`UsageRecord` 的新增字段为向后兼容的零值扩展：非拼车
或旧插件看到空 ID 时行为不变。

`UsageReporter.Publish*` 的语义调整为：解析器明确获得 usage 时发布
`usage_known=true`；`EnsurePublished` 只保证事件存在并标记 false。不能通过
“Token 是否全零”推断字段存在性。实施探针要覆盖所有 reporter 调用点，并记录
额外模型事件和重试事件是否共享逻辑 ID。

持久化采用有界单写者队列以减少 SQLite 锁竞争：

- 授权与 `BeginRequest` 同步事务写入，失败则拒绝请求；授权拒绝由该事务直接
  写终态，不进入异步完成队列。
- usage 与终态进入有界队列；入队不得携带可变 headers/body/map，必须先复制并
  变成安全值对象。usage 值对象从请求快照取得 assignment/account 安全归属，
  不按落库时的当前关系反推。
- 极端迟到的 usage 若已找不到父请求或请求账号快照，writer 必须安全丢弃并增加
  分类计数，不能绕过外键、重建父行或按当前分配猜测归属。
- 队列满或写失败时增加结构化丢弃计数并安全记录 request ID；不能阻塞已经建立
  的上游流，也不能向客户端泄露数据库错误。
- 优雅关闭先停止接收、限时排空，再关闭数据库。崩溃窗口可能产生
  `incomplete` 或缺失事件，界面明确显示覆盖信息，不宣称严格一次。

## 10. 账号状态安全投影

运行时桥接只读取 `Auth` 的克隆，并折叠为：

| 对外状态 | 判定优先级 |
|---|---|
| `disabled` | `Disabled` 或运行时 `StatusDisabled` |
| `unknown` | Auth 不存在、无可信观测，或最新观测超过 5 分钟 |
| `unavailable` | 当前整体不可选且不是显式禁用 |
| `degraded` | 处于临时冷却、额度受限、近期失败或仅部分模型不可用 |
| `healthy` | 当前可选、有 5 分钟内观测且无上述降级信号 |

`observed_at` 取参与判定的最新安全时间，`stale` 独立返回。DTO 只包含：

```json
{
  "account_ref": "acc_...",
  "label": "车内账号 A",
  "provider": "codex",
  "status": "degraded",
  "observed_at": "2026-09-04T10:00:00Z",
  "stale": false,
  "usage": {
    "logical_requests": 12,
    "known_total_tokens": 42000,
    "unknown_usage_events": 1
  }
}
```

状态映射函数是纯函数并使用可注入时钟。不得返回 `StatusMessage`、LastError、
Quota reason/signals、Metadata、Attributes、FileName、Label 或完整 AuthID。

## 11. HTTP API 与权限矩阵

### 11.1 路由

静态入口：

- `GET /carpool` → 308 到 `/carpool/`
- `GET /carpool/` → 内嵌 `index.html`
- `GET /carpool/assets/*filepath` → 内嵌且只读的版本化静态资源

不注册 `/carpool/*asset` 或其他服务器端 SPA catch-all。前端页面状态使用 URL
fragment（如 `/carpool/#/cars`），fragment 不发送到服务器。未知
`/carpool/api/*` 由现有 `pluginManagementNoRoute` 链新增的拼车分支返回统一 JSON
404；其他未知 `/carpool/*` 返回普通 404，绝不回退 `index.html`。该组合不得改变
既有 plugin management/resource 的 NoRoute 行为。

会话与本人：

- `POST /carpool/api/v1/session`
- `GET /carpool/api/v1/session`
- `DELETE /carpool/api/v1/session`
- `POST /carpool/api/v1/me/password`
- `GET|POST /carpool/api/v1/me/api-keys`（仅乘客）
- `DELETE /carpool/api/v1/me/api-keys/:key_ref`（仅乘客）
- `GET /carpool/api/v1/me/car`
- `GET /carpool/api/v1/me/members/usage?period=today|7d|30d`
- `GET /carpool/api/v1/me/accounts?period=today|7d|30d`

拼车管理员：

- `/carpool/api/v1/admin/users`：列表、创建、修改非角色字段、启禁用、重置密码、
  撤销 Key；第一阶段没有角色变更端点
- `/carpool/api/v1/admin/cars`：列表、创建、修改、启禁用、退役
- `/carpool/api/v1/admin/cars/:car_ref/members`：加入、移除、换车
- `/carpool/api/v1/admin/cars/:car_ref/accounts`：安全候选、分配、撤销、移车
- `/carpool/api/v1/admin/usage`：按车辆、用户、账号与 `[from,to)` 聚合
- `/carpool/api/v1/admin/audit-events`：分页安全审计列表

具体 CRUD 使用 `GET/POST/PATCH/DELETE`，DELETE 对成员和分配表示终止而非物理
删除。列表使用基于游标的稳定分页，默认 50、最大 200。错误统一为：

```json
{"error":{"code":"membership_conflict","message":"成员分配冲突","request_id":"..."}}
```

登录失败统一 401；角色或车辆权限不足 403；不存在或不可见 404；并发/席位冲突
409；验证错误 422；登录限速 429；数据库或账号池暂不可用 503。代理路由的拒绝
使用对应协议兼容错误外壳。

### 11.2 权限矩阵

| 凭证 | `/management.html` / Management API | `/carpool/api/v1/session` 后接口 | 代理白名单 | 其他代理入口 |
|---|---:|---:|---:|---:|
| 现有管理密钥 | 现状 | 否 | 否 | 否 |
| 拼车管理员 Cookie | 否 | 管理员权限 | 否 | 否 |
| 乘客 Cookie | 否 | 本人/本车只读与 Key 管理 | 否 | 否 |
| 用户 API Key | 否 | 否 | 是，强制 scope | 明确拒绝 |
| 旧全局 Key | 现状不变 | 否 | 现状不变 | 现状不变 |
| 无凭证 | 仅公开静态壳 | 登录接口/静态壳 | 否 | 否 |

所有 `/carpool/api/*` 响应使用 `no-store`、`X-Content-Type-Options: nosniff` 和
一致安全头。写接口校验 `Origin` 与 CSRF header；不启用带凭证的通配 CORS。

## 12. 前端交付

`internal/carpool/web/assets/` 保存浏览器原生 HTML、CSS 和 ES Module 源文件，
由 `go:embed` 打包。第一阶段不引入 Node、远程 CDN、运行时下载、source map 或
第三方脚本。前端使用 hash 路由，不要求服务端 history fallback。

页面最小集合：登录/强制改密、乘客车辆概览、成员用量、账号状态、本人 Key
管理、管理员用户、车辆、成员、账号分配、聚合报表和审计。

- 角色由 `/session` 响应决定，前端隐藏菜单不是授权边界，后端逐接口校验。
- 不把 Cookie、CSRF Token 或 API Key 写入 localStorage；新 Key/临时密码只在
  一次性对话框展示，关闭后无法恢复。
- CSP 至少为 `default-src 'self'`，禁用 object/frame/base，脚本和样式均来自
  self 且无内联执行。
- `index.html` 使用 `no-cache`，静态资源使用 ETag；API 和一次性秘密响应
  `no-store`。
- `/management.html` 的下载器、缓存文件和路由完全不变。

## 13. 并发、撤权与错误策略

- 用户换车、账号移车、席位调整、禁用和撤销在 SQLite 短事务中完成；服务层
  预检用于友好错误，数据库约束是最终真相。
- 不缓存正向授权快照；每个新用户 Key 请求都读取当前状态。`last_used_at` 与
  session `last_seen_at` 限频写入，不能成为授权依据。
- 请求定义的“开始”是授权快照与请求账号映射提交并成功写入
  `proxy_requests` 的时点；此后即使尚未连接上游，管理操作也不重读或强杀该
  请求。运行时凭据禁用等现有条件仍可缩小候选。WebSocket/Realtime 因撤权边界
  不清而不开放。
- 数据库不可用、迁移失败或授权读取不一致时用户 Key fail-closed；绝不能回退
  到旧全局凭据池。
- 上游账号不存在、全被冷却或不可用时返回通用 503，不透露集合大小、AuthID 或
  其他车辆信息。
- 任何插件返回范围外 AuthID 视为内部安全违规，记录安全事件并终止，不尝试用
  全局候选“恢复服务”。

## 14. 报表查询规则

- 所有查询使用服务端计算并返回的 `[from,to)`；`today` 按配置时区当天零点，
  `7d` 与 `30d` 为包含当前时刻的滚动窗口。
- 请求数和 outcome 来自 `proxy_requests.started_at`；Token 与未知事件来自
  `usage_events.requested_at`，两者都使用请求开始时的 car/user/assignment 快照。
- 成员当前列表只含 `ended_at IS NULL`；周期报表可以返回期间有用量的已离车成员，
  使用 `display_name_snapshot` 并标记 `left`。
- 乘客查询强制当前 `car_id`，忽略客户端任何车 ID。管理员过滤器使用公开 ref
  解析内部 ID。
- 自定义范围不能早于实际保留边界；响应包含 `data_from`、`data_to`、
  `retention_cutoff` 与 `coverage`，避免把已清理数据误解为零。
- 查询由复合索引覆盖：请求按 `(car_id,started_at)`、`(user_id,started_at)` 和
  `(car_id,request_id)`；请求账号快照按 `(request_id,auth_id)`；事件按
  `(request_id,requested_at)` 与 `(assignment_id,requested_at)`。车辆 Token
  聚合先按 `proxy_requests.car_id` 取得请求，再以 `request_id` 联表事件；不引用
  `usage_events` 中不存在的车辆字段。具体执行计划在真实数据夹具上检查。

## 15. 备份、恢复、保留与可观测性

### 15.1 备份恢复

第一阶段保证停机备份：停止 CPA，确认 WAL checkpoint/连接关闭后复制数据库到
新文件，记录二进制版本、schema 版本和 SHA-256。恢复时仍保持停止状态，先校验
checksum 和 schema，再原子替换目标文件并启动验证。不能在运行中只复制主
`.db` 文件。

若纯 Go 驱动探针证明在线 backup API 可靠，可额外提供在线命令，但它不是第一
阶段验收承诺。Docker Compose 增加独立数据目录挂载示例。

### 15.2 清理

单个后台维护器按批次执行以下顺序：

1. `usage_events` 按自己的 `requested_at < usage_cutoff` 删除。
2. 对 `completed_at < request_cutoff` 的终态请求，仅在 `NOT EXISTS` 任何仍保留
   的 usage 子行时删除其请求账号快照，再删除父 `proxy_requests`。
3. 普通清理器永不删除 `in_progress`；启动恢复后的 `incomplete` 视为终态，以
   恢复时设置的 `completed_at` 计算保留期。

`request_cutoff` 与 `usage_cutoff` 都由 `usage-retention-days` 计算；第一阶段不
提供独立的逻辑请求保留配置。因此跨越 cutoff 的延迟 usage 会保住所需父请求和
scope 快照。审计独立按自己的
`occurred_at` 与 180 天 cutoff 清理。所有边界使用严格 `< cutoff`，清理采用短
事务、可取消上下文和可注入 `nowFunc`，并以边界时刻、延迟事件和父子交错夹具做
确定性测试。不自动 VACUUM 阻塞在线服务；文档提供停机维护命令。

### 15.3 可观测性

记录但不泄密的指标/日志包括：授权拒绝原因码、scope 大小、SQLite 写延迟/失败、
队列深度/丢弃数、incomplete 请求数、usage 已知率、迁移版本和清理行数。日志只
记录 request ID、公开号或不可逆指纹，不记录用户名与完整 AuthID 的组合。

模块健康状态区分：数据库不可用、迁移失败、writer 积压、运行正常。公共
`/healthz` 的既有语义不轻易改变；拼车管理员页单独显示模块状态。

## 16. 技术探针与设计冻结点

在生产实现前先提交以下证据；它们决定接线细节，不改变 PRD：

1. **调用图**：枚举 `internal/api/server_routes.go` 的每个代理入口，追踪鉴权、
   handler、Options、初选、快速/传统调度、重试、亲和和 usage。
2. **scope 原型**：两辆车互斥候选，覆盖普通/混合 provider、插件、pinned、亲和、
   Count、Stream 和故障转移。任何越界即阻断。
3. **usage 生命周期**：记录非流式、流式、取消、重试、无 usage、额外模型的事件
   数量和顺序，确认 request ID 能贯通，并决定 `attempt_no` 是否可可靠填充。
4. **SQLite**：验证纯 Go 构建、每连接 PRAGMA、部分唯一索引、并发换车、锁错误
   分类、迁移 checksum 和停机备份恢复。
5. **入口白名单**：只把 scope 与终态均有端到端测试的 HTTP 路由加入用户 Key
   白名单；长连接和 Home 保持拒绝。

如果无法可靠识别 attempt，`usage_events.attempt_no` 保持 NULL，接口仍按逻辑
请求与上报事件分别统计；不得为了填字段而猜测。若某个 HTTP 路由无法贯通，缩小
白名单并在兼容错误中说明该凭证不支持，而不是削弱隔离。

## 17. 发布、回滚与兼容

- 功能以 `carpool.enabled=false` 作为发布默认值。先完成迁移和首个管理员创建，
  再显式启用。
- 升级既有部署时，旧全局 Key、管理页及原配置存储后端不迁移、不改义；拼车
  SQLite 是并列业务存储。
- 发布阻断测试覆盖所有用户 Key 白名单路由、所有 selector 分支、凭证域矩阵、
  历史归属、DTO 脱敏、SQLite 并发/恢复和常规构建。
- 代码回滚优先关闭拼车模块，恢复旧代理通道；数据回滚必须恢复升级前备份，不
  对新 schema 执行猜测性降级。
- 未来扩展到多实例时新增仓库接口的 PostgreSQL 实现和正式迁移工具；不能让两个
  实例挂载当前 SQLite 文件。

## 18. 关键取舍

- **选择核心内模块而非纯插件**：车辆范围必须覆盖宿主的重试、快速调度和亲和，
  现有插件调度无法保证委托后仍使用过滤候选。模块化目录和少量通用 SDK 字段可
  将分叉面控制在公共边界。
- **选择 SQLite 而非 PostgreSQL**：用户已明确单机单实例，SQLite 提供所需事务、
  外键和部分唯一索引，减少部署组件；代价是第一阶段不支持水平扩展。
- **选择独立 `/carpool/` 而非改造管理页**：管理页源码和更新在外部仓库，直接
  修改会被覆盖并扩大高权限前端风险。双入口让升级和权限边界更清楚。
- **选择无构建静态前端**：仓库发布流程没有 Node 前置条件，浏览器原生模块可由
  现有 `go:embed` 模式直接发布；代价是第一阶段 UI 组件能力较朴素。
- **选择展示而非硬额度**：usage 缺失、重试和并发会让硬限制产生错误拒绝；先
  形成可信可见性，后续再单独设计配额账本。
- **选择停机备份基线**：它能可靠覆盖 WAL 并简化恢复承诺；在线备份在驱动能力
  和演练证据充分后再增加。
