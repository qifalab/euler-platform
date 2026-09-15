-- =============================================================================
-- svc-iam DDL — account_db
--
-- Sharding: account_id single key (裁决 C5/S12; 04§6.3/§6.4).
--   account_db 4 库 × 16 表 (04§6.3 lock box). Vitess (vtgate), vindex 承载.
--   Physical column / vindex field / Kafka partition key all write account_id
--   (≡ uid ≡ user_id ≡ tenant_id — 00 附录A 租户标识等价声明).
--
-- Conventions enforced (03 附录A DDL 评审模板):
--   * lowercase snake_case names; index prefixes uk_ / idx_
--   * money DECIMAL (never float); time DATETIME + UTC
--   * enums TINYINT + COMMENT dictionary
--   * state-machine tables carry version INT for optimistic locking
--   * created_at / updated_at on every table
--   * every table/column/enum/special semantic has a COMMENT
--   * NO cross-database JOIN; cross-service data via API or event
--
-- Source of truth: 07-security.md §2 (identity model, AK/SK, MFA, resource
-- group) and 03-backend-services.md §6.1. Where the two differ, 07 wins for
-- security semantics (裁决 S5: SK is KMS-envelope reversible, NOT sk_hash).
-- =============================================================================

-- -----------------------------------------------------------------------------
-- account — 主账号 (资源归属与计费主体)
--
-- balance (现金余额) and 余额流水 deliberately DO NOT live here: they belong to
-- trade_db ledger, owned by svc-billing (裁决 S29 / 04§6.3). This table holds
-- identity attributes only.
-- -----------------------------------------------------------------------------
CREATE TABLE `account` (
  `account_id`       BIGINT UNSIGNED NOT NULL COMMENT '账号ID(雪花),全局唯一,≡uid≡user_id≡tenant_id;分片键',
  `account_name`     VARCHAR(64)  NOT NULL COMMENT '登录名(邮箱/手机)',
  `email`            VARCHAR(128) DEFAULT NULL COMMENT '邮箱',
  `mobile_cipher`    VARBINARY(256) DEFAULT NULL COMMENT '手机号 KMS 信封加密密文(mk-user);展示脱敏',
  `password_hash`    VARCHAR(128) NOT NULL COMMENT 'argon2id 散列;5 次失败锁定 15 分钟(07§2.4)',
  `account_type`     TINYINT NOT NULL DEFAULT 1 COMMENT '1个人 2企业',
  `real_name_status` TINYINT NOT NULL DEFAULT 0 COMMENT '0未实名 1个人已实名 2企业已实名;下单/开资源/开API前必须≥1(07§2.3)',
  `mfa_bound`        TINYINT NOT NULL DEFAULT 0 COMMENT '0未绑定 1已绑定 TOTP',
  `status`           TINYINT NOT NULL DEFAULT 1 COMMENT '1正常 2冻结 3注销锁定',
  `version`          INT NOT NULL DEFAULT 0 COMMENT '乐观锁',
  `created_at`       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'UTC',
  `updated_at`       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT 'UTC',
  PRIMARY KEY (`account_id`),
  UNIQUE KEY `uk_account_name` (`account_name`),
  KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='主账号表(仅身份属性);现金余额与余额流水归 trade_db ledger(裁决S29/04§6.3)';

-- -----------------------------------------------------------------------------
-- iam_user — RAM 子用户 (主账号下的子身份)
-- -----------------------------------------------------------------------------
CREATE TABLE `iam_user` (
  `id`             BIGINT UNSIGNED NOT NULL COMMENT '主键(雪花)',
  `account_id`     BIGINT UNSIGNED NOT NULL COMMENT '所属主账号;分片键',
  `username`       VARCHAR(64) NOT NULL COMMENT '账号内唯一的登录名',
  `display_name`   VARCHAR(64) DEFAULT NULL COMMENT '显示名',
  `login_enabled`  TINYINT NOT NULL DEFAULT 0 COMMENT '0禁止控制台登录 1允许',
  `password_hash`  VARCHAR(128) DEFAULT NULL COMMENT 'argon2id;login_enabled=1 时必填',
  `ak_enabled`     TINYINT NOT NULL DEFAULT 0 COMMENT '0不允许持有AK 1允许',
  `mfa_required`   TINYINT NOT NULL DEFAULT 0 COMMENT '1登录强制 MFA',
  `last_login_at`  DATETIME DEFAULT NULL,
  `status`         TINYINT NOT NULL DEFAULT 1 COMMENT '1启用 2禁用 3已删除(审计保留)',
  `version`        INT NOT NULL DEFAULT 0 COMMENT '乐观锁',
  `created_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_account_name` (`account_id`,`username`),
  KEY `idx_account_status` (`account_id`,`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='RAM 子用户;删除为软删(status=3)以保留审计链(07§2.1)';

-- -----------------------------------------------------------------------------
-- user_group / group_member — 用户组 (按职能批量授权)
-- -----------------------------------------------------------------------------
CREATE TABLE `user_group` (
  `id`         BIGINT UNSIGNED NOT NULL,
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `group_name` VARCHAR(64) NOT NULL,
  `comments`   VARCHAR(256) DEFAULT NULL,
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_account_group` (`account_id`,`group_name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户组;权限挂在组上,成员继承';

CREATE TABLE `group_member` (
  `id`             BIGINT UNSIGNED NOT NULL,
  `account_id`     BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键(避免跨库JOIN)',
  `group_id`       BIGINT UNSIGNED NOT NULL,
  `member_user_id` BIGINT UNSIGNED NOT NULL COMMENT 'iam_user.id',
  `created_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_group_user` (`group_id`,`member_user_id`),
  KEY `idx_account_member` (`account_id`,`member_user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户组成员关系';

-- -----------------------------------------------------------------------------
-- iam_role — 角色 (无固定凭证的虚拟身份,可被 AssumeRole)
-- -----------------------------------------------------------------------------
CREATE TABLE `iam_role` (
  `id`                  BIGINT UNSIGNED NOT NULL,
  `account_id`          BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `role_name`           VARCHAR(64) NOT NULL,
  `role_type`           TINYINT NOT NULL COMMENT '1用户角色 2服务角色(ServiceLinkedRole) 3跨账号角色',
  `assume_policy`       JSON NOT NULL COMMENT '信任策略:定义谁可以 AssumeRole 本角色',
  `max_session_seconds` INT NOT NULL DEFAULT 3600 COMMENT 'STS 临时凭证最大有效期(900~3600s)',
  `description`         VARCHAR(256) DEFAULT NULL,
  `created_at`          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_account_role` (`account_id`,`role_name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='角色;STS 临时凭证不落库(07§2.2 规则4),仅签发时校验本表';

-- -----------------------------------------------------------------------------
-- access_key — AK/SK
--
-- SK 存储方案 (裁决 S5,以 07§2.2/§2.5 为准,推翻 03§6.1 的 sk_hash 单向哈希):
--   验签需要真实 SK 计算 HMAC,故 SK 必须【可逆】—— KMS 信封加密存储
--   (sk_cipher + sk_key_version),不使用单向哈希。
--   SK 仅创建时一次性返回,平台不存明文、不可明文查询。
--   网关验签走"AK→SK 解密副本"二级缓存(Redis TTL 5min,按密文+版本号键控),
--   密钥轮转时按 sk_key_version 失效缓存。
--
-- 每身份最多 2 把 AK;轮转时新 AK 生效后旧 AK 进入 ≤72h 宽限期再自动禁用。
-- 删除为软删,保留 7 天供审计与恢复。
-- -----------------------------------------------------------------------------
CREATE TABLE `access_key` (
  `ak`             VARCHAR(32)    NOT NULL COMMENT '公开标识,32字符,前缀 EU(07§2.5)',
  `account_id`     BIGINT UNSIGNED NOT NULL COMMENT '所属主账号',
  `owner_type`     TINYINT NOT NULL COMMENT '1主账号 2RAM子用户 3角色',
  `owner_id`       BIGINT UNSIGNED DEFAULT NULL COMMENT 'owner_type=1 时为 NULL',
  `sk_cipher`      VARBINARY(512) NOT NULL COMMENT 'SK 的 KMS 信封加密密文(mk-ak);验签需真实SK算HMAC,故可逆存储而非单向哈希(裁决S5/07§2.2)',
  `sk_key_version` INT            NOT NULL COMMENT 'KMS 主密钥版本;轮转时按版本失效验签缓存',
  `status`         TINYINT NOT NULL DEFAULT 1 COMMENT '1启用 2禁用 3已删除(软删,保留7天可恢复)',
  `last_used_at`   DATETIME DEFAULT NULL COMMENT '最后使用时间(泄露排查:一期提供"最后使用时间/来源IP"报表)',
  `last_used_ip`   VARCHAR(45) DEFAULT NULL COMMENT '最后使用来源IP(IPv6兼容)',
  `rotated_at`     DATETIME DEFAULT NULL COMMENT 'SK 轮转时间',
  `grace_until`    DATETIME DEFAULT NULL COMMENT '轮转宽限期截止(≤72h),到期自动置 status=2',
  `deleted_at`     DATETIME DEFAULT NULL COMMENT '软删时间;+7天后物理删除',
  `created_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`ak`),
  KEY `idx_account` (`account_id`,`status`),
  KEY `idx_owner` (`owner_type`,`owner_id`),
  KEY `idx_grace` (`grace_until`) COMMENT '轮转宽限期到期扫描'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='AK表(网关鉴权热表,全量缓存);SK仅创建时一次性展示,平台不存明文、不可明文查询';

-- -----------------------------------------------------------------------------
-- mfa_device — MFA 设备
-- 一期仅 TOTP(Google Authenticator 兼容,±1 个 30s 窗口);短信仅作找回辅助。
-- 硬件密钥/WebAuthn 后置二期(07§2.4)。
-- -----------------------------------------------------------------------------
CREATE TABLE `mfa_device` (
  `id`            BIGINT UNSIGNED NOT NULL,
  `account_id`    BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `owner_type`    TINYINT NOT NULL COMMENT '1主账号 2RAM子用户',
  `owner_id`      BIGINT UNSIGNED NOT NULL,
  `device_type`   TINYINT NOT NULL DEFAULT 1 COMMENT '1TOTP 2短信(仅辅助找回)',
  `secret_cipher` VARBINARY(256) NOT NULL COMMENT 'TOTP 种子的 KMS 信封加密密文(mk-user)',
  `bound_at`      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_owner` (`owner_type`,`owner_id`,`device_type`),
  KEY `idx_account` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='MFA 设备;一期 TOTP,WebAuthn 后置二期';

-- -----------------------------------------------------------------------------
-- ram_policy — 权限策略 (RAM Policy JSON)
--
-- 求值规则 (07§3.3): 默认拒绝 → 任一显式 Deny 即最终 Deny(Deny wins) →
-- 有匹配 Allow 且无 Deny 才 Allow。
-- 一期支持的 Condition 运算符: StringEquals/NotEquals, IpAddress/NotIpAddress,
-- DateGreaterThan/LessThan, Bool。
-- 一期 eu: 前缀上下文键: SourceIp, CurrentTime, MFAPresent, ResourceGroupId。
-- -----------------------------------------------------------------------------
CREATE TABLE `ram_policy` (
  `policy_id`      BIGINT UNSIGNED NOT NULL,
  `account_id`     BIGINT UNSIGNED NOT NULL COMMENT '分片键;系统策略 account_id=0',
  `policy_name`    VARCHAR(64) NOT NULL COMMENT '系统策略名 Sc{Product}FullAccess / Sc{Product}ReadOnlyAccess(07§3.2)',
  `document`       JSON NOT NULL COMMENT '{"Version":"1","Statement":[{Effect,Action,Resource,Condition}]}',
  `scope`          TINYINT NOT NULL COMMENT '1系统策略(平台内置,按产品API注册自动生成) 2自定义策略(租户自建)',
  `policy_version` BIGINT UNSIGNED NOT NULL DEFAULT 1 COMMENT '策略版本号;变更即 ++,经 cloud.sys.authz.policy.changed 广播失效缓存(07§3.5)',
  `description`    VARCHAR(256) DEFAULT NULL,
  `created_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`policy_id`),
  UNIQUE KEY `uk_acc_name` (`account_id`,`policy_name`),
  KEY `idx_scope` (`scope`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='RAM 策略;权限边界(Permission Boundary)后置二期';

-- -----------------------------------------------------------------------------
-- ram_user_policy / ram_group_policy — 策略绑定关系
-- -----------------------------------------------------------------------------
CREATE TABLE `ram_user_policy` (
  `id`         BIGINT UNSIGNED NOT NULL,
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键',
  `user_id`    BIGINT UNSIGNED NOT NULL COMMENT 'iam_user.id',
  `policy_id`  BIGINT UNSIGNED NOT NULL,
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_user_policy` (`user_id`,`policy_id`),
  KEY `idx_account` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户-策略绑定';

CREATE TABLE `ram_group_policy` (
  `id`         BIGINT UNSIGNED NOT NULL,
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键',
  `group_id`   BIGINT UNSIGNED NOT NULL,
  `policy_id`  BIGINT UNSIGNED NOT NULL,
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_group_policy` (`group_id`,`policy_id`),
  KEY `idx_account` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户组-策略绑定';

-- -----------------------------------------------------------------------------
-- resource_group — 资源组 (Project)
--
-- 每个资源创建时必须携带 resource_group_id(缺省 `default`)。
-- 跨资源组移动需要 rg:MoveResource 权限 + 审计(07§3.6)。
-- 强制约束: 所有云产品资源表必须包含
--   account_id VARCHAR/BIGINT NOT NULL, resource_group_id VARCHAR(32) NOT NULL,
--   region VARCHAR(32) NOT NULL + 复合索引 (account_id, resource_group_id)
-- -----------------------------------------------------------------------------
CREATE TABLE `resource_group` (
  `id`         BIGINT UNSIGNED NOT NULL,
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `rg_id`      VARCHAR(32) NOT NULL COMMENT '资源组标识;缺省 default',
  `rg_name`    VARCHAR(64) NOT NULL,
  `status`     TINYINT NOT NULL DEFAULT 1 COMMENT '1正常 2删除中',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_rg_id` (`rg_id`),
  KEY `idx_account` (`account_id`,`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='资源组(Project);ABAC 标签授权 eu:ResourceTag/<key> 后置二期,一期仅资源组维度';

-- -----------------------------------------------------------------------------
-- login_attempt — 登录失败计数 (5 次失败锁定 15 分钟,07§2.4)
-- 热路径实际走 Redis;本表为审计与离线分析留痕。
-- -----------------------------------------------------------------------------
CREATE TABLE `login_attempt` (
  `id`           BIGINT UNSIGNED NOT NULL,
  `account_id`   BIGINT UNSIGNED NOT NULL COMMENT '分片键;未知账号记 0',
  `account_name` VARCHAR(64) NOT NULL COMMENT '尝试的登录名',
  `success`      TINYINT NOT NULL COMMENT '0失败 1成功',
  `source_ip`    VARCHAR(45) NOT NULL,
  `user_agent`   VARCHAR(256) DEFAULT NULL,
  `attempted_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_account_time` (`account_id`,`attempted_at`),
  KEY `idx_name_time` (`account_name`,`attempted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='登录尝试留痕;热路径计数在 Redis,本表供审计与异常登录检测(cloud.user.login.event)';

-- -----------------------------------------------------------------------------
-- outbox_message — 本地消息表 (03§8.2 标准范式)
--
-- 与业务表【同库同事务】写入,由 Outbox Relay 组件扫描投递 Kafka。
-- 资金/资源相关事件禁止业务代码直接发 Kafka。
-- -----------------------------------------------------------------------------
CREATE TABLE `outbox_message` (
  `id`          BIGINT UNSIGNED NOT NULL,
  `account_id`  BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键,与业务表同片',
  `biz_type`    VARCHAR(32) NOT NULL COMMENT 'account/ak/policy',
  `biz_key`     VARCHAR(64) NOT NULL,
  `topic`       VARCHAR(64) NOT NULL COMMENT 'cloud.user.event / cloud.sys.authz.policy.changed 等',
  `partition_key` VARCHAR(64) NOT NULL COMMENT 'Kafka 分区键(account_id)',
  `payload`     MEDIUMTEXT NOT NULL COMMENT '统一信封 {event_id,event_type,occurred_at,aggregate_id,payload}',
  `status`      TINYINT NOT NULL DEFAULT 0 COMMENT '0待发 1已发 2放弃(超阈值告警人工介入)',
  `retry_count` INT NOT NULL DEFAULT 0,
  `next_retry`  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '指数退避下次重试时间',
  `created_at`  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_status_retry` (`status`,`next_retry`) COMMENT 'Relay 扫描索引'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='本地消息表(Outbox);至少一次投递,消费端必须幂等';

-- -----------------------------------------------------------------------------
-- idempotent_record — 幂等表 (03§8.3)
-- 服务间 RPC 与 Kafka 消费前先查本表,唯一索引冲突即视为重复,直接返回已有结果。
-- -----------------------------------------------------------------------------
CREATE TABLE `idempotent_record` (
  `id`         BIGINT UNSIGNED NOT NULL,
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键',
  `biz_type`   VARCHAR(32) NOT NULL,
  `biz_key`    VARCHAR(64) NOT NULL COMMENT 'event_id / task_id / ClientToken',
  `result`     TEXT DEFAULT NULL COMMENT '已有结果快照,重复请求直接返回',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_biz` (`biz_type`,`biz_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='幂等记录;写操作强制 ClientToken(10min窗口)';
