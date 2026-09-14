# 本轮交付质量记录（2026-09-14）

## 结论

本轮新增四项中的代码与真实浏览器验收已完成，用户已确认两批本地提交及当前任务归档。
不将旧 PRD 的所有未勾选要求自动标为完成；访问配额/数据保留子任务从错误的 planning
校正为 in_progress，并指向本次集成证据。

## 自动化门禁

- `gofmt -w .`、`git diff --check`：通过。
- `go test ./...`、`go vet ./...`：最后一次在账期重置修复后通过。
- 正常 `go build -o test-output ./cmd/server` 和 `CGO_ENABLED=0` 构建通过，临时产物删除。
- `node --check internal/carpool/web/assets/app.js`、浏览器脚本语法检查通过。
- `node --test test/carpool-progress.test.mjs`：18 项通过。
- 价格/授权/上下文及拼车记账相关包 race 通过；实际 14 路由缺价矩阵、目录 A→B
  与延迟回退、90/180/365 保留设置下当前账期重置补充 race 通过。
- 独立 trellis-check 复核实际执行、流式、模型池、401/bootstrap、scope、旧 Key、Home
  禁止乘客范围；补齐全协议 HTTP 错误码与 observer 取消后继承回归。

本地最后全量日志：`/tmp/carpool-release-test.log`、`/tmp/carpool-release-vet.log`；
状态 `/tmp/carpool-release-status` 为 `ALL_PASS`。日志不是提交产物。

## 浏览器门禁

`test/carpool-browser.mjs` 在最终重新构建的隔离 fixture 上全部通过。
完整流程和代表截图见 `browser-acceptance.md`、`browser-evidence/`。
最终一轮额外等待 toast 消失后截图，保留 900%/$0.08 超额展示证据。

## 非零风险记录

首轮扩大 race 在未归属拼车的
`TestResponsesWebsocketReplaysImmediatelyAfterPinnedAuthFailure` 两个子例收到关闭码
1006 EOF 而非 1012。未出现 DATA RACE；定向 `-race -count=3` 和整个 OpenAI handler
包重跑通过。没有充分证据证明根因，不计为已修复，也未将其隐藏为全程一次通过。

不承诺崩溃期间精确恢复或迟到后台生产者自动 join；保持用户批准的简化取舍。

## 提交范围

本轮未发现其他人员未知工作文件。拟分两批：
1. 冻结计价、执行前校验、协议错误码及账期重置修复和对应测试。
2. 浏览器 fixture/脚本、配额显示修复、规范、发布说明、任务状态和验收截图。

用户已按 Phase 3.4 确认提交归档；不推送、不部署。
