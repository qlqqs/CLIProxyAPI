# 管理鉴权、OAuth 回调与拼车登录边界

## 1. 适用范围

修改 management HTTP 鉴权、Codex/Claude CLI OAuth、OAuth token 落盘、拼车登录限流或账号删除时读取。Home 证书注册另见 `internal/home/TLS_COMPATIBILITY_CN.md`。

## 2. 接口与存储

- `(*management.Handler).Middleware()`：以直接 TCP 对端建立管理请求身份。
- `(*management.Handler).AuthenticateManagementKey(clientIP string, localClient bool, provided string)`：供 HTTP 与非 HTTP 入口复用；调用方必须先确定可信来源。
- `codex.NewOAuthServer(port int, expectedState string)` / `claude.NewOAuthServer(port int, expectedState string)`：必须传入本次登录随机 state。
- `tokenfile.WriteJSON(path string, value any) error`：同目录私有临时文件、完整写入后原子替换。
- `(*service.LoginLimiter).AcquireVerification(clientAddress string) (func(), time.Duration)`：非 nil release 表示获得口令验证名额，调用方必须释放。
- 拼车删除仍走 `DELETE /carpool/api/v1/admin/auth-files`，目标校验复用 `ValidateCarpoolAccountTarget`。

## 3. 行为合同

- 不从 Gin 默认可信的 `ClientIP()` 推导管理 localhost 特权或封禁身份。直接对端为 loopback 但携带 `Forwarded`、`X-Forwarded-*` 等代理元数据时，必须按远程请求处理，并使用独立的 forwarded-peer 封禁桶，不能封禁真正直连 localhost。
- 反向代理需保留转发元数据，并显式开启远程管理；仅配置本地密码不能授权代理转发的远程访问。完全移除转发元数据的 loopback 代理与真正本地客户端无法区分，部署不能依赖应用猜测来源。
- 仅有 local password、没有配置或环境 management key 时，真实本地请求仍可认证；远程请求不能使用此例外。
- OAuth 成功、错误、缺 code 的回调都先校验唯一且匹配的 state；非法回调不能消费待登录结果。监听固定为 IPv4 loopback，不能绑定所有接口。SDK 手动回调路径也须先校验 state 再处理错误。
- OAuth 成功页面的目标 URL 仅允许对应官方平台 HTTPS host，并在写入 HTML 前转义；不允许 `javascript:` 或任意外部 host。
- 新建和覆盖 OAuth token 文件均为 `0600`；父目录新建为 `0700`，已有目录权限不自动扩大。编码或替换失败不得损坏旧 token，临时秘密文件必须清理，元数据合并保持原行为。
- 用户名/来源组合限流之外，来源地址具有独立预算；口令哈希还受全局并发名额限制。成功登录只重置用户名桶，不重置来源预算。不存在用户与错误密码仍使用统一失败响应和审计，不泄露账号是否存在。
- 拼车 DELETE 与列表、PATCH 使用相同提供商/插件虚拟账号边界；`auth_index` 是定位信息，不是绕过权限的秘密。

## 4. 错误矩阵

| 条件 | 预期 |
| --- | --- |
| 远程伪造转发 localhost | 不获得本地例外，不污染真正 localhost 封禁桶 |
| loopback 代理转发，但只配 local password | 拒绝；只有直接本地调用可用该密码 |
| OAuth state 错误、缺失或重复 | HTTP 400，登录结果 channel 不被消费 |
| 正确 state 的 OAuth 拒绝 | 返回本次登录错误，可终结该等待 |
| 成功页传入脚本 scheme 或属性注入 | 回退安全平台 URL，无可执行注入 |
| 登录来源预算或验证并发耗尽 | 429 与 Retry-After，不启动额外口令哈希 |
| 删除不属于拼车允许范围的账号 | 403，文件与运行时凭据均保持不变 |

## 5. 正常、基础与错误案例

- 正常：真实 localhost 使用 standalone TUI 的 local password；OAuth 浏览器回调携带本次 state，完成兑换。
- 基础：远程管理已显式开启且使用有效管理 key；不存在用户与错误密码得到相同对外语义。
- 错误：先发送 OAuth error 到一次性 channel，再由 SDK 校验 state；攻击者已经终止了合法登录等待。
- 错误：只对新文件设置私有 mode，覆盖已有 `0644` token 后仍保留宽权限。

## 6. 必须测试

覆盖直接远端伪造 IP、loopback 代理、封禁隔离、local-password-only；OAuth 非法回调不打断后续合法回调、绑定地址、安全成功页面；新建与覆盖权限、原子替换失败保留旧文件；用户名轮换、来源独立、并发释放及正确重试时间；不支持的账号 DELETE 不改变文件/运行时。仅使用合成 token、loopback 和注入时钟，不接真实上游。

## 7. 错误与正确写法

错误：`localClient := c.ClientIP() == "127.0.0.1"`。

正确：从直接对端识别 loopback，并拒绝让携带代理转发元数据的请求继承本地例外；封禁键同样不能取自可伪造 header。
