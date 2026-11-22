-- 011_payout_intent.sql
-- 提现走 txmanager 统一管线: outbound_tx_intents 放行 MERCHANT_PAYOUT 类型.
BEGIN;
ALTER TABLE outbound_tx_intents DROP CONSTRAINT chk_intents_type;
ALTER TABLE outbound_tx_intents ADD CONSTRAINT chk_intents_type
    CHECK (intent_type IN ('SWEEP_BATCH', 'NONCE_GAP_FILLER', 'MERCHANT_PAYOUT'));
COMMIT;
