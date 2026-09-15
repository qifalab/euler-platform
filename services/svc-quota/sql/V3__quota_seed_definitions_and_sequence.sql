-- =============================================================================
-- support_db V3 — 号段表 + 配额定义 seed
--
-- 1. id_sequence — 号段分配器状态 (04§6.6)
--
-- Same table shape as trade_db's (svc-payment V2). Each logical schema carries
-- its own allocator state, exactly as each carries its own schema_migration:
-- support_db's writers must not reach into trade_db to mint an id, and the two
-- schemas are separate failure domains (04§6.3).
--
-- The row seeded here is quota_token: reservation ids must be unique across
-- restarts. A repeated token_id would make PutToken overwrite a LIVE reservation
-- instead of creating one — the newer request silently steals the older one's
-- capacity, which is leaked occupancy with no error anywhere.
--
-- 2. quota_definition seed
--
-- quota_definition is a global broadcast table (V1:17): the rules are platform
-- configuration, not per-tenant data, so they are seeded by migration rather than
-- upserted by the service at startup — a service that writes config on boot
-- cannot tell "this environment is configured" from "this process just started",
-- and two replicas would race to own the same row.
--
-- The two rows mirror what the service's in-memory store used to hardcode:
--   quota_scecs_instance — 20 instances per region (REGION scope)
--   quota_scoss_bucket   — 100 buckets across regions (GLOBAL scope)
-- =============================================================================

CREATE TABLE IF NOT EXISTS `id_sequence` (
  `name`         VARCHAR(64) NOT NULL COMMENT '序列名;约定为消费它的表,如 quota_token',
  `next_value`   BIGINT UNSIGNED NOT NULL COMMENT '下一个可发放的号段起点',
  `segment_size` BIGINT UNSIGNED NOT NULL DEFAULT 1000 COMMENT '号段长度;未用完的部分作废',
  `updated_at`   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='号段分配器状态(04§6.6);业务主键不依赖 AUTO_INCREMENT';

INSERT IGNORE INTO `id_sequence` (`name`, `next_value`, `segment_size`) VALUES
  ('quota_token', 1, 1000);

INSERT IGNORE INTO `quota_definition`
  (`quota_code`, `product_code`, `default_value`, `scope`, `adjustable`, `unit`)
VALUES
  ('quota_scecs_instance', 'scecs', 20,  'REGION', 1, '个'),
  ('quota_scoss_bucket',   'scoss', 100, 'GLOBAL', 0, '个');
