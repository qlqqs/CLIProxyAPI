# OpenAI 账号管理实施与验收

## 需求与范围

在管理员导航按「车辆管理 → 账号管理 → 用户管理」排列新增入口，乘客端「账号状态」保持不变。只支持 OpenAI/Codex，不增加其他供应商。

必须完成：

- 账号列表、搜索、状态筛选、刷新、详情，以及加载、空态、错误恢复。
- 导入一个或多个账号 JSON，逐项显示成功或失败；拒绝非法内容与其他供应商。
- OAuth 登录：生成授权链接、打开或复制链接、等待状态、手工回填回调地址、成功刷新与失败重试。
- 启用和停用账号，变更作用于原有凭据管理和调度，而非仅改变页面状态。

不复制原版 OAuth、Token 存储或调度实现；复用 `management.Handler`。不新增供应商配置管理，不改乘客权限。

## 实施拆分

本轮按用户要求不创建 Trellis 任务，采用独立的小改动并行推进。

1. 后端：复用原版 handler，添加明确允许的账号路由、提供者限制、上传校验、会话与 CSRF 门禁，补齐 Go 测试。
2. 前端：独立管理员路由 `/admin-accounts`，沿用 `DESIGN.md` 的布局与组件，实现列表、导入、启停、OAuth 完整交互。
3. 独立审查：核对原版接口行为和安全边界，提出具体回归用例。
4. 集成验收：核对双方接口，运行单测、全仓测试、编译与桌面/移动浏览器测试。

## 安全契约

- 仅已改初始密码的拼车管理员可操作。乘客、未登录者、用户代理 Key 均无权使用。
- 写操作及 OAuth 发起同时检查 Origin 与 CSRF；不得改变 `/v0/management` 的认证规则。
- 只委托明确允许的账号 handler，不提供任意 management 代理。
- OAuth state 归发起会话所有，不能查询或提交其他会话、其他供应商的 OAuth 流程。
- 页面列表不返回 Token、原始 JSON、服务器文件路径或任意 metadata；上传与错误日志不得记录凭据。
- 使用原版上传、启停和 OAuth 持久化逻辑；配置热更新不能让账号管理使用过期配置。
- 非法 JSON、越界文件名、超大请求和非 OpenAI/Codex 凭据在调用原版写入之前拒绝。

## 设计契约

沿用暖纸底色、陶土主动作和窄轨导航；不引入新的视觉方向。首屏能判断账号数量、启停状态并找到导入与登录。筛选可恢复，弹层支持键盘关闭和焦点返回，窄屏无整页横向溢出。OAuth 轮询在离页、关闭和退出时停止，旧响应不能覆盖新页面。

## 验收方法

- Go：有效导入、启停持久化、提供者隔离、未登录/乘客/强制改密/CSRF/Origin 拒绝、OAuth state 所有权、错误与超大请求。
- JavaScript：实际页面函数的过滤、请求编码、错误处理与异步清理回归测试。
- 浏览器：使用 `carpool_browser` 隔离测试服务与虚构凭据，验证真实导入和启停，桌面及手机截图、筛选、错误恢复、OAuth 交互和离页清理。
- OAuth 成功交互可由测试响应模拟；真实外部 OpenAI 授权需要用户本人登录，不使用真实账号替用户完成授权。报告中明确区分模拟验证与真实外部授权。
- 必跑：`gofmt -w .`、`go test ./...`、`go build -o test-output ./cmd/server && rm test-output`，以及受影响的 Node 与浏览器脚本。

## 结果

功能实现、并行审查、全量测试和浏览器验收已完成。未创建 Trellis 任务；实现验收阶段未部署生产，后续部署见下方记录。

已取得证据：

- `gofmt -w .`、`go test ./...`、`go build -o test-output ./cmd/server && rm test-output`、`git diff --check`：通过。
- `go test -race ./sdk/auth ./sdk/cliproxy/auth ./internal/store ./internal/carpool/httpapi ./internal/api/handlers/management ./internal/api/middleware`：通过。
- `node --test test/*.test.mjs`：95 项通过。
- 新账号页浏览器：13 组检查通过，桌面 1440px、手机 390px / 360px 截图人工复核；无整页横向溢出，手机直接可启停。
- 原有界面回归：管理端九个原入口、乘客端五入口、用户/车辆详情、强制改密、路由与异步响应门禁通过。
- OAuth 的授权链接生成与回调所有权由 Go 测试验证；浏览器用受控响应验证成功、失败、重试、复制、回填、关闭和离页停止轮询。未进行真实外部 OpenAI 登录。

自发现缺陷及防回归：文件名不能被重复邮箱隐藏；不能用 DOM 元素变量遮蔽刷新函数；写后刷新必须使旧快照失效；OAuth 在发起每次请求前再次检查弹层是否仍有效。真实浏览器检查覆盖这些路径，而非只依赖源码断言。

## 最终接口约定

所有入口位于 `/carpool/api/v1/admin`，复用同一个原版 `management.Handler`，不要求浏览器提供 Management Key。

| 方法与路径 | 契约 |
| --- | --- |
| `GET /auth-files` | 返回 `{files: [...]}`；只含 OpenAI/Codex 的安全展示字段，不含凭据、服务器路径或任意错误正文 |
| `POST /auth-files` | 每次一个文件：multipart 字段 `file` / `files`，或 `application/json` 配合 `?name=`；总请求不超过 1 MiB |
| `PATCH /auth-files/status` | `{name, auth_index?, disabled}`；索引不匹配或歧义时拒绝，持久化成功后才确认 |
| `POST /codex-auth-url` | 创建当前管理员会话独占的 OAuth state，返回原版授权 URL |
| `GET /get-auth-status?state=` | 返回 `wait` / `ok` / `error`；拒绝其他会话或供应商的 state |
| `POST /oauth-callback` | `{provider:"codex", redirect_url}`；暂存回调不等于完成登录，必须等待最终状态 |

- 导入接收 `type: codex` 且 `access_token` 非空的 OAuth JSON，以及本文末尾说明的 `sub2api-data` v1 导出文件。只含 `api_key`、只含 `refresh_token` 或 `type: openai` 的文件不是原版文件执行器可直接使用的授权记录，返回 `422`；API Key 继续通过原版管理配置创建，已有 OpenAI/Codex 配置账号仍可在这里查看与启停。
- 同名文件返回 `409 account_file_exists`，提示重命名；通过原子创建避免并发覆盖。原版管理入口的覆盖行为保持不变。
- 停用文件账号采用显式严格持久化；失败不发布新运行时状态。远程写入不持有全局账号锁；同一账号并发变化返回冲突，并协调存储与最新运行时状态。
- PostgreSQL / 对象存储严格写入即使本地镜像相同也会重试远程保存，避免远程失败后被本地缓存错误确认。

## 复现浏览器验收

以下脚本仅运行隔离的虚构账号服务；测试会拒绝非本机或未标记为 synthetic 的服务，不连接真实 OpenAI 登录。

```bash
go test -tags carpool_browser -c -o /tmp/carpool-accounts-browser.test ./internal/carpool
CARPOOL_BROWSER=1 CARPOOL_BROWSER_READY_FILE=/tmp/carpool-accounts-ready.json \
  /tmp/carpool-accounts-browser.test -test.run '^TestCarpoolBrowserFixture$' -test.timeout 0
# 在另一个终端执行；需要本机已有 Playwright 和 Chromium。
CARPOOL_BROWSER_READY_FILE=/tmp/carpool-accounts-ready.json \
  PLAYWRIGHT_MODULE=/path/to/playwright/index.mjs CHROMIUM_PATH=/path/to/chromium \
  node test/carpool-accounts-browser.mjs
```

结束时向隔离服务发送 `SIGTERM`，清理测试数据库与凭据。旧 readiness 文件不覆盖；新运行使用新的文件名。


## 最终证据索引

- 全仓 Go 测试：`/tmp/carpool-accounts-release-go.log`。
- 并发检测：`/tmp/carpool-accounts-release-race.log`。
- Node 回归：`/tmp/carpool-accounts-release-node.log`。
- 新账号页浏览器：`/tmp/carpool-accounts-release-browser.log`，截图与结构化结果在 `/tmp/carpool-accounts-evidence/`。
- 原有界面回归：`/tmp/carpool-accounts-existing-ui.log`。
- 默认文件存储、PostgreSQL、对象存储、Git 均覆盖严格保存；远程失败后相同内容重试、缺失镜像、并发版本冲突和不阻塞其他账号读取/选择都有确定性测试。
- 真实外部 OAuth 登录仍需用户本人执行。测试中的成功/失败/重试由模拟响应驱动，不声称已经使用真实 OpenAI 账号完成授权。


## 2026-09-18 部署记录

按用户明确要求部署至现有独立运行目录 `/home/qlqq/.local/share/cliproxy-carpool-runtime`，沿用原配置、环境和 `8317` 端口。已备份旧程序、配置、SQLite 一致性快照及账号目录，发送 `SIGTERM` 平滑停止旧进程后替换程序并重新启动。

- 备份：`backups/accounts-20260918T074421Z/`（相对运行目录）。
- 新进程 PID：`1093041`；首页返回 `200`，线上 JS/CSS 与本次源码构建逐字节一致。
- 未登录访问会话、账号列表和 OAuth 状态接口均返回 `401 session_required`；未使用真实管理员凭据或执行真实 OAuth 授权。
- 部署摘要与二进制 SHA-256 位于备份目录的 `deployment.json`；原配置和业务数据保留，不新增数据库迁移。

## sub2api 导出文件兼容（追加实现）

本次仅扩展账号导入，不修改已有部署配置或自动导入上传的真实账号。

- 接收 `type: sub2api-data`、`version: 1` 的导出文件，处理 `accounts` 中的 OpenAI OAuth 账号。
- 转换成原版 Codex 文件后，仍调用原版上传与运行时注册逻辑；原版 Codex JSON 保持支持。
- 使用字段白名单映射 access/refresh/id token、账号 ID、邮箱及绝对过期时间。丢弃恢复密码、TOTP、恢复信息、代理及其他系统专属调度设置。
- 单文件多账号逐个生成安全文件名，不覆盖已有账号；非法账号在写入前校验，存储或同名错误逐项反馈，部分成功不显示为全部成功。
- 测试只使用虚构数据；用户上传的文件仅进行结构检查与无持久化的转换验证，不写入测试代码、日志或线上账号目录。

### 本次验证与部署状态

- 已对用户样例进行仅内存转换验证；未导入真实账号。
- `go test ./...`、服务构建、HTTP API 竞态检测及 97 项 Node 测试通过。
- 隔离浏览器验证通过：5 项 sub2api 检查及 13 项原账号页回归，覆盖单账号映射、敏感字段丢弃、批量部分成功、全部重名提示与移动端展示。
- 日志：`/tmp/sub2api-final-go.log`、`/tmp/sub2api-final-race.log`、`/tmp/sub2api-node.log`、`/tmp/sub2api-final-browser.log`、`/tmp/sub2api-final-browser-regression.log`。
- 追加兼容已于 2026-09-18 08:06 UTC 部署，详情见下方记录。

### sub2api 兼容部署记录

- 2026-09-18 08:06 UTC 按用户要求部署至现有运行目录，端口仍为 `8317`；进程 PID `1143840`。
- 备份目录：`backups/sub2api-20260918T080645Z/`，含旧程序、配置、SQLite 一致性快照和账号目录。
- 已验证首页 `200`、线上 JS/CSS 与当前源码一致、未登录访问会话/账号/OAuth 状态接口均为 `401`；原配置保持不变。
- 二进制 SHA-256 与检查摘要存于备份目录的 `deployment.json`。未自动导入用户上传的真实账号。
