-- =============================================================================
-- svc-ticket DDL — support_db (工单)
--
-- Sharding: account_id single key, aligned with the unified account_id shard
-- model (04§6.3). Every table carries account_id, including ticket_message —
-- a redundant shard key is cheaper than a cross-shard join, which the sharding
-- rules forbid outright (03 附录A: 禁止跨库 JOIN).
--
-- 职责 (03§4.4.4): 工单创建/流转/评价、分类与优先级、SLA 计时(计划分级后置)、
-- 客服工作台接口. 工单通道属"不可后置"清单, MVP 以最简形态上线.
--
-- Source of truth: 03-backend-services.md §4.4.4.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- ticket — 工单
--
-- uk_acc_token gives idempotency: a client_token retry cannot create a second
-- ticket for the same request — the "不可后置" channel must not double-open a
-- ticket under network retry.
--
-- version drives the optimistic lock: every state transition (流转/回复/评价)
-- runs UPDATE ... WHERE status = ? AND version = ?, so a duplicate dispatch
-- affects zero rows and is idempotent by construction.
--
-- sla_deadline is the schema-visible SLA timer: the sweeper scans idx_sla
-- for tickets past deadline still in OPEN/ASSIGNED, so "不可后置" is enforced
-- by a single index lookup, not scattered application logic.
-- -----------------------------------------------------------------------------
CREATE TABLE `ticket` (
  `ticket_id`    BIGINT UNSIGNED NOT NULL,
  `account_id`   BIGINT UNSIGNED NOT NULL COMMENT '分片键(≡uid≡user_id≡tenant_id)',
  `category`     VARCHAR(32) NOT NULL COMMENT '分类: 故障/咨询/配额提升/退款/其他',
  `priority`     TINYINT NOT NULL DEFAULT 2 COMMENT '1紧急 2高 3中 4低',
  `status`       TINYINT NOT NULL DEFAULT 1 COMMENT '1待受理 2处理中 3待用户确认 4已关闭 5已评价',
  `sla_deadline` DATETIME DEFAULT NULL COMMENT 'SLA 截止时间;超时未关闭即违约(计划分级后置)',
  `assignee`     VARCHAR(64) DEFAULT NULL COMMENT '受理客服ID/名称;未受理为空',
  `client_token` VARCHAR(64) NOT NULL COMMENT '幂等键(UUID)',
  `version`      INT NOT NULL DEFAULT 0 COMMENT '乐观锁;状态迁移带 WHERE status=? AND version=?',
  `created_at`   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`ticket_id`),
  UNIQUE KEY `uk_client_token` (`account_id`,`client_token`) COMMENT '幂等;重试不重复开工单(不可后置通道)',
  KEY `idx_acc_created` (`account_id`,`status`,`created_at`),
  KEY `idx_sla` (`status`,`sla_deadline`) COMMENT 'SLA 超时扫描'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='工单;SLA 计时为 schema 可见的扫描索引(03§4.4.4)';

-- -----------------------------------------------------------------------------
-- ticket_message — 工单会话(回复/流转记录)
--
-- Append-only message stream under a ticket. created_at is the ordering key;
-- account_id is carried redundantly so "my account's whole thread" is a
-- single-shard read. No cross-database join: customer/agent identity is
-- resolved via svc-iam.
-- -----------------------------------------------------------------------------
CREATE TABLE `ticket_message` (
  `message_id` BIGINT UNSIGNED NOT NULL,
  `ticket_id`  BIGINT UNSIGNED NOT NULL,
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键;与 ticket 同片,避免跨片 JOIN',
  `sender`     VARCHAR(64) NOT NULL COMMENT '发送方: 用户ID/客服ID/system',
  `content`    TEXT NOT NULL COMMENT '消息内容;附件外置,此处只存正文',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`message_id`),
  KEY `idx_ticket_time` (`ticket_id`,`created_at`) COMMENT '工单会话按时间排序',
  KEY `idx_account` (`account_id`,`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='工单会话消息;只增不改(03§4.4.4)';
