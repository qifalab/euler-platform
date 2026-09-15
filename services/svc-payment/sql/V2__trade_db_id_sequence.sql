-- =============================================================================
-- trade_db V2 — id_sequence (号段分配器, 04§6.6)
--
-- Several tables in trade_db take their primary key from the segment service
-- rather than from AUTO_INCREMENT: ledger_entry.entry_id
-- ("号段服务发号,内嵌分片因子"), resource_pack_journal.entry_id, order ids,
-- payment ids. Business ids carry a shard factor and must be allocated, not
-- counted by the database.
--
-- Until now that allocator had no implementation and the services minted ids
-- from a process-local counter. That works only while the store is in memory:
-- pointed at a real database, the counter restarts at 1 on every process start
-- and the first journal write after a restart collides with the row the previous
-- run already committed (duplicate PRIMARY KEY) — the service cannot record a
-- single entry.
--
-- This table is the allocator's state: one row per sequence, holding the start of
-- the next unissued segment. A caller takes a segment under FOR UPDATE, hands the
-- ids out locally, and comes back only when the segment runs out, so the hot path
-- is not a round trip per id. Segments are allowed to be wasted (a process that
-- dies mid-segment skips the rest): ids must be unique, not gapless.
--
-- Shared trade_db infrastructure, like outbox_message (defined once, in
-- svc-order's V1): do not create a second copy in another directory of this
-- schema — the migration fails on the duplicate, and two drifting copies would
-- silently hand out overlapping segments.
-- =============================================================================

CREATE TABLE `id_sequence` (
  `name`        VARCHAR(64) NOT NULL COMMENT '序列名;约定为消费它的表,如 ledger_entry',
  `next_value`  BIGINT UNSIGNED NOT NULL COMMENT '下一个可发放的号段起点;发号方一次领走一整段',
  `segment_size` BIGINT UNSIGNED NOT NULL DEFAULT 1000 COMMENT '号段长度;重启/崩溃未用完的号段作废',
  `updated_at`  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='号段分配器状态(04§6.6);业务主键不依赖 AUTO_INCREMENT';

-- Seed the sequences the trade_db writers need. INSERT IGNORE keeps the file
-- re-runnable against a database where an operator already added one.
INSERT IGNORE INTO `id_sequence` (`name`, `next_value`, `segment_size`) VALUES
  ('ledger_entry', 1, 1000),
  ('resource_pack_journal', 1, 1000);
