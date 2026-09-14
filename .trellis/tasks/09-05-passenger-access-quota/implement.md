# 改密说明与原生配额实施计划

## 启动条件

四方向选择完成，项目级 `DESIGN.md` 已编译；父任务启动后可启动本子任务。
方向 B 的圆角、留白和中性深色规则必须贯穿所有页面，仍处于 `planning` 时不写产品代码。
读取本目录三份文档、JSONL 和父任务设计第 6、8 节；无前置业务依赖。

## 顺序

- [ ] 读取 `trellis-before-dev` 与拼车/质量规范，确认当前共享文件没有未理解的变更。
- [ ] 依据选定设计重建全局导航、登录/改密和全部现有基础页面，不保留旧布局，逐项核对旧操作。
- [ ] 添加密码重置/首次临时密码/主动改密区别及数据恢复测试。
- [ ] 添加 quota 投影表驱动测试，覆盖 Claude、Codex、其他供应商、部分字段、非法值、
  多窗口排序、绝对/相对重置时间及独立陈旧时间。
- [ ] 更新 service/DTO 和会话原因字段，保留同车过滤及 Origin/CSRF/改密门禁。
- [ ] 实现说明、进度与展开状态，窄屏、错误和键盘操作完整。
- [ ] 运行定向检查与浏览器，保存证据后派发 `trellis-check`，关闭问题。

## 验证

```bash
gofmt -w .
go test ./internal/carpool/runtime ./internal/carpool/service ./internal/carpool/httpapi ./internal/carpool/web
node --check internal/carpool/web/assets/app.js
go build -o test-output ./cmd/server
```

复用父任务的输出路径安全规则及桌面/移动截图要求。用同车两个账号、至少三个 quota 窗口、
一个缺失账号和一个陈旧账号，验证刷新/展开与脱敏。完成本项后把实际字段契约交给下一项。

## 回滚与检查

只改展示和安全投影，不迁移数据库。若发现必须改变原生配额语义，先回到父任务核对，
不能把账号剩余美元额度当成乘客限额。共享前端代码不能与下一子任务并行修改。
