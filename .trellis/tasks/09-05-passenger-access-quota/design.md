# 改密说明与原生账号配额设计

## 契约来源

完整设计见 `../09-04-passenger-quota-usage/design.md` 第 6、8 节，负责 R1、R7 与 R12 基础页面。
研究证据见父任务 `research/implementation-boundaries.md`。无数据库迁移和计价改动。

## 改密

沿用 `must_change_password` 的 handler 门禁与导航限制。`CreateUser` 初始
`PasswordVersion=1`，`ResetPassword` 增加版本并设置 must-change，可由服务端派生
安全 `password_change_reason=initial_password/admin_reset`，不向前端暴露密码版本或审计体。
普通主动改密沿用普通说明；管理员重置说明位于表单前，包含临时密码、安全目的、业务数据
未删除及重新登录。改密成功继续退出当前会话，不自动恢复旧会话。

## 原生配额

扩展 `runtime` 安全投影和 `service.AccountView`，`GET /me/accounts` 添加 `quota`。
复用 CPA `Quota.Signals`/`ObservedAt`，只读快照；不影响调度 cooldown，不向上游发新请求。
窗口形状、顺序及 null 规则按父设计；局部投影函数统一解析，前端不解析原始响应头。
账号健康和 quota 观测时间分别计算陈旧性。解析失败按缺失而不是 0；相对 reset 以观测时刻
换算，避免每次刷新重置倒计时。

## 界面与所有权

修改 `internal/carpool/runtime/account_status.go` 或新增相邻 quota 投影文件、
`service/control.go`、`httpapi/api.go`、`web/assets/app.js`、`app.css` 及对应测试。
必要时只补 SDK 配额投影的最小可复用接口，不更改 quota 采集协议或账号选择规则。
方向选择后依据项目级 `DESIGN.md` 重建全局壳、导航、登录/改密及现有基础页面的视觉与布局，
保留全部操作、原生可访问展开语义、固定进度槽和可见状态文字，不复用旧 CSS 骨架。
两个主要窗口可在移动端上下排列；UI 定时刷新保留展开状态，不重新抢焦点。

## 风险与回退

信号可能缺失、无百分比或陈旧，这些均是正常数据状态，不算接口故障。
不直接序列化 `Auth` 或原始 signal，安全 DTO 使用已知字段白名单。
本项无迁移，可单独撤销新增展示；不能撤销或放宽原改密/车辆授权门禁。
