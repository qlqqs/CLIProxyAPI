# 全页面重设计实现与验收

## 最终决定

采用 B 的窄轨导航与主从工作区，配以 Claude 风格的暖纸白、象牙表面、陶土动作色、深灰文字与柔和圆角。以用户最后的对话指示为准；`selection.json` 保留初次选择的原始记录，不篡改为后来的调整。

没有创建 Trellis 任务，没有提交代码或重启生产服务。其他工作流的请求明细与数据保留改动已保留。

## 实际修改

- `internal/carpool/web/assets/app.js`：导航与移动菜单、登录、两端概览、用户/车辆非模态详情、路由错误恢复、旧响应隔离、详情关闭与焦点。
- `internal/carpool/web/assets/app.css`：整套暖色令牌、布局、列表、表单、表格、状态、弹层、原生进度、响应式与 reduced-motion。
- `internal/carpool/web/assets/index.html`：浅色主题与浏览器主题色；移除整个应用树的重复 live 播报，具体状态仍由组件播报。
- `DESIGN.md`：最终设计锚、功能与兼容边界。
- `test/carpool-browser.mjs`：原业务回归适配折叠移动导航，保留原业务断言。
- `test/carpool-redesign.test.mjs`、`test/carpool-redesign-browser.mjs`：导航、金额语义、CSP、全路由、布局、焦点、错误恢复与异步竞态测试。

## 全页面覆盖

管理端：概览、用户、车辆、用量、请求、价格、保留、审计、改密共九个入口；用户端：概览、成员、账号、Key、改密共五个入口。登录、首次强制改密、创建用户、上车、调额、Key 一次性展示、请求展开和清理确认均覆盖。

桌面 1440px、窄桌面 1024px、手机 390px / 360px；另有 200% 缩放。桌面窄轨、列表和详情有独立滚动边界，手机折叠导航与单列详情无整页横向溢出。业务表格仍允许自身横向滚动，没有缩小到不可读。

## 两轮视觉检查

第一轮检查发现并修正：窄轨品牌文字裁切、概览 `dt/dd` 未采用新字阶、侧栏说明挤压、旧车辆保存响应可能关闭新编辑面板。

第二轮使用重新编译后的内嵌资源复查：窄轨仅显示 CPA 标记；数值与次级文字有明确层级；详情可独立滚动；手机内容不被固定高度裁切；晚到的保存结果不影响当前草稿。补充跳到正文入口与手机菜单打开后的焦点移动，关闭后返回触发按钮。

视觉证据（均为合成数据，无生产账号）：

- `implemented-admin-desktop.png`：管理概览。
- `implemented-cars-desktop.png`：车辆列表与并排详情。
- `implemented-passenger-desktop.png`：个人额度与车辆工作区。
- `implemented-passenger-mobile.png`：用户端手机布局。
- `implemented-cars-mobile.png`：管理详情手机布局。
- `implemented-login-desktop.png`：登录页面。

完整逐页截图与机器结果保存在本次环境的 `/tmp/carpool-redesign-evidence` 和 `/tmp/carpool-redesign-functional`。

## 可复现的异步契约

- `renderRoute()` 在 `await` 前替换 `#content`，每轮渲染拥有自己的节点。旧渲染只能写脱离文档的节点。
- 错误处理先比对内容节点、路由和会话，旧 `401` 不得清空当前会话或新页面。
- `openEntityPanel()` 在读取详情前建立所属面板；用户更换选择后旧面板立即关闭，晚到读取不得附着新选择。
- `showCarManagement()` 的成功与失败回调只有在 `panelIsCurrent()` 时才能关闭面板、更新控件或刷新路由。独立调额回调还必须确认其编辑弹层仍有效。
- 错误示例：车辆 A 的 PATCH 未返回时选择 B 并编辑，A 返回后直接 `renderRoute()` 会丢弃 B 草稿。正确做法：检查原面板仍在文档且仍打开，否则不更新当前界面；服务端的成功结果不回滚。
- 回归测试用请求拦截和显式 Promise 同步构造乱序响应，不使用墙钟等待制造竞态。

## 检查记录

- JavaScript 语法检查通过。
- Node 单元测试 52 项通过（重设计、配额进度、保留与保留检查器）。
- `go test ./...`、`go vet ./...`、`go test -race ./internal/carpool/...` 通过。
- `go build -o test-output ./cmd/server && rm test-output` 通过；`CGO_ENABLED=0` 构建也通过。
- 全页面浏览器回归通过：全部导航、非模态详情、跳到正文、移动菜单焦点、重试、旧路由响应隔离与旧保存响应保护。
- 原业务浏览器回归通过：首次改密、重置密码、Key 创建、跨车隔离、普通/流式各自计费、缺价拦截、零额度、调低拦截/调高恢复、原生窗口、明细清理与本期重置。

## 发布边界

仍使用 Go 内嵌静态文件，无 Node 生产构建，无新依赖，无 CSP 放宽。运行中的旧二进制不会自动读取源码变更；需要按原部署方式重新构建并重启后生效。本次只运行隔离测试服务，没有连接或重启生产服务，也没有修改生产数据。
