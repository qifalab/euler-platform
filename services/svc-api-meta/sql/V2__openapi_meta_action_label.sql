-- =============================================================================
-- openapi_meta V2 — api_action 补对外版本标签与登记人
--
-- 领域的 Action.Version 是对外 API 版本标签(日期制,"2026-08-01"——OpenAPI 调用方
-- 按它构造请求),而 api_action.version INT 是内部契约修订号(uk 的一部分,"发布后
-- 不可变"指它)。两者是不同的东西,DDL 却没有给标签留位置 —— 写不进去,SDK 生成
-- 与文档页就没有版本可显示。updated_by 同理:03§9.4 要求登记可追溯,登记人是谁
-- 没有列就无从追溯。
-- =============================================================================

ALTER TABLE `api_action`
  ADD COLUMN `api_version_label` VARCHAR(16) DEFAULT NULL
    COMMENT '对外 API 版本标签(日期制,如 2026-08-01);与 version(内部契约修订号)分离'
    AFTER `action_name`,
  ADD COLUMN `updated_by` VARCHAR(64) DEFAULT NULL
    COMMENT '最后登记人(account_id 或 system-seed);登记可追溯(03§9.4)'
    AFTER `api_version_label`;
