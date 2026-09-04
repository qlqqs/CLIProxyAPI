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
