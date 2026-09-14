# 模型价格快照

该目录内置 LiteLLM 模型价格快照，作为拼车账务的离线回退数据。价格字段采用美元/Token，运行时会保存目录哈希和事件使用的单价，后续更新不会改写历史金额。

快照参考同级项目 `sub2api/backend/resources/model-pricing/model_prices_and_context_window.json`，上游来源为 LiteLLM 项目。更新快照时应同步记录来源和更新时间，并运行价格解析测试。
