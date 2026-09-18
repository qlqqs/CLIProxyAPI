# Responses 根路径兼容

## 行为约定

客户端把供应商 `base_url` 配置为站点根地址时，会请求 `/responses`。服务为其提供以下别名；原 `/v1` 路径保持不变。

| 兼容入口 | 原入口 | 处理方式 |
| --- | --- | --- |
| `POST /responses` | `POST /v1/responses` | 共用 Responses handler，支持普通及 SSE 响应 |
| `GET /responses` | `GET /v1/responses` | 共用 WebSocket handler |
| `POST /responses/compact` | `POST /v1/responses/compact` | 共用 Compact handler |

不使用 HTTP 重定向，不改变请求体、鉴权头或流式协议，不增加网络超时。

## 安全和计费边界

- 别名与原入口共用 API Key 鉴权，缺失或无效 Key 返回 `401`。
- 拼车用户的 `POST /responses` 沿用原 `/v1/responses` 授权快照、可用账号范围、配额、并发及计费规则。
- 凭据冲突检查与请求结束清理必须识别根路径，避免出现绕过或并发许可泄漏。
- 拼车用户仍不能使用 Responses WebSocket 和 Compact；这些限制与原入口一致，别名不增加权限。
- 不新增数据库迁移，不修改用户或供应商凭据。

## 故障定位

已实测旧版本本机 `/responses` 返回空 `404`，公网 Lucky 将该响应替换成 gzip 压缩的 HTML 错误页。客户端直接显示压缩内容时会出现乱码。部署后应同时检查本机与公网新入口的 JSON `401`，不能仅凭进程启动就判定兼容成功。

线上匿名/无效 Key 探测仅验证入口与鉴权，不证明真实账号的上游调用成功；正常调用、流式响应、隔离与计费由虚构凭据的集成测试覆盖。

## 验证记录

- 全仓 `go test ./...` 与服务构建通过；日志 `/tmp/responses-alias-full-go.log`。
- 97 项 Node 回归通过；日志 `/tmp/responses-alias-node.log`。
- 新增测试覆盖原入口/别名 handler 与响应一致性、缺失/无效 Key、安全模式、冲突凭据、拼车普通/SSE 成功与 scope 隔离、价格缺失拒绝、额度耗尽拒绝、错误请求后的并发名额释放。
- 六个相关包的 `go test -race` 通过；日志 `/tmp/responses-alias-race.log`。
- 独立检查通过，定向 `go vet` 与格式检查通过。

## 2026-09-18 部署记录

已于 08:37 UTC 按用户确认部署至 `/home/qlqq/.local/share/cliproxy-carpool-runtime`，保留配置和业务数据，无数据库迁移。

- PID：`1187402`；端口：`8317`。
- 备份：`backups/responses-alias-20260918T083711Z/`，包含旧程序、配置、SQLite 一致性快照和账号目录。
- 首页 `200`，JS/CSS 与当前源码一致，管理鉴权保持 `401`。
- 本机和公网 `https://cpa.qlqqs.com:8888` 的 `GET /responses`、`POST /responses`、`POST /responses/compact` 及原 `POST /v1/responses`，缺失和无效 Key 均返回 JSON `401`，共 16 项验证通过；公网不再返回 Lucky 404 压缩错误页。
- 未使用 `test` 用户的真实 Key 发起模型调用，请用户重试确认其账号、模型和车辆配置。
- 部署摘要与二进制 SHA-256：备份目录 `deployment.json`；运行记录 `/tmp/responses-alias-deployment.log`。
