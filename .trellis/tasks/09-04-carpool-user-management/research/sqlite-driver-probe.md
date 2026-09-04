# SQLite 驱动与仓库探针

## 结论

第一阶段采用 `modernc.org/sqlite v1.58.0`。该版本：

- 模块声明要求 Go 1.25，已在本仓库 Go 1.26.0 工具链通过编译与测试；
- 使用纯 Go 实现，不依赖 CGO；
- 许可证为 BSD-3-Clause，与本项目的分发方式兼容；
- 支持通过 DSN 对每条新建连接应用已校验的 PRAGMA，并支持
  `_txlock=immediate`。

实际验证命令：

```bash
go test ./internal/carpool/...
go test -race ./internal/carpool/...
CGO_ENABLED=0 go build -o test-output ./cmd/server
```

以上命令均通过，构建产物已删除。

## 连接配置

数据库使用文件 URI，固定连接参数为：

```text
_foreign_keys=1
_journal_mode=WAL
_busy_timeout=5000
_synchronous=NORMAL
_txlock=immediate
_defensive=1
_dqs=0
```

`busy_timeout` 可由仓库构造参数覆盖，默认 5000 ms。连接池默认最多 4 条打开
连接和 4 条空闲连接。测试同时固定 4 条连接，逐条读取并确认：

- `foreign_keys=1`
- `journal_mode=wal`
- `busy_timeout` 等于配置值
- `synchronous=1`，即 `NORMAL`

写事务通过 `_txlock=immediate` 在 `BeginTx` 时取得保留锁，使席位检查、当前关系
检查与写入串行发生；并发命令还携带 `ExpectedCurrentID`，避免后到的换车或移车
请求在等待锁后被当作新的顺序操作再次提交。锁等待耗尽映射为稳定的
`domain.ErrBusy`，唯一约束和跨表触发器失败映射为 `domain.ErrConflict`。

## 文件与迁移

- 新建数据库目录使用 `0700`，数据库主文件使用 `0600`。
- SQL migration 通过 `go:embed` 随二进制发布，版本从 1 连续递增。
- `schema_migrations` 保存版本、名称、固定 SHA-256 checksum 和应用时间。
- 单个 migration 与 ledger 写入处于同一事务；故障测试确认失败 DDL 和版本记录
  都会回滚。
- 启动时拒绝 checksum 变化、迁移历史缺口和高于当前二进制的新版本。
- `Checkpoint` 使用 `PRAGMA wal_checkpoint(TRUNCATE)`。探针在 checkpoint 并关闭
  数据库后只复制主文件，再从副本重开并验证 schema 与业务数据。

首版 migration 建立 10 张业务表及 migration ledger，并包含：

- 用户名、`user_ref`、会话摘要、Key ID 和公开引用唯一约束；
- 用户角色、用户/车辆状态、请求终态和布尔值 CHECK；
- 每名乘客一个当前车辆、每个 `AuthID` 一个当前车辆，以及车内展示名/安全标签
  的两个部分唯一索引组；
- 管理员不得拥有乘客关系或用户 API Key 的数据库触发器；
- 请求 scope 必须匹配请求车辆和分配记录的触发器；
- usage 通过 `(request_id, auth_id, assignment_id)` 引用冻结 scope，且不重复保存
  `car_id` 或 `user_id`。

## 事务与并发验证

仓库测试已覆盖：

- 8 个并发 bootstrap 只有一个管理员创建成功；
- 同一乘客的并发首次上车只有一个提交；
- 同一 `AuthID` 的并发首次分配只有一个提交；
- 席位上限、车内展示名、账号安全标签和角色约束；
- 用户/账号换车保留历史且只有一个当前关系；
- 用户、Key、成员、车辆和运行时存在账号集合在同一授权事务读取，随后同步写入
  `proxy_requests` 与全部 `proxy_request_auth_scopes`；
- 授权失败在同一事务写 `rejected`，已进入 handler 的请求也可从
  `in_progress` 更新为 `rejected`；
- 账号移车后，迟到 usage 仍从请求 scope 取得原 `assignment_id`、引用和标签；
- 未知 usage 不接受 Token 数字，已知零保持为已知零，`event_id` 重放幂等；
- 启动恢复将全部遗留 `in_progress` 原子转成 `incomplete`；
- 审计游标分页及按成员、账号的基础 UTC 半开区间聚合。

所有 TTL、顺序和并发测试使用固定时钟及显式同步，不使用 `time.Sleep` 判断
正确性。

## 已知边界

- 只支持单机、单 CPA 进程写入同一数据库文件；不支持网络文件系统或多实例共享。
- 运行中只复制主数据库文件不是受支持的备份方式；必须先停止写入、checkpoint、
  关闭数据库后复制，后续 CLI 运维命令由实施阶段 8 接入。
- 当前聚合接口是第一阶段基础查询，不包含保留清理器、配置时区预设周期或计费
  语义；这些仍由后续领域服务和运维阶段完成。
- `BeginProxyRequest` 是已冻结事实的底层写入接口；代理授权入口必须使用
  `AuthorizeAndBeginProxyRequest`，禁止先读取业务关系再单独调用前者。
