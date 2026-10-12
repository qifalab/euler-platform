-- =============================================================================
-- V4: account_db — email_verification_code (注册邮箱验证码, web-auth)
--
-- POST /api/auth/register/send-code mails a 6-digit code to an address that is
-- NOT yet an account, so the code cannot live in password_reset_code (keyed
-- by account_id). This table holds pending codes keyed by (purpose, address):
--
--   - purpose separates the flows: a 'register' code is only ever checked by
--     registration, and password-reset codes stay in password_reset_code, so
--     a reset code cannot register an address and a register code cannot
--     reset a password. The code HMAC is also keyed per purpose.
--   - email_hash is a keyed HMAC of the normalised address (EULER_IDENTITY_
--     PEPPER-derived key): addresses of people who never finished signing up
--     are not kept in clear text.
--   - code_hash is a keyed HMAC of (purpose, address, code); the code itself
--     is never stored. TTL 10 minutes, 5 wrong guesses void it; a new send
--     replaces the pending code and resets the guesses.
--
-- 可重入: CREATE TABLE IF NOT EXISTS.
-- =============================================================================

CREATE TABLE IF NOT EXISTS `email_verification_code` (
  `purpose`    VARCHAR(32)  NOT NULL COMMENT '用途: register(注册);与找回密码码互不通用',
  `email_hash` CHAR(64)     NOT NULL COMMENT 'keyed HMAC(规范化邮箱) hex;邮箱明文不落库',
  `code_hash`  CHAR(64)     NOT NULL COMMENT 'keyed HMAC(用途, 邮箱, 验证码) hex;验证码明文从不落库',
  `attempts`   INT          NOT NULL DEFAULT 0 COMMENT '错误尝试次数;达到 5 次作废',
  `expires_at` DATETIME(3)  NOT NULL COMMENT 'UTC;签发 +10 分钟',
  `created_at` DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`purpose`, `email_hash`),
  KEY `idx_expires_at` (`expires_at`) COMMENT '过期清理'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='邮箱验证码(注册等;仅存 keyed hash)';
