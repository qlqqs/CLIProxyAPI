# `sqlite-core` 实现简报

## 目标

完成实施计划阶段 0.3 与阶段 2.2 的基础：验证纯 Go SQLite 驱动，建立
`internal/carpool/domain` 与 `internal/carpool/store/sqlite`，实现版本化迁移、
核心 schema、约束和可独立测试的仓库事务。

## 文件所有权

- `internal/carpool/domain/**`。
- `internal/carpool/store/sqlite/**`。
- `go.mod`、`go.sum` 中仅 SQLite 驱动所需依赖。
- 新建并维护 `research/sqlite-driver-probe.md`。

本轮不要接 Gin、server、CLI、配置加载、access provider 或前端。仓库接口应服务
领域用例，但不要为了尚不存在的调用者做宽泛抽象。

## 首批必做能力

- 纯 Go `modernc.org/sqlite`（或有证据的等价驱动）与 `CGO_ENABLED=0` 构建。
- 每条池连接启用外键、WAL、`busy_timeout`、`synchronous=NORMAL`。
- 内嵌递增 migration、checksum、未知新版本拒绝。
- 建立设计第 6 节全部表、CHECK、外键、复合索引和两个部分唯一索引。
- 仓库最小事务：bootstrap 管理员唯一性、用户/车辆基本写入、成员加入/换车、
  账号分配/移车、请求及 scope 快照、usage、审计、崩溃恢复。
- 使用 `t.TempDir`、可注入时钟和确定性并发测试；不得用 `time.Sleep` 判定正确性。

## 硬约束

- SQL 全部参数化并传递调用方 context；正确处理 rows/rollback/commit。
- 数据库目录/文件权限分别为 `0700`/`0600`。
- `usage_events` 不重复存 `car_id`/`user_id`；历史归属来自请求快照。
- 不修改 `internal/translator/**`。
- 你不是仓库中唯一工作的代理。不要回退或覆盖其他代理的改动；遇到 `go.mod` 或
  共享文件变化，保留他人新增内容并基于最新文件调整。

## 验证

运行新包测试、race 测试、`CGO_ENABLED=0` server build 和 `gofmt`。报告尚未实现
的仓库方法，不能把部分骨架描述为完整阶段交付；不要提交 Git commit。
