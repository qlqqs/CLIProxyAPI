# 真实浏览器集成验收（2026-09-14）

## 环境与范围

实际 Chromium 无头浏览器执行真实嵌入页面、HTTP API、会话/CSRF 与独立临时 SQLite。
假 executor 每次产生 10,000 输入、2,000 输出 Token，内置 `gpt-4o` 计费 $0.045；
普通及流式合计 $0.09。不是静态页面，也未 mock 前端 API 响应。

桌面 1440×900；移动 390×844、360×844；桌面 CSS zoom 200%。后者是重排检查，
不是原生浏览器菜单缩放或移动真机验收。按现有 DESIGN.md 的 B 深色圆润设计，不重设计。

## 已通过流程

1. 初次临时密码登录仅允许改密；业务接口 403，改密后重新登录恢复。
2. 创建乘客、上车必填金额及负数校验，零限额可建立成员关系。
3. 一次性 Key 创建后关闭弹窗；模型列表不暴露另一车辆独有模型。
4. 普通/流式请求真实同步落账；缺价 422、安全 code、fake executor 调用数不增加。
5. 调低至 $0.01 后生成 403 且上游调用数不增加；调高至 $2 后立即恢复。
6. 数字 53%/81% 独立显示；仅状态窗口没有假零，缺观测与陈旧观测有明确说明。
7. 管理员请求行展开事件；保留策略 180→永久→实际清理→180。预览与删除数量均大于零，
   请求明细清空，乘客金额保持不变，避免“实际没有删除任何记录”的空验证。
8. 管理员重置密码使旧会话失效；临时密码登录提示管理员重置，工作台 403；再次改密
   后车辆、Key、金额保留，原 Key 可继续消费。未把网页门禁误写成 API Key 自动撤销。
9. 390/360 两种移动尺寸均覆盖导航、额度弹窗、Esc 逐层关闭、调额保存、实际消费、
   请求展开和本期重置。180 天保留下重置后金额为 0，限额仍为 $3。
10. 整页无水平溢出；宽表与导航在自身容器滚动。无 JavaScript pageerror。

## 发现并修复

- `Number(null)` 将仅状态配额伪造为 0%；现保留真实未知。
- CSP 拒绝行内 style 导致全部进度条满格；改为原生 progress，未放宽安全策略。
- 当前账期重置错误使用 180 天前截止时间，HTTP 成功但金额未变；按操作区分截止时间，
  真实浏览器与固定时钟 service→SQLite 回归共同验证。

## 复现

需单独安装 Playwright 并准备 Chromium。仓库不新增生产 npm 依赖。

```bash
go test -tags carpool_browser -c -o /tmp/carpool-browser.test ./internal/carpool
# readiness 文件必须尚不存在；fixture 仅监听 loopback，结束后删除临时数据库。
CARPOOL_BROWSER=1 CARPOOL_BROWSER_READY_FILE=/tmp/carpool-ready.json \
  /tmp/carpool-browser.test -test.run '^TestCarpoolBrowserFixture$' -test.timeout=0
# 另一终端，PLAYWRIGHT_MODULE 可设为已安装模块的绝对 index.mjs 路径。
PLAYWRIGHT_MODULE=playwright CHROMIUM_PATH=/path/to/chromium \
  CARPOOL_BROWSER_READY_FILE=/tmp/carpool-ready.json \
  CARPOOL_BROWSER_OUTPUT=/tmp/carpool-browser-evidence node test/carpool-browser.mjs
```

只在合成环境执行：脚本会创建用户、重置密码、删除明细和重置用量。
输出 `results.json` 与截图不含秘密；ready 文件内的凭据虽是虚构的，也不纳入仓库。
仓库保留代表截图及结果于 `browser-evidence/`，完整临时截图位于上述输出目录。

## 未覆盖范围

不把本验收等同于真实上游 OAuth、远端价格源可用性、Firefox/Safari、屏幕阅读器、
物理移动设备或全部历史 PRD 的组合测试。历史验收清单未一键全部勾选。
