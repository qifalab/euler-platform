-- =============================================================================
-- svc-iam DDL — account_db, V3: web-auth persistence (accounts, sessions, RAM,
-- MFA, real-name, password reset)
--
-- web-auth used to keep every account, refresh session, RAM user/role/policy,
-- MFA seed and real-name result in process memory: a restart lost every user,
-- console-created AccessKeys never reached the verify container, and RAM-user
-- keys were denied by /internal/authz/check because no RAM data was in the
-- database. With EULER_DB_DSN set, web-auth now runs on account_db; this file
-- adds what the V1 schema lacked.
--
--   account          real-name result (name / masked document / HMAC identity
--                    fingerprint / enterprise name) and token_version (bumped on
--                    password change or reset: outstanding access tokens die);
--                    account_name / email widened to the RFC 5321 254 chars the
--                    register endpoint accepts.
--   iam_user         remark (console field).
--   iam_role         status (1 enabled / 2 disabled — AssumeRole and CheckAccess
--                    honour it).
--   ram_policy       document_text: the document exactly as submitted (the JSON
--                    column normalises key order and whitespace; the API returns
--                    what the tenant wrote).
--   mfa_device       key_version (KMS master-key version the seed is sealed
--                    under; rotation-aware like access_key.sk_key_version).
--   ram_role_policy  role–policy attachment (V1 only had user and group).
--   auth_refresh_session  refresh tokens (SHA-256 only) with the rotation chain.
--   password_reset_code   pending forgot-password codes (keyed HMAC only).
--   id_sequence      account_db's 号段 allocator (also created by svc-org V2 with
--                    the identical definition; svc-iam's directory is applied
--                    first, so it must not depend on it) + svc-iam's sequences.
--   ram_policy rows  the platform system policies (account_id 0, scope 1).
--
-- 可重入: MySQL has no ADD COLUMN / ADD INDEX IF NOT EXISTS, so those go through
-- information_schema + PREPARE; MODIFY / CREATE TABLE IF NOT EXISTS / INSERT
-- IGNORE are naturally re-runnable.
-- =============================================================================

ALTER TABLE `account`
  MODIFY COLUMN `account_name` VARCHAR(254) NOT NULL COMMENT '登录名(邮箱,小写规范化;RFC 5321 上限 254)',
  MODIFY COLUMN `email` VARCHAR(254) DEFAULT NULL COMMENT '邮箱';

SET @exists := (SELECT COUNT(*) FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'account' AND COLUMN_NAME = 'real_name');
SET @ddl := IF(@exists = 0, 'ALTER TABLE `account` ADD COLUMN `real_name` VARCHAR(64) DEFAULT NULL COMMENT ''实名姓名(个人实名后写入)'' AFTER `real_name_status`', 'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @exists := (SELECT COUNT(*) FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'account' AND COLUMN_NAME = 'enterprise_name');
SET @ddl := IF(@exists = 0, 'ALTER TABLE `account` ADD COLUMN `enterprise_name` VARCHAR(128) DEFAULT NULL COMMENT ''企业名称(企业实名后写入)'' AFTER `real_name`', 'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @exists := (SELECT COUNT(*) FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'account' AND COLUMN_NAME = 'id_no_masked');
SET @ddl := IF(@exists = 0, 'ALTER TABLE `account` ADD COLUMN `id_no_masked` VARCHAR(32) DEFAULT NULL COMMENT ''证件号/统一社会信用代码脱敏值;明文从不落库'' AFTER `enterprise_name`', 'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @exists := (SELECT COUNT(*) FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'account' AND COLUMN_NAME = 'identity_fingerprint');
SET @ddl := IF(@exists = 0, 'ALTER TABLE `account` ADD COLUMN `identity_fingerprint` CHAR(64) DEFAULT NULL COMMENT ''HMAC-SHA256(EULER_IDENTITY_PEPPER, kind:证件号) hex;一人多号识别(试用防刷)'' AFTER `id_no_masked`', 'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @exists := (SELECT COUNT(*) FROM information_schema.STATISTICS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'account' AND INDEX_NAME = 'idx_identity_fingerprint');
SET @ddl := IF(@exists = 0, 'ALTER TABLE `account` ADD KEY `idx_identity_fingerprint` (`identity_fingerprint`)', 'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @exists := (SELECT COUNT(*) FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'account' AND COLUMN_NAME = 'token_version');
SET @ddl := IF(@exists = 0, 'ALTER TABLE `account` ADD COLUMN `token_version` INT NOT NULL DEFAULT 0 COMMENT ''凭证版本;改密/重置密码时 +1,旧 access_token 立即失效'' AFTER `status`', 'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @exists := (SELECT COUNT(*) FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'iam_user' AND COLUMN_NAME = 'remark');
SET @ddl := IF(@exists = 0, 'ALTER TABLE `iam_user` ADD COLUMN `remark` VARCHAR(256) DEFAULT NULL COMMENT ''备注'' AFTER `display_name`', 'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @exists := (SELECT COUNT(*) FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'iam_role' AND COLUMN_NAME = 'status');
SET @ddl := IF(@exists = 0, 'ALTER TABLE `iam_role` ADD COLUMN `status` TINYINT NOT NULL DEFAULT 1 COMMENT ''1启用 2禁用;禁用后不可 AssumeRole,CheckAccess 拒绝'' AFTER `description`', 'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @exists := (SELECT COUNT(*) FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ram_policy' AND COLUMN_NAME = 'document_text');
SET @ddl := IF(@exists = 0, 'ALTER TABLE `ram_policy` ADD COLUMN `document_text` MEDIUMTEXT DEFAULT NULL COMMENT ''提交时的原文(JSON 列会规范化键序与空白);API 原样返回'' AFTER `document`', 'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @exists := (SELECT COUNT(*) FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'mfa_device' AND COLUMN_NAME = 'key_version');
SET @ddl := IF(@exists = 0, 'ALTER TABLE `mfa_device` ADD COLUMN `key_version` INT NOT NULL DEFAULT 1 COMMENT ''secret_cipher 所用 KMS 主密钥版本(mk-user)'' AFTER `secret_cipher`', 'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

-- Pure surrogate keys: nothing in the domain carries these ids, so the database
-- assigns them (same reasoning as svc-org V2).
ALTER TABLE `ram_user_policy` MODIFY COLUMN `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;
ALTER TABLE `mfa_device` MODIFY COLUMN `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;

CREATE TABLE IF NOT EXISTS `ram_role_policy` (
  `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键',
  `role_id`    BIGINT UNSIGNED NOT NULL COMMENT 'iam_role.id',
  `policy_id`  BIGINT UNSIGNED NOT NULL COMMENT 'ram_policy.policy_id',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_role_policy` (`role_id`,`policy_id`),
  KEY `idx_account` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='角色-策略绑定';

-- -----------------------------------------------------------------------------
-- auth_refresh_session — web-auth refresh tokens (02§5.1)
--
-- Only SHA-256(token) is stored. Every refresh rotates: the presented token is
-- revoked (reason rotated, replaced_by = successor) and its successor inserted
-- in the same transaction. family_id ties every token of one login together: a
-- rotated-away token presented again after the short concurrent-refresh grace
-- is a replay, and revokes the whole family.
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `auth_refresh_session` (
  `token_hash`    CHAR(64)        NOT NULL COMMENT 'SHA-256 hex of the opaque refresh token',
  `account_id`    BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `token_version` INT             NOT NULL DEFAULT 0 COMMENT '签发时账号的 token_version;改密/重置后旧会话随之作废',
  `family_id`     CHAR(32)        NOT NULL COMMENT '轮转链 id(一次登录产生的全部 token 共享)',
  `parent_hash`   CHAR(64)        DEFAULT NULL COMMENT '本 token 轮转自哪个 token',
  `replaced_by`   CHAR(64)        DEFAULT NULL COMMENT '轮转后的继任 token',
  `expires_at`    DATETIME(3)     NOT NULL COMMENT 'UTC',
  `revoked_at`    DATETIME(3)     DEFAULT NULL COMMENT 'UTC;NULL = 有效',
  `revoke_reason` VARCHAR(16)     DEFAULT NULL COMMENT 'rotated/logout/password/reset/reuse/account',
  `created_at`    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`token_hash`),
  KEY `idx_account_revoked` (`account_id`,`revoked_at`),
  KEY `idx_family` (`family_id`),
  KEY `idx_expires_at` (`expires_at`) COMMENT '过期清理'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='web-auth 刷新会话(仅存哈希)+轮转链';

-- -----------------------------------------------------------------------------
-- password_reset_code — 找回密码验证码
--
-- One pending code per account (a new send replaces it). Only
-- HMAC-SHA256(server key, account_id:code) is stored — a bare SHA-256 of six
-- digits falls to enumeration in milliseconds. TTL 10 minutes, 5 guesses.
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `password_reset_code` (
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '分片键;每账号最多一枚待用验证码',
  `code_hash`  CHAR(64)        NOT NULL COMMENT 'keyed HMAC hex;验证码明文从不落库',
  `attempts`   INT             NOT NULL DEFAULT 0 COMMENT '错误尝试次数;达到 5 次作废',
  `expires_at` DATETIME(3)     NOT NULL COMMENT 'UTC;签发 +10 分钟',
  `created_at` DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`account_id`),
  KEY `idx_expires_at` (`expires_at`) COMMENT '过期清理'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='找回密码验证码(仅存 keyed hash)';

CREATE TABLE IF NOT EXISTS `id_sequence` (
  `name`         VARCHAR(64) NOT NULL COMMENT '序列名;约定为消费它的表,如 org_project',
  `next_value`   BIGINT UNSIGNED NOT NULL COMMENT '下一个可发放的号段起点',
  `segment_size` BIGINT UNSIGNED NOT NULL DEFAULT 1000 COMMENT '号段长度;未用完的部分作废',
  `updated_at`   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='号段分配器状态(04§6.6);业务主键不依赖 AUTO_INCREMENT';

-- New accounts start at 1000000, clear of the fixed dev/demo ids (100123, ...).
-- ram_policy ids below 1000 are reserved for system policies.
INSERT IGNORE INTO `id_sequence` (`name`, `next_value`, `segment_size`) VALUES
  ('account', 1000000, 100),
  ('iam_user', 1, 100),
  ('iam_role', 1, 100),
  ('ram_policy', 1000, 100);

-- Platform system policies (07§3.2 naming), visible to every account. The
-- documents are byte-identical to web-auth's systemPolicyDefs.
INSERT IGNORE INTO `ram_policy` (`policy_id`, `account_id`, `policy_name`, `document`, `document_text`, `scope`, `description`) VALUES
  (1, 0, 'EuEcsFullAccess', '{"Version":"1","Statement":[{"Effect":"Allow","Action":"euecs:*","Resource":"*"}]}', '{"Version":"1","Statement":[{"Effect":"Allow","Action":"euecs:*","Resource":"*"}]}', 1, '云服务器 ECS 全部权限'),
  (2, 0, 'EuReadOnlyAccess', '{"Version":"1","Statement":[{"Effect":"Allow","Action":["euecs:Describe*","euecs:List*"],"Resource":"*"}]}', '{"Version":"1","Statement":[{"Effect":"Allow","Action":["euecs:Describe*","euecs:List*"],"Resource":"*"}]}', 1, '云服务器 ECS 只读(Describe*/List*)');
