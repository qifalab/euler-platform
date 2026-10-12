-- =============================================================================
-- svc-iam DDL — account_db, V2: CPS1 anti-replay nonces (07§4.2 layer 2)
--
-- The verify endpoint used to keep seen nonces in process memory, so with more
-- than one replica a captured request could be replayed against another
-- replica inside the 15-minute window. With persistence on, nonces live here
-- and are shared by every replica. Keys are SHA-256 hex of
-- "sec:nonce:{ak}:{nonce}" (fixed width whatever the client sends); rows past
-- expires_at are reusable and swept in batches.
-- =============================================================================

CREATE TABLE IF NOT EXISTS `openapi_nonce` (
  `nonce_key`  CHAR(64)    NOT NULL COMMENT 'SHA-256 hex of sec:nonce:{ak}:{nonce}',
  `expires_at` DATETIME(3) NOT NULL COMMENT 'UTC; first instant the nonce may be reused (x-cps-date window + 1 min)',
  `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT 'UTC',
  PRIMARY KEY (`nonce_key`),
  KEY `idx_expires_at` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='CPS1 签名防重放 nonce(多副本共享)';
