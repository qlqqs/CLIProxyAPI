# 请求明细与数据保留收尾验收（2026-09-14）

## 本轮完成范围

本轮复用现有任务，聚焦查询、破坏性操作确认、设置持久化的正确性；用户暂停前端重设计，
不修改 CSS/布局/设计系统，也不触碰已有未提交 `design-previews/2026-09-14-carpool-full-redesign/`。
为使业务流程真实可用，仅修补既有页面的事件加载、筛选恢复、确认引用和结果反馈。

### 已补齐的真实缺口

1. 游标仅有末项时间/ID，翻页会重算 today。现固定首屏 `[from,to)`、绑定筛选、拒绝
   错误/跨筛选游标并返回真实 period；固定 25 条，补齐缓存读写安全 DTO。
2. 列表不包含事件，但旧展开没有请求详情接口。现在真正加载详情，提供加载/错误/重试，
   展示真实各 Token 维度、费用和安全引用；筛选可刷新/回退恢复。
3. `confirm:true` 未绑定预览且可重复重置。现在复用现有作业表，绑定管理员/操作/目标，
   15 分钟预览、原子结果/审计，同一 ID 并发或重复提交不会再次擦除新费用。
4. 每批 1000 对象，重置已归零账期不再抢占后续批次。三类操作均验证 1000→1→0 推进。
5. 自定义配置回退的保存响应原先硬编码 90；修复后与 GET/真实重启一致。
6. 自动清理设置读取失败原先回退默认，存在错误删除风险。现每批重读策略，失败跳过；
   永久模式也保留 1960 年的合法时间记录，审计仍按独立策略清理。
7. 自动清理可能删除唯一未知事件，但持久请求仍未知。现在小计是否完整也读取持久
   `billing_status`，不因剩余未知事件数为零而错误宣称费用完整。

## 证据与质量门禁

- 查询：`research/query-acceptance.md`，包含 256 组合、跨午夜55条25/25/5、固定上界、
  所有筛选游标冲突、settings 错误停止查询、DTO 0/null。
- 设置：`research/settings-acceptance.md`，真实 SQLite Close/Open、配置47/400、
  页面覆盖矩阵、审计回滚、每批策略变化、读取失败不删除。
- 确认：`research/retention-confirmation.md`，并发/重启重放、费用未知与新消费、15分钟
  精确边界、跨期/跨管理员、三操作每阶段回滚、1201事件完整删除、1000+1。
- 独立审查：`research/final-review.md`，包含 checker-only 安全金丝雀及 SQLite→UI
  持久未知状态验证。无本轮未修复的已证实阻塞项。
- 最终 `gofmt -w .`、`go test ./...`、`go vet ./...`、正常及 `CGO_ENABLED=0` 服务构建
  全部通过，构建临时文件删除。日志 `/tmp/carpool-retention-final-{test,vet}.log`。
- 最终完整 `go test -race ./internal/carpool/...` 通过，SQLite 109.391s；不是仅复用局部
  race。日志 `/tmp/carpool-retention-final-race.log`，退出码0。
- Node：`node --test test/carpool-progress.test.mjs test/carpool-retention.test.mjs test/carpool-retention-checker.test.mjs`，42项通过。
- 新增及原有两套真实 Chromium 测试在最终重新构建 fixture 上均通过；不是 mock 页面。

## 浏览器核心断言

`test/carpool-retention-browser.mjs`：

- 真实假上游消费30次、确认 $1.35；页面25+5条，展开读取12,000 Token/$0.045事件。
- 模型+结果+ID组合过滤，刷新恢复；错误筛选游标422、响应period非null。
- 无预览422、乘客403；预览后新增消费，旧确认409且金额保持$1.395。
- 页面有效确认后净额0；再消费$0.045，重复原ID返回同作业且仍保留$0.045。
- 360px下清理取消不改变32条记录；确认实际删除32条，账期金额仍$0.045。

`test/carpool-browser.mjs` 同时回归首次/管理员重置改密、零限额上车、普通及流式计费、
14路由中的实际OpenAI缺价流程、调额、390/360布局及本期重置、200%重排。
14路由完整后端缺价矩阵仍由Go测试覆盖，不把单个浏览器协议冒充14协议。

代表截图/结果已保留于 `research/browser-evidence/`；没有保存 Key 明文或临时密码。

## 复现命令

```bash
go test -tags carpool_browser -c -o /tmp/carpool-retention.test ./internal/carpool
CARPOOL_BROWSER=1 CARPOOL_BROWSER_READY_FILE=/tmp/carpool-ready.json \
  /tmp/carpool-retention.test -test.run '^TestCarpoolBrowserFixture$' -test.timeout=0
# 在另一终端；ready 文件必须是本次新启动fixture生成，不复用已经操作过的数据。
PLAYWRIGHT_MODULE=playwright CHROMIUM_PATH=/path/to/chromium \
  CARPOOL_BROWSER_READY_FILE=/tmp/carpool-ready.json \
  node test/carpool-retention-browser.mjs
```

Playwright/Chromium由开发环境提供，不新增生产依赖。只准对合成fixture运行，测试会清理明细。
原有全流程脚本需要另一份全新 fixture，不能在上述32条记录被删除后复用同库执行。

## 复盘与防复发

- 根因类别 B/D/E：跨层契约缺口、集成测试断言不足、动态时间/默认值的隐式假设。
- 旧浏览器证明“展开行可打开”没有证明事件真实加载；新增必须等待事件 DOM 并断言费用。
- 不能以成功 toast/202 证明操作正确：现在检查实际记录减少、金额变化与重复请求后的金额。
- 不能用同连接查询冒充重启，也不能仅检查第一页来声称游标稳定。
- 规范已新增 `carpool-retention-guidelines.md`，既有同步记账/价格冻结边界保持不变。

## 未扩大的边界与任务状态

本轮后端/现有流程收尾通过，但不将原PRD一键全勾选或宣称整个父任务完成：

- 完整布局重设计、主行总Token/安全Key名称、完整时间选择UI按用户暂停前端要求留待后续。
- 自动清理沿用逐事件批处理；只保证独立账期金额/未知累计不变，本轮未改成整请求算法。
- 游标固定范围不是数据库快照；范围内迟到回填/状态变化/并行清理可能改变实时total。
- 手工事务限制1000个逻辑对象，不限制单请求事件总数，不增加细粒度恢复/续跑体系。
- 配置文件仍要求合法正天数，数据库覆盖0支持永久；不增加新配置语义。
- 不做真实上游、生产升级/备份恢复演练，不推送/部署。当前任务保持in_progress，
  本轮已获提交确认，后续暂缓验收项仍需明确处置，不自动归档其他父/子任务。

## 本轮提交批次（用户已确认）

1. 明细/保留后端及回归测试：`internal/carpool` 中 domain、httpapi、service、sqlite、
   retention worker 的本轮改动。
2. 现有页面功能修复、浏览器验收与文档：`web/assets/app.js`、`test/carpool-retention*`、
   `.trellis/spec/backend`、本任务研究/状态、`CARPOOL_RELEASE_NOTES.md`。

`design-previews/2026-09-14-carpool-full-redesign/` 为先前未提交的独立设计工作，明确排除。
不自动归档仍含暂缓验收项的任务，不提交其他人的未知改动。

后端工作提交：`dc89f4fc`。页面/文档工作提交及会话记录见 Git 历史。未推送、未部署、未归档。
