# 拼车用户管理第一阶段实施计划

## 1. 实施策略

按“先证明隔离边界，再建业务域；先后端契约，再前端”的顺序实施。每个阶段都有
独立阻断条件，未通过时不得继续扩大用户 Key 可访问范围。实现过程中保持
`carpool.enabled=false` 默认值，直到最终端到端验证完成。

本计划只描述批准后的执行步骤；当前规划阶段不得运行 `task.py start` 或修改业务
代码。用户明确批准最终规划摘要后，先激活任务，再按下列顺序执行。

## 2. 阶段 0：证据探针与契约冻结

### 0.1 建立真实调用图

- [x] 从 `internal/api/server_routes.go` 枚举所有代理路由，记录 HTTP 方法、是否
  Upgrade、使用的鉴权中间件和 handler。
- [x] 逐条追踪 OpenAI、Claude、Gemini、Codex 普通 HTTP 与 HTTP streaming：
  Gin 身份 → handler context → `coreexecutor.Options` → AuthManager Execute/Count/
  ExecuteStream → 首选 → 快速/传统 scheduler → mixed provider → plugin scheduler
  → retry/failover → affinity → usage reporter。
- [x] 单独追踪 Responses WebSocket、Realtime、wsrelay 和 Home，证明它们为何
  第一阶段拒绝用户 Key，以及拒绝点发生在上游连接之前。
- [x] 把锚点和最终用户 Key 路由白名单写入任务目录的
  `research/request-routing-callgraph.md`。

阻断条件：任一拟开放入口绕过共同 selection eligibility，或无法在每次重试中
取得同一不可扩张 scope。

### 0.2 用量生命周期探针

- [x] 用捕获型 usage/lifecycle fake 运行：非流式成功、无 usage 成功、非流式失败、
  首次失败后重试成功、多次失败、流式正常结束、客户端中断、上游中断、Count、
  额外模型记录。
- [x] 每例记录逻辑 request ID、完成次数、usage 事件数、AuthID、事件顺序、Token
  字段是否来自真实 usage，以及 reporter 的 `EnsurePublished` 行为。
- [x] 验证 `RequestCompletion` 在开放 HTTP 路径恰好一次，并确定 request ID 注入
  Options 和 reporter 的最小公共接线。
- [x] 确定 `attempt_no` 是否能可靠生成；不能证明时保留 NULL，不猜测。
- [x] 把结果写入 `research/usage-lifecycle-probe.md`，并据实修正设计中的实现细节，
  但不得把未知 Token 改为零或把事件数改称请求数。

阻断条件：逻辑请求与 usage 无法关联，或已开放流式路径无法生成可信终态且没有
明确的 incomplete/coverage 降级语义。

### 0.3 SQLite 驱动探针

- [x] 在隔离测试包验证 `modernc.org/sqlite` 与 Go 1.26、当前依赖图和许可证兼容。
- [x] 运行 `CGO_ENABLED=0 go build -o test-output ./cmd/server`，并删除测试产物。
- [x] 验证每个池连接启用外键、WAL、`busy_timeout` 和同步级别；不要只检查首次
  初始化连接。
- [x] 验证部分唯一索引、短事务、锁错误分类、并发成员换车和并发账号分配只有
  一个提交。
- [x] 验证迁移 checksum、未知新版本拒绝以及停机 checkpoint/备份/恢复。
- [x] 把驱动、DSN、连接池大小和限制写入 `research/sqlite-driver-probe.md`。

阻断条件：无 CGO 构建失败、外键并非每连接生效，或并发约束只能靠有竞态的服务
层检查。失败时评估其他纯 Go 驱动，不允许静默依赖 CGO。

### 0.4 scope 内存原型

- [x] 先只增加不可变 `CredentialScope` 和测试，不接 SQLite 或 HTTP CRUD。
- [x] 构造车辆 A 允许 Auth 1/2、车辆 B 允许 Auth 3/4，覆盖首次选择、强制失败
  重试、mixed providers、快速 scheduler、传统 selector、插件选择、pinned auth
  和范围外 affinity。
- [x] 验证 scope 为 nil 时旧行为不变，非 nil 空集合默认拒绝，插件返回范围外 ID
  时终止且不回退全局池。
- [x] 验证 Home 检测到非 nil scope 后在 dispatch 前拒绝。

阻断条件：任何执行记录包含范围外 AuthID。

## 3. 阶段 1：通用凭据范围契约

- [x] 在 `sdk/cliproxy/executor/types.go` 定义不可变 scope 值对象，构造时复制、
  规范化、去重；给 `Options` 增加向后兼容的可空字段。
- [x] 扩展 `authSelectionEligibility`，让 `allows()` 成为范围、kind、credential
  policy、free-auth 等条件的唯一组合入口。
- [x] 检查并接入 `conductor_selection.go` 中传统、快速、单 provider、mixed
  provider 与插件路径，以及 `scheduler.go` 的 `scheduledAuthPredicate`。
- [x] 对插件候选输入先过滤，对插件返回 ID 再校验；安全违规返回分类错误并使用
  安全日志，不携带完整候选列表。
- [x] 保证 Execute、Count、ExecuteStream 的所有 retry/failover 复用同一 scope；
  范围外 affinity/pinned auth 按无效候选处理。
- [x] 在 Home 路径增加 scoped request 的明确拒绝，不改变 nil scope 的旧行为。
- [x] 添加表驱动单元测试与现有 selector/scheduler 回归测试。

检查点：此阶段只改变通用执行契约和测试，不引入用户数据库。若回滚，移除新增
Options 字段与 eligibility 条件即可恢复现状。

## 4. 阶段 2：配置、SQLite 仓库与领域约束

### 2.1 配置和生命周期

- [x] 在 `internal/config` 增加 `CarpoolConfig`、默认值、IANA 时区/时长/保留期/
  路径校验、YAML/JSON round-trip 和示例配置；用户可见说明使用简体中文，代码
  注释保持英文。
- [x] 保证关闭时不创建目录/数据库、不注册路由或 provider；启用但配置非法时
  fail-fast，不能回退到无 scope 模式。
- [x] 在 CLI 主程序和 `sdk/cliproxy.Builder/Service` 的共同生命周期初始化模块，
  并在关停时有序停止 writer、排空事件、关闭数据库。

### 2.2 迁移与仓库

- [x] 新建 `internal/carpool/store/sqlite`，内嵌递增 SQL migration 和 checksum，
  实现 schema 版本检测、原子迁移和未知新版本拒绝。
- [x] 建立 `users`、`sessions`、`user_api_keys`、`cars`、`memberships`、
  `car_auth_assignments`、`proxy_requests`、`proxy_request_auth_scopes`、
  `usage_events`、`audit_events` 和必要索引/检查约束；`proxy_requests.outcome`
  的数据库约束必须包含 `in_progress|succeeded|failed|rejected|canceled|incomplete`。
- [x] 所有 SQL 参数化，所有 Query/Exec/Begin 传递调用方 context；检查 rows
  Close/Err、rollback 和 commit 错误并用 `%w` 包装。
- [x] 实现用户/会话/Key、车辆、成员、账号分配、请求/请求账号快照/用量、审计
  和聚合的小接口，不暴露 `*sql.DB` 给 handler。
- [x] 使用事务实现席位检查、成员换车、账号移车、状态变更与审计同提交；把唯一
  约束和锁冲突映射为稳定领域错误。
- [x] 使用 `t.TempDir` 和可注入时钟测试初始化、逐版本迁移、约束、并发、取消、
  清理、数据库重开和未知 schema；不使用 `time.Sleep` 判断 TTL。

检查点：领域仓库测试必须在未接 HTTP 的情况下证明 AC4、AC5 的数据库最终状态。
回滚前保留数据库备份；不执行自动 down migration。

## 5. 阶段 3：密码、会话、用户 Key 与审计

- [x] 实现用户名/展示名规范化、密码策略和 Argon2id PHC 编解码；使用固定测试
  参数注入加速单测，生产参数由设计值与探针确认。
- [x] 将用户角色建模为创建后不可变；服务与 API 拒绝角色修改，管理员不能拥有
  当前成员关系、创建用户代理 Key 或通过该 Key 认证。
- [x] 实现 TTY 隐藏输入的 `--carpool-bootstrap-admin <username>` 命令模式；
  并发/重复 bootstrap 只有一个成功，不把密码传入 flag、环境日志或错误。
- [x] 实现随机会话、绝对/空闲过期、password version、限频 last-seen 更新、
  登出和全会话撤销；会话 Token 只保存摘要，当前随机 CSRF 值明文保存在会话行，
  但不得进入日志、URL 或浏览器持久存储。
- [x] 实现 `cpk_v1_<key_id>.<secret>` 创建、解析、常量时间摘要比较、可选过期、
  last-used 限频、单个/全部撤销和一次性展示；轮换使用“创建、切换、撤销”流程，
  不新增原子 rotate 端点。
- [x] 新增 `sdk/access.Provider`，只处理保留前缀；从 Bearer、`X-Api-Key`、
  `X-Goog-Api-Key`、`key` 和 `auth_token` 提取唯一候选，拒绝冲突候选；配置校验
  禁止旧全局 Key 占用该前缀。Provider 的 Principal 不得是明文 Key。
- [x] 定义按 action 的审计值对象，覆盖登录结果、密码、会话、Key、用户/车辆、
  成员/账号分配和授权拒绝；添加敏感字段拒绝/脱敏测试。
- [x] 登录限速采用有界内存桶与可注入时钟，键为规范化用户名指纹和可信客户端
  地址；只有 TCP 对端命中 `trusted-proxy-cidrs` 时才读取转发地址。用户名不存在
  和密码错误具有相同状态与普通响应，不泄露枚举信号。限速必须在 Argon2id 前
  原子取得准入，未知用户仍验证 dummy hash，结果再更新桶；明确测试并记录进程
  重启会清空限速桶这一首期限制。

阻断条件：数据库、普通日志或审计中出现测试明文密码、Cookie、完整用户 Key、
Authorization 或原始上游凭据。

## 6. 阶段 4：拼车业务服务与 HTTP API

### 4.1 领域服务

- [x] 实现用户、车辆、席位、成员、账号分配和状态转换服务，领域层不依赖 Gin。
- [x] 注入只读 AuthManager 投影接口；创建分配时验证 AuthID 当前存在，读取状态
  时只使用 clone，并通过白名单函数生成安全 DTO。
- [x] 账号消失、禁用、冷却、部分模型不可用、陈旧和无观测分别映射五种安全状态；
  使用可注入时钟测试 5 分钟边界。
- [x] 实现报表范围解析与聚合：预设周期、管理员自定义 UTC 半开区间、保留 cutoff、
  已离车成员快照、请求结果和已知/未知 Token。

### 4.2 HTTP 与权限

- [x] 通过 `api.ServerOption` 或等价模块注册点挂载 `/carpool/api/v1/*`，不要把
  业务 handler 塞进现有 management 包。
- [x] 实现独立 Cookie middleware、RBAC、Origin/CSRF、no-store、安全头、游标
  分页和统一错误 DTO；登录要求 Origin，其他 Cookie 写操作要求 Origin + CSRF，
  Cookie 固定 `SameSite=Strict`；所有响应使用显式结构体，不返回数据库实体。
- [x] 实现 session/me/API Key、管理员 users/cars/members/accounts/usage/audit
  路由；DELETE 成员和分配仅写终止时间。
- [x] 使用 httptest 覆盖权限矩阵、会话生命周期、CSRF、Cookie 属性、登录及其他
  写操作的同源/可信 Origin、跨站登录拒绝、席位/并发冲突、不可见资源 404、DTO
  字段白名单和错误脱敏。
- [x] 覆盖角色不变式：角色 PATCH 被拒绝，管理员成员分配、管理员创建/使用用户
  Key 均失败，乘客创建新 Key 后旧 Key 持续可用直至显式撤销。
- [x] 验证 `GET /session` 可在页面刷新后返回当前会话的同一 CSRF 值，两个标签页
  并发写操作不会互相使 Token 失效；新登录产生独立会话和值，撤销会话后一并失效。
- [x] 保持 `/management.html` 及 Management API 注册、鉴权和自动更新测试不变。

阻断条件：任意凭证跨域成功，或乘客能通过请求参数覆盖 car/user/auth 过滤条件。

## 7. 阶段 5：用户 Key 代理接入

- [x] 在代理认证后的共同中间件识别拼车 provider；Key 秘密验证失败、已撤销或
  已过期时只写安全审计，不创建逻辑请求。Key 验证成功后立即生成 request ID，
  再在一个事务中解析用户、当前成员、车辆、路由白名单和有效分配。
- [x] 拼车启用时在任何 access provider 分派前汇总 Bearer、`X-Api-Key`、
  `X-Goog-Api-Key`、`key`、`auth_token`；完全相同的凭证值去重，出现不同值时
  统一拒绝。覆盖“旧全局 Key + 用户 Key”跨位置冲突，不能让 provider 注册顺序
  决定结果；拼车关闭时不改变当前提取行为。
- [x] 授权失败时同步写入恰好一条归属可空的 `rejected` 请求事实和安全
  `reason_code`，然后在 handler 与上游连接前返回；授权成功时同事务写入
  `in_progress` 请求事实及全部 `proxy_request_auth_scopes` 行，并据此构造
  `AuthorizationSnapshot`。
- [x] 将 scope 与安全身份放入 request context；只在 handler 内转换成不可变
  `Options.CredentialScope`。客户端 header/query/body 中同名字段一律忽略。
- [x] 授权与请求账号快照事务提交即定义成功请求开始；此后即使管理员在建立上游
  连接前禁用车辆或移除关系，该请求仍使用原不可扩张快照继续，selector 不再读取
  业务授权。运行时全局禁用、模型限制和冷却仍可继续缩小候选。
- [x] 将用户 Key 的 route allowlist 做成显式代码表，并对 Upgrade、Realtime、
  wsrelay、Home 和未知路由在上游前返回协议兼容 403。
- [x] 对 scoped `/models` 只返回允许账号能够服务的模型；不得通过全局模型列表
  泄露其他车辆的可用性。
- [x] 确认用户 Key 的 caller scope 由用户/Key 的非敏感稳定值生成，而不是 car ID；
  换车和账号移车后范围外旧亲和自然失效。
- [x] 为 OpenAI Chat/Completions/Responses HTTP、Claude Messages、Gemini
  Generate/Stream/Interactions 和模型查询逐入口增加端到端隔离测试；只有测试通过
  的路由进入 allowlist。
- [x] 增加旧全局 Key 回归：nil scope 的 HTTP、流式、WebSocket/Realtime 和 Home
  路径保持现状，且不写拼车报表。
- [x] 增加鉴权组合回归：启用拼车后匿名代理请求被拒绝；与
  `FrontendAuthProviderExclusive` 同时启用时启动失败；拼车关闭时保留原插件
  exclusive 行为。逐项覆盖 Bearer、`X-Api-Key`、`X-Goog-Api-Key`、`key` 和
  `auth_token`，以及多位置冲突候选拒绝。

发布阻断：注入错误 plugin/pinned/affinity 结果并强制多轮 retry 后，任何选择历史
出现范围外 AuthID。

## 8. 阶段 6：逻辑请求与持久化用量

- [x] 扩展现有 request lifecycle：拼车请求复用阶段 5 中间件生成并已同步写入的
  request ID，非拼车请求仍由 handler 生成；同一 ID 进入 Options，并继续由
  `RequestCompletion` 恰好一次落终态，不得重复 `BeginRequest`。
- [x] 给公共 Usage Record/插件 DTO 增加向后兼容的 `EventID`、`RequestID` 和
  `UsageKnown`；验证旧插件对零值字段无回归。
- [x] 调整 `UsageReporter`：真实解析到 usage 才标 known，`EnsurePublished` 标
  unknown；不要以非零 Token 作为字段存在性判定。
- [x] 构建有界单写者，把安全不可变的完成和 usage 事件写入现有请求事实及
  `usage_events`；绝不持久化 `Failure.Body`、headers、请求/响应正文或任意
  metadata map。授权拒绝由同步事务直接落终态，不进入异步完成队列；父请求或
  scope 已清理的极端迟到 usage 安全丢弃并计数，不重建或猜测归属。
- [x] reporter 选中 AuthID 后只从该 request 的 `proxy_request_auth_scopes` 快照
  取得 assignment/account ref/label/provider 并形成不可变 usage 值对象；禁止按
  当前关系或 request 时间区间反查归属。
- [x] 把 `incomplete` 纳入领域枚举、数据库 CHECK、报表 coverage 和 API DTO；
  在注册路由/provider 和启动 writer 前，将前一进程遗留的全部 `in_progress`
  原子标为 `incomplete` 并设置恢复时间与安全原因码，不使用墙钟阈值。
  实现 writer 指标、队列饱和/写失败安全日志和关停排空；`incomplete` 不计入
  成功、失败、拒绝或取消，崩溃缺口不宣传严格一次。
- [x] 用阶段 0 的场景形成回归测试，证明每个逻辑请求只计一次、多个实际已知事件
  的 Token 均累计、未知事件不变成零、`incomplete` coverage 正确，且请求开始后
  账号立刻移车、usage 延迟落库时历史归属仍不漂移。

阻断条件：request ID 丢失、重试增加逻辑请求数、未知 Token 序列化为数字 0，或
usage 写入会阻塞已建立的上游流。

## 9. 阶段 7：独立 `/carpool/` 前端

- [x] 在 `internal/carpool/web` 创建无 Node 的 HTML/CSS/ES Module，使用
  `go:embed`；不使用 CDN、内联脚本、localStorage 凭证或 source map。
- [x] 只注册 `GET /carpool`、`GET /carpool/` 和
  `GET /carpool/assets/*filepath`；页面使用 `#/...` 导航，不注册
  `/carpool/*asset` 或 history fallback。把拼车 JSON 404 分支安全组合进现有
  `pluginManagementNoRoute`，保留原 plugin management/resource 行为。
- [x] 实现登录/强制改密、乘客车辆概览、成员用量、账号状态、本人 Key 管理，
  以及管理员用户、车辆、成员、账号分配、报表和审计页面。
- [x] 对一次性 API Key/临时密码使用不可恢复对话框；关闭后不在 DOM、URL、
  console 或浏览器持久存储中保留。
- [x] 设置 CSP、安全头、`index.html` no-cache、API no-store、静态资源 ETag 和
  `/carpool` 重定向；不得影响 `/management.html`。
- [x] 页面按配置时区展示服务端回显区间；状态刷新不快于 60 秒，失败时显示陈旧。
- [x] 通过 Go httptest 验证路由可同时注册、嵌入资源、MIME、缓存和 CSP；未知
  `/carpool/api/*` 返回 JSON 404，未知页面路径不返回 `index.html`，hash 导航仍
  加载 `/carpool/`，且 `/management.html` 与既有 NoRoute 行为不变。用浏览器
  手工冒烟验证两种角色的完整主流程。

## 10. 阶段 8：运维、保留与文档

- [x] 实现分批保留清理器，使用注入时钟和短事务；不在线自动 VACUUM。
- [x] 用各事实自己的时间字段计算 cutoff：先按 `usage_events.requested_at` 清理
  事件，再只对 `completed_at` 过期且 `NOT EXISTS` 保留 usage 子行的终态请求删除
  scope 与父行；两个 cutoff 都使用 `usage-retention-days`，不增加独立请求保留
  配置；永不清理 `in_progress`，并确定性覆盖边界时刻和延迟 usage。
- [x] 提供停机备份、checksum、schema 记录和恢复校验命令/文档；运行时只复制
  主 DB 的操作必须明确标为不支持。
- [x] 更新 `README_CN.md` 和 `config.example.yaml`：启用步骤、bootstrap、反向
  代理 TLS/Origin、数据路径、Docker volume、备份/恢复、保留、Home/长连接限制、
  旧全局 Key 风险和禁用方法。
- [x] 更新 Docker Compose 数据目录挂载示例，验证容器重建后数据库仍在。
- [x] 文档全部使用简体中文；代码注释保持英文，用户可见文本沿拼车前端使用
  简体中文。

## 11. 阶段 9：最终验证与发布门槛

### 11.1 格式与静态检查

```bash
gofmt -w .
git diff --check
```

- [x] 格式化前记录工作树与本任务改动列表；若存在无关用户 Go 改动，不得改写它们，
  只对本任务 Go 文件运行 `gofmt -w`，再用 `gofmt -l .` 做全仓只读核验。工作树仅
  含本任务 Go 改动时按仓库要求执行 `gofmt -w .`。
- [x] 检查新增 Markdown 为简体中文，JSONL 每行可独立解析。
- [x] `rg` 扫描新增日志、审计和 DTO，确认没有 raw password/token/header/body。
- [x] `git diff --name-only` 确认没有业务授权改动落入 `internal/translator/**`，没有
  无关文件被修改。

### 11.2 聚焦测试

按实际包名执行至少：

```bash
go test -v ./internal/carpool/...
go test -v ./sdk/cliproxy/auth/...
go test -v ./sdk/api/handlers/...
go test -v ./internal/api/...
go test -race ./internal/carpool/...
```

聚焦测试必须覆盖：

- 四类凭证权限矩阵与保留前缀冲突。
- 两车互斥账号的初选、重试、故障转移、插件、pinned、mixed 和 affinity。
- 用户/账号换车、席位和并发唯一性。
- 密码、会话、Key 撤销范围和 TTY bootstrap。
- 逻辑请求、usage known/unknown、流式取消与历史快照。
- DTO/日志/审计敏感值金丝雀扫描。
- migration、未知 schema、保留、停机备份恢复。

### 11.3 全量测试与构建

```bash
go test ./...
go build -o test-output ./cmd/server && rm test-output
CGO_ENABLED=0 go build -o test-output ./cmd/server && rm test-output
```

如果环境具备 Docker，再运行镜像构建与 volume 重建冒烟；无法运行时必须在交付
报告中明确说明并给出对应 CI 验证项。常规 build 不得要求 Node。

### 11.4 人工验收脚本

- [x] 新库 bootstrap 管理员并登录。
- [ ] 创建两车、至少三名乘客、四个互斥上游账号，验证席位和安全标签。
- [ ] 为两个乘客创建 Key，分别调用所有白名单协议的非流式与 streaming。
- [ ] 强制账号失败触发 retry，核对选择历史、请求数、Token 和未知数。
- [ ] 移动用户和账号，验证新请求、旧历史、成员列表和状态。
- [ ] 测试改密、重置、单 Key 撤销、全 Key 撤销、禁用用户/车辆。
- [ ] 用用户 Key 尝试 Home、WebSocket、Realtime、wsrelay、Management 和
  carpool Cookie API，确认全部按契约拒绝。
- [x] 停止服务、备份、恢复到新路径并重启，对比行数与聚合。

所有 AC1 至 AC14 有测试或可复验记录后，才可把第一阶段标为完成。

## 12. 高风险文件与审查重点

| 区域 | 风险 | 必须审查 |
|---|---|---|
| `sdk/cliproxy/auth/conductor_*`、`scheduler.go` | 某分支绕过 scope | 每种 selector、retry、plugin、Home 的故障注入 |
| `sdk/api/handlers/handlers_*` | request ID 或流式终态丢失 | 成功、失败、拒绝、取消恰好一次 |
| `sdk/cliproxy/usage`、usage helper | 未知当零、事件重复 | 字段存在性与重试实测 |
| `internal/api/server_*` | 路由漏挂中间件或凭证跨域 | 路由清单与权限矩阵逐条核对 |
| `internal/carpool/store/sqlite` | 并发、迁移、WAL、外键 | 约束、恢复、未知 schema、每连接 PRAGMA |
| `internal/carpool/httpapi` | 越权与 DTO 泄密 | 服务端强制过滤、CSRF、白名单序列化 |
| `cmd/server/main.go`、`sdk/cliproxy` | CLI 与嵌入式启动漂移 | 两条生命周期路径都测启停和失败清理 |
| `internal/carpool/web` | 一次性秘密和缓存泄漏 | CSP、no-store、无本地持久化 |

## 13. 回滚点

1. **scope 契约后**：尚无数据库，可直接回滚新增 Options/eligibility，旧行为由
   nil scope 测试保证。
2. **首次 schema 前**：保存空库迁移与版本测试；之后每次 schema 变更先停机备份。
3. **用户 Key 接入前**：功能保持关闭，旧全局 Key 路径可独立发布/验证。
4. **用量接入后**：若报表故障，可关闭拼车模块；不得只关闭 scope 而继续接受
   用户 Key。
5. **发布回滚**：停止服务，回退二进制并恢复匹配 schema 的备份，或保持拼车关闭
   仅运行旧通道；禁止旧二进制写入新版本数据库。

## 14. `task.py start` 前复核

- [x] `prd.md`、`design.md`、本文件和两份 JSONL 均存在且非空。
- [x] PRD 无待决用户问题，验收标准覆盖目标、范围和兼容性。
- [x] 同一独立审查代理已复核三份最终文档，所有发布阻断意见已处理或明确保留。
- [x] `python3 ./.trellis/scripts/task.py validate 09-04-carpool-user-management`
  通过。
- [x] 用户在看到最新最终规划摘要后的下一条消息中明确批准进入实现。
- [x] 只有满足上一项后，才运行：

```bash
python3 ./.trellis/scripts/task.py start 09-04-carpool-user-management
```
