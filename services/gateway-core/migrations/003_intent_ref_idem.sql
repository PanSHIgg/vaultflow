-- VaultFlow gateway-core — 003_intent_ref_idem.sql (FIX-B: B3)
--
-- outbound_tx_intents 业务幂等键: 同一 (intent_type, reference_id) 至多一个
-- "存活" 意图 — QueueIntent 调用方重试不再产生第二独立意图
-- (MERCHANT_PAYOUT 双付 / SWEEP_BATCH 撞地址 revert / EIP3009_RELAY 重复广播).
--
-- 部分唯一索引只覆盖存活态 (QUEUED/IN_FLIGHT/INCLUDED_PENDING_SAFE/CONFIRMED/
-- NONCE_STOLEN_HALT): 终态 (FAILED_REVERTED / CANCELLED_BY_FILLER /
-- REJECTED_BY_SIGNER) 行退出索引, 使 §5.4 FAILED_RETRYABLE -> IN_FLIGHT 的
-- 换 Nonce 重试能为同一 reference 分配新意图.
--
-- NONCE_GAP_FILLER 不在约束内: 其 reference_id 语义为父意图/订单关联键,
-- 同一引用可合法存在多个填洞意图.
--
-- Target: PostgreSQL 16. Forward-only.

BEGIN;

CREATE UNIQUE INDEX uq_intents_live_type_ref
    ON outbound_tx_intents (intent_type, reference_id)
    WHERE intent_type IN ('SWEEP_BATCH', 'MERCHANT_PAYOUT', 'EIP3009_RELAY')
      AND status IN ('QUEUED', 'IN_FLIGHT', 'INCLUDED_PENDING_SAFE',
                     'CONFIRMED', 'NONCE_STOLEN_HALT');

COMMIT;
