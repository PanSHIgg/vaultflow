-- VaultFlow gateway-core — 004_sweep_release.sql (FIX-B: B2)
--
-- sweep_batch_items.item_status 增加 'RELEASED': 批次 FAILED / 关联出站意图
-- 进入失败终态 / PENDING 批次超时未派发时, 未结条目置 RELEASED 作废,
-- 解除对 vault 精确余额的在途占用 (资金重新可聚合归集).
--
-- sweep_batches.queue_attempts 记录 PENDING 批次重试 QueueIntent 的次数,
-- 供 reaper 判定放弃阈值 (配合 created_at 超时).
--
-- Target: PostgreSQL 16. Forward-only.

BEGIN;

ALTER TABLE sweep_batch_items DROP CONSTRAINT chk_items_status;
ALTER TABLE sweep_batch_items ADD CONSTRAINT chk_items_status CHECK (item_status IN
    ('PENDING', 'SWEPT', 'QUARANTINED_REVERT', 'RELEASED'));

COMMENT ON COLUMN sweep_batch_items.item_status IS
    'FIX-B/B2: +RELEASED — batch terminally failed/expired, vault balance occupation released';

ALTER TABLE sweep_batches
    ADD COLUMN queue_attempts INT NOT NULL DEFAULT 0,
    ADD CONSTRAINT chk_sweep_batches_queue_attempts CHECK (queue_attempts >= 0);

COMMENT ON COLUMN sweep_batches.queue_attempts IS
    'FIX-B/B2: PENDING-batch reaper retry counter for QueueIntent re-dispatch';

COMMIT;
