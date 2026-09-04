# 当前架构证据

## 结论摘要

CLIProxyAPI 已经具备可扩展的入站鉴权、成熟的上游凭据管理器、与供应商无关的用量事件，以及管理 API 扩展点。但它还没有持久化的业务身份数据库，也没有请求级授权边界，能够把原生选择、重试、故障转移、会话亲和及 Home 调度全部限制在某辆车的凭据集合内。

因此，最安全的方案不是另建一套代理流水线，而是复用现有处理器和执行器，增加业务领域模块，并在任何调度器选择上游凭据之前增加一个“默认拒绝”的凭据范围约束。

## 可复用契约

### 入站鉴权

- 请求鉴权供应商注册在公开的 `sdk/access.Provider` 契约之后，鉴权结果能够返回主体和字符串元数据（`sdk/access/registry.go:30`、`sdk/access/registry.go:82`）。
- HTTP 中间件已经把认证主体、鉴权供应商和元数据写入 Gin 上下文（`internal/api/server_middleware.go:159`），适合作为不可变乘客身份的内部交接点。
- 代理执行层目前会把下游主体转换为哈希后的调用者范围，用于隔离会话亲和，但不会传递用户 ID、车辆 ID 或允许使用的凭据集合（`sdk/api/handlers/handlers.go:155`、`sdk/api/handlers/handlers.go:213`）。

### 上游凭据状态

- 每个上游凭据都有稳定的 `Auth.ID`、可读标签、供应商、生命周期状态、禁用/不可用标记、额度观测、模型级状态和时间戳（`sdk/cliproxy/auth/types.go:45`）。
- 现有管理接口已经生成面向运维人员的账号投影，其中包含状态、最近请求次数、额度观测和部分账号元数据（`internal/api/handlers/management/auth_files.go:321`）。该投影不能整体暴露给乘客，但乘客专用 DTO 可以复用其状态语义。

### 凭据选择

- 原生执行流程会先构造候选凭据列表，再执行路由策略、重试和故障转移（`sdk/cliproxy/auth/conductor_selection.go:1467`）。这是本地模式下插入车辆范围强制约束的最佳位置。
- 调度插件可以收到可用候选项的安全副本，并选择一个确定的 `AuthID`（`sdk/cliproxy/auth/conductor_selection.go:789`）。但插件不能先返回一个过滤后的候选子集，再委托全部原生策略处理；委托内置调度器时仍会使用原候选集合（`sdk/cliproxy/auth/conductor_selection.go:817`）。
- Home 模式会绕过本地和插件选择，交由 Home 控制面调度（`sdk/cliproxy/auth/conductor_selection.go:1468`、`sdk/cliproxy/auth/conductor_selection.go:1724`）。因此，如果第一阶段必须支持 Home，约束还需要进入 Home 调度契约，不能只实现本地插件。

### 用量核算

- 统一用量事件已经包含下游 `APIKey`、所选上游 `AuthID`、供应商、模型、时间、成功/失败、延迟及标准化 Token 明细（`sdk/cliproxy/usage/manager.go:21`）。只要再带上稳定的用户和车辆身份，就足以形成持久化的用户/车辆/账号三维归属。
- 当前内置用量能力是内存队列，默认只保留 60 秒，最长一小时，并且读取会弹出删除记录（`internal/redisqueue/queue.go:9`、`internal/redisqueue/queue.go:55`、`internal/redisqueue/queue.go:88`）。它不能作为乘客面板或历史汇总的事实来源。
- 用量插件能够收到大部分必要字段，包括客户端 API Key 和所选 `AuthID`（`sdk/pluginapi/types.go:1340`）。因此可以集中增加一个持久化用量接收器，无需逐个修改供应商执行器。

### 扩展点与界面边界

- 插件可以提供前端鉴权、调度、用量处理和管理 API 路由（`sdk/pluginapi/types.go:65`），所以插件或模块适合承载部分业务 CRUD 和用量持久化。
- 插件管理路由使用管理员鉴权，而浏览器资源路由明确不经过管理鉴权（`sdk/pluginapi/types.go:1262`、`sdk/pluginapi/types.go:1299`）。乘客登录和面板 API 仍需要独立的鉴权路由组。
- 内置管理界面是从独立发布仓库下载的 `management.html`，源码不在本仓库（`internal/managementasset/updater.go:28`）。因此，前端应作为独立交付物明确规划。
- 服务端选项已经支持增加中间件和路由，而不必替换协议处理器（`internal/api/server_options.go:56`、`internal/api/server_options.go:70`）。

## 持久化缺口与 SQLite 决策

- 默认文件、Git、对象存储和 PostgreSQL 集成实现的是凭据与配置持久化，不是通用业务数据仓库。
- PostgreSQL 初始化目前只创建 `config_store`、`auth_store` 和 `cooldown_store`（`internal/store/postgresstore.go:117`），没有用户、密码凭据、会话、车辆、成员关系、账号分配、用量事实或审计事件；复用该后端仍然需要新建完整业务表和迁移机制。
- 用户已确定第一阶段使用 SQLite。对于单机、单 CPA 实例，这能保留事务、外键和索引能力，同时避免部署额外数据库服务，复杂度低于 PostgreSQL。
- 当前仓库没有 SQLite 驱动。发布流程包含 `CGO_ENABLED=0` 构建，因此应优先评估纯 Go 驱动（例如 `modernc.org/sqlite`），避免 `mattn/go-sqlite3` 对 CGO 和跨平台发布流程的影响。
- SQLite 初始化必须启用 `foreign_keys`、WAL 和合理的 `busy_timeout`，使用版本化迁移，并为用户、车辆、成员、账号分配、会话、用量事实和审计事件建立必要约束与索引。
- 数据库文件路径必须可配置并置于持久化卷中。备份应使用 SQLite 在线备份机制或在一致性条件下复制，不能只在写入期间任意复制主文件。
- 第一阶段边界是不支持多个 CPA 实例同时通过网络文件系统共享一个 SQLite 文件；未来需要水平扩展时，再增加 PostgreSQL 仓库实现和数据迁移路径。

## 候选开发路径

### 路径一：直接进行核心二开

增加 `internal/carpool` 领域、存储、服务和 API 包，并在核心选择边界增加一个小型通用凭据范围钩子。该路径的安全性最高，与重试及原生路由结合最好，但以后同步上游 CLIProxyAPI 时会增加合并成本。

### 路径二：可信拼车插件加少量核心钩子

把身份、CRUD、SQLite 仓库、用量持久化和大部分 HTTP API 放入可信本地插件或模块，只在宿主增加缺失的契约：传递稳定认证身份，并在调度前以默认拒绝方式过滤候选凭据。这样可以降低分叉漂移，但现有 C ABI 和插件生命周期不适合承载复杂事务应用与数据库迁移；Home 模式仍需修改宿主或控制面契约。

### 路径三：独立控制服务或网关

把登录、界面、SQLite、策略和用量报表放在独立服务中，把 CLIProxyAPI 当作执行引擎。该路径隔离最好，但普通反向代理不能保证上游账号隔离；除非 CLIProxyAPI 接受带签名的允许凭据范围，并在原生选择内部强制执行。该路径还会增加一个部署服务和跨系统故障点。

## 现有管理前端的处置路径

### 第一阶段推荐：双入口、同一后端进程

- 保留现有 `/management.html`，继续用于 OAuth 登录、上游账号文件、模型和 CPA 底层配置等高权限运维功能。
- 新增 `/carpool/`，承载拼车管理员和乘客界面；它只调用新的拼车业务 API，不直接调用现有高权限管理 API。
- 两个网页都可以由同一个 CPA 进程提供，因此“独立 Web 前端”指独立的前端代码和权限边界，不等于必须再部署一个后端服务。
- 拼车模块只保存现有上游账号的稳定 `AuthID` 引用，通过面向业务的安全 DTO 展示标签、状态和用量，不向浏览器返回原始凭据。
- 管理密钥和乘客会话必须分离；乘客页面即使被攻破，也不能获得现有管理 API 的权限。

### 可选后续：合并为单一管理入口

现有 `/management.html` 由 `remote-management.panel-github-repository` 指向的独立发布仓库下载（`config.example.yaml:33`、`internal/managementasset/updater.go:189`），源码不在当前仓库。若要求一个入口，需要 fork `Cli-Proxy-API-Management-Center`，加入拼车菜单并发布自己的 `management.html`，然后把该配置指向自有仓库。直接手改本地 `management.html` 会被自动更新覆盖，除非关闭面板自动更新，因此不适合作为长期方案。

单入口的体验更统一，但会把后端分叉之外再增加一个前端分叉，需要持续同步上游管理中心的功能、安全修复和构建发布流程，第一阶段成本明显更高。

## 初步推荐

对于需要长期同步上游的私有分叉，第一阶段推荐采用“模块化单体后端 + 独立前端”：

1. 保持协议处理器、翻译器、执行器和凭据生命周期逻辑不变。
2. 在 CPA 进程内增加普通 Go 拼车领域模块和 SQLite 仓库，第一阶段限定单机、单实例部署。
3. 在所有本地候选选择前增加通用、默认拒绝的请求凭据范围约束。
4. 使用稳定的请求、用户、车辆和 `AuthID` 异步持久化统一用量事件。
5. 保留 `/management.html` 管理 CPA 底层能力，新增 `/carpool/`；后者提供相互隔离的拼车管理员界面和乘客界面，并只调用新的业务 API。
6. 除非第一阶段明确需要，否则推迟 Home/集群模式支持。

这种方式比通过现有 C ABI 实现完整业务域更简单、更安全，同时可以避免修改翻译器和各供应商执行器。它也不是“外挂网页直接调用现有管理 API”：现有管理 API 权限过大，且无法在底层强制车辆账号隔离。
