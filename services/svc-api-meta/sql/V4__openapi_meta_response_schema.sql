-- =============================================================================
-- openapi_meta V4 — api_action 补响应 schema
--
-- Action 元数据此前只登记参数 schema 与错误码;成功信封 Data 里有哪些字段
-- (DescribeInstances 的 Instances[].Status、RunInstances 的 InstanceId/OrderId)
-- 无处登记,文档页、SDK 生成与 OpenAPI Explorer 只能靠猜。新增可空 JSON 列,
-- 与 param_schema_json 同为 JSON Schema 子集;未登记的 Action 保持 NULL
-- (API 输出中省略 response_schema 字段,对既有读者是纯增量)。
-- =============================================================================

ALTER TABLE `api_action`
  ADD COLUMN `response_schema_json` JSON DEFAULT NULL
    COMMENT '响应 Data 的 schema(JSON Schema 子集);驱动文档与 SDK 生成'
    AFTER `param_schema_json`;
