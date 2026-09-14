# 2026-09-14 简化记账修复

## 用户最终决定

用户否决租约树、全局容量与独立结算迁移等复杂兜底，明确接受：正常同步记账，运行中
数据库失败拒绝新生成，固定缓存满后等待落库；崩溃后仅提示账务可能不完整，不因历史
记录封锁服务，不保证崩溃期间费用精确恢复。此决定取代较早研究中的更强恢复要求。

## 当前实现交接

- `module.go` 删除启动历史封锁，继续调用原 `RecoverInterruptedRequests`，有中断
  记录时输出安全警告；新生成仍按原鉴权与额度规则判断，自动清理正常启动。
- `writer.pending` 使用现有 QueueSize 为条数上限。满时阻塞当前回调，单一 worker
  用唤醒信号与本地重试周期尝试补写，成功后通知等待者。没有每事件 goroutine。
- 等待者仍持有自己已发生的记录；不宣称全部等待回调或全进程内存严格有界。
- 非计费模型 completion 使用独立原 best-effort 队列，不污染计费 pending。
- 插件流 defer 改为先发布 usage、后完成通知，不扩大 provider 退出协议。
- 清理/预览保护 in_progress 与 incomplete 请求、事件、scope 和关联账期。
- 不新增 schema、配置体系、UI、价格功能或对账解锁接口。

## 新增或调整的回归

- `TestModuleRestartAllowsGenerationWithInterruptedBillingMarker`
- `TestBoundedPendingWaitsRecoversAndDrainsCanceledPublishers`
- `TestNonBillableCompletionsNeverEnterFullBillingCache`
- `TestAdmissionDoesNotOvertakeWaitingIncurredUsage`
- `TestPluginStreamPublishesUsageBeforeCompletion`
- `TestRetentionProtectsInterruptedFactsAndPeriods`

保留原 Publish→SQLite、503、去重、旧全局 Key 隔离、取消和 Close 保库测试。

## 检查记录

实现代理报告定向、全量 test/vet、正常与无 CGO 构建通过。扩大 race 首次出现
`TestResponsesWebsocketReplaysImmediatelyAfterPinnedAuthFailure/unauthorized_to_websocket`
预期 close1012、实际 close1006 unexpectedEOF。随后该测试连续三次及整个扩大范围
race 重跑通过；未跳过测试，不能仅凭重跑就声称已经修复该偶发失败。

相关日志：`/tmp/billing-race.log`、`/tmp/billing-websocket-recheck.log`、
`/tmp/billing-race-final.log`、`/tmp/billing-all.log`、`/tmp/billing-vet.log`。
独立检查与主会话最终复跑结果待追加。

## 明确保留的边界

精确 crash replay、所有取消尾部生产者识别、等待者总体上限不在本轮承诺中；不把这些
用户已接受取舍重新列为要求复杂方案的阻断。价格冻结、执行前缺价、浏览器验收仍属于
整个金额/父任务的后续工作。本轮结束不自动提交或归档。


## 独立检查与主会话复核

独立检查修复 Close 排空后越过迟到等待者的问题：`RetryPending` 在仍有等待者时返回
记账不可用，避免提前关闭 SQLite。新增 `TestCloseDoesNotOvertakeLateWaitingUsage`。
worker 已退出后的迟到生产者需显式 RetryPending/Close 推进，保留为不承诺自动恢复的
边界，不再次引入生产者体系。

主会话发现本轮误改了 reset 的未知累计清零行为，已要求恢复父设计原契约：管理员
显式 reset 抵消确认金额及未知累计，但保留原始事件与 incomplete 标记。保护未完成
记录不等于改变 reset 产品行为。

主会话完整复跑 test/vet、两种构建和扩大 race 已通过。首次 WebSocket EOF 在这次
扩大 race 未再出现；最终 reset 小修后的命令结果另行追加。
日志：`/tmp/billing-simple-final-test.log`、`/tmp/billing-simple-final-vet.log`、
`/tmp/billing-simple-final-race.log`。


## 最终质量门禁

reset 契约恢复后再次执行：`gofmt -w .`、`go test ./...`、`go vet ./...`、
`go test -race ./internal/carpool/... ./sdk/cliproxy/usage ./sdk/cliproxy/auth ./sdk/api/handlers/...`、
正常和 `CGO_ENABLED=0` 服务构建、`git diff --check`。整条命令链退出码 0，全部通过。
最终日志沿用 `/tmp/billing-simple-final-{test,vet,race}.log`。

本轮简化范围检查通过；无新增迁移、无产品范围扩大、未提交或归档。之前三项风险中，
故障 pending 条数和明确未完成记录清理已修复；精确 crash 恢复按用户决定接受限制，
不能记为实现了完整结算系统。
