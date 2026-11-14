-- VaultFlow gateway-core — 002_intent_payload.sql (FIX-B: B1 + B6)
--
-- outbound_tx_intents 持久化 EIP-1559 业务载荷与流水线错误诊断:
--   1) payload_* 列使 QUEUED/重发路径在进程重启后仍可确定性重建 tx(to,data,
--      value,gas,fee) — 修复 EIP3009_RELAY 仅存内存 pendingParams 导致
--      QUEUED 意图占位已分配 Nonce 永远无法入块的级联死锁 (B1);
--   2) last_error/last_error_at 透出最近一次签名/广播失败原因 (B6);
--   3) chk_intents_status 扩展 'REJECTED_BY_SIGNER' — vault-signer 明确
--      RejectCode 拒签且 GAP_FILLER 兜底亦失败时的显式终态 (需人工处置:
--      该 Nonce 洞须由运维带外填充或解除配置错误后重放).
--
-- Target: PostgreSQL 16. Forward-only — 001_init.sql 冻结不可改.

BEGIN;

-- ============================================================================
-- OUTBOUND_TX_INTENTS — 载荷持久化 + 诊断列
--   payload_to        EIP-1559 to (0x 小写 hex, §2.5.3); NULL = 迁移前旧行
--   payload_data      calldata 原始字节 (含 4B selector); NULL = 无数据
--   payload_value     msg.value wei (合约调用恒 0)
--   payload_gas       gas 上限; NULL/0 → 按 intent_type 默认值重建
--   payload_max_fee   首播 maxFeePerGas (RBF/Dropped 以 attempts 表为准)
--   payload_max_tip   首播 maxPriorityFeePerGas
--   last_error        最近一次签名/广播/重建失败的结构化描述 (B6)
--   last_error_at     该错误发生时间
-- ============================================================================
ALTER TABLE outbound_tx_intents
    ADD COLUMN payload_to      VARCHAR(42),
    ADD COLUMN payload_data    BYTEA,
    ADD COLUMN payload_value   NUMERIC(78,0),
    ADD COLUMN payload_gas     BIGINT,
    ADD COLUMN payload_max_fee NUMERIC(78,0),
    ADD COLUMN payload_max_tip NUMERIC(78,0),
    ADD COLUMN last_error      VARCHAR,
    ADD COLUMN last_error_at   TIMESTAMPTZ;

ALTER TABLE outbound_tx_intents
    ADD CONSTRAINT chk_intents_payload_to
        CHECK (payload_to IS NULL OR payload_to ~ '^0x[0-9a-f]{40}$'),
    ADD CONSTRAINT chk_intents_payload_nonnegative CHECK (
        (payload_value   IS NULL OR payload_value   >= 0) AND
        (payload_gas     IS NULL OR payload_gas     >= 0) AND
        (payload_max_fee IS NULL OR payload_max_fee >= 0) AND
        (payload_max_tip IS NULL OR payload_max_tip >= 0));

-- §5.3 状态机扩展第 8 态 REJECTED_BY_SIGNER (终态; 由 FIX-B 引入, types 包
-- 迁移白名单无出边即天然终态 — Wave-2 统一合拢 Go 侧常量).
ALTER TABLE outbound_tx_intents DROP CONSTRAINT chk_intents_status;
ALTER TABLE outbound_tx_intents ADD CONSTRAINT chk_intents_status CHECK (status IN
    ('QUEUED', 'IN_FLIGHT', 'INCLUDED_PENDING_SAFE', 'CONFIRMED',
     'FAILED_REVERTED', 'CANCELLED_BY_FILLER', 'NONCE_STOLEN_HALT',
     'REJECTED_BY_SIGNER'));

COMMENT ON COLUMN outbound_tx_intents.payload_to      IS 'FIX-B/B1: persisted EIP-1559 to (rebuild after restart)';
COMMENT ON COLUMN outbound_tx_intents.payload_data    IS 'FIX-B/B1: persisted calldata (e.g. payOrderWithAuthorization)';
COMMENT ON COLUMN outbound_tx_intents.payload_value   IS 'FIX-B/B1: persisted msg.value (contract calls = 0)';
COMMENT ON COLUMN outbound_tx_intents.payload_gas     IS 'FIX-B/B1: persisted gas limit; NULL/0 -> type default';
COMMENT ON COLUMN outbound_tx_intents.payload_max_fee IS 'FIX-B/B1: first-broadcast maxFeePerGas';
COMMENT ON COLUMN outbound_tx_intents.payload_max_tip IS 'FIX-B/B1: first-broadcast maxPriorityFeePerGas';
COMMENT ON COLUMN outbound_tx_intents.last_error      IS 'FIX-B/B6: last pipeline failure diagnostic';
COMMENT ON COLUMN outbound_tx_intents.last_error_at   IS 'FIX-B/B6: timestamp of last_error';

COMMIT;
