-- 010_withdrawals.sql
-- 用户提现单: 请求时余额冻结 (available->pending), 确认后烧掉 pending,
-- 失败/拒签回退到 available. 白名单租约注册与冷却经 whitelisted_at 记录.
--
-- 状态机: QUEUED -> WHITELISTED -> IN_FLIGHT -> CONFIRMED
--                          \-------> FAILED (链上 revert / 意图取消, 已退款)
--                          \-------> REJECTED (signer 策略硬拒, 已退款)

BEGIN;

CREATE TABLE withdrawals (
    id             UUID          NOT NULL DEFAULT gen_random_uuid(),
    user_id        UUID          NOT NULL,
    chain_id       BIGINT        NOT NULL,
    asset_symbol   VARCHAR       NOT NULL,
    token_contract VARCHAR       NOT NULL,        -- 该链上 ERC-20 合约 (payout 目标)
    to_address     VARCHAR       NOT NULL,
    amount         NUMERIC(78,0) NOT NULL,
    status         VARCHAR       NOT NULL DEFAULT 'QUEUED',
    whitelisted_at TIMESTAMPTZ,                   -- 白名单租约注册时间 (冷却锚点)
    intent_id      UUID,                          -- 关联 outbound_tx_intents
    tx_hash        VARCHAR,
    fail_reason    VARCHAR       NOT NULL DEFAULT '',
    client_ref     VARCHAR,                         -- Idempotency-Key 幂等槽
    created_at     TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT pk_withdrawals PRIMARY KEY (id),
    CONSTRAINT fk_wd_user FOREIGN KEY (user_id) REFERENCES users (id),
    CONSTRAINT chk_wd_amount CHECK (amount > 0),
    CONSTRAINT chk_wd_status CHECK (status IN
        ('QUEUED', 'WHITELISTED', 'IN_FLIGHT', 'CONFIRMED', 'FAILED', 'REJECTED'))
);

CREATE INDEX idx_withdrawals_user   ON withdrawals (user_id, created_at DESC);
CREATE INDEX idx_withdrawals_status ON withdrawals (status)
    WHERE status IN ('QUEUED', 'WHITELISTED', 'IN_FLIGHT');
-- 幂等槽: 同 (user, client_ref) 只落一行, 重放返回原单.
CREATE UNIQUE INDEX uq_withdrawals_client_ref ON withdrawals (user_id, client_ref)
    WHERE client_ref IS NOT NULL;

COMMIT;
