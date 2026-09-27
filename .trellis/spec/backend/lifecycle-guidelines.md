# 服务生命周期与关停超时规范

## 1. 适用范围与触发条件

当 `Run`、`Start` 或长期运行 goroutine 通过 `defer`、信号处理或父 context 取消来
触发资源关闭，并且关闭过程需要超时预算时，必须遵守本规范。典型资源包括 HTTP
server、队列 writer、WebSocket gateway、插件宿主和数据库。

## 2. 签名

当前服务使用以下关停边界：

```go
func shutdownOnRunExit(shutdown func(context.Context) error) error
func (s *Service) Shutdown(ctx context.Context) error
```

`Shutdown` 接收由调用方在“真正开始关停”时创建的 context；资源拥有者不能在服务
启动时预创建一个供未来关停使用的短期 context。

## 3. 契约

- 超时预算从关停操作开始时计时，而不是从服务启动、对象构造或注册 defer 时计时。
- `Run` 的父 context 通常已经被取消，因此优雅关停预算应从
  `context.Background()` 派生，再传给所有关停子步骤。
- 子步骤需要更短预算时，可从关停 context 派生；不能重新脱离父关停预算。
- 所有 ticker、生产者和入口先停止，再排空消费者，最后关闭持久化资源。
- `Shutdown` 必须幂等，保留第一个有意义的错误并继续完成仍可执行的清理。
- 关闭错误使用 `%w` 或 `errors.Join` 保留原因；日志不得包含凭据或请求正文。

## 4. 校验与错误矩阵

| 条件 | 预期行为 |
|---|---|
| 服务运行时间超过关停超时 | 关停 context 仍有效，deadline 位于未来 |
| `Run` 的父 context 已取消 | 独立的优雅关停预算仍可使用 |
| 关停回调为 `nil` | 返回 `nil`，不 panic |
| 子资源在预算内关闭 | 返回 `nil` |
| 子资源超时 | 返回包装后的 `context.DeadlineExceeded`，其他可执行清理仍运行 |
| 重复调用 `Shutdown` | 不重复关闭 channel、数据库或 server |

## 5. Good、Base 与 Bad 场景

- **Good**：服务运行数小时后收到 SIGINT，此时才创建 30 秒 context，writer 排空、
  WAL checkpoint 和数据库关闭成功。
- **Base**：测试服务启动后立即退出；仍断言传入回调的 context 有未来 deadline，
  而不是只依赖测试执行得足够快。
- **Bad**：在 `Run` 开头创建 30 秒 context，再把它捕获到 defer。运行超过 30 秒后，
  所有关停步骤一开始就收到 `context deadline exceeded`。

## 6. 必须测试的断言

- 关停回调收到非 nil context，`ctx.Err()==nil` 且存在未来 deadline。
- 回调返回的 sentinel error 可通过 `errors.Is` 识别。
- 重复关停不 panic、不二次关闭资源。
- 有队列时关停等待已入队记录完成；超时路径返回明确错误。
- 至少一次真实进程冒烟：服务运行超过旧预算后发送 SIGINT，日志中不得出现由过期
  context 引起的关停错误。

## 7. Wrong 与 Correct

### Wrong

```go
shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
defer service.Shutdown(shutdownCtx)
runUntilSignal()
```

### Correct

```go
defer func() {
    shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    if errShutdown := service.Shutdown(shutdownCtx); errShutdown != nil {
        log.WithError(errShutdown).Error("service shutdown failed")
    }
}()
runUntilSignal()
```

关键差异是 `context.WithTimeout` 位于 deferred closure 内，deadline 在执行清理时才
开始计时。

## 8. 插件替换与流终结的取消边界

- `(*Host).ApplyConfig(ctx, cfg)` 取消等待插件替换时必须释放全局 apply 锁。原生 register、quiesce、shutdown 或 rollback 尚未真正完成时，保留对应插件 loading token；仅后台清理该插件，不允许提前卸载仍在执行的模块。后续无关插件可以继续应用和卸载；被显式卸载、禁用或关停的旧实例不得被后台 rollback 复活。屏障期间新的重新启用意图必须在清理完成后合并到最新快照；同时恢复宿主维护的 thinking provider 注册，不能仅更新可见状态。Thinking Identifier 的原生调用在全局锁外准备，屏障内刷新使用已核对实例身份的缓存；发布时重算当前优先级并保留其他插件。
- `ResponsesWebsocket` 在上游首包 bootstrap 阻塞时也必须有唯一 reader 观察客户端断开并取消会话。待处理请求队列有界（16），满时关闭并取消，不以网络超时替代生命周期取消。
- wsrelay 正常 terminal 必须在全部已排队数据之后投递；不能为正常结束腾位置而丢最早一帧，也不能让等待 terminal 堵住共享 reader。错误/取消可以采用独立的优先失败策略。取消回调按 pending 实例比较删除，旧请求不能因复用 ID 而取消新请求。
- Gemini scanner 错误先于成功终结处理；Responses WebSocket 的 data channel 关闭时先检查已排队原始错误，再决定是否生成缺失完成事件错误。流内执行错误仍需把 `RetryAfter` 和凭据级作用域送入冷却结果。
- 必须用屏障覆盖原生调用阻塞时取消、单插件屏障/后台回滚、无关插件进展；用满缓冲覆盖 64 帧加正常结束不丢帧、请求 ID 复用取消隔离；用半截流和 bootstrap 阻塞覆盖“不先报成功”和断线回收。不得通过增加上游超时或 Sleep 掩盖生命周期错误。

- OAuth 测试调用 `CompleteOAuthSession` 只标记会话，不代表轮询协程已经退出。替换或恢复 `oauthSessions` 等全局夹具前必须等待 worker 退出；`oauth_codex_concurrency_test.go` 用 `synctest.Test` 隔离请求、在返回前推进虚拟轮询时钟并执行 `synctest.Wait`，长期存活的 handler 清理 ticker 留在 bubble 外。成功路径必须断言 `Completed`，不能把失败或过期当成成功。

错误：取消后继续同步等 replacement result，并持有全局 apply 锁。正确：保留单插件屏障，后台等待安全清理，当前调用按 context 返回。
