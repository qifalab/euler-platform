-- =============================================================================
-- support_db V2 — quota_usage.id 补 AUTO_INCREMENT
--
-- V1 declared `id BIGINT UNSIGNED NOT NULL` as the PRIMARY KEY with no
-- AUTO_INCREMENT, and nothing can supply it: the domain's quota.Usage has no id
-- field (its identity is the natural key uk_acc_quota_region —
-- account_id + quota_code + region). The consequence was a table no writer
-- could insert into: every account's first reservation died with
--   Error 1364 (HY000): Field 'id' doesn't have a default value
-- on the INSERT path, and the same error blocked any seeded usage row.
--
-- Why this stayed hidden: the service ran on an in-memory store, so the DDL was
-- never executed. It is exactly the class of defect (a column no caller can
-- fill) that only a real database surfaces — which is what the SQL-store tests
-- in pkg-go/quota now run against this file's schema.
--
-- Note on the platform's id policy: business tables take their ids from the
-- segment service (04§6.6, e.g. ledger_entry.entry_id) and therefore must NOT
-- auto-increment. This column is a pure surrogate with no domain identity, so
-- it is the exception rather than a precedent.
--
-- Written as a NEW V2 file rather than an edit to V1: sqlmigrate records a
-- checksum per applied file and refuses to re-apply an edited one ("write a NEW
-- V<N> migration instead" — tools/sqlmigrate doc). History is append-only, the
-- same rule the ledger's journal follows.
-- =============================================================================

ALTER TABLE `quota_usage`
  MODIFY COLUMN `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;
