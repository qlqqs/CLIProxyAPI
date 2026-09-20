# Sub2API 5h / 7d 额度对标证据

## 对标范围

用户明确要求直接对标本机 `/home/qlqq/workspace/sub2api`。本任务只提取 Sub2API 的 5h / 7d USD 限额模型，不引入其 1d 限额、Key 总额度、余额、订阅或支付体系。

## 数据合同

- `frontend/src/types/index.ts:747-758`：API Key DTO 独立返回 `rate_limit_5h`、`rate_limit_7d`、`usage_5h`、`usage_7d`、窗口起点与重置时间。
- `backend/ent/schema/api_key.go:80-110`：5h / 7d 限额与用量是独立字段；限额 `0` 表示不限。
- `backend/internal/service/api_key.go:19,56-73,116-121`：窗口时长固定为 5 小时与 7 天；窗口过期后有效用量按 0 处理。

## 窗口与累计

- `backend/internal/repository/api_key_repo.go:808-841`：计费时原子更新各窗口；首次使用或窗口过期时重新建立窗口并以本次费用作为新窗口首笔用量。
- 5h 从实际首次使用时刻起算；7d 以首次使用当天起点建立 7 天窗口。
- `backend/internal/service/billing_cache_service.go:635-679`：准入按有效用量与限额比较；限额大于 0 且已用达到限额时拒绝。
- `backend/internal/service/api_key_service.go:41`：5h 达额使用独立稳定错误；7d 同理。

## 表单与展示

- `frontend/src/views/user/KeysView.vue:748-889`：5h 与 7d 各自使用金额输入、已用/上限、进度条和状态色；`0` 表示不限。
- `frontend/src/views/user/KeysView.vue:1823-1842`：表单关闭或非正数统一提交 0，避免额外的 nullable 三态。
- `frontend/src/i18n/locales/zh/dashboard.ts:286-303`：用户文案明确为“5小时限额 (USD)”与“7天限额 (USD)”。
- `frontend/src/views/KeyUsageView.vue:624-751`：使用窗口标签、已用/上限、剩余额度与重置时间表达额度状态。

## 转译到 CLIProxyAPI

- 对标的是行为与信息层级，不复制 Vue/Tailwind 或 Sub2API 品牌外观。
- Carpool 的限制归属仍按稳定用户身份共享全部 Carpool API Key，不改成每个 Key 独立；这是现有产品契约与用户并发归属的一致边界。
- 仅保留 5h / 7d：不引入 Sub2API 的 1d、总额度、余额与订阅。
- 移除依赖上游账号窗口观测的 `pending_sync` 产品语义，改为本地确定性滚动窗口；账号原生 CPA 配额仍按现有路径单独展示。
- 旧月额度列与历史账期作为兼容数据保留，但不再公开、不再写入、不再准入、不再提供重置操作。
