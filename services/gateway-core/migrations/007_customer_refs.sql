-- ============================================================================
-- 007_customer_refs.sql — 终端用户级充值地址 (完整 CEX 模型).
--
-- 006 的开放充值地址粒度从商户级 (merchant,chain,asset) 下钻到终端用户级
-- (merchant, customer_ref, chain, asset):
--   customer_ref = 商户侧终端用户标识 (商户内部 user_id/账号), 由商户在
--   POST /v1/deposit-addresses 时传入; '' 哨兵 = 商户级地址 (006 语义不变).
--   同一 customer_ref 重复请求返回同一常驻 CREATE2 地址 — 与 Coldtill/
--   MakaPay 的 reference-keyed deposit address 模型一致.
--
-- supported_assets.min_deposit = 最小充币额 (原子单位, 0=无下限, CEX 标配):
--   OPEN_DEPOSIT 同币入账低于阈值时不结 MERCHANT_AVAILABLE — 结算腿
--   改挂 SUSPENSE_UNMATCHED_LIAB (商户所有, 运营可人工补记/退回), webhook
--   事件 OPEN_DEPOSIT_BELOW_MIN. 防粉尘归集亏本.
-- ============================================================================

-- NOTE: open_deposit_addresses 由缺失的 006 迁移创建 — IF EXISTS 使本迁移在
-- 无 006 的库上成为 no-op (该表在 008_exchange 中整体 DROP).
ALTER TABLE IF EXISTS open_deposit_addresses
    ADD COLUMN customer_ref VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE IF EXISTS open_deposit_addresses
    DROP CONSTRAINT uq_oda_merchant_chain_asset;
ALTER TABLE IF EXISTS open_deposit_addresses
    ADD CONSTRAINT uq_oda_merchant_customer_chain_asset
        UNIQUE (merchant_id, customer_ref, chain_id, asset_symbol);
DO $$
BEGIN
    IF to_regclass('public.open_deposit_addresses') IS NOT NULL THEN
        CREATE INDEX IF NOT EXISTS idx_oda_merchant_customer
            ON open_deposit_addresses (merchant_id, customer_ref);
    END IF;
END $$;

ALTER TABLE supported_assets
    ADD COLUMN min_deposit NUMERIC(78,0) NOT NULL DEFAULT 0;
ALTER TABLE supported_assets
    ADD CONSTRAINT chk_supported_assets_min_deposit CHECK (min_deposit >= 0);
