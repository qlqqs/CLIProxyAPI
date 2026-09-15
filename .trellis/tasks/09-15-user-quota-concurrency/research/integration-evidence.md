# 多周期额度与并发接入证据

> 本文记录实施前调查和方案演变，不代表最终代码状态。最终实现与验证见 `final-verification.md` 和 backend 多周期额度规范。

## 实施前链路

- `internal/carpool/httpapi/proxy.go` 的 `AuthenticatedRequestHook` 在鉴权后调用 `Control.AuthorizeProxy`，将不可变授权快照、凭据 scope 和请求 ID 写入上下文。
- `internal/carpool/service/control.go:1513` 的 `authorizeProxy` 先做路径、Home、同步记账故障检查，再调用 SQLite 原子授权并创建请求记录。Home 当前直接拒绝为 `home_not_supported`，本任务不需要新增 Home 接入或改写其并发协议。
- `internal/carpool/store/sqlite/authorization.go:112` 对当前成员月账期的已确认费用做准入判断。拒绝应保留安全原因码与请求记录，不能仅在前端限制。
- `internal/carpool/module.go:164` 起安装同步 usage 观察，`observeRequestCompletion` 向 writer 交付可信快照中的请求 ID。
- `internal/carpool/store/sqlite/billing.go` 的 `RecordUsageEventBilled` 以事件 ID 去重，事件、请求累计、月账期同事务持久化；新增短周期累计必须纳入同一事务，不能让异步重复投递再扣一次。

## 存储与清理约束

- 现有迁移为 `001_initial.sql`、`002_billing.sql`、`003_retention.sql`，需新增版本而非修改旧迁移。
- `002_billing.sql` 的 `billing_periods` 唯一键为 `(membership_id, period_from)`，同时保存月、周、5h 会冲突；推荐独立短周期表和请求关联，不破坏月账期主键与历史外键。
- 额度账本不能依赖当前仍保留的请求明细 SUM；删除明细不能恢复额度。
- 同步记账失败门禁、冻结价格、事件未知费用、历史 incomplete 与管理员清理确认契约沿用 `.trellis/spec/backend/carpool-accounting-guidelines.md` 和 `carpool-retention-guidelines.md`。

## 账号周期观测

- `internal/carpool/runtime/quota_projection.go` 有 Claude `5h` / `7d` 及 Codex 主次窗口投影，`ResetAt` 可缺失，Codex 可提供 `WindowMinutes`；不得无依据将任意主次窗口强制当作 5h / 7d。
- 已知且尚未结束的周期可继续使用，不应因为展示用快照新鲜度阈值就清空已知重置时间。
- `sdk/cliproxy/auth/quota_signals_test.go` 覆盖从业务响应头观测；首次或周期切换后缺失边界时不能既拒绝所有请求又依赖这些请求获得观测。
- 用户已同意未知周期暂不执行对应短周期限制并明确提示，保留月额度与并发。不能伪造用量为零、自动猜测无限重复的本地重置点，或收到同周期重复观测就清零。

## 并发接入方向与验证风险

- 本轮按一车一账号的拼车请求定义用户、账号请求并发；应按稳定用户 ID / AuthID 共享，不能按 API Key 或账号分配记录切分计数。
- 可在服务准入时原子取得两类名额，请求持有期间重试不重复占用；未开始执行的鉴权后错误、无效正文等也必须释放。
- `sdk/cliproxy/executor/lifecycle.go` 存在执行尝试资源绑定，但请求级名额不能在一次重试结束就过早释放。应结合 HTTP 生命周期与 completion 验证普通/流式/取消路径，释放保持幂等。
- 不用数据库历史 `in_progress` 行数代表当前进程活跃并发，不增加租约树、Redis 或上游网络超时；实现前在最终方案明确单实例边界。
- 模型查询不占新增生成并发；旧全局 Key、现有不支持路径与 Home 不扩展本轮范围。

## 实施拆分判断

暂保留一个任务：多周期与并发共享准入、配置 DTO、前端表单和生命周期验收，不拆成会争抢同一写集的独立子任务。实施可先做核心存储/运行时，再做 API/UI，最后全链路核查；交付仍需完整集成验收。

## 排队需求修订

用户最新要求并发不足时允许排队，覆盖立即拒绝方案。候选实现需满足：

- 等待期间不占用用户或账号执行名额，不启动上游，不持有数据库事务或互斥锁睡等。
- 原子判断并取得两类名额，避免持有账号名额等待用户名额导致死锁或空转。
- 等待受请求 context 取消与服务关停驱动，不添加请求执行或上游网络超时。
- 出队重新验证月/短周期额度、记账失败门禁、用户/API Key/成员资格和账号 scope，不能执行排队前已失效的授权。
- 中途调高限制或请求释放应能唤醒等待者；调低不强杀已开始请求。
- 使用 channel/显式同步做容量、取消、释放与调额测试，不依赖 Sleep。
- 现有 accounting.QueueSize 是记账失败缓冲，不能复用为请求排队容量或修改其已确认契约。
- 用户已确认有界队列、队满 HTTP 429、取消退队。具体落地为模块默认容量由用户指定为 10，保留容量配置能力，等待最终实施批准。
