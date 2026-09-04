---
name: trellis-brainstorm-review
description: "在复杂 Trellis 需求规划中，将 trellis-brainstorm 与独立子代理多轮反方审查组合成可追溯的收敛流程。用于用户明确要求 brainstorm 加子代理审查、让另一名代理参与方案讨论，或项目工作流明确要求独立规划审查；不用于普通小改动、实施阶段代码审查或一次性静态评审。"
---

# Trellis 规划与独立审查

把复杂需求收敛为可执行、可验收、经过独立压力测试的 Trellis 规划。整个流程属于 Phase 1：不得修改业务代码，不得运行 `task.py start`，最终仍由用户批准是否进入实现。

## 使用前提

1. 完整读取相邻的 `../trellis-brainstorm/SKILL.md`，以它的规划契约、证据规则、单问题规则和最终批准门槛为准。
2. 完整读取 `../trellis-channel/SKILL.md`，并按当前任务读取其 `references/workflows.md` 与 `references/workers.md`。命令参数与 worker 生命周期以这些文件和本机 `trellis channel --help` 为准，不在本 Skill 中复制一套易过期的协议。
3. 只有用户明确要求或授权子代理审查，或者更高优先级的项目工作流明确要求时，才启动 reviewer。显式调用 `$trellis-brainstorm-review` 视为对本次只读规划审查的授权，但不授权实施、外部发布或其他状态变更。
4. 若没有活动任务，遵循 `trellis-brainstorm` 的任务创建许可规则；若已有 `planning` 任务，继续使用它，不重复建任务。

## 角色边界

- 主代理负责与用户沟通、检索仓库证据、维护全部规划文档、核验审查意见并决定是否采纳。
- reviewer 只读审查，不修改文件、不实现代码、不启动任务、不提交 Git。它要挑战方案，而不是迎合主代理。
- 用户拥有产品、范围、体验、兼容性和风险取舍。除非用户明确把某类剩余决策委托给代理组，否则 reviewer 的建议不能替代用户决定。
- 仓库事实由代码、测试、配置和现有文档回答；不能把可检索问题转交给用户或让 reviewer 凭印象回答。

## 执行流程

1. 先按 `trellis-brainstorm` 建立或更新 `prd.md`，整理已确认决策、待决事项、范围边界和验收标准。
2. 读取当前任务的 `prd.md`、`design.md`、`implement.md`、`research/` 与上下文清单。只在关键信息未落盘时使用 `trellis-session-insight`，不要重复抽取已有材料。
3. 在任务的 `research/` 下创建审查 brief，明确用户已冻结且不得推翻的决策、需要挑战的问题、只读边界和期望证据。
4. 使用一个可追溯的项目级 Trellis channel 和一个稳定 reviewer 身份开展多轮审查。优先复用同一 reviewer 会话，使后续轮次能挑战前轮结论；不要为每个问题创建互不知情的临时代理。
5. 按 [审查与收敛协议](references/review-protocol.md) 覆盖方向、反方论证、数据契约、体验与运维、跨层可行性及发布门槛。轮次数由未闭合风险决定，不以机械次数代替覆盖度。
6. 每轮结束后，主代理必须读取原始结果、回到仓库验证关键事实，并为每条实质发现记录“采纳、部分采纳、拒绝或转为技术探针”。不得直接复制 reviewer 结论。
7. 新出现的用户所有决策仍按 `trellis-brainstorm` 一次只问一个。若用户已明确授权代理组收敛剩余决策，则采用最小安全范围，记录理由、代价和被推迟的替代方案。
8. 复杂任务完成 `prd.md`、`design.md`、`implement.md`，并根据当前工作流维护真实、非空的 `implement.jsonl` 与 `check.jsonl`。随后执行 PRD convergence pass。
9. 把最新规划交给 reviewer 做最终反方审查。修复或明确处置所有影响行为、安全、一致性、可实现性和验收的发现；每次实质修改后只复查受影响契约及其相邻边界。
10. 通过本地校验后，向用户给出最新规划摘要并请求单独批准。保持任务状态为 `planning`；只有用户在该摘要之后明确批准，才可在后续消息中进入实现。

## 必须留下的记录

- `prd.md`：目标、用户价值、范围、需求、验收标准和仍阻断规划的问题。
- `design.md`：复杂任务的架构边界、数据流、兼容性、迁移与回滚。
- `implement.md`：有序实施步骤、验证命令、风险点和发布门槛。
- `research/<topic>-review-brief.md`：reviewer 的只读任务、已冻结决策和审查范围。
- `research/<topic>-review.md`：经过主代理核验后的决策、证据、取舍和遗留风险；不要把频道全文当成规划结论。
- Trellis channel 事件日志：保留原始审查轨迹，只能通过 `trellis channel` 操作，不得手改 `events.jsonl`。

## 完成条件

- 所有仓库可回答的问题已有证据，所有用户所有的阻断决策已解决。
- PRD、设计、实施计划和验收标准互相一致，没有临时 brainstorm 段落或重复事实。
- 每条实质审查发现都有明确处置，且不存在未处理的发布阻断问题。
- 技术未知被验证，或被转换为不会改变 MVP 行为的技术探针与 fail-closed 发布门槛。
- reviewer 的最终意见和主代理的本地校验都支持提交用户评审。
- 用户看到最终摘要前，不宣称已获实现批准；用户批准前，不启动任务或修改业务代码。

## 不适用场景

- 单文件、小范围、低风险改动：直接使用 `trellis-brainstorm`，无需建立频道。
- 实施后的代码质量检查：使用 `trellis-check` 或 check agent。
- 只需要一次独立意见：使用一次性 advisor 或静态 review，不建立多轮 brainstorm channel。
- 跨会话找回旧结论：使用 `trellis-session-insight`，不要把 channel 当长期会话搜索工具。
