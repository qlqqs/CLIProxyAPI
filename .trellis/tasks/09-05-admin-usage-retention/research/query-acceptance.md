# 请求明细查询验收映射

## 范围与结论（2026-09-14）

本工作单仅负责请求明细查询及其 service/HTTP 游标契约，不修改计价、同步记账、
清理操作、迁移或前端。实际 SQLite 查询原已正确使用 `EXISTS`，未发现需要修改
`usage_details.go` 的业务错误，因此不做查询重构；补充真实数据库回归测试。

经主会话授权，额外修复：

1. 原游标仅包含末项键，没有绑定筛选，也没有冻结时间范围；默认 `today` 跨午夜加载
   下一页会丢失剩余主行，新记录还会改变下一页总数。现使用版本 2 的范围/筛选绑定游标。
2. 原 HTTP `period` 直接回显可空输入，默认查询返回 `null/null`。新增专用
   `UsageRequestPage`，嵌入原 `Page` 并携带 `ReportPeriod`；不改变其他集合的通用分页类型。
3. 原 HTTP 接受 `limit=1..25`，service 实际始终返回 25。按 PRD 固定每批 25，现只接受
   省略或 `25`，其他值返回 `422`。
4. `GetRetentionSettings` 失败不再静默使用配置默认范围；直接传播错误，不调用明细查询，
   防止数据库覆盖暂时不可读时扩大可读范围。
5. 事件 DTO 增加 `cache_read_tokens`、`cache_write_tokens`，保留兼容的 `cached_tokens`；
   缺失仍为 `null`，已知零仍为数字零。

## 最小公开契约

- `GET /admin/usage/requests` 返回 `items`、逻辑请求 `total`、可空 `next_cursor`，
  `period.from/to` 为该次分页实际使用的 UTC `[from,to)`。
- 首屏固定实际时间上界；自定义未来 `to` 收窄到首屏查询时刻。后页复用首屏范围，不能
  用加载更多时的 `now` 重算 `today/7d/30d`。
- 游标绑定去除空白的车辆、乘客、账号、Key、模型、结果、计价状态、完整请求 ID，以及
  `period` 或自定义起止输入。省略 `period` 与显式 `today` 等价；自定义时间按 UTC 规范化。
- 更换任一筛选或自定义边界必须清空游标；继续使用原游标返回 `422`。旧版本游标也返回
  `422`，调用方重新加载首屏即可恢复。
- 排序固定 `(started_at DESC, request_id DESC)`，末项键为排他边界。解码校验版本、类型、
  大小、非空安全请求 ID、有效起止及末项在范围内；拒绝未来上界、时间范围扩大和过期范围。
- 游标不授予权限，也不是签名快照；每次请求仍验证管理员权限及当前保留范围。不引入 HMAC、
  会话缓存或持久化快照体系。
- 模型匹配请求模型或任一实际事件模型。账号匹配实际事件，而不是仅在允许账号 scope
  中出现。不同维度可以由同一请求的不同事件满足；筛选选中主行后，展开返回全部事件。
- 当前 `total` 是固定时间范围内实时匹配的逻辑请求数，不是不可变快照总数；迟到的回填记录、
  结果/费用状态变化或并行清理仍可能改变数量。新开始且位于首屏上界之后的请求不进入后页。

## 确定性验收证据

| 验收项 | 测试与断言 |
| --- | --- |
| 全部 AND 组合 | `TestUsageRequestsFiltersAndLogicalCounts`：8 维全部 256 个子集；列表与计数一致；模型分别只匹配请求、只匹配事件、同时匹配；每请求 3 事件不变成 3 主行 |
| 身份过滤与精确 ID | 同上：car/user/assignment/key 不匹配；ID 大小写、前缀、SQL 特殊字符不扩大结果；`TestAdminUsageRequestsResolvesPublicIdentityFilters` 验证公共引用解析后组合命中真实 SQLite |
| 时间边界 | SQLite 测试覆盖 `from` 包含、`from-1µs` 排除、`to` 排除；事件发生于请求筛选范围之外也不会丢失已选主行 |
| 全事件展开 | `TestUsageRequestsExpandAllAttemptsAndPreserveSnapshots`：账号条件与模型条件由不同事件满足；3 事件完整返回，scope-only 请求不命中实际账号筛选 |
| 安全快照与缺失状态 | 同上：成员/账号改名后历史展示不变；已确认小计 `225000000` 与 unknown 状态共存；legacy 不重计价；已知零与未知 null 区分；不生成 event_seq/attempt_no |
| SQLite 稳定键分页 | `TestUsageRequestsStableTuplePages`：55 条同时间记录以固定 25/25/5 返回；插入更大同时间 ID 和更新请求后仍无重复/遗漏；空尾页正确 |
| service 固定范围分页 | `TestAdminUsageRequestsStablePagesAcrossMidnightAndNewInsert`：真实数据库、时钟显式跨午夜，25/25/5 与 `total=55` 保持，后续新请求不混入 |
| 筛选绑定 | `TestAdminUsageRequestsCursorBindsEveryFilter`：车辆/乘客/账号/Key/模型/结果/计价/ID/period/custom 修改均拒绝；等价空白/default today 可继续 |
| 范围/元组/权限校验 | `TestAdminUsageRequestsCustomHighWaterAndMalformedCursor`、`TestAdminUsageRequestsCursorRejectsInvalidTupleAndWidenedRange`：未来自定义上界、畸形/旧游标、零时间、坏 ID、范围外元组、逆序、扩大 today、未来上界、超过保留期限、乘客访问均受限 |
| 失败关闭 | `TestAdminUsageRequestsRetentionReadFailureDoesNotFallBack`：返回 `domain.ErrBusy`，`ListUsageRequests` 调用次数为零 |
| 真实 HTTP 契约 | `TestAdministratorUsageRequestsHTTPContract`：登录后请求列表/详情；25/1 分页、真实 period、非法 limit 和换筛选游标返回 422、完整 ID 查询、空详情 events 为数组、未知费用为 null |
| 缓存维度 | `TestUsageEventResponsePreservesSeparateCacheBuckets`：cached/read/write 为 19/12/7，单独缺失为 null，明确 0 不丢失 |

所有新测试使用 `t.TempDir`、隔离 SQLite、固定时钟和显式时间变更；不调用真实上游、
不使用 `time.Sleep`，不访问生产数据库。

## 文件归属

本工作单修改：

- `internal/carpool/service/control.go`：仅 `AdminUsageRequests` 及相邻专用 `UsageRequestPage`。
- `internal/carpool/service/values.go`：仅请求明细游标及规范化摘要辅助逻辑。
- `internal/carpool/httpapi/api.go`：仅明细列表实际 period、固定 limit、事件缓存字段。
- 新增 `internal/carpool/store/sqlite/usage_details_test.go`。
- 新增 `internal/carpool/service/usage_requests_test.go`。
- 新增 `internal/carpool/httpapi/usage_requests_test.go`。

其他工作单同时修改共享文件的保留方法及 UI；不属于本报告的实现或独立通过声明。

## 未覆盖项与交接

- 前端按需详情加载、筛选 URL、取消/恢复/桌面移动流程由 UI 工作单验证；本工作单不运行浏览器。
- 父 PRD R8 的主行总 Token、安全 Key 名称目前不是列表 DTO 字段：列表提供安全 Key 引用、
  事件数和金额，Token 在事件详情。不能据本组测试声称主行全部视觉字段已经验收。
- `unknown_cost_events` 当前仅计 `pricing_status=unknown`；legacy/pending 独立保留其状态。
  未改计价状态机，未将已确认小计冒充供应商完整账单。
- 读取保留设置失败的修复限定于 `AdminUsageRequests`；旧聚合 `AdminUsage` 不在本次修改范围。
- 时间范围固定不是跨请求数据库事务快照；列表与总数为独立 SQL。并发清理、迟到回填或状态
  变化仍可能影响实时结果，这是当前简单查询契约的边界，不增加锁或持久化快照。
- 未跑全套仓库测试；主会话负责最终全量质量门禁。

## 执行结果

- `gofmt -w`：仅本工作单修改的 Go 文件，完成；未全目录格式化以免触碰并行修改。
- 定向普通测试：`go test ./internal/carpool/store/sqlite ./internal/carpool/service ./internal/carpool/httpapi -run '^(TestUsageRequests|TestAdminUsageRequests|TestAdministratorUsageRequests|TestUsageEventResponse)' -count=1`，通过。
- 最终定向竞争检测：相同包和 `-run`，加 `-race -count=1`，通过；SQLite 9.437s、service 8.487s、HTTP 2.104s。
- 必需 server 编译：`go build -o "$build_dir/test-output" ./cmd/server`，通过；`build_dir` 为 `mktemp -d /tmp/carpool-query-build.XXXXXX` 的隔离目录，成功后删除本次输出及空目录。
- 未修改生产查询 SQL、未新增迁移、未运行全套测试、未提交或部署。
