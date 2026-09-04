# 拼车用户管理第一阶段验收记录

## 验收结论

第一阶段实现通过代码检查、自动化测试、真实浏览器主流程、停机备份恢复、最新
二进制冒烟和 Docker 容器重建持久化验证。PRD 中 AC1 至 AC14 均有自动化测试或
可复验记录支撑，未发现发布阻断项。

本记录对应分支 `feature/carpool-user-management`，验收日期为 2026-09-04。

## 自动化质量门

以下命令均已通过：

```text
gofmt -l .
git diff --check
node --check internal/carpool/web/assets/app.js
python3 ./.trellis/scripts/task.py validate 09-04-carpool-user-management
go vet ./...
go test ./... -count=1
go test -race ./internal/carpool/... -count=1
go test -race ./sdk/cliproxy/usage -count=1
go build -o test-output ./cmd/server
CGO_ENABLED=0 go build -o test-output ./cmd/server
docker build --build-arg HTTP_PROXY --build-arg HTTPS_PROXY --build-arg NO_PROXY .
```

最终源码另构建为镜像 `cliproxy-api:carpool-acceptance`，镜像构建成功。第一次未传入
宿主机代理的构建因容器无法直连 `proxy.golang.org` 超时；传入已有代理后成功，
确认该失败不是代码或 Dockerfile 问题。

## 自动化覆盖

自动化测试覆盖以下发布边界：

- 拼车配置默认关闭、非法配置拒绝启动、SQLite 迁移与未知 schema 拒绝。
- 用户、角色、密码、会话、CSRF、Origin、用户 API Key 和凭证位置冲突。
- 席位、成员换车、账号移车及并发唯一约束。
- `CredentialScope` 在初选、重试、插件、pinned、affinity、mixed 和 Home 路径
  中不可扩张，并覆盖 scoped models。
- 非流式、流式、取消、重试、已知与未知 usage、逻辑请求终态和崩溃恢复。
- 成员及账号历史快照、半开时间区间、五类账号状态和陈旧观测。
- `/management.html`、旧全局 Key、Management API、未知拼车 API 和静态资源兼容。
- Argon2id、数据库/目录权限、敏感 DTO、日志和审计金丝雀防泄漏。
- 命名 usage 插件注销、模块关闭后的 writer 清理和服务关停 context 生命周期。

`internal/translator/**` 没有业务授权改动。

## 浏览器主流程

真实 Chromium 验收覆盖管理员和乘客两个角色，结果如下：

```json
{"admin":true,"passenger":true,"scopedModels":200,"deniedCompact":403,"deniedManagement":401,"revokedKey":401,"consoleErrors":4,"requestFailures":3}
```

记录中的 console error 与 request failure 均来自脚本主动验证的 `401`/`403`，以及
Chromium 对 `204` 响应记录的 `ERR_ABORTED`，没有非预期前端异常。桌面与移动截图、
Playwright 依赖、测试库及脚本保留在：

```text
/tmp/cliproxy-carpool-browser-20260904
```

## 最新二进制冒烟

使用收尾阶段重新构建的二进制和既有验收数据库执行：

```json
{"login":201,"car_ref":"car_YBCjP-gyY--zMFfs","patch":200,"omitted_fields_preserved":true,"restore":200}
```

冒烟只提交车辆 `name`，确认省略的 `description`、`seat_limit` 和 `status` 均保持
不变，随后恢复原名称。发送 SIGINT 后进程立即退出，未出现
`context deadline exceeded`。

SQLite 主库不能位于系统临时目录；最新二进制正确拒绝了 `/tmp` 下的相对数据库
路径。因此浏览器验收证据仍放在 `/tmp`，实际运行库放在：

```text
/home/qlqq/.local/share/cliproxy-carpool-browser-20260904/carpool.db
```

## Docker 重建持久化

以最终源码构建 `cliproxy-api:carpool-acceptance`，把同一宿主机数据目录依次挂载到
两个全新容器。第一个容器登录返回 `201` 并读取到 1 辆车；停止并删除该容器后，
第二个容器登录仍返回 `201` 并读取到同一辆车：

```json
{"container":1,"login":201,"cars":1}
{"container":2,"login":201,"cars":1,"persisted":true}
```

两个一次性容器均已删除，bind-mounted 数据保留在：

```text
/home/qlqq/.local/share/cliproxy-carpool-docker-smoke-20260904
```

## 停机备份与恢复

- schema 版本：`1`
- 数据库、备份和恢复库文件权限：`0600`
- 原库、备份、恢复库 SHA-256：
  `f8428d04b6139485706b24b188dd3af1520185ff32cc014b244095b50c8d1975`
- 恢复后登录成功。
- 恢复后数据：2 个用户、1 辆车、13 条审计、1 个用量分组。

## AC 对照

| 验收项 | 结论 | 主要证据 |
|---|---|---|
| AC1 | 通过 | bootstrap、登录、会话、改密、CSRF 与凭证防泄漏测试 |
| AC2 | 通过 | 多 Key、撤销、禁用、角色不变式测试及浏览器撤销冒烟 |
| AC3 | 通过 | 四凭证域、匿名、exclusive、跨位置冲突和旧 Key 回归测试 |
| AC4 | 通过 | HTTP 流程和 SQLite 并发席位/成员唯一约束测试 |
| AC5 | 通过 | 多账号、并发分配、换车及历史快照测试 |
| AC6 | 通过 | selector、retry、plugin、pinned、affinity、mixed 与模型测试 |
| AC7 | 通过 | 禁用入口前置拒绝和 nil scope 兼容测试 |
| AC8 | 通过 | 乘客 DTO 白名单、报表权限与敏感值金丝雀测试 |
| AC9 | 通过 | 五态映射、5 分钟边界与账号消失测试 |
| AC10 | 通过 | 请求生命周期、重试、流式取消、未知 usage 和恢复测试 |
| AC11 | 通过 | 半开区间、成员用量边界与冻结归属测试 |
| AC12 | 通过 | migration、并发、清理、备份恢复及双容器重建验证 |
| AC13 | 通过 | 管理端回归、无 Node 构建、静态/API 路由及浏览器测试 |
| AC14 | 通过 | 格式、vet、全量测试、race、双构建及 translator 路径检查 |

## 第一阶段边界

验收不扩大已经批准的范围：用户 Key 仍不开放 Home、Responses WebSocket、
Realtime 或 wsrelay；不支持多实例共享 SQLite、自助上车、硬额度、计费和在线热备。
人工验收计划中需要真实上游账号的多协议调用及强制故障重试没有用假账号伪造，
对应隔离与生命周期由自动化集成测试提供发布证据。
