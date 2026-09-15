-- =============================================================================
-- account_db V2 — org 落库缺口补齐 (svc-org 接入 SQL store)
--
-- 1. 代理主键补 AUTO_INCREMENT
--
-- org_project_resource.id / org_tag.tag_id / org_tag_resource.id 都是纯代理键:
-- 业务身份是 uk_resource(resource_id)、uk_acc_key(account_id,tag_key)、
-- uk_res_key(resource_id,tag_key),而领域结构里根本没有这三个 id 字段 ——
-- 没有 AUTO_INCREMENT 就没有任何写入方填得出这一列(与 quota_usage.id 同一类问题)。
--
-- 2. account_db 的号段表
--
-- org_project.project_id 是号段发号的业务主键(04§6.6),本 schema 需要自己的
-- id_sequence(trade_db/support_db 各有一份,见 svc-payment V2 / svc-quota V3:
-- 每个逻辑库各自持有分配器状态,不跨库取号)。本目录是 account_db 里第一个需要它的
-- 目录,所以由这里创建;后来者只 INSERT 自己的序列行,不再重复建表。
--
-- 注意 svc-iam 的 V1 里也有一张 idempotent_record,与本文件无关;这里只动 org 三表。
-- =============================================================================

CREATE TABLE IF NOT EXISTS `id_sequence` (
  `name`         VARCHAR(64) NOT NULL COMMENT '序列名;约定为消费它的表,如 org_project',
  `next_value`   BIGINT UNSIGNED NOT NULL COMMENT '下一个可发放的号段起点',
  `segment_size` BIGINT UNSIGNED NOT NULL DEFAULT 1000 COMMENT '号段长度;未用完的部分作废',
  `updated_at`   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='号段分配器状态(04§6.6);业务主键不依赖 AUTO_INCREMENT';

INSERT IGNORE INTO `id_sequence` (`name`, `next_value`, `segment_size`) VALUES
  ('org_project', 1, 1000);

ALTER TABLE `org_project_resource`
  MODIFY COLUMN `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;

ALTER TABLE `org_tag`
  MODIFY COLUMN `tag_id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;

ALTER TABLE `org_tag_resource`
  MODIFY COLUMN `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;
