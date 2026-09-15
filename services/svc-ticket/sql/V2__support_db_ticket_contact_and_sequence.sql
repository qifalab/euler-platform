-- =============================================================================
-- support_db V2 — ticket 落库缺口补齐 (工单通道接入 svc-ticket 的 SQL store)
--
-- V1 的两张表在设计上超前于当时的实现,接真实库时才暴露三处缺口:
--
-- 1. ticket.contact 缺失
--    Ticket.Contact(手机/邮箱)是客服工作台外呼的依据,服务里注释写着"必须落库,
--    不能收集完就丢",但 V1 没有这一列 —— 落库时它无处可写。
--
-- 2. ticket_message.from_user 缺失
--    DTO 暴露 from_user(消息是否来自用户),而 V1 只有 sender。靠"sender 是否等于
--    账号ID"反推是脆的:客服账号与账号ID同形时就会误判,所以按事实存一列。
--
-- 3. id_sequence 缺 ticket / ticket_message 两行
--    V1 的两张表主键是号段发号的数字 ID(04§6.6),而服务在内存模式里用 UUID 充当
--    主键。接上真实库后,UUID 无法写进 BIGINT — 由 store 从号段取号并回写成十进制
--    字符串,对外仍是不可解析的字符串 ID,DTO 与前端契约不变。
--
-- 三处都不是"接库时才有的新需求",而是 V1 的 schema 与实现之间的差:DDL 是记录
-- 在案的真相,差在实现这一侧,所以补在 schema 上并说明原因。
--
-- 依赖:下面的 INSERT 需要 support_db 已存在 id_sequence。该表由本 schema 的第一个
-- 目录 svc-quota(V3)创建并拥有 —— 一个 schema 里同一张表只声明一次(与 outbox_message
-- 同理),manifest 的顺序(quota 在 ticket 之前)保证了执行顺序;单独应用本目录会失败,
-- 这是共享 schema 的固有依赖,测试里显式按同样顺序应用两个目录。
-- =============================================================================

ALTER TABLE `ticket`
  ADD COLUMN `contact` VARCHAR(128) DEFAULT NULL
    COMMENT '客户联系方式;工作台外呼依据,收集了就必须落库' AFTER `assignee`;

ALTER TABLE `ticket_message`
  ADD COLUMN `from_user` TINYINT NOT NULL DEFAULT 1
    COMMENT '1用户 2客服/系统;DTO from_user 的事实来源' AFTER `sender`;

INSERT IGNORE INTO `id_sequence` (`name`, `next_value`, `segment_size`) VALUES
  ('ticket', 1, 1000),
  ('ticket_message', 1, 1000);
