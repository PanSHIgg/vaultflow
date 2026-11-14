-- VaultFlow gateway-core — 005_audit_anchor.sql (FIX-E: E7)
--
-- signer_audit_anchor: 巡检协程持久化的 signer 审计链锚点.
-- 每次 VerifyAuditChain 成功校验后, 按 (chain_id, signer_id) 维度记录最近
-- 校验通过的 tip_hash 与 entries_checked — 下一轮巡检对比检出两类篡改痕迹:
--   (a) entries_checked 回退   — 审计日志尾部被物理删除/截断;
--   (b) 计数持平但 tip 漂移    — 同高度链内容被改写.
-- 无锚点行时首轮校验即写首锚 (单调前进随后续轮自动更新).
--
-- Target: PostgreSQL 16. Forward-only.

BEGIN;

CREATE TABLE signer_audit_anchor (
    chain_id             BIGINT      NOT NULL,
    signer_id            VARCHAR     NOT NULL,
    last_tip_hash        VARCHAR     NOT NULL,
    last_entries_checked BIGINT      NOT NULL,
    verified_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pk_signer_audit_anchor PRIMARY KEY (chain_id, signer_id),
    CONSTRAINT chk_audit_anchor_tip_hex
        CHECK (last_tip_hash ~ '^0x[0-9a-f]{64}$'),
    CONSTRAINT chk_audit_anchor_checked_nonneg CHECK (last_entries_checked >= 0)
);

COMMENT ON TABLE signer_audit_anchor IS
    'FIX-E/E7: persisted last-verified signer audit-chain tip per (chain, signer) — tail truncation / tip drift detection';
COMMENT ON COLUMN signer_audit_anchor.last_tip_hash IS 'tip_hash returned by last successful VerifyAuditChain (0x lowercase hex, 32B)';
COMMENT ON COLUMN signer_audit_anchor.last_entries_checked IS 'entries_checked returned by last successful VerifyAuditChain (monotonic progress marker)';
COMMENT ON COLUMN signer_audit_anchor.verified_at IS 'gateway clock at last successful verification';

COMMIT;
